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

