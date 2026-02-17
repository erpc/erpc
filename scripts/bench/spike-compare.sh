#!/usr/bin/env bash
set -euo pipefail

ref_a="${1:-4ca935a}"
ref_b="${2:-HEAD}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
tmp_root="${TMPDIR:-/tmp}/erpc-spike-compare.$(date +%s)"
keep_tmp="${KEEP_TMP:-0}"

erpc_port="${ERPC_PORT:-14030}"
pprof_port="${PPROF_PORT:-16060}"
upstream_port="${UPSTREAM_PORT:-14031}"

oversize_mb="${OVERSIZE_MB:-80}"     # upstream oversize target (MiB), > newer default cap (64MiB)
ok_range="${OK_RANGE:-25}"

requests="${REQUESTS:-120}"
concurrency="${CONCURRENCY:-30}"
timeout_s="${TIMEOUT_S:-25}"
span_blocks="${SPAN_BLOCKS:-8000}"
jitter_blocks="${JITTER_BLOCKS:-2000}"
abort_rss_kib="${ABORT_RSS_KIB:-0}"

erpc_secret="${ERPC_SECRET:-local-secret}"

sanitize_ref() { echo "$1" | tr '/:\\ ' '____'; }

mkdir -p "${tmp_root}"
work_a="${tmp_root}/a"
work_b="${tmp_root}/b"

stop_pid() {
  local pid="$1"
  if [[ -z "${pid}" ]]; then
    return 0
  fi
  kill "${pid}" >/dev/null 2>&1 || true
  for _ in $(seq 1 25); do
    if ! kill -0 "${pid}" >/dev/null 2>&1; then
      wait "${pid}" >/dev/null 2>&1 || true
      return 0
    fi
    sleep 0.1
  done
  kill -9 "${pid}" >/dev/null 2>&1 || true
  wait "${pid}" >/dev/null 2>&1 || true
}

git -C "${repo_root}" worktree add --detach "${work_a}" "${ref_a}" >/dev/null
git -C "${repo_root}" worktree add --detach "${work_b}" "${ref_b}" >/dev/null

pg_container="erpc-postgresql-spike-compare"
pg_port="${PG_PORT:-15432}"

redis_container="erpc-redis-spike-compare"
redis_port="${REDIS_PORT:-16379}"
redis_image="${REDIS_IMAGE:-redis:7.2-alpine}"

cleanup() {
  set +e
  stop_pid "${up_pid:-}"
  stop_redis
  stop_postgres
  git -C "${repo_root}" worktree remove -f "${work_a}" >/dev/null 2>&1 || true
  git -C "${repo_root}" worktree remove -f "${work_b}" >/dev/null 2>&1 || true
  if [[ "${keep_tmp}" == "1" ]]; then
    echo "KEEP_TMP=1 artifacts: ${tmp_root}" >&2
  else
    rm -rf "${tmp_root}" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

start_postgres() {
  if docker ps --format '{{.Names}}' | rg -q "^${pg_container}\$"; then
    return 0
  fi
  if docker ps -a --format '{{.Names}}' | rg -q "^${pg_container}\$"; then
    docker rm -f "${pg_container}" >/dev/null 2>&1 || true
  fi
  docker run -d --name "${pg_container}" -e POSTGRES_USER=erpc -e POSTGRES_PASSWORD=erpc -e POSTGRES_DB=erpc -p "127.0.0.1:${pg_port}:5432" postgres:13.4 >/dev/null
  # wait ready
  for _ in $(seq 1 60); do
    if docker exec "${pg_container}" pg_isready -U erpc >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  echo "postgres not ready" >&2
  return 1
}

stop_postgres() {
  docker rm -f "${pg_container}" >/dev/null 2>&1 || true
}

start_redis() {
  if docker ps --format '{{.Names}}' | rg -q "^${redis_container}\$"; then
    return 0
  fi
  if docker ps -a --format '{{.Names}}' | rg -q "^${redis_container}\$"; then
    docker rm -f "${redis_container}" >/dev/null 2>&1 || true
  fi
  docker run -d --name "${redis_container}" -p "127.0.0.1:${redis_port}:6379" "${redis_image}" >/dev/null
  for _ in $(seq 1 60); do
    if docker exec "${redis_container}" redis-cli ping >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.2
  done
  echo "redis not ready" >&2
  return 1
}

stop_redis() {
  docker rm -f "${redis_container}" >/dev/null 2>&1 || true
}

start_upstream() {
  python3 "${repo_root}/scripts/mock_evm_upstream_biglogs.py" \
    --port "${upstream_port}" \
    --ok-range "${ok_range}" \
    --oversize-mb "${oversize_mb}" \
    --data-hex-len "${UPSTREAM_DATA_HEX_LEN:-131072}" \
    --throttle-mibps "${UPSTREAM_THROTTLE_MIBPS:-0}" \
    >"${tmp_root}/upstream.stdout.log" 2>"${tmp_root}/upstream.stderr.log" &
  echo $!
}

write_config() {
  local path="$1"
  local supports_envelope="${2:-1}"
  local supports_shared_state="${3:-1}"

  {
    cat <<YAML
logLevel: warn
database:
  evmJsonRpcCache:
YAML

    if [[ "${supports_envelope}" == "1" ]]; then
      cat <<YAML
    envelope: true
YAML
    fi

    cat <<YAML
    connectors:
      - id: "postgres-connector"
        driver: "postgresql"
        postgresql:
          connectionUri: "postgres://erpc:erpc@127.0.0.1:${pg_port}/erpc?sslmode=disable"
          table: rpc_cache
          initTimeout: 5s
          getTimeout: 5s
          setTimeout: 5s
          minConns: 10
          maxConns: 50
      - id: "redis-connector"
        driver: "redis"
        redis:
          uri: "redis://127.0.0.1:${redis_port}"
          getTimeout: 3s
          setTimeout: 3s
    policies:
      # Copy of prod intent: immutable block-specific reads longer TTL; defaults by finality.
      - network: "*"
        method: "eth_getBlockByNumber"
        finality: "finalized"
        empty: "ignore"
        connector: "postgres-connector"
        ttl: 0s

      - network: "*"
        method: "eth_call"
        params: ["*", "0x*"]
        finality: "unfinalized"
        connector: "redis-connector"
        ttl: 300s

      - network: "*"
        method: "eth_getStorageAt"
        params: ["*", "*", "0x*"]
        finality: "unfinalized"
        connector: "redis-connector"
        ttl: 300s

      - network: "*"
        method: "eth_getBalance"
        params: ["*", "0x*"]
        finality: "unfinalized"
        connector: "redis-connector"
        ttl: 300s

      - network: "*"
        method: "eth_getBlockByNumber"
        params: ["0x*", "*"]
        finality: "unfinalized"
        connector: "redis-connector"
        ttl: 300s

      - network: "*"
        method: "eth_getCode"
        params: ["*", "0x*"]
        finality: "unfinalized"
        connector: "redis-connector"
        ttl: 300s

      - network: "*"
        method: "eth_chainId"
        finality: "unknown"
        connector: "postgres-connector"
        ttl: 0s

      - network: "*"
        method: "*"
        finality: "finalized"
        empty: "allow"
        connector: "postgres-connector"
        ttl: 0s

      - network: "*"
        method: "*"
        finality: "unfinalized"
        connector: "redis-connector"
        ttl: 30s

      - network: "*"
        method: "*"
        finality: "realtime"
        connector: "redis-connector"
        ttl: 5s

      - network: "*"
        method: "*"
        finality: "unknown"
        connector: "redis-connector"
        ttl: 5s
YAML

    if [[ "${supports_shared_state}" == "1" ]]; then
      cat <<YAML
  sharedState:
    clusterKey: "erpc-morpho-cluster"
    connector:
      driver: "redis"
      redis:
        uri: "redis://127.0.0.1:${redis_port}"
YAML
    fi

    cat <<YAML
server:
  httpHostV4: 127.0.0.1
  httpPortV4: ${erpc_port}
  maxTimeout: 120s
metrics:
  enabled: false
projects:
  - id: cache
    auth:
      strategies:
        - type: secret
          rateLimitBudget: local
          secret:
            value: "\${ERPC_SECRET}"
    networks:
      - architecture: evm
        evm:
          chainId: 8453
          integrity:
            enforceGetLogsBlockRange: true
            enforceHighestBlock: false
          getLogsSplitOnError: true
          getLogsSplitConcurrency: 100
          getLogsMaxAllowedRange: 150001
        failsafe:
          timeout:
            duration: "120s"
          retry:
            maxAttempts: 2
            delay: "1s"
            backoffMaxDelay: "10s"
            backoffFactor: 3
          hedge:
            delay: "2s"
            minDelay: "1s"
            maxDelay: "5s"
            maxCount: 1
            quantile: 0.95
    upstreams:
      - id: up
        type: evm
        endpoint: http://127.0.0.1:${upstream_port}
        evm:
          chainId: 8453
rateLimiters:
  budgets:
    - id: local
      rules:
        - method: "*"
          maxCount: 1000000
          period: 1s
YAML
  } >"${path}"
}

build_erpc() {
  local workdir="$1"
  local out="$2"
  (cd "${workdir}" && go build -tags pprof -o "${out}" ./cmd/erpc)
}

run_one_ref() {
  local ref="$1"
  local workdir="$2"
  local safe
  safe="$(sanitize_ref "${ref}")"

  local out_dir="${tmp_root}/${safe}"
  mkdir -p "${out_dir}"

  local bin="${out_dir}/erpc"
  build_erpc "${workdir}" "${bin}"

  local cfg="${out_dir}/erpc.yaml"
  local supports_envelope=0
  local supports_shared_state=0
  if rg -q --no-messages 'yaml:"envelope"' "${workdir}"; then
    supports_envelope=1
  fi
  if rg -q --no-messages 'yaml:"sharedState"' "${workdir}"; then
    supports_shared_state=1
  fi
  write_config "${cfg}" "${supports_envelope}" "${supports_shared_state}"

  local erpc_log="${out_dir}/erpc.log"
  local rss_log="${out_dir}/rss_kib.tsv"
  local spike_out="${out_dir}/spike.stdout.log"
  local spike_err="${out_dir}/spike.stderr.log"
  local heap_before="${out_dir}/heap.before.prof"
  local heap_after="${out_dir}/heap.after.prof"
  local heap_before_top="${out_dir}/heap.before.top.txt"
  local heap_after_top="${out_dir}/heap.after.top.txt"
  local cpu_prof="${out_dir}/cpu.prof"
  local cpu_top="${out_dir}/cpu.top.txt"
  local docker_stats="${out_dir}/docker_stats.tsv"

  ERPC_PPROF_PORT="${pprof_port}" ERPC_SECRET="${erpc_secret}" "${bin}" start --config "${cfg}" >"${erpc_log}" 2>&1 &
  local erpc_pid=$!

  # wait http
  local ready=0
  for _ in $(seq 1 100); do
    if curl -fsS "http://127.0.0.1:${pprof_port}/debug/pprof/" >/dev/null 2>&1; then
      ready=1
      break
    fi
    sleep 0.1
  done
  if [[ "${ready}" != "1" ]]; then
    echo "pprof not ready on :${pprof_port} (ref=${ref})" >&2
    tail -n 200 "${erpc_log}" >&2 || true
    stop_pid "${erpc_pid}"
    return 1
  fi

  # baseline heap + rss
  curl -fsS "http://127.0.0.1:${pprof_port}/debug/pprof/heap?gc=1" >"${heap_before}"

  (
    while kill -0 "${erpc_pid}" >/dev/null 2>&1; do
      ts="$(date +%s)"
      rss="$(ps -o rss= -p "${erpc_pid}" | tr -dc '0-9' || true)"
      if [[ -n "${rss}" ]]; then
        printf "%s\t%s\n" "${ts}" "${rss}" >>"${rss_log}"
        if [[ "${abort_rss_kib}" != "0" ]] && [[ "${rss}" -gt "${abort_rss_kib}" ]]; then
          echo "ABORT_RSS_KIB exceeded: rss_kib=${rss} limit=${abort_rss_kib}" >>"${erpc_log}"
          kill -9 "${erpc_pid}" >/dev/null 2>&1 || true
          break
        fi
      fi
      sleep 0.2
    done
  ) &
  local mon_pid=$!

  (
    while kill -0 "${erpc_pid}" >/dev/null 2>&1; do
      ts="$(date +%s)"
      docker stats --no-stream --format '{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.BlockIO}}\t{{.NetIO}}' "${pg_container}" "${redis_container}" 2>/dev/null \
        | awk -v ts="${ts}" '{print ts "\t" $0}' >>"${docker_stats}" || true
      sleep 0.5
    done
  ) &
  local docker_mon_pid=$!

  curl -fsS "http://127.0.0.1:${pprof_port}/debug/pprof/profile?seconds=10" >"${cpu_prof}" 2>/dev/null &
  local cpu_pid=$!

  # spike
  printf "%s\n" "${erpc_secret}" | python3 "${repo_root}/scripts/spike_getlogs_upstream.py" \
    --secret-stdin \
    --url "http://127.0.0.1:${erpc_port}/cache/evm/8453" \
    --requests "${requests}" \
    --concurrency "${concurrency}" \
    --timeout "${timeout_s}" \
    --span-blocks "${span_blocks}" \
    --jitter-blocks "${jitter_blocks}" \
    --skip-cache-read \
    >"${spike_out}" 2>"${spike_err}" || true

  curl -fsS "http://127.0.0.1:${pprof_port}/debug/pprof/heap?gc=1" >"${heap_after}"

  go tool pprof -top -inuse_space "${heap_before}" >"${heap_before_top}" 2>/dev/null || true
  go tool pprof -top -inuse_space "${heap_after}" >"${heap_after_top}" 2>/dev/null || true
  wait "${cpu_pid}" >/dev/null 2>&1 || true
  go tool pprof -top "${cpu_prof}" >"${cpu_top}" 2>/dev/null || true

  stop_pid "${docker_mon_pid}"
  stop_pid "${mon_pid}"
  stop_pid "${erpc_pid}"

  # Extract spike summary + max rss
  local sum_line
  sum_line="$(rg -n "^summary " -m 1 "${spike_err}" | sed -E 's/^.*summary /summary /' || true)"
  local max_rss
  max_rss="$(awk '{if ($2>m) m=$2} END{print m+0}' "${rss_log}" 2>/dev/null || echo 0)"

  printf "ref=%s\n" "${ref}"
  printf "max_rss_kib=%s\n" "${max_rss}"
  printf "%s\n" "${sum_line}"
  printf "heap_after_top=%s\n" "${heap_after_top}"
  printf "\n"
}

start_postgres
start_redis
up_pid="$(start_upstream)"

echo "running spike compare in ${tmp_root}" >&2
echo >&2

run_one_ref "${ref_a}" "${work_a}" | tee "${tmp_root}/summary.${ref_a}.txt" >&2
run_one_ref "${ref_b}" "${work_b}" | tee "${tmp_root}/summary.${ref_b}.txt" >&2

echo "artifacts: ${tmp_root}" >&2
