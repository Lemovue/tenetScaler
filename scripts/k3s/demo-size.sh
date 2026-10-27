#!/usr/bin/env bash
# Size the running cluster like the laptop's live demo (config/live-demo.yaml):
# budget 4 replicas, tenants a, b and c each guaranteed 1 and capped at 3,
# and c (the noisy neighbour) at priority 50 so a and b borrow the spare
# replica first. c's spike then wants more than its cap. The controller turns
# on over-share shedding (SHEDDING true in `kubectl get tenantscalers`), and
# the gateway answers c's sheddable requests with 429 while a and b are served.
#   bash scripts/k3s/demo-size.sh          # demo sizing
#   bash scripts/k3s/demo-size.sh --undo   # back to deploy/k8s (budget 10, 3/3/3, max 8)
# scripts/k3s/up.sh also brings back the deploy/k8s sizing.
# Order matters: the controller refuses a tenant set whose guarantees exceed
# its budget. So the guarantees shrink before the budget does, and the budget
# grows before the guarantees do.
set -euo pipefail
cd "$(dirname "$0")/../.."
ns=tenantscaler
args=/spec/template/spec/containers/0/args

# capacity sets the controller's -capacity flag: args[10] is -capacity
# (TestDeploymentsRunTheImageWithExpectedArgs pins the order), and the test op
# makes the patch fail rather than replace the wrong argument.
capacity() {
  kubectl -n "$ns" patch deployment tenantscaler-controller --type json -p \
    "[{\"op\":\"test\",\"path\":\"$args/10\",\"value\":\"-capacity\"},{\"op\":\"replace\",\"path\":\"$args/11\",\"value\":\"$1\"}]"
  kubectl -n "$ns" rollout status deployment tenantscaler-controller --timeout=120s
}

if [ "${1:-}" = "--undo" ]; then
  capacity 10
  kubectl apply -f deploy/k8s/40-tenants.yaml
else
  for t in a b; do
    kubectl -n "$ns" patch tenantscaler "$t" --type merge -p '{"spec":{"guaranteedReplicas":1,"maxReplicas":3}}'
  done
  kubectl -n "$ns" patch tenantscaler c --type merge -p '{"spec":{"guaranteedReplicas":1,"maxReplicas":3,"priority":50}}'
  capacity 4
fi
kubectl -n "$ns" get tenantscalers
