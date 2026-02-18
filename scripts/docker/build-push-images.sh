#!/usr/bin/env bash
set -euo pipefail

repo="${REPO:-morphoorg}"
tag="${TAG:-0.0.0-dev-$(date -u +%Y%m%d%H%M)-g$(git rev-parse --short HEAD)}"
platforms="${PLATFORMS:-linux/amd64,linux/arm64}"
push="${PUSH:-true}"
commit_sha="$(git rev-parse --short HEAD)"

if [[ "${push}" == "true" ]]; then
  out_flag="--push"
else
  out_flag="--load"
fi

echo "repo=${repo}"
echo "tag=${tag}"
echo "platforms=${platforms}"
echo "push=${push}"

docker buildx build \
  --platform "${platforms}" \
  --build-arg VERSION="${tag}" \
  --build-arg COMMIT_SHA="${commit_sha}" \
  -t "${repo}/erpc:${tag}" \
  ${out_flag} \
  .

docker buildx build \
  --platform "${platforms}" \
  --build-arg VERSION="${tag}" \
  --build-arg COMMIT_SHA="${commit_sha}" \
  -f Dockerfile.validator \
  -t "${repo}/erpc-validator:${tag}" \
  ${out_flag} \
  .

echo "built:"
echo "  ${repo}/erpc:${tag}"
echo "  ${repo}/erpc-validator:${tag}"
