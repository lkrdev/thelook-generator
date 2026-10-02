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
IMAGE="${IMAGE:-}"
MODE="testing"
BACKFILL_DAYS=""
DB_PASSWORD="${DB_PASSWORD:-LookerPass${RANDOM}${RANDOM}!}"
SECRET="${SECRET:-thelook-${RANDOM}${RANDOM}}"
PORT="8080"
SETUP_LOOKER="false"
LOOKER_CONNECTION="thelook_alloydb"
LOOKERSDK_BASE_URL="${LOOKERSDK_BASE_URL:-}"
LOOKERSDK_CLIENT_ID="${LOOKERSDK_CLIENT_ID:-}"
LOOKERSDK_CLIENT_SECRET="${LOOKERSDK_CLIENT_SECRET:-}"
AUTHORIZED_NETWORKS=""

usage() {
  echo "Usage: ./deploy-alloydb.sh [--mode testing|production] [--project ID] [--region REGION] [--days N] [--image URI] [--looker] [--looker-connection NAME] [--looker-base-url URL] [--looker-client-id ID] [--looker-client-secret SECRET] [--authorized-networks CIDRS]"
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
    --image)                IMAGE="$2"; shift 2 ;;
    --mode)                 MODE="$(echo "$2" | tr '[:upper:]' '[:lower:]')"; shift 2 ;;
    --days)                 BACKFILL_DAYS="$2"; shift 2 ;;
    --password)             DB_PASSWORD="$2"; shift 2 ;;
    --secret)               SECRET="$2"; shift 2 ;;
    --port)                 PORT="$2"; shift 2 ;;
    --looker)               SETUP_LOOKER="true"; shift 1 ;;
    --looker-connection)    SETUP_LOOKER="true"; LOOKER_CONNECTION="$2"; shift 2 ;;
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

# 1. Enable required APIs
gcloud services enable alloydb.googleapis.com compute.googleapis.com servicenetworking.googleapis.com --project="${PROJECT}"

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

# 4. Firewall rule for status UI port 8080
gcloud compute firewall-rules create allow-thelook-status --network="${NETWORK}" --allow=tcp:8080 --target-tags=thelook-gen --project="${PROJECT}" 2>/dev/null || true

# 5. Provision separate Compute Engine VM (e2-micro Free Tier with 2GB swap) to run the generator
STARTUP_SCRIPT=$(cat <<EOF
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

mkdir -p /var/lib/thelook
export TARGET_DB=postgres DATABASE_URL="${DATABASE_URL}" SECRET="${SECRET}"

if [ -n "${IMAGE}" ]; then
  # Container mode: ultra-light RAM footprint (~40MB RSS), no Go compiler or source build needed
  apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y docker.io ca-certificates
  gcloud auth configure-docker "\$(echo "${IMAGE}" | cut -d/ -f1)" --quiet || true
  docker pull "${IMAGE}"
  CID=\$(docker create "${IMAGE}")
  docker cp "\${CID}:/usr/local/bin/thelook" /usr/local/bin/thelook
  docker rm "\${CID}"
  chmod +x /usr/local/bin/thelook
else
  # Source mode: install official Go toolchain and compile binary using 2GB swap
  apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y git wget ca-certificates
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
)

gcloud compute instances create "${VM_NAME}" \
  --zone="${ZONE}" \
  --machine-type="${MACHINE_TYPE}" \
  --network="${NETWORK}" \
  --tags=thelook-gen \
  --metadata=startup-script="${STARTUP_SCRIPT}" \
  --project="${PROJECT}" 2>/dev/null || \
  gcloud compute instances add-metadata "${VM_NAME}" --zone="${ZONE}" --metadata=startup-script="${STARTUP_SCRIPT}" --project="${PROJECT}"

# 6. Optional: Create or update the Looker connection (thelook_alloydb) via Looker SDK
if [[ "${SETUP_LOOKER}" == "true" && -n "${ALLOYDB_PUBLIC_IP}" ]]; then
  if ! command -v uvx &>/dev/null; then
    echo "Warning: 'uvx' command not found. Skipping Looker connection registration." >&2
    echo "Please install 'uv' (https://github.com/astral-sh/uv) to enable automatic Looker registration." >&2
  else
    echo "==> Registering Looker connection '${LOOKER_CONNECTION}' -> ${ALLOYDB_PUBLIC_IP}:5432..."
    uvx --from "lkr-dev-cli[code-mode]" lkr-dev-cli \
      --base-url "${LOOKERSDK_BASE_URL}" \
      --client-id "${LOOKERSDK_CLIENT_ID}" \
      --client-secret "${LOOKERSDK_CLIENT_SECRET}" \
      code-mode sandbox \
      -v conn_name="${LOOKER_CONNECTION}" \
      -v host="${ALLOYDB_PUBLIC_IP}" \
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
[[ -n "${ALLOYDB_PUBLIC_IP}" ]] && echo "  AlloyDB Public IP:  ${ALLOYDB_PUBLIC_IP} (Looker: ${LOOKER_CONNECTION})"
echo "  Database URL:       ${DATABASE_URL}"
echo "  Generator VM:       ${VM_NAME} (${ZONE}, ${MACHINE_TYPE} + 2GB swap)"
echo "  Web UI Tunnel:      gcloud compute ssh ${VM_NAME} --zone=${ZONE} -- -L 8080:localhost:8080"
echo "  Live Logs:          gcloud compute ssh ${VM_NAME} --zone=${ZONE} -- sudo journalctl -u thelook -f"
echo "================================================================="
