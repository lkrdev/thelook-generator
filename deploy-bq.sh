#!/usr/bin/env bash
# Thin wrapper delegating BigQuery cloud deploy/destroy to the Go CLI.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
if [[ " $* " == *" --destroy "* ]]; then
  exec go run . destroy --target bq "${@/--destroy/}"
fi
exec go run . deploy --cloud --target bq "$@"
