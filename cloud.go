package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"thelook-generator/internal/bq"
	"thelook-generator/internal/postgres"
	"thelook-generator/internal/server"
)

type cmdRunner func(name string, args ...string) (string, error)

func defaultRunner(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func resolveProject(project string, run cmdRunner) string {
	if project != "" {
		return project
	}
	if p := os.Getenv("GCP_PROJECT"); p != "" {
		return p
	}
	out, _ := run("gcloud", "config", "get-value", "project")
	return strings.TrimSpace(out)
}

func saveSecret(project, name, val string, run cmdRunner) {
	if val == "" || project == "" {
		return
	}
	if _, err := run("gcloud", "secrets", "describe", name, "--project="+project); err != nil {
		_, _ = run("gcloud", "secrets", "create", name, "--replication-policy=automatic", "--project="+project, "--quiet")
	}
	cmd := exec.Command("gcloud", "secrets", "versions", "add", name, "--data-file=-", "--project="+project, "--quiet")
	cmd.Stdin = strings.NewReader(val)
	_ = cmd.Run()
}

func readSecret(project, name string, run cmdRunner) string {
	if project == "" {
		return ""
	}
	out, _ := run("gcloud", "secrets", "versions", "access", "latest", "--secret="+name, "--project="+project)
	return out
}

func promptIfEmpty(val, label string, secret bool) string {
	if val != "" {
		return val
	}
	fmt.Fprintf(os.Stderr, "%s: ", label)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if secret {
		fmt.Fprintln(os.Stderr)
	}
	return strings.TrimSpace(line)
}

func (d *DeployCmd) runCloud(run cmdRunner) error {
	d.Project = resolveProject(d.Project, run)
	if d.Project == "" {
		return fmt.Errorf("no active GCP project; set --project or GCP_PROJECT")
	}
	if d.Region != "us-central1" && d.Zone == "us-central1-a" {
		d.Zone = d.Region + "-a"
	}
	if d.Days == 0 {
		switch strings.ToLower(d.Mode) {
		case "prod", "production", "full":
			d.Days = 3650
		default:
			d.Days = 7
		}
	}
	if d.Secret == "" {
		d.Secret = readSecret(d.Project, "thelook-dashboard-secret", run)
		if d.Secret == "" {
			d.Secret = fmt.Sprintf("thelook-%d", 100000+rand.IntN(900000))
		}
	}
	if d.UseBinary {
		d.Image = ""
	}
	if d.LookerConn != "" || d.LookerHost != "" || d.LookerPSC || d.LookerInstance != "" || d.LookerBaseURL != "" || d.SAKey != "" {
		d.Looker = true
	}
	if d.Looker {
		d.LookerBaseURL = promptIfEmpty(d.LookerBaseURL, "Looker Base URL (e.g. https://instance.cloud.looker.com)", false)
		d.LookerClientID = promptIfEmpty(d.LookerClientID, "Looker Client ID", false)
		d.LookerSecret = promptIfEmpty(d.LookerSecret, "Looker Client Secret", true)
	}

	apis := []string{"compute.googleapis.com", "logging.googleapis.com", "secretmanager.googleapis.com", "bigquery.googleapis.com"}
	if d.Target == "alloydb" {
		apis = []string{"compute.googleapis.com", "logging.googleapis.com", "secretmanager.googleapis.com", "alloydb.googleapis.com", "servicenetworking.googleapis.com"}
	}
	_, _ = run("gcloud", append([]string{"services", "enable", "--project=" + d.Project}, apis...)...)
	saveSecret(d.Project, "thelook-dashboard-secret", d.Secret, run)
	if d.Looker {
		saveSecret(d.Project, "lookersdk-base-url", d.LookerBaseURL, run)
		saveSecret(d.Project, "lookersdk-client-id", d.LookerClientID, run)
		saveSecret(d.Project, "lookersdk-client-secret", d.LookerSecret, run)
	}

	if d.Target == "alloydb" {
		return d.deployAlloyDB(run)
	}
	return d.deployBQ(run)
}

func bindComputeSARoles(project string, roles []string, run cmdRunner) {
	projNum, _ := run("gcloud", "projects", "describe", project, "--format=value(projectNumber)")
	if projNum == "" {
		return
	}
	sa := projNum + "-compute@developer.gserviceaccount.com"
	fmt.Fprintf(os.Stderr, "==> Ensuring %s has required IAM roles...\n", sa)
	for _, role := range roles {
		_, _ = run("gcloud", "projects", "add-iam-policy-binding", project,
			"--member=serviceAccount:"+sa, "--role="+role, "--condition=None", "--quiet")
	}
}

func (d *DeployCmd) deployBQ(run cmdRunner) error {
	if d.Dataset == "" {
		d.Dataset = "thelook"
	}
	if d.VMName == "" {
		d.VMName = "thelook-bq-gen"
	}
	if d.LookerConn == "" {
		d.LookerConn = "thelook_bq"
	}
	fmt.Fprintf(os.Stderr, "==> Deploying TheLook to BigQuery (%s.%s) [%s: %dd backfill]\n", d.Project, d.Dataset, d.Mode, d.Days)

	bindComputeSARoles(d.Project, []string{
		"roles/logging.logWriter",
		"roles/bigquery.dataEditor",
		"roles/bigquery.jobUser",
		"roles/secretmanager.secretAccessor",
	}, run)

	if _, err := run("gcloud", "alpha", "bq", "datasets", "create", d.Dataset, "--project="+d.Project); err != nil {
		_, _ = run("bq", "--location="+d.Location, "mk", "-d", d.Project+":"+d.Dataset)
	}

	if d.Looker {
		certB64 := ""
		if d.SAKey != "" {
			if raw, err := os.ReadFile(d.SAKey); err == nil {
				certB64 = base64.StdEncoding.EncodeToString(raw)
			}
		}
		fmt.Fprintf(os.Stderr, "==> Registering Looker BigQuery connection '%s' -> %s.%s...\n", d.LookerConn, d.Project, d.Dataset)
		code := `
body = {"name": conn_name, "dialect_name": "bigquery_standard_sql", "host": project_id, "database": dataset}
if cert_b64:
    body["certificate"] = cert_b64
    body["file_type"] = ".json"
existing = [c["name"] for c in all_connections() if c.get("name") == conn_name]
return update_connection(conn_name, body=body) if existing else create_connection(body=body)
`
		_, _ = run("uvx", "--from", "lkr-dev-cli[code-mode]", "lkr-dev-cli",
			"--base-url", d.LookerBaseURL, "--client-id", d.LookerClientID, "--client-secret", d.LookerSecret,
			"code-mode", "sandbox", "-v", "conn_name="+d.LookerConn, "-v", "project_id="+d.Project, "-v", "dataset="+d.Dataset, "-v", "cert_b64="+certB64,
			"--code", strings.TrimSpace(code))
	}

	if d.Local {
		_ = os.Setenv("GCP_PROJECT", d.Project)
		_ = os.Setenv("GCP_DATASET", d.Dataset)
		_ = os.Setenv("SECRET", d.Secret)
		if err := d.deploySchemaOnly(); err != nil {
			return err
		}
		if d.Days > 0 {
			bf := &BackfillCmd{Days: d.Days, State: d.State, Profile: "profile.json", InitialProducts: 14560, HTTP: d.HTTP}
			if err := bf.Run(); err != nil {
				return err
			}
		}
		rc := &RunCmd{State: d.State, Profile: "profile.json", Dataset: d.Project + "." + d.Dataset, InitialProducts: 14560, HTTP: ":" + d.Port}
		return rc.Run()
	}

	_, _ = run("gcloud", "compute", "firewall-rules", "create", "allow-thelook-status",
		"--network="+d.Network, "--allow=tcp:8080", "--target-tags=thelook-gen", "--project="+d.Project)

	envBlock := fmt.Sprintf("export GCP_PROJECT=%q GCP_DATASET=%q SECRET=%q", d.Project, d.Dataset, d.Secret)
	deployCmd := fmt.Sprintf("/usr/local/bin/thelook deploy --project %q --dataset %q", d.Project, d.Dataset)
	unitEnv := fmt.Sprintf("Environment=GCP_PROJECT=%s\nEnvironment=GCP_DATASET=%s\nEnvironment=SECRET=%s", d.Project, d.Dataset, d.Secret)
	startup := buildVMStartupScript(envBlock, deployCmd, unitEnv, d.UseBinary, d.Image, d.Days)
	if err := provisionGeneratorVM(d.Project, d.Zone, d.VMName, d.MachineType, d.Network, startup, run); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "\n=================================================================\n")
	fmt.Fprintf(os.Stderr, "  TheLook BigQuery Generator running on VM: %s (%s)\n", d.VMName, d.Zone)
	fmt.Fprintf(os.Stderr, "  Target Dataset:     %s.%s\n", d.Project, d.Dataset)
	if d.Looker {
		fmt.Fprintf(os.Stderr, "  Looker Connection:  %s\n", d.LookerConn)
	}
	fmt.Fprintf(os.Stderr, "  Web UI Tunnel:      gcloud compute ssh %s --zone=%s -- -L 8080:localhost:8080\n", d.VMName, d.Zone)
	fmt.Fprintf(os.Stderr, "  Live Logs:          gcloud compute ssh %s --zone=%s -- sudo journalctl -u thelook -f\n", d.VMName, d.Zone)
	fmt.Fprintf(os.Stderr, "=================================================================\n")
	return nil
}

func (d *DeployCmd) deployAlloyDB(run cmdRunner) error {
	if d.VMName == "" {
		d.VMName = "thelook-alloydb-gen"
	}
	if d.LookerConn == "" {
		d.LookerConn = "thelook_alloydb"
	}
	if d.Password == "" {
		d.Password = readSecret(d.Project, "thelook-alloydb-password", run)
		if d.Password == "" {
			d.Password = fmt.Sprintf("LookerPass%d!", 100000+rand.IntN(900000))
		}
	}
	fmt.Fprintf(os.Stderr, "==> Provisioning AlloyDB (%s, ZONAL) + Compute Engine VM (%s) (%s) [%s: %dd backfill]\n",
		d.AlloyDBMachineType, d.MachineType, d.Project, d.Mode, d.Days)

	if d.Looker && d.AuthorizedNetworks == "" {
		code := `
res = public_egress_ip_addresses()
ips = res.get("egress_ip_addresses") or []
return ",".join([ip if "/" in ip else f"{ip}/32" for ip in ips])
`
		out, _ := run("uvx", "--from", "lkr-dev-cli[code-mode]", "lkr-dev-cli",
			"--base-url", d.LookerBaseURL, "--client-id", d.LookerClientID, "--client-secret", d.LookerSecret,
			"code-mode", "sandbox", "--code", strings.TrimSpace(code))
		d.AuthorizedNetworks = strings.Trim(out, "\" \n\r")
	}

	bindComputeSARoles(d.Project, []string{"roles/logging.logWriter", "roles/secretmanager.secretAccessor"}, run)

	_, _ = run("gcloud", "compute", "addresses", "create", "alloydb-range", "--global", "--purpose=VPC_PEERING", "--prefix-length=16", "--network="+d.Network, "--project="+d.Project)
	_, _ = run("gcloud", "services", "vpc-peerings", "connect", "--service=servicenetworking.googleapis.com", "--ranges=alloydb-range", "--network="+d.Network, "--project="+d.Project)
	_, _ = run("gcloud", "alloydb", "clusters", "create", d.Cluster, "--region="+d.Region, "--network="+d.Network, "--password="+d.Password, "--project="+d.Project)

	instArgs := []string{"alloydb", "instances", "create", d.Instance,
		"--cluster=" + d.Cluster, "--region=" + d.Region, "--instance-type=PRIMARY",
		"--machine-type=" + d.AlloyDBMachineType, "--availability-type=ZONAL", "--project=" + d.Project}
	var pubFlags []string
	if d.Looker || d.AuthorizedNetworks != "" {
		pubFlags = append(pubFlags, "--assign-inbound-public-ip=ASSIGN_IPV4")
		if d.AuthorizedNetworks != "" {
			pubFlags = append(pubFlags, "--authorized-external-networks="+d.AuthorizedNetworks)
		}
	}
	instArgs = append(instArgs, pubFlags...)
	if _, err := run("gcloud", instArgs...); err != nil && len(pubFlags) > 0 {
		updArgs := append([]string{"alloydb", "instances", "update", d.Instance, "--cluster=" + d.Cluster, "--region=" + d.Region, "--project=" + d.Project}, pubFlags...)
		_, _ = run("gcloud", updArgs...)
	}

	privIP, _ := run("gcloud", "alloydb", "instances", "describe", d.Instance, "--cluster="+d.Cluster, "--region="+d.Region, "--project="+d.Project, "--format=value(ipAddress)")
	pubIP, _ := run("gcloud", "alloydb", "instances", "describe", d.Instance, "--cluster="+d.Cluster, "--region="+d.Region, "--project="+d.Project, "--format=value(publicIpAddress)")
	dbURL := fmt.Sprintf("postgres://postgres:%s@%s:5432/postgres?sslmode=require", d.Password, privIP)

	saveSecret(d.Project, "thelook-alloydb-password", d.Password, run)
	saveSecret(d.Project, "thelook-alloydb-url", dbURL, run)
	if pubIP != "" {
		saveSecret(d.Project, "thelook-alloydb-url-public", fmt.Sprintf("postgres://postgres:%s@%s:5432/postgres?sslmode=require", d.Password, pubIP), run)
	}

	_, _ = run("gcloud", "compute", "firewall-rules", "create", "allow-thelook-status",
		"--network="+d.Network, "--allow=tcp:8080", "--target-tags=thelook-gen", "--project="+d.Project)

	envBlock := fmt.Sprintf("export TARGET_DB=postgres DATABASE_URL=%q SECRET=%q", dbURL, d.Secret)
	deployCmd := "/usr/local/bin/thelook deploy"
	unitEnv := fmt.Sprintf("Environment=TARGET_DB=postgres\nEnvironment=DATABASE_URL=%s\nEnvironment=SECRET=%s", dbURL, d.Secret)
	startup := buildVMStartupScript(envBlock, deployCmd, unitEnv, d.UseBinary, d.Image, d.Days)
	if err := provisionGeneratorVM(d.Project, d.Zone, d.VMName, d.MachineType, d.Network, startup, run); err != nil {
		return err
	}

	if d.Looker {
		d.configureAlloyDBLooker(pubIP, run)
	}

	fmt.Fprintf(os.Stderr, "\n=================================================================\n")
	fmt.Fprintf(os.Stderr, "  AlloyDB Private IP: %s\n", privIP)
	if pubIP != "" {
		fmt.Fprintf(os.Stderr, "  AlloyDB Public IP:  %s\n", pubIP)
	}
	if d.LookerHost != "" {
		fmt.Fprintf(os.Stderr, "  Looker Host:        %s (Connection: %s)\n", d.LookerHost, d.LookerConn)
	}
	fmt.Fprintf(os.Stderr, "  Database URL:       %s\n", dbURL)
	fmt.Fprintf(os.Stderr, "  Generator VM:       %s (%s, %s + 2GB swap)\n", d.VMName, d.Zone, d.MachineType)
	fmt.Fprintf(os.Stderr, "  Web UI Tunnel:      gcloud compute ssh %s --zone=%s -- -L 8080:localhost:8080\n", d.VMName, d.Zone)
	fmt.Fprintf(os.Stderr, "=================================================================\n")
	return nil
}

func (d *DeployCmd) configureAlloyDBLooker(pubIP string, run cmdRunner) {
	if d.LookerInstance == "" {
		full, _ := run("gcloud", "looker", "instances", "list", "--project="+d.Project, "--format=value(name)", "--limit=1")
		if full != "" {
			d.LookerInstance = filepath.Base(full)
			if d.LookerRegion == "" {
				parts := strings.Split(full, "/")
				if len(parts) >= 4 {
					d.LookerRegion = parts[len(parts)-3]
				}
			}
		}
	}
	if d.LookerRegion == "" {
		d.LookerRegion = "us-east1"
	}
	if d.LookerInstance != "" && !d.LookerPSC {
		desc, _ := run("gcloud", "looker", "instances", "describe", d.LookerInstance, "--region="+d.LookerRegion, "--project="+d.Project, "--format=json")
		if strings.Contains(desc, `"pscEnabled": true`) || strings.Contains(desc, `"controlledEgressEnabled": true`) {
			d.LookerPSC = true
		}
	}
	if d.LookerPSC && pubIP != "" {
		if d.LookerNetwork == "" && d.LookerInstance != "" {
			vpcFull, _ := run("gcloud", "looker", "instances", "describe", d.LookerInstance, "--region="+d.LookerRegion, "--project="+d.Project, "--format=value(pscConfig.allowedVpcs[0])")
			if vpcFull != "" {
				d.LookerNetwork = filepath.Base(vpcFull)
			}
		}
		if d.LookerNetwork == "" {
			d.LookerNetwork = "looker-psc-demo"
		}
		_, _ = run("gcloud", "compute", "networks", "subnets", "create", "alloydb-psc-nat-subnet",
			"--network="+d.LookerNetwork, "--region="+d.LookerRegion, "--range=172.16.30.0/28", "--purpose=PRIVATE_SERVICE_CONNECT", "--project="+d.Project, "--quiet")
		_, _ = run("gcloud", "compute", "network-endpoint-groups", "create", "alloydb-internet-neg",
			"--region="+d.LookerRegion, "--network-endpoint-type=internet-ip-port", "--project="+d.Project, "--quiet")
		_, _ = run("gcloud", "compute", "network-endpoint-groups", "update", "alloydb-internet-neg",
			"--region="+d.LookerRegion, "--add-endpoint=ip="+pubIP+",port=5432", "--project="+d.Project, "--quiet")
		_, _ = run("gcloud", "compute", "backend-services", "create", "alloydb-backend-svc",
			"--load-balancing-scheme=INTERNAL_MANAGED", "--protocol=TCP", "--region="+d.LookerRegion, "--project="+d.Project, "--quiet")
		_, _ = run("gcloud", "compute", "backend-services", "add-backend", "alloydb-backend-svc",
			"--region="+d.LookerRegion, "--network-endpoint-group=alloydb-internet-neg", "--project="+d.Project, "--quiet")
		_, _ = run("gcloud", "compute", "target-tcp-proxies", "create", "alloydb-lb-tcp-proxy",
			"--backend-service=alloydb-backend-svc", "--region="+d.LookerRegion, "--project="+d.Project, "--quiet")
		frSubnet, _ := run("gcloud", "compute", "networks", "subnets", "list", "--network="+d.LookerNetwork, "--regions="+d.LookerRegion, "--project="+d.Project, "--filter=range:172.16.20.0/28 OR name:producer-psc-fr-subnet", "--format=value(name)", "--limit=1")
		if frSubnet == "" {
			frSubnet = "producer-psc-fr-subnet"
		}
		_, _ = run("gcloud", "compute", "forwarding-rules", "create", "alloydb-psc-fr",
			"--region="+d.LookerRegion, "--load-balancing-scheme=INTERNAL_MANAGED", "--network="+d.LookerNetwork, "--subnet="+frSubnet,
			"--target-tcp-proxy=alloydb-lb-tcp-proxy", "--target-tcp-proxy-region="+d.LookerRegion, "--ports=5432", "--project="+d.Project, "--quiet")
		_, _ = run("gcloud", "compute", "service-attachments", "create", "alloydb-svc-attachment",
			"--region="+d.LookerRegion, "--producer-forwarding-rule=alloydb-psc-fr", "--connection-preference=ACCEPT_AUTOMATIC", "--nat-subnets=alloydb-psc-nat-subnet", "--project="+d.Project, "--quiet")

		saURI := fmt.Sprintf("projects/%s/regions/%s/serviceAttachments/alloydb-svc-attachment", d.Project, d.LookerRegion)
		if d.LookerInstance != "" {
			attachFlags := existingLookerPSCAttachments(d.Project, d.LookerRegion, d.LookerInstance, d.PSCDomain, run)
			attachFlags = append(attachFlags, fmt.Sprintf("--psc-service-attachment=domain=%s,attachment=%s", d.PSCDomain, saURI))
			updArgs := append([]string{"looker", "instances", "update", d.LookerInstance, "--region=" + d.LookerRegion, "--project=" + d.Project, "--quiet"}, attachFlags...)
			_, _ = run("gcloud", updArgs...)
		}
		d.LookerHost = d.PSCDomain
		saveSecret(d.Project, "thelook-alloydb-psc-host", d.PSCDomain, run)
	}
	if d.LookerHost == "" {
		d.LookerHost = readSecret(d.Project, "thelook-alloydb-psc-host", run)
		if d.LookerHost == "" {
			d.LookerHost = pubIP
		}
	}
	if d.LookerHost != "" {
		fmt.Fprintf(os.Stderr, "==> Registering Looker connection '%s' -> %s:5432...\n", d.LookerConn, d.LookerHost)
		code := `
body = {"name": conn_name, "dialect_name": "alloydb", "host": host, "port": "5432", "database": "postgres", "schema": "public", "username": "postgres", "password": password, "ssl": True}
existing = [c["name"] for c in all_connections() if c.get("name") == conn_name]
return update_connection(conn_name, body=body) if existing else create_connection(body=body)
`
		_, _ = run("uvx", "--from", "lkr-dev-cli[code-mode]", "lkr-dev-cli",
			"--base-url", d.LookerBaseURL, "--client-id", d.LookerClientID, "--client-secret", d.LookerSecret,
			"code-mode", "sandbox", "-v", "conn_name="+d.LookerConn, "-v", "host="+d.LookerHost, "-v", "password="+d.Password,
			"--code", strings.TrimSpace(code))
	}
}

func existingLookerPSCAttachments(project, region, instance, excludeDomain string, run cmdRunner) []string {
	raw, err := run("gcloud", "looker", "instances", "describe", instance, "--region="+region, "--project="+project, "--format=json")
	if err != nil || raw == "" {
		return nil
	}
	var data struct {
		PSCConfig struct {
			ServiceAttachments []struct {
				LocalFQDN                  string `json:"localFqdn"`
				TargetServiceAttachmentURI string `json:"targetServiceAttachmentUri"`
			} `json:"serviceAttachments"`
		} `json:"pscConfig"`
	}
	if json.Unmarshal([]byte(raw), &data) != nil {
		return nil
	}
	var flags []string
	for _, sa := range data.PSCConfig.ServiceAttachments {
		if sa.LocalFQDN != "" && sa.LocalFQDN != excludeDomain {
			flags = append(flags, fmt.Sprintf("--psc-service-attachment=domain=%s,attachment=%s", sa.LocalFQDN, sa.TargetServiceAttachmentURI))
		}
	}
	return flags
}

func buildVMStartupScript(envBlock, deployCmd, unitEnv string, useBinary bool, image string, backfillDays int) string {
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
exec > /var/log/thelook-startup.log 2>&1

if [ ! -f /swapfile ]; then
  fallocate -l 2G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile
  echo '/swapfile none swap sw 0 0' >> /etc/fstab
fi

if ! command -v google-cloud-ops-agent &>/dev/null; then
  curl -sSO https://dl.google.com/cloudagents/add-google-cloud-ops-agent-repo.sh 2>/dev/null || true
  [ -f add-google-cloud-ops-agent-repo.sh ] && bash add-google-cloud-ops-agent-repo.sh --also-install 2>/dev/null || true
  rm -f add-google-cloud-ops-agent-repo.sh
fi

mkdir -p /var/lib/thelook
%s

USE_BINARY=%q
IMAGE=%q
FALLBACK_TO_BINARY="false"

if [ "${USE_BINARY}" != "true" ] && [ -n "${IMAGE}" ]; then
  apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y docker.io ca-certificates
  REGISTRY_HOST="$(echo "${IMAGE}" | cut -d/ -f1)"
  gcloud auth configure-docker "${REGISTRY_HOST}" --quiet || true
  if docker pull "${IMAGE}"; then
    CID=$(docker create "${IMAGE}")
    docker cp "${CID}:/usr/local/bin/thelook" /usr/local/bin/thelook
    docker rm "${CID}"
    chmod +x /usr/local/bin/thelook
  else
    FALLBACK_TO_BINARY="true"
  fi
fi

if [ "${USE_BINARY}" = "true" ] || [ ! -f /usr/local/bin/thelook ] || [ "${FALLBACK_TO_BINARY}" = "true" ]; then
  apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y git wget ca-certificates
  export HOME=/root GOPATH=/root/go GOMODCACHE=/root/go/pkg/mod
  mkdir -p /root/go/pkg/mod
  if ! command -v go &>/dev/null; then
    wget -qO- https://go.dev/dl/go1.22.2.linux-amd64.tar.gz | tar -C /usr/local -xzf -
    ln -sf /usr/local/go/bin/go /usr/local/bin/go
  fi
  mkdir -p /opt/thelook && cd /opt/thelook
  [ -d "thelook-generator" ] || git clone https://github.com/lkrdev/thelook-generator.git
  cd thelook-generator
  GOTOOLCHAIN=auto /usr/local/bin/go build -ldflags="-s -w" -o /usr/local/bin/thelook .
fi

%s
if [ %d -gt 0 ]; then
  /usr/local/bin/thelook backfill --days %d --state /var/lib/thelook/state.gob
fi

cat <<UNIT > /etc/systemd/system/thelook.service
[Unit]
Description=TheLook Data Generator
After=network-online.target

[Service]
Type=simple
%s
ExecStart=/usr/local/bin/thelook run --state /var/lib/thelook/state.gob --http :8080
Restart=always

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload && systemctl enable --now thelook.service
`, envBlock, strconv.FormatBool(useBinary), image, deployCmd, backfillDays, backfillDays, unitEnv)
}

func provisionGeneratorVM(project, zone, vmName, machineType, network, startup string, run cmdRunner) error {
	f, err := os.CreateTemp("", "thelook-startup-*.sh")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(startup); err != nil {
		f.Close()
		return err
	}
	f.Close()

	if _, err := run("gcloud", "compute", "instances", "create", vmName,
		"--zone="+zone, "--machine-type="+machineType, "--network="+network,
		"--tags=thelook-gen", "--scopes=cloud-platform", "--shielded-secure-boot",
		"--metadata-from-file=startup-script="+f.Name(), "--project="+project); err != nil {
		_, err = run("gcloud", "compute", "instances", "add-metadata", vmName,
			"--zone="+zone, "--metadata-from-file=startup-script="+f.Name(), "--project="+project)
		return err
	}
	return nil
}

func (d *DestroyCmd) execute(in io.Reader, run cmdRunner) error {
	if d.Region != "us-central1" && d.Zone == "us-central1-a" {
		d.Zone = d.Region + "-a"
	}
	if d.DeleteTables {
		// Legacy --delete-tables mode: only drop the 14 database tables + local state
		d.VM = false
		d.Looker = false
		d.PSC = false
		d.Secrets = false
		d.ServiceAccount = false
		d.Database = true
	}

	hasCloudWork := d.VM || d.Database || d.Looker || d.PSC || d.Secrets || d.ServiceAccount
	if hasCloudWork {
		d.Project = resolveProject(d.Project, run)
	}

	if hasCloudWork && !d.Yes && !d.DryRun {
		confirmTarget := d.Project
		if isPostgres() && d.DeleteTables {
			confirmTarget = postgresSchema(d.Dataset)
			fmt.Fprintf(os.Stderr, "Confirm deleting all %d tables in PostgreSQL schema %s by typing %q: ", len(postgres.AllTables), confirmTarget, confirmTarget)
		} else if d.Project != "" && d.Dataset != "" {
			confirmTarget = d.Project + "." + d.Dataset
			fmt.Fprintf(os.Stderr, "Confirm deleting all %d tables in %s by typing %q: ", len(bq.AllTables), confirmTarget, confirmTarget)
		} else if confirmTarget != "" {
			fmt.Fprintf(os.Stderr, "Confirm tearing down TheLook cloud resources in %s by typing %q: ", confirmTarget, confirmTarget)
		}
		if confirmTarget != "" {
			line, err := bufio.NewReader(in).ReadString('\n')
			if err != nil && err != io.EOF {
				return fmt.Errorf("reading confirmation: %w", err)
			}
			if got := strings.TrimSpace(line); got != confirmTarget {
				return fmt.Errorf("aborted: confirmation %q did not match %q", got, confirmTarget)
			}
		}
	}

	doBQ := d.Target == "bq" || d.Target == "all"
	doAlloy := d.Target == "alloydb" || d.Target == "all"

	execStep := func(name string, args ...string) (string, error) {
		if d.DryRun {
			fmt.Fprintf(os.Stderr, "[dry-run] %s %s\n", name, strings.Join(args, " "))
			return "", nil
		}
		return run(name, args...)
	}

	// 1. Looker connections & PSC attachment (run before deleting Secret Manager credentials)
	if d.Looker && !d.DeleteTables {
		if d.LookerBaseURL == "" && d.Project != "" && !d.DryRun {
			d.LookerBaseURL = readSecret(d.Project, "lookersdk-base-url", run)
			d.LookerClientID = readSecret(d.Project, "lookersdk-client-id", run)
			d.LookerSecret = readSecret(d.Project, "lookersdk-client-secret", run)
		}
		var conns []string
		if d.LookerConn != "" {
			conns = []string{d.LookerConn}
		} else {
			if doBQ {
				conns = append(conns, "thelook_bq")
			}
			if doAlloy {
				conns = append(conns, "thelook_alloydb")
			}
		}
		if d.LookerBaseURL != "" || d.DryRun {
			code := `
existing = [c["name"] for c in all_connections() if c.get("name") == conn_name]
if existing:
    delete_connection(conn_name)
`
			for _, conn := range conns {
				fmt.Fprintf(os.Stderr, "==> Removing Looker connection '%s'...\n", conn)
				_, _ = execStep("uvx", "--from", "lkr-dev-cli[code-mode]", "lkr-dev-cli",
					"--base-url", d.LookerBaseURL, "--client-id", d.LookerClientID, "--client-secret", d.LookerSecret,
					"code-mode", "sandbox", "-v", "conn_name="+conn, "--code", strings.TrimSpace(code))
			}
		}
		if doAlloy && d.Project != "" && d.LookerInstance != "" {
			rem := existingLookerPSCAttachments(d.Project, d.LookerRegion, d.LookerInstance, d.PSCDomain, execStep)
			updArgs := []string{"looker", "instances", "update", d.LookerInstance, "--region=" + d.LookerRegion, "--project=" + d.Project, "--quiet"}
			if len(rem) > 0 {
				updArgs = append(updArgs, rem...)
			} else {
				updArgs = append(updArgs, "--clear-psc-service-attachments")
			}
			_, _ = execStep("gcloud", updArgs...)
		}
	}

	// 2. Looker Core Hybrid PSC networking pipeline (reverse dependency order)
	if d.PSC && doAlloy && d.Project != "" {
		fmt.Fprintf(os.Stderr, "==> Tearing down PSC pipeline in %s...\n", d.LookerRegion)
		_, _ = execStep("gcloud", "compute", "service-attachments", "delete", "alloydb-svc-attachment", "--region="+d.LookerRegion, "--project="+d.Project, "--quiet")
		_, _ = execStep("gcloud", "compute", "forwarding-rules", "delete", "alloydb-psc-fr", "--region="+d.LookerRegion, "--project="+d.Project, "--quiet")
		_, _ = execStep("gcloud", "compute", "target-tcp-proxies", "delete", "alloydb-lb-tcp-proxy", "--region="+d.LookerRegion, "--project="+d.Project, "--quiet")
		_, _ = execStep("gcloud", "compute", "backend-services", "delete", "alloydb-backend-svc", "--region="+d.LookerRegion, "--project="+d.Project, "--quiet")
		_, _ = execStep("gcloud", "compute", "network-endpoint-groups", "delete", "alloydb-internet-neg", "--region="+d.LookerRegion, "--project="+d.Project, "--quiet")
		_, _ = execStep("gcloud", "compute", "networks", "subnets", "delete", "alloydb-psc-nat-subnet", "--region="+d.LookerRegion, "--project="+d.Project, "--quiet")
	}

	// 3. Compute Engine generator VMs & status firewall rule
	if d.VM && d.Project != "" {
		var vms []string
		if d.VMName != "" {
			vms = []string{d.VMName}
		} else {
			if doBQ {
				vms = append(vms, "thelook-bq-gen")
			}
			if doAlloy {
				vms = append(vms, "thelook-alloydb-gen")
			}
		}
		for _, vm := range vms {
			fmt.Fprintf(os.Stderr, "==> Deleting Compute Engine VM '%s' (%s)...\n", vm, d.Zone)
			_, _ = execStep("gcloud", "compute", "instances", "delete", vm, "--zone="+d.Zone, "--project="+d.Project, "--quiet")
		}
		_, _ = execStep("gcloud", "compute", "firewall-rules", "delete", "allow-thelook-status", "--project="+d.Project, "--quiet")
	}

	// 4. Database tables, BigQuery dataset, and AlloyDB cluster/instance
	if d.Database {
		if isPostgres() && !d.DryRun {
			sch := postgresSchema(d.Dataset)
			sink, err := postgres.NewSink(context.Background(), "", sch)
			if err != nil {
				if d.DeleteTables {
					return err
				}
			} else {
				defer sink.Close()
				fmt.Fprintf(os.Stderr, "Dropping %d tables in %s...\n", len(postgres.AllTables), sch)
				if err := sink.DropTables(context.Background()); err != nil && d.DeleteTables {
					return fmt.Errorf("failed to drop tables: %w", err)
				}
			}
		} else if doBQ && d.Project != "" && d.Dataset != "" {
			if d.DeleteTables && !d.DryRun {
				ddl := bq.DropTablesDDL(d.Project, d.Dataset)
				fmt.Fprintf(os.Stderr, "Dropping %d tables in %s.%s...\n", len(bq.AllTables), d.Project, d.Dataset)
				if _, err := bq.RunQuery(d.Project, ddl, false, 0); err != nil {
					return fmt.Errorf("failed to drop tables: %w", err)
				}
			} else {
				fmt.Fprintf(os.Stderr, "==> Removing BigQuery dataset '%s:%s'...\n", d.Project, d.Dataset)
				_, _ = execStep("bq", "rm", "-r", "-f", "-d", d.Project+":"+d.Dataset)
			}
		}
		if doAlloy && !d.DeleteTables && d.Project != "" {
			fmt.Fprintf(os.Stderr, "==> Deleting AlloyDB instance '%s' and cluster '%s'...\n", d.Instance, d.Cluster)
			_, _ = execStep("gcloud", "alloydb", "instances", "delete", d.Instance, "--cluster="+d.Cluster, "--region="+d.Region, "--project="+d.Project, "--quiet")
			_, _ = execStep("gcloud", "alloydb", "clusters", "delete", d.Cluster, "--region="+d.Region, "--project="+d.Project, "--force", "--quiet")
			_, _ = execStep("gcloud", "compute", "addresses", "delete", "alloydb-range", "--global", "--project="+d.Project, "--quiet")
		}
	}

	// 5. Secret Manager secrets
	if d.Secrets && d.Project != "" {
		secrets := []string{"thelook-dashboard-secret", "lookersdk-base-url", "lookersdk-client-id", "lookersdk-client-secret"}
		if doAlloy {
			secrets = append(secrets, "thelook-alloydb-password", "thelook-alloydb-url", "thelook-alloydb-url-public", "thelook-alloydb-psc-host")
		}
		fmt.Fprintf(os.Stderr, "==> Deleting secrets from Secret Manager...\n")
		for _, sec := range secrets {
			_, _ = execStep("gcloud", "secrets", "delete", sec, "--project="+d.Project, "--quiet")
		}
	}

	// 6. Compute Engine Service Account IAM bindings
	if d.ServiceAccount && d.Project != "" {
		projNum := "000000"
		if !d.DryRun {
			projNum, _ = run("gcloud", "projects", "describe", d.Project, "--format=value(projectNumber)")
		}
		if projNum != "" {
			sa := projNum + "-compute@developer.gserviceaccount.com"
			roles := []string{"roles/logging.logWriter", "roles/secretmanager.secretAccessor"}
			if doBQ {
				roles = append(roles, "roles/bigquery.dataEditor", "roles/bigquery.jobUser")
			}
			fmt.Fprintf(os.Stderr, "==> Revoking IAM role bindings from %s...\n", sa)
			for _, role := range roles {
				_, _ = execStep("gcloud", "projects", "remove-iam-policy-binding", d.Project,
					"--member=serviceAccount:"+sa, "--role="+role, "--quiet")
			}
		}
	}

	// 7. Local state files
	if d.LocalState && !d.DryRun {
		for _, p := range []string{d.State, d.State + ".tmp", server.CommandLogPath(d.State), d.Profile} {
			if err := os.Remove(p); err == nil {
				fmt.Fprintf(os.Stderr, "Removed %s\n", p)
			}
		}
	}
	return nil
}
