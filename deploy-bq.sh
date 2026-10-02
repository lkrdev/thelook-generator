#!/usr/bin/env bash
# deploy-bq.sh - Deploy TheLook Data Generator to BigQuery from scratch.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

PROJECT="${GCP_PROJECT:-$(gcloud config get-value project 2>/dev/null || true)}"
DATASET="${GCP_DATASET:-thelook_test_$(date +%Y%m%d_%H%M%S)}"
LOCATION="US"
MODE="testing"
BACKFILL_DAYS=""
SECRET="${SECRET:-thelook-${RANDOM}${RANDOM}}"
PORT="8080"
SETUP_LOOKER="false"
LOOKER_CONNECTION="thelook_bq"
LOOKERSDK_BASE_URL="${LOOKERSDK_BASE_URL:-}"
LOOKERSDK_CLIENT_ID="${LOOKERSDK_CLIENT_ID:-}"
LOOKERSDK_CLIENT_SECRET="${LOOKERSDK_CLIENT_SECRET:-}"
SA_KEY_FILE="${SA_KEY_FILE:-}"

usage() {
  echo "Usage: ./deploy-bq.sh [--mode testing|production] [--dataset NAME] [--project ID] [--days N] [--port 8080] [--looker] [--looker-connection NAME] [--looker-base-url URL] [--looker-client-id ID] [--looker-client-secret SECRET] [--sa-key FILE]"
  exit 0
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --project)              PROJECT="$2"; shift 2 ;;
    --dataset)              DATASET="$2"; shift 2 ;;
    --location)             LOCATION="$2"; shift 2 ;;
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

# 1. Create dataset via gcloud or bq (thelook deploy also runs CREATE SCHEMA IF NOT EXISTS)
gcloud alpha bq datasets create "${DATASET}" --project="${PROJECT}" 2>/dev/null || \
  bq --location="${LOCATION}" mk -d "${PROJECT}:${DATASET}" 2>/dev/null || true

# 2. Build Go binary if needed
[[ -f "./thelook" ]] || go build -o thelook .

export GCP_PROJECT="${PROJECT}" GCP_DATASET="${DATASET}" SECRET="${SECRET}"

# 3. Deploy tables + retail calendar
./thelook deploy --project "${PROJECT}" --dataset "${DATASET}"

# 4. Backfill historical data & analyze profile
if [[ "${BACKFILL_DAYS}" -gt 0 ]]; then
  echo "==> Backfilling ${BACKFILL_DAYS} days..."
  ./thelook backfill --days "${BACKFILL_DAYS}" --state state.gob --profile profile.json
  ./thelook analyze --dataset "${PROJECT}.${DATASET}" --out profile.json 2>/dev/null || true
fi

# 5. Optional: Create or update the Looker BigQuery connection (thelook_bq) via Looker SDK
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

echo ""
echo "================================================================="
echo "  TheLook Generator running on BigQuery: ${PROJECT}.${DATASET}"
[[ "${SETUP_LOOKER}" == "true" ]] && echo "  Looker Connection: ${LOOKER_CONNECTION}"
echo "  Web UI: http://localhost:${PORT}/?key=${SECRET}"
echo "================================================================="
echo ""
exec ./thelook run --state state.gob --profile profile.json --http ":${PORT}"
