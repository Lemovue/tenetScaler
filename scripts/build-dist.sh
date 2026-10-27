#!/usr/bin/env bash
# Cross-compile the tenantscaler binary (it includes `loadgen` for teammates'
# laptops) for windows/amd64, darwin/arm64 and linux/amd64 into dist/.
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p dist
commit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
for t in windows/amd64/.exe darwin/arm64/ linux/amd64/; do
	IFS=/ read -r os arch ext <<<"$t"
	out="dist/tenantscaler-${os}-${arch}${ext}"
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -p 4 -trimpath -ldflags "-s -w -X main.version=0.1.0+${commit}" -o "$out" ./cmd/tenantscaler
	echo "built $out"
done
