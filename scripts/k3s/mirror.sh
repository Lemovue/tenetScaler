#!/usr/bin/env bash
# Serve the teammates' traffic side by side from the cluster's node itself
# (WSL), with no kubectl port-forward in the data path: `tenantscaler
# mirror` sends every completion to the TenantScaler gateway's ClusterIP
# (the client gets its answer) and a copy to the baseline gateway's ClusterIP.
# With WSL mirrored networking the address is the laptop's own, so
# teammates use http://<laptop IP>:8081; inbound traffic needs the Hyper-V
# firewall rule of deploy/k3s/README.md, and port 8081 must be free on
# Windows. Run as the WSL user (kubeconfig), until Ctrl+C:
#   bash scripts/k3s/mirror.sh dist/tenantscaler-linux-amd64
set -euo pipefail
bin=$(realpath "${1:?usage: mirror.sh <linux-amd64 tenantscaler binary>}")
ns=tenantscaler
# As the WSL user: the kubeconfig copied in deploy/k3s/README.md (root uses k3s's own).
if [ -z "${KUBECONFIG:-}" ] && [ -r "$HOME/.kube/config" ]; then export KUBECONFIG="$HOME/.kube/config"; fi
ip() { kubectl -n "$ns" get svc "$1" -o jsonpath='{.spec.clusterIP}'; }
exec "$bin" mirror -addr 0.0.0.0:8081 -primary "http://$(ip gateway):8080" -copy "http://$(ip gateway-baseline):8080"
