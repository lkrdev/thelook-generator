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

usage() {
  echo "Usage: ./deploy-bq.sh [--mode testing|production] [--dataset NAME] [--project ID] [--days N] [--port 8080]"
  exit 0
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --project)  PROJECT="$2"; shift 2 ;;
    --dataset)  DATASET="$2"; shift 2 ;;
    --location) LOCATION="$2"; shift 2 ;;
    --mode)     MODE="$(echo "$2" | tr '[:upper:]' '[:lower:]')"; shift 2 ;;
    --days)     BACKFILL_DAYS="$2"; shift 2 ;;
    --secret)   SECRET="$2"; shift 2 ;;
    --port)     PORT="$2"; shift 2 ;;
    -h|--help)  usage ;;
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

echo ""
echo "================================================================="
echo "  TheLook Generator running on BigQuery: ${PROJECT}.${DATASET}"
echo "  Web UI: http://localhost:${PORT}/?key=${SECRET}"
echo "================================================================="
echo ""
exec ./thelook run --state state.gob --profile profile.json --http ":${PORT}"
