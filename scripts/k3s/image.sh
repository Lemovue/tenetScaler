#!/usr/bin/env bash
# Build the tenantscaler:dev image without Docker and import it into k3s's
# containerd, so the Deployments in deploy/k8s/ (imagePullPolicy
# IfNotPresent) start it without any registry.
# The image has the Dockerfile runtime stage's binary path, user and
# entrypoint: the static linux/amd64 binary at /tenantscaler, USER
# 65532:65532 (numeric, for runAsNonRoot), entrypoint /tenantscaler. It is
# one layer with only that binary: no distroless base, so no CA bundle,
# /etc/passwd, tzdata or /tmp. The binary needs none of them in the cluster:
# client-go uses the service account's CA, and the root filesystem is
# read-only anyway. The binary comes from ./scripts/build-dist.ps1 (the same
# CGO_ENABLED=0 -trimpath -s -w build as the Dockerfile's build stage). Then,
# as root in WSL/Linux:
#   bash scripts/k3s/image.sh dist/tenantscaler-linux-amd64
set -euo pipefail
bin=${1:?usage: image.sh <linux-amd64 tenantscaler binary> [tag]}
tag=${2:-tenantscaler:dev}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/rootfs"
install -m 0755 "$bin" "$work/rootfs/tenantscaler"
# A reproducible layer: same binary, same digest.
tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner \
  -C "$work/rootfs" -cf "$work/layer.tar" tenantscaler
layer=$(sha256sum "$work/layer.tar" | cut -d' ' -f1)
printf '{"architecture":"amd64","os":"linux","config":{"User":"65532:65532","Entrypoint":["/tenantscaler"],"ExposedPorts":{"8000/tcp":{},"8080/tcp":{}}},"rootfs":{"type":"layers","diff_ids":["sha256:%s"]}}' \
  "$layer" >"$work/config.json"
config=$(sha256sum "$work/config.json" | cut -d' ' -f1)

# docker-archive layout (what `docker save` writes), which ctr imports.
mkdir "$work/archive" "$work/archive/$layer"
mv "$work/layer.tar" "$work/archive/$layer/layer.tar"
mv "$work/config.json" "$work/archive/$config.json"
printf '[{"Config":"%s.json","RepoTags":["%s"],"Layers":["%s/layer.tar"]}]' \
  "$config" "$tag" "$layer" >"$work/archive/manifest.json"
tar -C "$work/archive" -cf "$work/image.tar" manifest.json "$config.json" "$layer/layer.tar"

k3s ctr -n k8s.io images import "$work/image.tar"
k3s crictl images | grep -F "${tag%%:*}"
echo "imported $tag (layer sha256:$layer)"
