#!/usr/bin/env bash
set -euo pipefail

# Sustained upstream eth_getLogs spike.
# - Secret read once from stdin (no echo), never printed.
# - Uses X-ERPC-Skip-Cache-Read to force upstream (cache read bypass).
#
# Tuning (env):
# - ERPC_URL (default https://rpc.morpho.dev/cache/evm/8453)
# - CONCURRENCY (default 200)
# - REQUESTS_PER_ROUND (default 2000)
# - SPAN_BLOCKS (default 12000)
# - JITTER_BLOCKS (default 8000)
# - TIMEOUT_S (default 30)
#
# Example:
#   ./scripts/run_getlogs_spike_upstream_forever.sh

ERPC_URL="${ERPC_URL:-https://rpc.morpho.dev/cache/evm/8453}"
CONCURRENCY="${CONCURRENCY:-200}"
PROCESSES="${PROCESSES:-2}"
REQUESTS_PER_ROUND="${REQUESTS_PER_ROUND:-2000}"
SPAN_BLOCKS="${SPAN_BLOCKS:-12000}"
JITTER_BLOCKS="${JITTER_BLOCKS:-8000}"
TIMEOUT_S="${TIMEOUT_S:-30}"

if [[ -t 0 ]]; then
  stty -echo || true
fi
IFS= read -r ERPC_SECRET || true
if [[ -t 0 ]]; then
  stty echo || true
  printf "\n" >&2
fi

if [[ -z "${ERPC_SECRET}" ]]; then
  echo "missing secret on stdin" >&2
  exit 2
fi

while true; do
  # Bump FD soft limit for this shell + its children; avoids local client flakiness.
  ulimit -n 4096 >/dev/null 2>&1 || true

  ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  # Split load across processes to avoid single-process thread/fd pressure.
  # Per-process values round up to keep total roughly >= requested.
  if [[ "${PROCESSES}" -lt 1 ]]; then
    PROCESSES=1
  fi
  conc_per=$(( (CONCURRENCY + PROCESSES - 1) / PROCESSES ))
  req_per=$(( (REQUESTS_PER_ROUND + PROCESSES - 1) / PROCESSES ))

  echo "round_start ts=${ts} url=${ERPC_URL} procs=${PROCESSES} conc_total=${CONCURRENCY} conc_each=${conc_per} req_total=${REQUESTS_PER_ROUND} req_each=${req_per} span=${SPAN_BLOCKS} jitter=${JITTER_BLOCKS} timeout_s=${TIMEOUT_S}" >&2

  pids=()
  for _ in $(seq 1 "${PROCESSES}"); do
    (
      printf '%s\n' "${ERPC_SECRET}" | \
        REQUESTS="${req_per}" \
        CONCURRENCY="${conc_per}" \
        SPAN_BLOCKS="${SPAN_BLOCKS}" \
        JITTER_BLOCKS="${JITTER_BLOCKS}" \
        TIMEOUT_S="${TIMEOUT_S}" \
        PYTHONUNBUFFERED=1 \
        ./scripts/spike_getlogs_upstream.py \
          --url "${ERPC_URL}" \
          --secret-stdin \
        >/dev/null
    ) &
    pids+=("$!")
  done

  rc=0
  for pid in "${pids[@]}"; do
    if ! wait "${pid}"; then
      rc=1
    fi
  done

  ts2="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "round_end ts=${ts2} rc=${rc}" >&2

  # Short breather to avoid synchronized thundering.
  sleep 1
done
