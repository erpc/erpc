#!/usr/bin/env bash
set -euo pipefail

root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
cd "$root"

full=0
if [[ "${1:-}" == "--full" ]]; then
  full=1
fi

pkg_list=()
while IFS= read -r pkg; do
  pkg_list+=("$pkg")
done < <(
  go list ./... \
    | rg -v '^github\.com/erpc/erpc/cmd($|/)' \
    | rg -v '^github\.com/erpc/erpc/test($|/)'
)

if [[ ${#pkg_list[@]} -eq 0 ]]; then
  echo "No non-cmd/test packages discovered via go list ./..."
  exit 1
fi

scripts/agent-harness/check.sh
scripts/agent-harness/review-load.sh || true
make build
if [[ $full -eq 1 ]]; then
  go clean -testcache
  go test ./cmd/... -count 1 -parallel 1
  go test "${pkg_list[@]}" -covermode=atomic -v -race -count 1 -parallel 1 -timeout 15m -failfast=false
else
  go clean -testcache
  go test ./cmd/... -count 1 -parallel 1 -v
  go test "${pkg_list[@]}" -count 1 -parallel 1 -v -timeout 10m -failfast=false
fi
