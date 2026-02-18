# Local Perf Benches

Compare CPU + allocs between two git refs; optional pprof.

## Quick Run (single ref)

```bash
ERPC_BENCH_RESULT_MB=16 go test ./common -run '^$' -bench BenchmarkJsonRpcResponse_ParseFromStream_LargeResult -benchmem -count 5
ERPC_BENCH_READALL_MB=16 ERPC_BENCH_BUFPOOL_MB=16 go test ./util -run '^$' -bench 'Benchmark(ReadAll_Large|BufPool_CapAfterLargeReadAll)$' -benchmem -count 5
```

## Compare Two Refs

```bash
ERPC_BENCH_RESULT_MB=16 ERPC_BENCH_READALL_MB=16 ERPC_BENCH_BUFPOOL_MB=16 \
BENCH_COUNT=5 \
./scripts/bench/compare-refs.sh <ref-a> <ref-b>
```

Notes:
- Uses same benchmark files for both refs (overlays bench sources into each worktree).
- `BenchmarkBufPool_CapAfterLargeReadAll` reports `pool_cap_bytes` (post-return pool behavior).

## Optional pprof

```bash
PROFILE_DIR=/tmp/erpc-profiles \
BENCH_COUNT=3 \
./scripts/bench/compare-refs.sh <ref-a> <ref-b>

go tool pprof -top "${PROFILE_DIR}/<ref-a-sanitized>/cpu.pprof"
go tool pprof -top "${PROFILE_DIR}/<ref-b-sanitized>/cpu.pprof"
```

## Spike Compare (Local, e2e)

Runs two refs against a local mock upstream that returns oversized `eth_getLogs` bodies and measures:
- client success/failure + latency percentiles (via `scripts/spike_getlogs_upstream.py`)
- max RSS for the eRPC process
- heap pprof snapshots (before/after) + CPU pprof
- Postgres/Redis container stats (CPU%, Mem, IO)

Also starts local Redis + Postgres containers and writes an eRPC config that is intended to match prod behavior
as closely as possible (cache policies + sharedState + getLogs split settings). For older refs, the script will
omit YAML fields the binary doesn't support.

```bash
# Defaults: ref-a=4ca935a ref-b=HEAD
REQUESTS=20 CONCURRENCY=5 OVERSIZE_MB=80 OK_RANGE=1000 \
./scripts/bench/spike-compare.sh 4ca935a HEAD
```

Useful env vars:
- `KEEP_TMP=1`: keep artifacts (prints the path on exit).
- `ABORT_RSS_KIB=...`: kill eRPC if RSS exceeds this KiB threshold (safety).
- `UPSTREAM_THROTTLE_MIBPS=...`: throttle oversized upstream response throughput (simulates remote upstream).
- `UPSTREAM_DATA_HEX_LEN=...`: log `data` hex chars per item (bigger = fewer items, less Python overhead).
- `PG_PORT=15432`, `REDIS_PORT=16379`, `REDIS_IMAGE=redis:7.2-alpine`.

Repro "OOM-like" memory blowup on older refs (while keeping the machine safe):
```bash
KEEP_TMP=1 ABORT_RSS_KIB=1300000 \
REQUESTS=5 CONCURRENCY=5 OVERSIZE_MB=80 OK_RANGE=1000 \
UPSTREAM_THROTTLE_MIBPS=10 TIMEOUT_S=120 \
./scripts/bench/spike-compare.sh 4ca935a 959b71b
```
