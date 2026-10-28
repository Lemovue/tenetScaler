#!/usr/bin/env bash
# Make the in-cluster gateway and controller reachable from this machine until
# Ctrl+C. With WSL mirrored networking and the Hyper-V firewall rule the owner
# approves (deploy/k3s/README.md), teammates' laptops can reach it too:
#   gateway    0.0.0.0:8081    -> svc/gateway:8080 (loadgen -gateway http://<ip>:8081)
#   controller 127.0.0.1:18090 -> svc/tenantscaler-controller:8090 (/decisions, /status)
# Side by side (deploy/k3s/side-by-side), `bash scripts/k3s/expose.sh side-by-side`
# forwards to this machine only, and `tenantscaler mirror` on Windows serves
# 8081 to the teammates:
#   gateway          127.0.0.1:18081 -> svc/gateway:8080
#   gateway-baseline 127.0.0.1:18082 -> svc/gateway-baseline:8080
#   controller       127.0.0.1:18090 -> svc/tenantscaler-controller:8090
# A port-forward follows ONE pod. When that pod is replaced (up.sh restarts
# it), kubectl exits and the loop below forwards to the current pod. In
# mirrored mode Windows and WSL share ports, so 8081 must be free on Windows:
# stop a Windows `demo -live` first.
# Port-forward tunnels through the API server, so its latency is a demo number,
# never a measurement. Run this in its own terminal.
set -uo pipefail
ns=tenantscaler
# As the WSL user: the kubeconfig copied in deploy/k3s/README.md (root uses k3s's own).
if [ -z "${KUBECONFIG:-}" ] && [ -r "$HOME/.kube/config" ]; then export KUBECONFIG="$HOME/.kube/config"; fi

forward() {
  while :; do
    kubectl -n "$ns" port-forward "$@"
    echo "port-forward $* ended (pod replaced?); reconnecting in 2 s" >&2
    sleep 2
  done
}
cleanup() {
  trap - EXIT INT TERM
  for job in $(jobs -p); do
    pkill -P "$job" 2>/dev/null # first the loop's kubectl (or sleep),
    kill "$job" 2>/dev/null     # then the loop, so no kubectl keeps the port
  done
}
trap cleanup EXIT INT TERM
forward --address 127.0.0.1 svc/tenantscaler-controller 18090:8090 &
if [ "${1:-}" = "side-by-side" ]; then
  forward --address 127.0.0.1 svc/gateway 18081:8080 &
  forward --address 127.0.0.1 svc/gateway-baseline 18082:8080 &
else
  forward --address 0.0.0.0 svc/gateway 8081:8080 &
fi
wait
