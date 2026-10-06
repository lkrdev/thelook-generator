#!/usr/bin/env bash
# Thin wrapper delegating AlloyDB cloud deploy/destroy to the Go CLI.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
if [[ " $* " == *" --destroy "* ]]; then
  exec go run . destroy --target alloydb "${@/--destroy/}"
fi
exec go run . deploy --cloud --target alloydb "$@"
