#!/usr/bin/env bash
# deploy-alloydb.sh - Minimally provision AlloyDB and a separate Compute Engine VM for TheLook.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

PROJECT="${GCP_PROJECT:-$(gcloud config get-value project 2>/dev/null || true)}"
REGION="us-central1"
ZONE="us-central1-a"
NETWORK="default"
CLUSTER_ID="thelook-cluster"
INSTANCE_ID="thelook-primary"
ALLOYDB_MACHINE_TYPE="c4a-highmem-1"
VM_NAME="thelook-alloydb-gen"
MACHINE_TYPE="e2-micro"
DEFAULT_CONTAINER_IMAGE="us-central1-docker.pkg.dev/lkr-dev-production/thelook-generator/thelook-generator:latest"
IMAGE="${IMAGE:-${DEFAULT_CONTAINER_IMAGE}}"
USE_BINARY="false"
MODE="testing"
BACKFILL_DAYS=""
DB_PASSWORD="${DB_PASSWORD:-}"
SECRET="${SECRET:-}"
PORT="8080"
SETUP_LOOKER="false"
LOOKER_CONNECTION="thelook_alloydb"
LOOKER_HOST="${LOOKER_HOST:-}"
LOOKER_PSC="false"
LOOKER_INSTANCE="${LOOKER_INSTANCE:-}"
LOOKER_REGION="${LOOKER_REGION:-}"
LOOKER_NETWORK="${LOOKER_NETWORK:-}"
PSC_DOMAIN="${PSC_DOMAIN:-alloydb.thelook.internal}"
LOOKERSDK_BASE_URL="${LOOKERSDK_BASE_URL:-}"
LOOKERSDK_CLIENT_ID="${LOOKERSDK_CLIENT_ID:-}"
LOOKERSDK_CLIENT_SECRET="${LOOKERSDK_CLIENT_SECRET:-}"
AUTHORIZED_NETWORKS=""

usage() {
  echo "Usage: ./deploy-alloydb.sh [--mode testing|production] [--project ID] [--region REGION] [--zone ZONE] [--days N] [--image URI] [--use-binary] [--vm-name NAME] [--machine-type TYPE] [--looker] [--looker-psc] [--looker-instance NAME] [--looker-region REGION] [--looker-network VPC] [--psc-domain DOMAIN] [--looker-connection NAME] [--looker-base-url URL] [--looker-client-id ID] [--looker-client-secret SECRET] [--authorized-networks CIDRS]"
  exit 0
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --project)              PROJECT="$2"; shift 2 ;;
    --region)               REGION="$2"; ZONE="${REGION}-a"; shift 2 ;;
    --zone)                 ZONE="$2"; shift 2 ;;
    --network)              NETWORK="$2"; shift 2 ;;
    --cluster)              CLUSTER_ID="$2"; shift 2 ;;
    --instance)             INSTANCE_ID="$2"; shift 2 ;;
    --vm-name)              VM_NAME="$2"; shift 2 ;;
    --machine-type)         MACHINE_TYPE="$2"; shift 2 ;;
    --image)                IMAGE="$2"; USE_BINARY="false"; shift 2 ;;
    --use-binary)           USE_BINARY="true"; IMAGE=""; shift 1 ;;
    --mode)                 MODE="$(echo "$2" | tr '[:upper:]' '[:lower:]')"; shift 2 ;;
    --days)                 BACKFILL_DAYS="$2"; shift 2 ;;
    --password)             DB_PASSWORD="$2"; shift 2 ;;
    --secret)               SECRET="$2"; shift 2 ;;
    --port)                 PORT="$2"; shift 2 ;;
    --looker)               SETUP_LOOKER="true"; shift 1 ;;
    --looker-psc)           SETUP_LOOKER="true"; LOOKER_PSC="true"; shift 1 ;;
    --looker-instance)      SETUP_LOOKER="true"; LOOKER_INSTANCE="$2"; shift 2 ;;
    --looker-region)        SETUP_LOOKER="true"; LOOKER_REGION="$2"; shift 2 ;;
    --looker-network)       SETUP_LOOKER="true"; LOOKER_NETWORK="$2"; shift 2 ;;
    --psc-domain)           PSC_DOMAIN="$2"; shift 2 ;;
    --looker-connection)    SETUP_LOOKER="true"; LOOKER_CONNECTION="$2"; shift 2 ;;
    --looker-host)          SETUP_LOOKER="true"; LOOKER_HOST="$2"; shift 2 ;;
    --looker-base-url)      SETUP_LOOKER="true"; LOOKERSDK_BASE_URL="$2"; shift 2 ;;
    --looker-client-id)     SETUP_LOOKER="true"; LOOKERSDK_CLIENT_ID="$2"; shift 2 ;;
    --looker-client-secret) SETUP_LOOKER="true"; LOOKERSDK_CLIENT_SECRET="$2"; shift 2 ;;
    --authorized-networks)  AUTHORIZED_NETWORKS="$2"; shift 2 ;;
    -h|--help)              usage ;;
    *) echo "Unknown arg: $1" >&2; exit 1 ;;
  esac
done

[[ -z "${PROJECT}" ]] && { echo "Error: no active GCP project. Set --project or GCP_PROJECT." >&2; exit 1; }

if [[ -z "${BACKFILL_DAYS}" ]]; then
  case "${MODE}" in
    prod|production|full) BACKFILL_DAYS=3650 ;;
    *)                    BACKFILL_DAYS=7 ;;
  esac
fi

save_secret() {
  local name="$1"
  local val="$2"
  [[ -z "${val}" ]] && return 0
  if ! gcloud secrets describe "${name}" --project="${PROJECT}" &>/dev/null; then
    gcloud secrets create "${name}" --replication-policy="automatic" --project="${PROJECT}" --quiet >/dev/null 2>&1 || true
  fi
  printf "%s" "${val}" | gcloud secrets versions add "${name}" --data-file=- --project="${PROJECT}" --quiet >/dev/null 2>&1 || true
}

# Load secrets from Secret Manager if not set via CLI or environment
if [[ -z "${DB_PASSWORD}" ]]; then
  DB_PASSWORD="$(gcloud secrets versions access latest --secret="thelook-alloydb-password" --project="${PROJECT}" 2>/dev/null || true)"
  [[ -z "${DB_PASSWORD}" ]] && DB_PASSWORD="LookerPass${RANDOM}${RANDOM}!"
fi

if [[ -z "${SECRET}" ]]; then
  SECRET="$(gcloud secrets versions access latest --secret="thelook-dashboard-secret" --project="${PROJECT}" 2>/dev/null || true)"
  [[ -z "${SECRET}" ]] && SECRET="thelook-${RANDOM}${RANDOM}"
fi

echo "==> Provisioning AlloyDB (${ALLOYDB_MACHINE_TYPE}, ZONAL) + Compute Engine VM (${MACHINE_TYPE}) (${PROJECT}) [${MODE}: ${BACKFILL_DAYS}d backfill]"

# Optional: Prompt for Looker credentials & fetch Looker public_egress_ip_addresses if --looker is set
if [[ "${SETUP_LOOKER}" == "true" ]]; then
  [[ -z "${LOOKERSDK_BASE_URL}" ]] && read -rp "Looker Base URL (e.g. https://instance.cloud.looker.com): " LOOKERSDK_BASE_URL
  [[ -z "${LOOKERSDK_CLIENT_ID}" ]] && read -rp "Looker Client ID: " LOOKERSDK_CLIENT_ID
  if [[ -z "${LOOKERSDK_CLIENT_SECRET}" ]]; then
    read -rsp "Looker Client Secret: " LOOKERSDK_CLIENT_SECRET
    echo ""
  fi
  export LOOKERSDK_BASE_URL LOOKERSDK_CLIENT_ID LOOKERSDK_CLIENT_SECRET

  if [[ -z "${AUTHORIZED_NETWORKS}" ]]; then
    if ! command -v uvx &>/dev/null; then
      echo "Warning: 'uvx' command not found. Cannot fetch Looker public_egress_ip_addresses automatically." >&2
    else
      echo "==> Fetching Looker public egress IP addresses via Looker SDK (public_egress_ip_addresses)..."
      AUTHORIZED_NETWORKS="$(uvx --from "lkr-dev-cli[code-mode]" lkr-dev-cli \
        --base-url "${LOOKERSDK_BASE_URL}" \
        --client-id "${LOOKERSDK_CLIENT_ID}" \
        --client-secret "${LOOKERSDK_CLIENT_SECRET}" \
        code-mode sandbox --code '
res = public_egress_ip_addresses()
ips = res.get("egress_ip_addresses") or []
return ",".join([ip if "/" in ip else f"{ip}/32" for ip in ips])
' 2>/dev/null | tr -d '"' || true)"
      echo "    Looker Egress CIDRs: ${AUTHORIZED_NETWORKS:-<none returned; pass --authorized-networks if on Looker Core>}"
    fi
  fi
fi

# 1. Enable required APIs & ensure Compute SA has Logging & Secret Manager roles
echo "==> Enabling required GCP APIs..."
gcloud services enable alloydb.googleapis.com compute.googleapis.com servicenetworking.googleapis.com logging.googleapis.com secretmanager.googleapis.com --project="${PROJECT}"

PROJECT_NUMBER="$(gcloud projects describe "${PROJECT}" --format="value(projectNumber)" 2>/dev/null || true)"
if [[ -n "${PROJECT_NUMBER}" ]]; then
  COMPUTE_SA="${PROJECT_NUMBER}-compute@developer.gserviceaccount.com"
  echo "==> Ensuring ${COMPUTE_SA} has required IAM roles..."
  gcloud projects add-iam-policy-binding "${PROJECT}" \
    --member="serviceAccount:${COMPUTE_SA}" \
    --role="roles/logging.logWriter" \
    --condition=None --quiet >/dev/null 2>&1 || true
  gcloud projects add-iam-policy-binding "${PROJECT}" \
    --member="serviceAccount:${COMPUTE_SA}" \
    --role="roles/secretmanager.secretAccessor" \
    --condition=None --quiet >/dev/null 2>&1 || true
fi

# 2. Private Services Access VPC peering
gcloud compute addresses create alloydb-range --global --purpose=VPC_PEERING --prefix-length=16 --network="${NETWORK}" --project="${PROJECT}" 2>/dev/null || true
gcloud services vpc-peerings connect --service=servicenetworking.googleapis.com --ranges=alloydb-range --network="${NETWORK}" --project="${PROJECT}" 2>/dev/null || true

# 3. Create cluster & smallest/cheapest instance (1 vCPU c4a-highmem-1, single-zone ZONAL)
gcloud alloydb clusters create "${CLUSTER_ID}" --region="${REGION}" --network="${NETWORK}" --password="${DB_PASSWORD}" --project="${PROJECT}" 2>/dev/null || true

PUBLIC_IP_FLAGS=()
if [[ "${SETUP_LOOKER}" == "true" || -n "${AUTHORIZED_NETWORKS}" ]]; then
  PUBLIC_IP_FLAGS+=(--assign-inbound-public-ip=ASSIGN_IPV4)
  [[ -n "${AUTHORIZED_NETWORKS}" ]] && PUBLIC_IP_FLAGS+=(--authorized-external-networks="${AUTHORIZED_NETWORKS}")
fi

gcloud alloydb instances create "${INSTANCE_ID}" \
  --cluster="${CLUSTER_ID}" \
  --region="${REGION}" \
  --instance-type=PRIMARY \
  --machine-type="${ALLOYDB_MACHINE_TYPE}" \
  --availability-type=ZONAL \
  "${PUBLIC_IP_FLAGS[@]}" \
  --project="${PROJECT}" 2>/dev/null || \
  { [[ ${#PUBLIC_IP_FLAGS[@]} -gt 0 ]] && gcloud alloydb instances update "${INSTANCE_ID}" --cluster="${CLUSTER_ID}" --region="${REGION}" "${PUBLIC_IP_FLAGS[@]}" --project="${PROJECT}"; } || true

ALLOYDB_IP="$(gcloud alloydb instances describe "${INSTANCE_ID}" --cluster="${CLUSTER_ID}" --region="${REGION}" --project="${PROJECT}" --format="value(ipAddress)")"
ALLOYDB_PUBLIC_IP="$(gcloud alloydb instances describe "${INSTANCE_ID}" --cluster="${CLUSTER_ID}" --region="${REGION}" --project="${PROJECT}" --format="value(publicIpAddress)" 2>/dev/null || true)"
DATABASE_URL="postgres://postgres:${DB_PASSWORD}@${ALLOYDB_IP}:5432/postgres?sslmode=require"

# Save credentials and connection URLs to Secret Manager
echo "==> Saving AlloyDB secrets to Secret Manager..."
save_secret "thelook-alloydb-password" "${DB_PASSWORD}"
save_secret "thelook-dashboard-secret" "${SECRET}"
save_secret "thelook-alloydb-url" "${DATABASE_URL}"
if [[ -n "${ALLOYDB_PUBLIC_IP}" ]]; then
  save_secret "thelook-alloydb-url-public" "postgres://postgres:${DB_PASSWORD}@${ALLOYDB_PUBLIC_IP}:5432/postgres?sslmode=require"
fi
if [[ "${SETUP_LOOKER}" == "true" ]]; then
  [[ -n "${LOOKERSDK_BASE_URL}" ]] && save_secret "lookersdk-base-url" "${LOOKERSDK_BASE_URL}"
  [[ -n "${LOOKERSDK_CLIENT_ID}" ]] && save_secret "lookersdk-client-id" "${LOOKERSDK_CLIENT_ID}"
  [[ -n "${LOOKERSDK_CLIENT_SECRET}" ]] && save_secret "lookersdk-client-secret" "${LOOKERSDK_CLIENT_SECRET}"
fi

# 4. Firewall rule for status UI port 8080
gcloud compute firewall-rules create allow-thelook-status --network="${NETWORK}" --allow=tcp:8080 --target-tags=thelook-gen --project="${PROJECT}" 2>/dev/null || true

# 5. Provision separate Compute Engine VM (e2-micro Free Tier with 2GB swap) to run the generator
STARTUP_FILE="$(mktemp)"
cat <<EOF > "${STARTUP_FILE}"
#!/usr/bin/env bash
set -euo pipefail
exec > /var/log/thelook-startup.log 2>&1

# Ensure 2GB swap is enabled on e2-micro (1GB RAM) so compilation/backfill never OOMs
if [ ! -f /swapfile ]; then
  fallocate -l 2G /swapfile
  chmod 600 /swapfile
  mkswap /swapfile
  swapon /swapfile
  echo '/swapfile none swap sw 0 0' >> /etc/fstab
fi

# Install Google Cloud Ops Agent so logs stream directly to Cloud Logging
if ! command -v google-cloud-ops-agent &>/dev/null; then
  curl -sSO https://dl.google.com/cloudagents/add-google-cloud-ops-agent-repo.sh 2>/dev/null || true
  if [ -f add-google-cloud-ops-agent-repo.sh ]; then
    bash add-google-cloud-ops-agent-repo.sh --also-install 2>/dev/null || true
    rm -f add-google-cloud-ops-agent-repo.sh
  fi
fi

mkdir -p /var/lib/thelook
export TARGET_DB=postgres DATABASE_URL="${DATABASE_URL}" SECRET="${SECRET}"

USE_BINARY="${USE_BINARY}"
IMAGE="${IMAGE}"
FALLBACK_TO_BINARY="false"

if [ "\${USE_BINARY}" != "true" ] && [ -n "\${IMAGE}" ]; then
  # Container mode (default): pull container image and extract binary
  echo "==> Deploying via container: \${IMAGE}"
  apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y docker.io ca-certificates
  REGISTRY_HOST="\$(echo "\${IMAGE}" | cut -d/ -f1)"
  gcloud auth configure-docker "\${REGISTRY_HOST}" --quiet || true
  if docker pull "\${IMAGE}"; then
    CID=\$(docker create "\${IMAGE}")
    docker cp "\${CID}:/usr/local/bin/thelook" /usr/local/bin/thelook
    docker rm "\${CID}"
    chmod +x /usr/local/bin/thelook
    echo "Successfully extracted /usr/local/bin/thelook from container \${IMAGE}"
  else
    echo "Warning: Failed to pull container image \${IMAGE}. Falling back to binary build..." >&2
    FALLBACK_TO_BINARY="true"
  fi
fi

if [ "\${USE_BINARY}" = "true" ] || [ ! -f /usr/local/bin/thelook ] || [ "\${FALLBACK_TO_BINARY}" = "true" ]; then
  # Source / Binary mode: install official Go toolchain and compile binary using 2GB swap
  echo "==> Building binary from source..."
  apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y git wget ca-certificates
  export HOME=/root GOPATH=/root/go GOMODCACHE=/root/go/pkg/mod
  mkdir -p /root/go/pkg/mod
  if ! command -v go &>/dev/null; then
    wget -qO- https://go.dev/dl/go1.22.2.linux-amd64.tar.gz | tar -C /usr/local -xzf -
    ln -sf /usr/local/go/bin/go /usr/local/bin/go
  fi
  mkdir -p /opt/thelook && cd /opt/thelook
  [[ -d "thelook-generator" ]] || git clone https://github.com/lkrdev/thelook-generator.git
  cd thelook-generator
  GOTOOLCHAIN=auto /usr/local/bin/go build -ldflags="-s -w" -o /usr/local/bin/thelook .
fi

/usr/local/bin/thelook deploy
if [ "${BACKFILL_DAYS}" -gt 0 ]; then
  /usr/local/bin/thelook backfill --days "${BACKFILL_DAYS}" --state /var/lib/thelook/state.gob
fi

cat <<UNIT > /etc/systemd/system/thelook.service
[Unit]
Description=TheLook AlloyDB Data Generator
After=network-online.target

[Service]
Type=simple
Environment=TARGET_DB=postgres
Environment=DATABASE_URL=${DATABASE_URL}
Environment=SECRET=${SECRET}
ExecStart=/usr/local/bin/thelook run --state /var/lib/thelook/state.gob --http :8080
Restart=always

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload && systemctl enable --now thelook.service
EOF

gcloud compute instances create "${VM_NAME}" \
  --zone="${ZONE}" \
  --machine-type="${MACHINE_TYPE}" \
  --network="${NETWORK}" \
  --tags=thelook-gen \
  --scopes=cloud-platform \
  --shielded-secure-boot \
  --metadata-from-file=startup-script="${STARTUP_FILE}" \
  --project="${PROJECT}" 2>/dev/null || \
  gcloud compute instances add-metadata "${VM_NAME}" --zone="${ZONE}" --metadata-from-file=startup-script="${STARTUP_FILE}" --project="${PROJECT}"
rm -f "${STARTUP_FILE}"

# 6. Optional: Create or update the Looker connection (thelook_alloydb) via Looker SDK
if [[ "${SETUP_LOOKER}" == "true" ]]; then
  # Auto-discover Looker Core instance and region if not provided
  if [[ -z "${LOOKER_INSTANCE}" ]]; then
    LOOKER_INSTANCE_FULL="$(gcloud looker instances list --project="${PROJECT}" --format="value(name)" --limit=1 2>/dev/null || true)"
    if [[ -n "${LOOKER_INSTANCE_FULL}" ]]; then
      LOOKER_INSTANCE="$(basename "${LOOKER_INSTANCE_FULL}")"
      [[ -z "${LOOKER_REGION}" ]] && LOOKER_REGION="$(echo "${LOOKER_INSTANCE_FULL}" | awk -F'/' '{print $(NF-2)}')"
    fi
  fi

  # Auto-detect Looker Core Hybrid / PSC deployment
  if [[ -n "${LOOKER_INSTANCE}" && -n "${LOOKER_REGION}" && "${LOOKER_PSC}" != "true" ]]; then
    LOOKER_JSON="$(gcloud looker instances describe "${LOOKER_INSTANCE}" --region="${LOOKER_REGION}" --project="${PROJECT}" --format=json 2>/dev/null || true)"
    if echo "${LOOKER_JSON}" | grep -q '"pscEnabled": *true' || echo "${LOOKER_JSON}" | grep -q '"controlledEgressEnabled": *true'; then
      echo "==> Detected Looker Core Hybrid / PSC deployment on instance '${LOOKER_INSTANCE}'"
      LOOKER_PSC="true"
    fi
  fi

  # Provision PSC pipeline for Looker Core Hybrid if needed
  if [[ "${LOOKER_PSC}" == "true" && -n "${ALLOYDB_PUBLIC_IP}" ]]; then
    echo "==> Configuring Private Service Connect (PSC) pipeline for Looker Core Hybrid..."
    [[ -z "${LOOKER_REGION}" ]] && LOOKER_REGION="us-east1"

    # Resolve Looker PSC VPC
    if [[ -z "${LOOKER_NETWORK}" && -n "${LOOKER_INSTANCE}" ]]; then
      VPC_FULL="$(gcloud looker instances describe "${LOOKER_INSTANCE}" --region="${LOOKER_REGION}" --project="${PROJECT}" --format="value(pscConfig.allowedVpcs[0])" 2>/dev/null || true)"
      [[ -n "${VPC_FULL}" ]] && LOOKER_NETWORK="$(basename "${VPC_FULL}")"
    fi
    [[ -z "${LOOKER_NETWORK}" ]] && LOOKER_NETWORK="looker-psc-demo"

    PSC_SA_NAME="alloydb-svc-attachment"
    PSC_NEG_NAME="alloydb-internet-neg"
    PSC_BS_NAME="alloydb-backend-svc"
    PSC_PROXY_NAME="alloydb-lb-tcp-proxy"
    PSC_FR_NAME="alloydb-psc-fr"
    PSC_NAT_SUBNET="alloydb-psc-nat-subnet"

    # 1. PSC NAT subnet
    if ! gcloud compute networks subnets describe "${PSC_NAT_SUBNET}" --region="${LOOKER_REGION}" --project="${PROJECT}" &>/dev/null; then
      echo "--> Creating PSC NAT subnet '${PSC_NAT_SUBNET}' in ${LOOKER_NETWORK}..."
      gcloud compute networks subnets create "${PSC_NAT_SUBNET}" \
        --network="${LOOKER_NETWORK}" \
        --region="${LOOKER_REGION}" \
        --range="172.16.30.0/28" \
        --purpose=PRIVATE_SERVICE_CONNECT \
        --project="${PROJECT}" --quiet >/dev/null 2>&1 || true
    fi

    # 2. Regional Internet NEG targeting AlloyDB public IP on port 5432
    if ! gcloud compute network-endpoint-groups describe "${PSC_NEG_NAME}" --region="${LOOKER_REGION}" --project="${PROJECT}" &>/dev/null; then
      echo "--> Creating regional Internet NEG '${PSC_NEG_NAME}'..."
      gcloud compute network-endpoint-groups create "${PSC_NEG_NAME}" \
        --region="${LOOKER_REGION}" \
        --network-endpoint-type=internet-ip-port \
        --project="${PROJECT}" --quiet >/dev/null 2>&1 || true
      sleep 3
      gcloud compute network-endpoint-groups update "${PSC_NEG_NAME}" \
        --region="${LOOKER_REGION}" \
        --add-endpoint="ip=${ALLOYDB_PUBLIC_IP},port=5432" \
        --project="${PROJECT}" --quiet >/dev/null 2>&1 || true
    fi

    # 3. Internal managed TCP backend service
    if ! gcloud compute backend-services describe "${PSC_BS_NAME}" --region="${LOOKER_REGION}" --project="${PROJECT}" &>/dev/null; then
      echo "--> Creating internal TCP backend service '${PSC_BS_NAME}'..."
      gcloud compute backend-services create "${PSC_BS_NAME}" \
        --load-balancing-scheme=INTERNAL_MANAGED \
        --protocol=TCP \
        --region="${LOOKER_REGION}" \
        --project="${PROJECT}" --quiet >/dev/null 2>&1 || true
      gcloud compute backend-services add-backend "${PSC_BS_NAME}" \
        --region="${LOOKER_REGION}" \
        --network-endpoint-group="${PSC_NEG_NAME}" \
        --project="${PROJECT}" --quiet >/dev/null 2>&1 || true
    fi

    # 4. Target TCP proxy
    if ! gcloud compute target-tcp-proxies describe "${PSC_PROXY_NAME}" --region="${LOOKER_REGION}" --project="${PROJECT}" &>/dev/null; then
      echo "--> Creating target TCP proxy '${PSC_PROXY_NAME}'..."
      gcloud compute target-tcp-proxies create "${PSC_PROXY_NAME}" \
        --backend-service="${PSC_BS_NAME}" \
        --region="${LOOKER_REGION}" \
        --project="${PROJECT}" --quiet >/dev/null 2>&1 || true
    fi

    # 5. Forwarding rule
    if ! gcloud compute forwarding-rules describe "${PSC_FR_NAME}" --region="${LOOKER_REGION}" --project="${PROJECT}" &>/dev/null; then
      echo "--> Creating forwarding rule '${PSC_FR_NAME}'..."
      FR_SUBNET="$(gcloud compute networks subnets list --network="${LOOKER_NETWORK}" --regions="${LOOKER_REGION}" --project="${PROJECT}" --filter="range:172.16.20.0/28 OR name:producer-psc-fr-subnet" --format="value(name)" --limit=1 2>/dev/null || true)"
      [[ -z "${FR_SUBNET}" ]] && FR_SUBNET="producer-psc-fr-subnet"
      gcloud compute forwarding-rules create "${PSC_FR_NAME}" \
        --region="${LOOKER_REGION}" \
        --load-balancing-scheme=INTERNAL_MANAGED \
        --network="${LOOKER_NETWORK}" \
        --subnet="${FR_SUBNET}" \
        --target-tcp-proxy="${PSC_PROXY_NAME}" \
        --target-tcp-proxy-region="${LOOKER_REGION}" \
        --ports=5432 \
        --project="${PROJECT}" --quiet >/dev/null 2>&1 || true
    fi

    # 6. Service attachment
    if ! gcloud compute service-attachments describe "${PSC_SA_NAME}" --region="${LOOKER_REGION}" --project="${PROJECT}" &>/dev/null; then
      echo "--> Creating service attachment '${PSC_SA_NAME}'..."
      gcloud compute service-attachments create "${PSC_SA_NAME}" \
        --region="${LOOKER_REGION}" \
        --producer-forwarding-rule="${PSC_FR_NAME}" \
        --connection-preference=ACCEPT_AUTOMATIC \
        --nat-subnets="${PSC_NAT_SUBNET}" \
        --project="${PROJECT}" --quiet >/dev/null 2>&1 || true
    fi

    PSC_SA_URI="projects/${PROJECT}/regions/${LOOKER_REGION}/serviceAttachments/${PSC_SA_NAME}"

    # 7. Attach to Looker Core instance (preserving existing attachments)
    if [[ -n "${LOOKER_INSTANCE}" ]]; then
      EXISTING_ATTACHMENT="$(gcloud looker instances describe "${LOOKER_INSTANCE}" --region="${LOOKER_REGION}" --project="${PROJECT}" --format=json 2>/dev/null | grep "${PSC_DOMAIN}" || true)"
      if [[ -z "${EXISTING_ATTACHMENT}" ]]; then
        echo "--> Attaching PSC service attachment to Looker instance '${LOOKER_INSTANCE}'..."
        ATTACH_FLAGS=()
        while IFS= read -r line; do
          [[ -n "${line}" ]] && ATTACH_FLAGS+=(--psc-service-attachment="${line}")
        done < <(gcloud looker instances describe "${LOOKER_INSTANCE}" --region="${LOOKER_REGION}" --project="${PROJECT}" --format=json 2>/dev/null | python3 -c '
import sys, json
data = json.load(sys.stdin)
for sa in data.get("pscConfig", {}).get("serviceAttachments", []):
    print(f"domain={sa.get(\"localFqdn\")},attachment={sa.get(\"targetServiceAttachmentUri\")}")
' 2>/dev/null || true)
        ATTACH_FLAGS+=(--psc-service-attachment="domain=${PSC_DOMAIN},attachment=${PSC_SA_URI}")
        gcloud looker instances update "${LOOKER_INSTANCE}" \
          --region="${LOOKER_REGION}" \
          --project="${PROJECT}" \
          "${ATTACH_FLAGS[@]}" \
          --quiet >/dev/null 2>&1 || true
      fi
    fi

    LOOKER_HOST="${PSC_DOMAIN}"
    save_secret "thelook-alloydb-psc-host" "${PSC_DOMAIN}"
  fi

  if [[ -z "${LOOKER_HOST}" ]]; then
    LOOKER_HOST="$(gcloud secrets versions access latest --secret="thelook-alloydb-psc-host" --project="${PROJECT}" 2>/dev/null || true)"
    [[ -z "${LOOKER_HOST}" ]] && LOOKER_HOST="${ALLOYDB_PUBLIC_IP}"
  fi
  if [[ -z "${LOOKER_HOST}" ]]; then
    echo "Warning: No public IP or PSC host found for AlloyDB. Skipping Looker connection registration." >&2
  elif ! command -v uvx &>/dev/null; then
    echo "Warning: 'uvx' command not found. Skipping Looker connection registration." >&2
    echo "Please install 'uv' (https://github.com/astral-sh/uv) to enable automatic Looker registration." >&2
  else
    echo "==> Registering Looker connection '${LOOKER_CONNECTION}' -> ${LOOKER_HOST}:5432..."
    uvx --from "lkr-dev-cli[code-mode]" lkr-dev-cli \
      --base-url "${LOOKERSDK_BASE_URL}" \
      --client-id "${LOOKERSDK_CLIENT_ID}" \
      --client-secret "${LOOKERSDK_CLIENT_SECRET}" \
      code-mode sandbox \
      -v conn_name="${LOOKER_CONNECTION}" \
      -v host="${LOOKER_HOST}" \
      -v password="${DB_PASSWORD}" \
      --code '
body = {
    "name": conn_name,
    "dialect_name": "alloydb",
    "host": host,
    "port": "5432",
    "database": "postgres",
    "schema": "public",
    "username": "postgres",
    "password": password,
    "ssl": True,
}
existing = [c["name"] for c in all_connections() if c.get("name") == conn_name]
if existing:
    return update_connection(conn_name, body=body)
return create_connection(body=body)
'
  fi
fi

echo ""
echo "================================================================="
echo "  AlloyDB Private IP: ${ALLOYDB_IP}"
[[ -n "${ALLOYDB_PUBLIC_IP}" ]] && echo "  AlloyDB Public IP:  ${ALLOYDB_PUBLIC_IP}"
[[ -n "${LOOKER_HOST}" ]] && echo "  Looker Host:        ${LOOKER_HOST} (Connection: ${LOOKER_CONNECTION})"
echo "  Database URL:       ${DATABASE_URL}"
echo "  Generator VM:       ${VM_NAME} (${ZONE}, ${MACHINE_TYPE} + 2GB swap)"
echo "  Secret Manager:     gcloud secrets versions access latest --secret=thelook-alloydb-url"
echo "  Web UI Tunnel:      gcloud compute ssh ${VM_NAME} --zone=${ZONE} -- -L 8080:localhost:8080"
echo "  Live Logs:          gcloud compute ssh ${VM_NAME} --zone=${ZONE} -- sudo journalctl -u thelook -f"
echo "================================================================="
