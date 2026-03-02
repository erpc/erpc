#!/usr/bin/env bash
set -euo pipefail

root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
cd "$root"

full=0
if [[ "${1:-}" == "--full" ]]; then
  full=1
fi

go_test_parallel="${GO_TEST_PARALLEL:-1}"
go_test_p="${GO_TEST_P:-$go_test_parallel}"

pkg_list=()
while IFS= read -r pkg; do
  pkg_list+=("$pkg")
done < <(
  if command -v rg >/dev/null 2>&1; then
    go list ./... \
      | rg -v '^github\.com/erpc/erpc/(cmd|test)($|/)'
  else
    go list ./... \
      | grep -Ev '^github\.com/erpc/erpc/(cmd|test)($|/)'
  fi
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
  go test ./cmd/... -count 1 -parallel "$go_test_parallel" -p "$go_test_p"
  go test "${pkg_list[@]}" -covermode=atomic -v -race -count 1 -parallel "$go_test_parallel" -p "$go_test_p" -timeout 15m -failfast=false
else
  go clean -testcache
  go test ./cmd/... -count 1 -parallel "$go_test_parallel" -p "$go_test_p" -v
  go test "${pkg_list[@]}" -count 1 -parallel "$go_test_parallel" -p "$go_test_p" -v -timeout 10m -failfast=false
fi
