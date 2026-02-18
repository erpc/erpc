#!/usr/bin/env bash
set -euo pipefail

ref_a="${1:-}"
ref_b="${2:-}"
if [[ -z "${ref_a}" || -z "${ref_b}" ]]; then
  echo "usage: $0 <ref-a> <ref-b>" >&2
  exit 2
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
tmp_root="${TMPDIR:-/tmp}/erpc-bench-compare.$(date +%s)"

bench_count="${BENCH_COUNT:-5}"
bench_re="${BENCH_RE:-Benchmark(JsonRpcResponse_ParseFromStream_LargeResult|ReadAll_Large|BufPool_CapAfterLargeReadAll)$}"
bench_pkgs="${BENCH_PKGS:-./common ./util}"

keep_tmp="${KEEP_TMP:-0}"
profile_dir="${PROFILE_DIR:-}"

sanitize_ref() {
  echo "$1" | tr '/:\\ ' '____'
}

run_ref() {
  local ref="$1"
  local dir="$2"
  local out="$3"

  # Overlay bench files from current HEAD so both refs run identical benches.
  if [[ -f "${repo_root}/common/json_rpc_bench_test.go" ]]; then
    cp "${repo_root}/common/json_rpc_bench_test.go" "${dir}/common/json_rpc_bench_test.go"
  else
    git -C "${repo_root}" show "HEAD:common/json_rpc_bench_test.go" > "${dir}/common/json_rpc_bench_test.go"
  fi
  if [[ -f "${repo_root}/util/reader_bench_test.go" ]]; then
    cp "${repo_root}/util/reader_bench_test.go" "${dir}/util/reader_bench_test.go"
  else
    git -C "${repo_root}" show "HEAD:util/reader_bench_test.go" > "${dir}/util/reader_bench_test.go"
  fi

  local extra=()
  if [[ -n "${profile_dir}" ]]; then
    local safe
    safe="$(sanitize_ref "${ref}")"
    mkdir -p "${profile_dir}/${safe}"
    extra+=("-cpuprofile" "${profile_dir}/${safe}/cpu.pprof")
    extra+=("-memprofile" "${profile_dir}/${safe}/mem.pprof")
  fi

  (
    cd "${dir}"
    # env passthrough for payload sizing
    if ((${#extra[@]})); then
      go test ${bench_pkgs} -run '^$' -bench "${bench_re}" -benchmem -count "${bench_count}" "${extra[@]}" -timeout 30m 2>&1 | tee "${out}"
    else
      go test ${bench_pkgs} -run '^$' -bench "${bench_re}" -benchmem -count "${bench_count}" -timeout 30m 2>&1 | tee "${out}"
    fi
  )
}

mkdir -p "${tmp_root}"
trap '[[ "${keep_tmp}" == "1" ]] || rm -rf "${tmp_root}"' EXIT

dir_a="${tmp_root}/a"
dir_b="${tmp_root}/b"

git -C "${repo_root}" worktree add --detach "${dir_a}" "${ref_a}" >/dev/null
git -C "${repo_root}" worktree add --detach "${dir_b}" "${ref_b}" >/dev/null
trap 'git -C "${repo_root}" worktree remove -f "${dir_a}" >/dev/null 2>&1 || true; git -C "${repo_root}" worktree remove -f "${dir_b}" >/dev/null 2>&1 || true; [[ "${keep_tmp}" == "1" ]] || rm -rf "${tmp_root}"' EXIT

safe_a="$(sanitize_ref "${ref_a}")"
safe_b="$(sanitize_ref "${ref_b}")"
out_a="${tmp_root}/out.${safe_a}.txt"
out_b="${tmp_root}/out.${safe_b}.txt"

run_ref "${ref_a}" "${dir_a}" "${out_a}"
run_ref "${ref_b}" "${dir_b}" "${out_b}"

echo
echo "bench outputs:"
echo "  ${out_a}"
echo "  ${out_b}"
echo

if command -v benchstat >/dev/null 2>&1; then
  benchstat "${out_a}" "${out_b}"
else
  # No hard dep; fetch on-demand.
  go run golang.org/x/perf/cmd/benchstat@latest "${out_a}" "${out_b}"
fi

if [[ "${keep_tmp}" == "1" ]]; then
  echo
  echo "kept tmp: ${tmp_root}"
fi
