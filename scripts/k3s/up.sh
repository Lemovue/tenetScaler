#!/usr/bin/env bash
# Deploy TenantScaler on the local k3s cluster (deploy/k3s/README.md): the
# image, the CRD, the key Secret, then everything in deploy/k8s/, and wait
# until it is up. Run as root in WSL/Linux:
#   sudo bash scripts/k3s/up.sh dist/tenantscaler-linux-amd64 /mnt/c/Users/<you>/.tenantscaler/k8s.env
# The key file is written by scripts/k3s/keys.ps1 (outside the repository);
# its values are never printed. Re-running it brings back the deploy/k8s
# sizing (see scripts/k3s/demo-size.sh).
set -euo pipefail
usage="usage: up.sh <linux-amd64 tenantscaler binary> <key env file>"
bin=$(realpath "${1:?$usage}")
keys=$(realpath "${2:?$usage}")
cd "$(dirname "$0")/../.."
ns=tenantscaler
[ -r "$keys" ] || { echo "cannot read key file $keys" >&2; exit 1; }
if ! grep -q '^TS_TENANT_KEYS=' "$keys" || ! grep -q '^TS_ADMIN_KEY=' "$keys"; then
  echo "$keys needs a TS_TENANT_KEYS= and a TS_ADMIN_KEY= line" >&2
  exit 1
fi

bash scripts/k3s/image.sh "$bin"

kubectl apply -f deploy/k8s/00-namespace.yaml -f deploy/k8s/01-crd.yaml
kubectl wait --for=condition=Established --timeout=60s crd/tenantscalers.scaling.tenantscaler.dev

# The new Secret is built first and swapped in with one call, so the old one
# stays until a valid replacement exists. replace/create (not apply) leaves
# no last-applied annotation holding a copy of the keys.
secret=$(mktemp)
trap 'rm -f "$secret"' EXIT
kubectl -n "$ns" create secret generic tenantscaler-keys --from-env-file="$keys" --dry-run=client -o yaml >"$secret"
if kubectl -n "$ns" get secret tenantscaler-keys >/dev/null 2>&1; then
  kubectl -n "$ns" replace -f "$secret"
else
  kubectl -n "$ns" create -f "$secret"
fi

kubectl apply -f deploy/k8s/
# Same tag, possibly a new image: restart so every pod runs the import above.
# Keys are also read only at start.
deployments="model-pool gateway tenantscaler-controller"
for d in $deployments; do kubectl -n "$ns" rollout restart deployment "$d"; done
for d in $deployments; do kubectl -n "$ns" rollout status deployment "$d" --timeout=180s; done
kubectl -n "$ns" get tenantscalers,deployments,pods -o wide
