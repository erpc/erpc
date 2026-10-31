# Streams: plan

Spec: [feature.md](feature.md). One PR per row; each ships alone and runs end to end. Before each PR its scenario rows (spec §14) run red for the reason the row gives; after it they pass, and so do every earlier PR's rows and the chain tracker rows that read subscriptions (CT§19 C1–C3, C6, D1–D7, X1, X4, N4). Build and test once per PR, at its end; heavy runs under `lockf /tmp/erpc-heavy-test.lock`. No GitHub Actions, no Docker.

Prerequisites:

1. erpc#1335 merged (chain tracker), with `erpc_tap_out_of_range_total` renamed there to `erpc_tap_end_total{reason,detail}` before its release (lead does it; spec §13 is the target).
2. manifesto#26 merged before MP opens.

## Order

```
wave 1   S1    MP    S5
            │
wave 2   S2    S3            (S3 merges after S2)
            │
wave 3   S4    K2            (S4 needs MP)
            │
wave 4   K1
            │
wave 5   K3
```

| PR | Repo | Step | Size | Needs | Runs beside |
|---|---|---|---|---|---|
| S1 | erpc | s1 session, ws extended | L | #1335 | MP, S5 |
| MP | manifesto | s4 proto | M | #26 merged | S1, S5, S2, S3 |
| S5 | erpc | s5 `X-ERPC-Min-Block` | S | #1335 | everything |
| S2 | erpc | s2 finality and reorg modes | L | S1 | S3, MP |
| S3 | erpc | s3 SSE | M | S1 (merges after S2) | S2 |
| S4 | erpc | s4 gRPC logs, pending, options | L | S2, MP | K2 |
| K1 | erpc | Go toolkit | L | S3, S4 | K2 |
| K2 | erpc | TypeScript toolkit | L | S2, S3 | S4, K1 |
| K3 | erpc | guide and docs page | M | K1, K2 | — |
| Q | erpc + manifesto | binary encoder | — | QUIC decision | — |

Sizes: S < 400 lines, M 400–1,200, L > 1,200, tests included. One writer per worktree.

## S1: session and ws extended

- Add:
  - `erpc/stream_options.go`: `StreamOptions`, parse from the ws `erpc` param and the ws URL query; accepts `from`, `heartbeat`, `blockEnd` (spec §3.1); `-32602` naming the key.
  - `erpc/stream_session.go`: start cursor (`HeadAsOf` or `from`), `ReadOpts.Resumable` with `from`, heartbeat timer (W clamped 5–30s), status watch (`HashVoting`, lag at half the bound), `stalled` for resumable taps (the CT§10.2 60s rule), end mapping (spec §3.4), `draining` on shutdown.
  - Encoder interface: `Data`, `Removed`, `Undo`, `BlockEnd`, `Heartbeat`, `Status`, `End`.
  - `erpc/stream_events.go`: control JSON (spec §5.2).
  - `erpc/ws_stream.go`: ws encoder; `erpc_subscription` frames; `end` removes the subscription only.
  - Metrics: `erpc_stream_messages_total`, `erpc_stream_bytes_total`, `erpc_stream_resume_total`, `erpc_stream_replay_blocks`, `erpc_stream_status_total`; `erpc_tap_end_total` (from #1335) gains reasons `stalled`, `draining`, `slow_client`.
- Change:
  - `runNewHeads`, `runLogs`, `StreamBlocks` run the session; standard ws keeps the legacy defaults (spec §5.4); `StreamBlocks` wire unchanged.
  - `removed_unsent` admits blocks at or below the start cursor (spec §4).
  - `eth_subscribe` pending strips the `erpc` object before naming the variant.
  - Docs: `docs/pages/operation/websocket.mdx` (extended section), `docs/pages/reference/metrics.mdx`.
- Delete: the three emit loops the session replaces.
- Harness: extended ws driver; fault proxy on client links.
- Tests: W1–W9; guards: CT D1, D2, D5, C1, C3, X4, and gRPC rows D3, N4.

## MP: manifesto proto

- Change `evm/stream.proto` exactly as spec §7; regenerate Go (`bun run generate:evm`); Rust compiles it in `rust/build.rs`.
- Change: `StreamBlocksResponse` construction in manifesto tests to the oneof.
- Tests: `go test ./evm/...`; `cargo jevtest run` in `rust/`; a round-trip check that a #26-era `StreamBlocksResponse` decodes as the new one (header set).
- Base: manifesto `main`, after #26 merges; its own PR.

## S5: `X-ERPC-Min-Block`

- Add: directive `X-ERPC-Min-Block` / `min-block` in the `common/request.go` registry; the wait before the request reads `Head` (spec §9), on the lane; `-32015` with `{minBlock, latest}`; `erpc_min_block_total`, `erpc_min_block_wait_seconds`.
- Change: `docs/pages/operation/directives.mdx`; `docs/pages/reference/errors.mdx`.
- Tests: M1–M4; guards CT P1, P5, P6, P12, K7.

## S2: finality and reorg modes

- Add:
  - Options `finality`, `reorg` (spec §3.1).
  - `erpc/tap_finality.go`: `finalityFeed`, composed over `laneFeed`; start at `HashAt(bound)`.
  - `indexer.ReadOpts.Hold` (`FinalHold`, the tap's place); the tracker holds headers from `reorgWindow` below the lowest place up while one is registered, at most 20,000 below the window per tap, 18,000 at subscribe (`finality_span`); `erpc_chain_tracker_final_hold_headers`.
  - `Head.Safe`, demand-driven: `safe` tags polled only while a `safe` tap is registered, with its finalized refreshes; not computed otherwise; ballot as `Finalized`; `evm.safeBlockSource` honoured; served-tip gauge `axis="safe"` while computed.
  - ws encoder: `undo` and `none` rules; `final_reverted`.
  - chainsim: `SafeDepth`; `FinalityDepth` above the window.
- Change: `erpc_tap_delivery_seconds` gains `finality`; docs ws and metrics pages; CT spec §4 `ReadOpts`, §6.4 `Head` (one-line pointers to this spec).
- Tests: F1–F7; guards CT C12, C13, P7, K6.

## S3: SSE

- Add: `server.sse.{enabled,maxStreams,maxStreamsPerIP}` (`common/config.go`, defaults, `tygo` → `typescript/config`); dispatch beside the ws upgrade in `http_server.go`; bypass `TimeoutHandler`, server `WriteTimeout` and the gzip middleware; `erpc/sse_stream.go` encoder with a flush per event, gzip only on `Accept-Encoding: gzip` (deflate flush per event), 10s write deadline (`http.ResponseController`), `retry:` rules; `erpc_sse_rejected_total`.
- Change: `docs/pages/operation/sse.mdx` (new), `_meta.js`, metrics page.
- Harness: SSE driver (plain `net/http`, parses `id`, `event`, `retry`).
- Tests: E1–E6, and F1, F3 through SSE.

## S4: gRPC logs, pending, options

- Add: `go.mod` to MP's manifesto commit; `erpc/grpc_stream_logs.go`, `erpc/grpc_stream_pending.go` encoders; `StreamBlocks` fields 5–7 and control messages; `ErrorInfo` on `OUT_OF_RANGE`; a stream interceptor capping long-lived streams at 100 per connection; `erpc_grpc_streams_rejected_total`.
- Change: `streamBlocksResponse` to the oneof; `docs/pages/reference/grpc-bds.mdx`.
- Harness: `StreamLogs` and pending drivers; an HTTP/2 idle-60s proxy that ignores PING.
- Tests: G1–G7, and F1, F2 through `StreamLogs`.

## K1: Go toolkit

- Add: `sdk/go/streams/` module (`github.com/erpc/erpc/sdk/go/streams`): config, `Run`, `Handler`, gRPC and SSE transports, watchdog, backoff, dedup, two-endpoint merge, `FileCursor` (spec §10.3, §10.4); `examples/`; root `go.mod` `replace` for `test/realworld`.
- Harness: in-process driver writing applied, undo and status records for the oracle.
- Tests: T1, T3, T4, T5 (Go).

## K2: TypeScript toolkit

- Add: `typescript/streams/` (`@erpc-cloud/streams`): same API over SSE and ws extended; Node ≥ 22, no runtime dependency; `examples/`; `test/driver.mjs` (config JSON in, JSONL out) run by `test/realworld` as a subprocess.
- Tests: T2, T3, T5, T6 (TS). `make test-realworld` requires `node` ≥ 22.

## K3: guide

- Add: `docs/pages/operation/streams-integration.mdx` (spec §10.5), including each package's `examples/` files; copies to `INTEGRATION.md` by `go generate` (K1 module) and `prepack` (K2); `make check-streams-guide` fails on drift.
- Tests: T7.

## Q: binary transport

Pending the QUIC decision: one encoder over the session framing spec §7 messages, plus its scenario rows, once the decision lands.
