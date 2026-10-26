#!/bin/sh
# Build the castplay image locally and run it on moro without GitHub Actions:
# the image is imported straight into k3s's containerd and the Deployment is
# pointed at it. Terraform Cloud still pins the ghcr image, so the next infra
# apply reverts this; release by pushing main and bumping the digest there.
set -eu

host=${CASTPLAY_HOST:-ubuntu@100.97.228.124}
tag="castplay:dev-$(git rev-parse --short HEAD)$(git diff --quiet HEAD || echo -dirty)-$(date +%s)"
image="docker.io/library/$tag"

cd "$(dirname "$0")/.."
docker buildx build --platform linux/amd64 -t "$tag" --load .
docker save "$tag" | ssh "$host" sudo k3s ctr images import -
kubectl -n castplay set image deploy/castplay castplay="$image" state-owner="$image"
kubectl -n castplay rollout status deploy/castplay --timeout=120s
echo "running $image"
