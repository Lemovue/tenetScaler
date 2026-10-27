#!/usr/bin/env bash
# Build the single tenantscaler binary into bin/.
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p bin
commit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
go build -p 4 -ldflags "-X main.version=0.1.0+${commit}" -o bin/tenantscaler ./cmd/tenantscaler
echo "built bin/tenantscaler"
