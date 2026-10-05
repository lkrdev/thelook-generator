#!/usr/bin/env bash
# deploy-bq.sh - Deploy TheLook Data Generator to BigQuery on a dedicated Compute Engine VM (or locally).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

PROJECT="${GCP_PROJECT:-$(gcloud config get-value project 2>/dev/null || true)}"
DATASET="${GCP_DATASET:-thelook_test_$(date +%Y%m%d_%H%M%S)}"
LOCATION="US"
REGION="us-central1"
ZONE="us-central1-a"
NETWORK="default"
VM_NAME="thelook-bq-gen"
MACHINE_TYPE="e2-micro"
DEFAULT_CONTAINER_IMAGE="us-central1-docker.pkg.dev/lkr-dev-production/thelook-generator/thelook-generator:latest"
IMAGE="${IMAGE:-${DEFAULT_CONTAINER_IMAGE}}"
USE_BINARY="false"
LOCAL_MODE="false"
MODE="testing"
BACKFILL_DAYS=""
SECRET="${SECRET:-}"
PORT="8080"
SETUP_LOOKER="false"
LOOKER_CONNECTION="thelook_bq"
LOOKERSDK_BASE_URL="${LOOKERSDK_BASE_URL:-}"
LOOKERSDK_CLIENT_ID="${LOOKERSDK_CLIENT_ID:-}"
LOOKERSDK_CLIENT_SECRET="${LOOKERSDK_CLIENT_SECRET:-}"
SA_KEY_FILE="${SA_KEY_FILE:-}"

usage() {
  echo "Usage: ./deploy-bq.sh [--mode testing|production] [--dataset NAME] [--project ID] [--region REGION] [--zone ZONE] [--days N] [--image URI] [--use-binary] [--vm-name NAME] [--machine-type TYPE] [--local] [--port 8080] [--looker] [--looker-connection NAME] [--looker-base-url URL] [--looker-client-id ID] [--looker-client-secret SECRET] [--sa-key FILE]"
  exit 0
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --project)              PROJECT="$2"; shift 2 ;;
    --dataset)              DATASET="$2"; shift 2 ;;
    --location)             LOCATION="$2"; shift 2 ;;
    --region)               REGION="$2"; ZONE="${REGION}-a"; shift 2 ;;
    --zone)                 ZONE="$2"; shift 2 ;;
    --network)              NETWORK="$2"; shift 2 ;;
    --vm-name)              VM_NAME="$2"; shift 2 ;;
    --machine-type)         MACHINE_TYPE="$2"; shift 2 ;;
    --image)                IMAGE="$2"; USE_BINARY="false"; shift 2 ;;
    --use-binary)           USE_BINARY="true"; IMAGE=""; shift 1 ;;
    --local)                LOCAL_MODE="true"; shift 1 ;;
    --mode)                 MODE="$(echo "$2" | tr '[:upper:]' '[:lower:]')"; shift 2 ;;
    --days)                 BACKFILL_DAYS="$2"; shift 2 ;;
    --secret)               SECRET="$2"; shift 2 ;;
    --port)                 PORT="$2"; shift 2 ;;
    --looker)               SETUP_LOOKER="true"; shift 1 ;;
    --looker-connection)    SETUP_LOOKER="true"; LOOKER_CONNECTION="$2"; shift 2 ;;
    --looker-base-url)      SETUP_LOOKER="true"; LOOKERSDK_BASE_URL="$2"; shift 2 ;;
    --looker-client-id)     SETUP_LOOKER="true"; LOOKERSDK_CLIENT_ID="$2"; shift 2 ;;
    --looker-client-secret) SETUP_LOOKER="true"; LOOKERSDK_CLIENT_SECRET="$2"; shift 2 ;;
    --sa-key)               SETUP_LOOKER="true"; SA_KEY_FILE="$2"; shift 2 ;;
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

# Load dashboard secret from Secret Manager if not provided
if [[ -z "${SECRET}" ]]; then
  SECRET="$(gcloud secrets versions access latest --secret="thelook-dashboard-secret" --project="${PROJECT}" 2>/dev/null || true)"
  [[ -z "${SECRET}" ]] && SECRET="thelook-${RANDOM}${RANDOM}"
fi

echo "==> Deploying TheLook to BigQuery (${PROJECT}.${DATASET}) [${MODE}: ${BACKFILL_DAYS}d backfill]"

if [[ "${SETUP_LOOKER}" == "true" ]]; then
  [[ -z "${LOOKERSDK_BASE_URL}" ]] && read -rp "Looker Base URL (e.g. https://instance.cloud.looker.com): " LOOKERSDK_BASE_URL
  [[ -z "${LOOKERSDK_CLIENT_ID}" ]] && read -rp "Looker Client ID: " LOOKERSDK_CLIENT_ID
  if [[ -z "${LOOKERSDK_CLIENT_SECRET}" ]]; then
    read -rsp "Looker Client Secret: " LOOKERSDK_CLIENT_SECRET
    echo ""
  fi
  export LOOKERSDK_BASE_URL LOOKERSDK_CLIENT_ID LOOKERSDK_CLIENT_SECRET
fi

# 1. Enable required APIs & ensure Compute Engine SA has Logging, BigQuery & Secret Manager permissions
echo "==> Enabling required GCP APIs..."
gcloud services enable bigquery.googleapis.com compute.googleapis.com logging.googleapis.com secretmanager.googleapis.com --project="${PROJECT}"

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
    --role="roles/bigquery.dataEditor" \
    --condition=None --quiet >/dev/null 2>&1 || true
  gcloud projects add-iam-policy-binding "${PROJECT}" \
    --member="serviceAccount:${COMPUTE_SA}" \
    --role="roles/bigquery.jobUser" \
    --condition=None --quiet >/dev/null 2>&1 || true
  gcloud projects add-iam-policy-binding "${PROJECT}" \
    --member="serviceAccount:${COMPUTE_SA}" \
    --role="roles/secretmanager.secretAccessor" \
    --condition=None --quiet >/dev/null 2>&1 || true
fi

# Save dashboard and Looker secrets to Secret Manager
echo "==> Saving secrets to Secret Manager..."
save_secret "thelook-dashboard-secret" "${SECRET}"
if [[ "${SETUP_LOOKER}" == "true" ]]; then
  [[ -n "${LOOKERSDK_BASE_URL}" ]] && save_secret "lookersdk-base-url" "${LOOKERSDK_BASE_URL}"
  [[ -n "${LOOKERSDK_CLIENT_ID}" ]] && save_secret "lookersdk-client-id" "${LOOKERSDK_CLIENT_ID}"
  [[ -n "${LOOKERSDK_CLIENT_SECRET}" ]] && save_secret "lookersdk-client-secret" "${LOOKERSDK_CLIENT_SECRET}"
fi

# 2. Create dataset via gcloud or bq (thelook deploy also runs CREATE SCHEMA IF NOT EXISTS)
gcloud alpha bq datasets create "${DATASET}" --project="${PROJECT}" 2>/dev/null || \
  bq --location="${LOCATION}" mk -d "${PROJECT}:${DATASET}" 2>/dev/null || true

# 3. Optional: Create or update the Looker BigQuery connection (thelook_bq) via Looker SDK
if [[ "${SETUP_LOOKER}" == "true" ]]; then
  if ! command -v uvx &>/dev/null; then
    echo "Warning: 'uvx' command not found. Skipping Looker connection registration." >&2
    echo "Please install 'uv' (https://github.com/astral-sh/uv) to enable automatic Looker registration." >&2
  else
    CERT_B64=""
    if [[ -n "${SA_KEY_FILE}" && -f "${SA_KEY_FILE}" ]]; then
      CERT_B64="$(base64 -w0 "${SA_KEY_FILE}" 2>/dev/null || base64 < "${SA_KEY_FILE}" | tr -d '\n')"
    fi
    echo "==> Registering Looker BigQuery connection '${LOOKER_CONNECTION}' -> ${PROJECT}.${DATASET}..."
    uvx --from "lkr-dev-cli[code-mode]" lkr-dev-cli \
      --base-url "${LOOKERSDK_BASE_URL}" \
      --client-id "${LOOKERSDK_CLIENT_ID}" \
      --client-secret "${LOOKERSDK_CLIENT_SECRET}" \
      code-mode sandbox \
      -v conn_name="${LOOKER_CONNECTION}" \
      -v project_id="${PROJECT}" \
      -v dataset="${DATASET}" \
      -v cert_b64="${CERT_B64}" \
      --code '
body = {
    "name": conn_name,
    "dialect_name": "bigquery_standard_sql",
    "host": project_id,
    "database": dataset,
}
if cert_b64:
    body["certificate"] = cert_b64
    body["file_type"] = ".json"
existing = [c["name"] for c in all_connections() if c.get("name") == conn_name]
if existing:
    return update_connection(conn_name, body=body)
return create_connection(body=body)
'
  fi
fi

if [[ "${LOCAL_MODE}" == "true" ]]; then
  # Local execution mode
  echo "==> Running generator locally..."
  if [[ "${USE_BINARY}" != "true" && -n "${IMAGE}" ]] && command -v docker &>/dev/null; then
    docker pull "${IMAGE}" 2>/dev/null && {
      CID=$(docker create "${IMAGE}")
      docker cp "${CID}:/usr/local/bin/thelook" ./thelook
      docker rm "${CID}"
      chmod +x ./thelook
    } || true
  fi
  [[ -f "./thelook" ]] || go build -o thelook .

  export GCP_PROJECT="${PROJECT}" GCP_DATASET="${DATASET}" SECRET="${SECRET}"
  ./thelook deploy --project "${PROJECT}" --dataset "${DATASET}"
  if [[ "${BACKFILL_DAYS}" -gt 0 ]]; then
    echo "==> Backfilling ${BACKFILL_DAYS} days..."
    ./thelook backfill --days "${BACKFILL_DAYS}" --state state.gob --profile profile.json
    ./thelook analyze --dataset "${PROJECT}.${DATASET}" --out profile.json 2>/dev/null || true
  fi

  echo ""
  echo "================================================================="
  echo "  TheLook Generator running locally on BigQuery: ${PROJECT}.${DATASET}"
  [[ "${SETUP_LOOKER}" == "true" ]] && echo "  Looker Connection: ${LOOKER_CONNECTION}"
  echo "  Web UI: http://localhost:${PORT}/?key=${SECRET}"
  echo "================================================================="
  echo ""
  exec ./thelook run --state state.gob --profile profile.json --http ":${PORT}"
else
  # Compute Engine VM mode (default)
  echo "==> Provisioning Compute Engine VM (${MACHINE_TYPE}, ${ZONE}) to run BigQuery generator..."
  gcloud compute firewall-rules create allow-thelook-status --network="${NETWORK}" --allow=tcp:8080 --target-tags=thelook-gen --project="${PROJECT}" 2>/dev/null || true

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
export GCP_PROJECT="${PROJECT}" GCP_DATASET="${DATASET}" SECRET="${SECRET}"

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

/usr/local/bin/thelook deploy --project "${PROJECT}" --dataset "${DATASET}"
if [ "${BACKFILL_DAYS}" -gt 0 ]; then
  /usr/local/bin/thelook backfill --days "${BACKFILL_DAYS}" --state /var/lib/thelook/state.gob
fi

cat <<UNIT > /etc/systemd/system/thelook.service
[Unit]
Description=TheLook BigQuery Data Generator
After=network-online.target

[Service]
Type=simple
Environment=GCP_PROJECT=${PROJECT}
Environment=GCP_DATASET=${DATASET}
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

  echo ""
  echo "================================================================="
  echo "  TheLook BigQuery Generator running on VM: ${VM_NAME} (${ZONE})"
  echo "  Target Dataset:     ${PROJECT}.${DATASET}"
  [[ "${SETUP_LOOKER}" == "true" ]] && echo "  Looker Connection:  ${LOOKER_CONNECTION}"
  echo "  Web UI Tunnel:      gcloud compute ssh ${VM_NAME} --zone=${ZONE} -- -L 8080:localhost:8080"
  echo "  Live Logs:          gcloud compute ssh ${VM_NAME} --zone=${ZONE} -- sudo journalctl -u thelook -f"
  echo "================================================================="
fi
