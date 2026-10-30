# Streams: plan

Spec: [feature.md](feature.md). One PR per row; each ships alone and runs end to end. Before each PR its scenario rows (spec §13) run red for the reason the row gives; after it they pass, and so do every earlier PR's rows and the chain tracker rows that read subscriptions (CT§19 C1–C3, C6, D1–D7, X1, X4, N4). Build and test once per PR, at its end; heavy runs under `lockf /tmp/erpc-heavy-test.lock`. No GitHub Actions, no Docker.

Prerequisites:

1. erpc#1335 merged (chain tracker), including the rename `erpc_tap_out_of_range_total` → `erpc_tap_end_total{reason,detail}` (`bc988b2e`).
2. manifesto#26 merged before MP opens.

## Order

```
wave 1   S1    MP    S5
            │
wave 2   S2    MS            (MS needs MP)
            │
wave 3   S4                  (needs S2 and MP)
            │
wave 4   K1    K2
            │
wave 5   K3                  (needs K1, K2, MS)
```

| PR | Repo | Step | Size | Needs | Runs beside |
|---|---|---|---|---|---|
| S1 | erpc | s1 session, ws extended | L | #1335 | MP, S5 |
| MP | manifesto | s4 proto | M | #26 merged | S1, S5 |
| S5 | erpc | s5 `X-ERPC-Min-Block` | S | #1335 | everything |
| S2 | erpc | s2 finality and reorg modes | L | S1 | MS |
| MS | manifesto | Python and Java stubs | M | MP | S2, S4 |
| S4 | erpc | s4 gRPC logs, pending, options, tuning | L | S2, MP | MS |
| K1 | erpc | Go library | L | S4 | K2 |
| K2 | erpc | TypeScript/Node library | L | S4 | K1 |
| K3 | erpc | guide | M | K1, K2, MS | — |

Sizes: S < 400 lines, M 400–1,200, L > 1,200, tests included. One writer per worktree.

## S1: session and ws extended

- Add:
  - `erpc/stream_options.go`: `StreamOptions`, parse from the ws `erpc` param and the ws URL query; accepts `from`, `heartbeat`, `blockEnd` (spec §3.1); `-32602` naming the key.
  - `erpc/stream_session.go`: start cursor (`HeadAsOf` or `from`), `ReadOpts.Resumable` with `from`, heartbeat timer (W clamped 5–30s), status watch (`HashVoting`, lag at half the bound), `stalled` for resumable taps (the CT§10.2 60s rule), end mapping (spec §3.4), `draining` on shutdown.
  - Encoder interface: `Data`, `Removed`, `Undo`, `BlockEnd`, `Heartbeat`, `Status`, `End`.
  - `erpc/stream_events.go`: control JSON (spec §5.2).
  - `erpc/ws_stream.go`: ws encoder; `erpc_subscription` frames; `end` removes the subscription only.
  - Metrics: `erpc_stream_messages_total`, `erpc_stream_bytes_total`, `erpc_stream_resume_total`, `erpc_stream_replay_blocks`, `erpc_stream_status_total`; `erpc_tap_end_total` gains reasons `stalled`, `draining`, `slow_client`.
- Change:
  - `runNewHeads`, `runLogs`, `StreamBlocks` run the session; standard ws keeps the legacy defaults (spec §5.4); `StreamBlocks` wire unchanged.
  - `removed_unsent` admits blocks at or below the start cursor (spec §4).
  - `eth_subscribe` pending strips the `erpc` object before naming the variant.
  - Docs: `docs/pages/operation/websocket.mdx` (extended section), `docs/pages/reference/metrics.mdx`.
- Delete: the three emit loops the session replaces.
- Harness: extended ws driver; fault proxy on client links.
- Tests: W1–W9; guards: CT D1, D2, D5, C1, C3, X4, and gRPC rows D3, N4.

## MP: manifesto proto

- Change `evm/stream.proto` exactly as spec §6.1; regenerate Go (`bun run generate:evm`); Rust compiles it in `rust/build.rs`.
- Change: `StreamBlocksResponse` construction in manifesto tests to the oneof.
- Tests: `go test ./evm/...`; `cargo jevtest run` in `rust/`; a round-trip check that a #26-era `StreamBlocksResponse` decodes as the new one (header set).
- Base: manifesto `main`, after #26 merges; its own PR.

## S5: `X-ERPC-Min-Block`

- Add: directive `X-ERPC-Min-Block` / `min-block` in the `common/request.go` registry; the wait before the request reads `Head` (spec §8), on the lane; `-32015` with `{minBlock, latest}`; `erpc_min_block_total`, `erpc_min_block_wait_seconds`.
- Change: `docs/pages/operation/directives.mdx`; `docs/pages/reference/errors.mdx`.
- Tests: M1–M4; guards CT P1, P5, P6, P12, K7.

## S2: finality and reorg modes

- Add:
  - Options `finality`, `reorg` (spec §3.1).
  - `erpc/tap_finality.go`: `finalityFeed`, composed over `laneFeed`; start at `HashAt(bound)`.
  - `indexer.ReadOpts.HoldFinalized`; the tracker holds headers from `Head.Finalized` up while one is registered, at most 20,000 (`finality_span`); `erpc_chain_tracker_final_hold_headers`.
  - `Head.Safe`, demand-driven: `safe` tags polled only while a `safe` tap is registered, with its finalized refreshes; not computed otherwise; ballot as `Finalized`; `evm.safeBlockSource` honoured; served-tip gauge `axis="safe"` while computed.
  - ws encoder: `undo` and `none` rules; `final_reverted`.
  - chainsim: `SafeDepth`; `FinalityDepth` above the window.
- Change: `erpc_tap_delivery_seconds` gains `finality`; docs ws and metrics pages; CT spec §4 `ReadOpts`, §6.4 `Head` (one-line pointers to this spec).
- Tests: F1–F7; guards CT C12, C13, P7, K6.

## MS: manifesto stubs

- Add: generated Python (`grpcio-tools`) and Java (`protoc-gen-grpc-java`) packages for `evm/*.proto` beside Go and Rust, with a `generate` script each and a README naming the guide; package names and publishing chosen with the manifesto owners.
- Tests: each package compiles; a smoke client per language calls `StreamBlocks` on a local eRPC built from S1.

## S4: gRPC logs, pending, options, tuning

- Add:
  - `go.mod` to MP's manifesto commit.
  - `erpc/grpc_stream_logs.go`: one `BlockLogs` per applied block, at most 1,000 logs and 1 MiB per message, `complete` on the last; `Undo`; proto logs converted once per block and cached with the block's receipts; a filter's messages reference them, no copy (spec §6.2).
  - `architecture/evm/filter_state.go`: per-filter state on held receipts (positions, encodings) bounded at 32 MiB per network, oldest out; `erpc_chain_tracker_filter_state_bytes`, `erpc_chain_tracker_filter_state_evicted_total`.
  - `erpc/grpc_stream_pending.go`: one `PendingTransactions` per mempool Read, at most 1,000; each transaction converted once on its ring entry (`indexer.PendingTx`, CT§4).
  - `StreamBlocks` fields 5–7 and control messages; AIP-193 `ErrorInfo` on `OUT_OF_RANGE` and `message_too_large` (spec §3.4).
  - Server options (spec §6.3): `SharedWriteBuffer(true)`, default windows; streams send uncompressed; `MaxConnectionAge` stays 10 min.
  - A stream interceptor capping long-lived streams at 100 per connection; `erpc_grpc_streams_rejected_total`.
- Change: `streamBlocksResponse` to the oneof; `docs/pages/reference/grpc-bds.mdx` (messages per block, connection age, resume).
- Harness: `StreamLogs` and pending drivers; client-link RTT; an HTTP/2 idle-60s proxy that ignores PING.
- Tests: G1–G10, and F1, F2 through `StreamLogs`.

## K1: Go library

- Add: `sdk/go/streams/` module (`github.com/erpc/erpc/sdk/go/streams`): config, `Run`, `Handler`, gRPC transport with the spec §6.3 windows, watchdog, reconnect (at once after a flowing `UNAVAILABLE`, else backoff), dedup, two-endpoint merge, `FileCursor` (spec §9.4, §9.5); `examples/`; root `go.mod` `replace` for `test/realworld`.
- Harness: in-process driver writing applied, undo and status records for the oracle.
- Tests: T1, T3, T4, T5, T6 (Go).

## K2: TypeScript/Node library

- Add: `typescript/streams/` (`@erpc-cloud/streams`): same API on `@grpc/grpc-js`, `grpc-node.flow_control_window` set; `ts-proto` stubs generated from MP's protos into the package; Node ≥ 22; `examples/`; `test/driver.mjs` (config JSON in, JSONL out) run by `test/realworld` as a subprocess.
- Tests: T2, T3, T5, T6 (TS). `make test-realworld` requires `node` ≥ 22.

## K3: guide

- Add: `docs/pages/operation/streams-integration.mdx` (spec §9.6): libraries, Python/Rust/Java stub sections, ws extended section; each code block a file under `examples/`; copies to `INTEGRATION.md` by `go generate` (K1 module) and `prepack` (K2); `make check-streams-guide` fails on drift.
- Tests: T7.
