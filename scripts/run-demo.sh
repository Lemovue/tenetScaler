#!/usr/bin/env bash
# Build (if needed) and start the live demo dashboard. Ctrl+C stops it.
set -euo pipefail
cd "$(dirname "$0")/.."
[ -x bin/tenantscaler ] || ./scripts/build.sh
exec bin/tenantscaler demo "$@"
