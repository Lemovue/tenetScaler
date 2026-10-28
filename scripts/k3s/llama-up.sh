#!/usr/bin/env bash
# Switch the side-by-side cluster (deploy/k3s/side-by-side) to REAL
# llama.cpp replicas (deploy/k3s/llama). Run as root in WSL/Linux, after
# up.sh and kubectl apply -f deploy/k3s/side-by-side/:
#   sudo bash scripts/k3s/llama-up.sh "/mnt/c/.../.cache/models/qwen2.5-0.5b-instruct-q4_k_m.gguf"
# The model already on disk is copied to the node and checked against its
# pinned SHA-256 (no download). The image is pulled only if absent; that is
# a download of about 309 MB, so ask the machine's owner first. Back to the
# simulated replicas: run up.sh and apply deploy/k3s/side-by-side/ again.
set -euo pipefail
usage="usage: llama-up.sh <qwen2.5-0.5b-instruct-q4_k_m.gguf>"
model=$(realpath "${1:?$usage}")
cd "$(dirname "$0")/../.."
ns=tenantscaler
img=ghcr.io/ggml-org/llama.cpp@sha256:989a1a193343d5e361e14b666e316186ab48be0152cf0a34b30e3c0648ee944f
dir=/var/lib/tenantscaler/models
sha=74a4da8c9fdbcd15bd1f6d01d621410d31c6fc00986f5eb687824e7b93d7a9db # scripts/setup-llama.ps1

mkdir -p "$dir"
install -m 0644 "$model" "$dir/qwen2.5-0.5b-instruct-q4_k_m.gguf"
chmod 0755 /var/lib/tenantscaler "$dir"
[ "$(sha256sum "$dir/qwen2.5-0.5b-instruct-q4_k_m.gguf" | cut -d' ' -f1)" = "$sha" ] ||
  { echo "the model's SHA-256 does not match the pinned one" >&2; exit 1; }
k3s crictl inspecti "$img" >/dev/null 2>&1 || k3s crictl pull "$img"

kubectl apply -f deploy/k3s/llama/05-models.yaml -f deploy/k3s/llama/15-config.yaml
# Guarantees first: the controller refuses tenants whose guarantees exceed
# its budget, and the budget is about to shrink to 2.
kubectl -n "$ns" patch tenantscaler a --type merge -p '{"spec":{"guaranteedReplicas":1,"maxReplicas":2,"priority":100}}'
kubectl -n "$ns" patch tenantscaler b --type merge -p '{"spec":{"guaranteedReplicas":1,"maxReplicas":2,"priority":100}}'
kubectl -n "$ns" patch tenantscaler c --type merge -p '{"spec":{"guaranteedReplicas":0,"maxReplicas":2,"priority":50}}'
# The controller: budget 2 and the llama configuration (whole args list,
# so running this twice changes nothing).
kubectl -n "$ns" patch deployment tenantscaler-controller --type json -p '[
  {"op":"replace","path":"/spec/template/spec/containers/0/args","value":["controller","-addr",":8090","-k8s","-namespace","tenantscaler",
    "-deployment","model-pool","-pool","shared-llm-pool","-capacity","2","-gateway","http://gateway:8080","-decisions-file","",
    "-config","/etc/tenantscaler/config.yaml"]},
  {"op":"add","path":"/spec/template/spec/volumes","value":[{"name":"config","configMap":{"name":"tenantscaler-llama-config"}}]},
  {"op":"add","path":"/spec/template/spec/containers/0/volumeMounts","value":[{"name":"config","mountPath":"/etc/tenantscaler","readOnly":true}]}]'
# Both gateways read the llama configuration (2 requests in flight per replica).
for gw in gateway gateway-baseline; do
  kubectl -n "$ns" patch deployment "$gw" --type json -p '[
    {"op":"test","path":"/spec/template/spec/volumes/0/name","value":"config"},
    {"op":"replace","path":"/spec/template/spec/volumes/0/configMap/name","value":"tenantscaler-llama-config"}]'
done
# KEDA: the same budget of 2, and 4 waiting per replica on top of
# llama-server's 4 slots = 8 in flight per replica.
kubectl -n "$ns" patch scaledobject model-pool-baseline --type merge -p '{"spec":{"maxReplicaCount":2,"triggers":[{
  "type":"metrics-api","metricType":"AverageValue","authenticationRef":{"name":"gateway-baseline-admin"},
  "metadata":{"url":"http://gateway-baseline.tenantscaler.svc.cluster.local:8080/stats?window=15s",
  "valueLocation":"inFlight","targetValue":"8","activationTargetValue":"0","authMode":"bearer"}}]}}'

kubectl apply -f deploy/k3s/llama/10-model-pool.yaml -f deploy/k3s/llama/11-model-pool-baseline.yaml
for d in tenantscaler-controller gateway gateway-baseline model-pool model-pool-baseline; do
  kubectl -n "$ns" rollout status deployment "$d" --timeout=300s
done
kubectl -n "$ns" get tenantscalers,scaledobjects,pods -o wide
