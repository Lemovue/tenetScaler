#!/usr/bin/env bash
# Repair the cluster after the laptop's network changed (for example the
# phone hotspot reconnected). With WSL mirrored networking the node's address
# is the laptop's, and k3s keeps the address it found at start: when that
# address is gone, pods lose the API server (10.43.0.1 times out), the
# gateways stop discovering pods, the controller fails static and KEDA
# crash-loops. A k3s restart picks the current address. A KEDA restart ends
# its crash-loop back-off at once. Run as root; scripts/k3s/demo-up.ps1 runs it
# first.
set -uo pipefail
cur=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p')
api() { kubectl get endpoints kubernetes -o jsonpath='{.subsets[0].addresses[0].ip}' 2>/dev/null; }
was=$(api)
if [ -z "$cur" ]; then
  echo "no network route: nothing to compare (node address $was)"
  exit 0
fi
if [ "$cur" = "$was" ]; then
  echo "node address $was is current"
  exit 0
fi
echo "node address changed ($was -> $cur): restarting k3s"
systemctl restart k3s
for _ in $(seq 60); do
  [ "$(api)" = "$cur" ] && break
  sleep 2
done
echo "API server now at $(api)"
kubectl -n keda delete pod --all --wait=false 2>/dev/null || true
kubectl -n keda rollout status deployment keda-operator --timeout=120s 2>/dev/null || true
