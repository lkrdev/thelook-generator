#!/usr/bin/env bash
# Thin wrapper delegating AlloyDB cloud deploy/destroy to the Go CLI.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
args=()
for a in "$@"; do [[ "$a" != "--destroy" ]] && args+=("$a"); done
if [[ $# -ne ${#args[@]} ]]; then
  exec go run . destroy --target alloydb "${args[@]}"
fi
exec go run . deploy --cloud --target alloydb "$@"
