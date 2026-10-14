# Chain tracker: plan

Spec: [feature.md](feature.md). One slice per commit; each ships on its own. At each slice's end: one `go build ./...` and the tests that slice extends; no builds between edits.

```
slice 0  skeleton: wss heads → intake → tracker → indexer → clients
slice 1  HTTP subscription sources
slice 2  linkage, gap fill, reorgs; follower ticker gone
slice 3  logs from receipts; vendor log subscriptions gone
slice 4  integrity on tracker data
slice 5  gRPC StreamBlocks as indexer egress; block_stream hub gone
slice 6  docs, last deletions, one build and test run, PR
```

## Before slice 0

| # | Who | What | Blocks |
|---|---|---|---|
| 1 | Aram | Answer spec §13 open questions 1–3 | slices 1, 3, 5 |
| 2 | Aram | Approve read-only `eth_getBlockReceipts` calls on each Edge vendor (eth, sepolia, hoodi, monad) | slice 3 |
| 3 | agent | Run those calls; record support per vendor in the spec | slice 3 |

## Slice 0: skeleton

Boundaries are final here; each component starts simple.

- Add:
  - `ChainTracker` in `architecture/evm/chain_tracker.go`, moved from `chainView`; integrity interfaces unchanged.
  - `indexer.ChainSink`: `Head` only does work; `WantsLogs`, `Logs`, `Unwind`, `Gap` are wired, never called yet.
  - `EvmStatePoller.ObserveHead(raw)`: parse header; gates 1–2, moved out of `SuggestLatestBlock`; accepted headers go to `OnHeader`.
  - `Network` registers `OnHeader` per upstream at bootstrap → `Network.intakeHead`.
  - Tracker `Head` calls `NoteObservedLatestBlock` before the sink.
- Change:
  - `wsupstream.handleNewHeads` calls `ObserveHead` instead of `sink.Ingest`.
  - Tracker adopts a header only when it is higher than the head (linkage comes in slice 2).
- Delete:
  - `NetworkHandle.SuggestLatestBlock`, `networkHandle.heads`, `headSource`.
  - Indexer `headMarker`, `lastHead`, `headMu`, and the `KindNewHead` branch of `Ingest`.
- Tests: `TestWebSocket_Subscriptions`, `TestWebSocket_SubscriptionDedup`, `TestWebSocket_EveryUpstreamHeadReachesItsPoller` stay green; the `networks_ws_tip_test.go` gate tests call `intakeHead`.

## Slice 1: HTTP subscription sources

- Add:
  - `upstreams[].evm.subscriptionSource` (`*bool`): defaults, validation, TS config type.
  - Network demand refcount: client subscription, gRPC stream, `integrity.follow.enabled`.
  - Poller fast timer in its existing goroutine at `resolveDebounce`, on while demanded.
  - Source liveness for gate 4: wss subscription live, or HTTP poll ok within 2 periods.
  - Idle stop per open question 1.
- Change:
  - `fetchBlock("latest")` returns the header bytes; the poll path calls `ObserveHead`.
  - `enrichStatePoller` passes `eth_getBlockByNumber(latest)` response headers to `ObserveHead`.
  - wss `newHeads` subscription starts and stops with demand.
  - `bootstrapNetwork` accepts any subscription source.
- Rename: `ErrNoWsUpstreamAvailable` → `ErrNoSubscriptionSource`.
- Tests, extend `erpc/ws_server_test.go`:
  - HTTP-only network, `subscriptionSource: true`: client gets polled heads in order.
  - Same network without the flag: `eth_subscribe` fails with `ErrNoSubscriptionSource`.
  - wss + HTTP on one network: each head reaches the client once.

## Slice 2: linkage, gap fill, reorgs

- Add, in the tracker:
  - Spec §6 rules 1–9: parent linkage, gap fill by number, ancestor walk by hash, push confirmation via `ReconfirmPin`.
  - `Unwind` and `Gap` events; the indexer ignores them for heads.
  - `ObserveTarget(served tip)` while no source is live.
  - Config `chainTracker.reorgWindow`, `chainTracker.maxBackfill`.
  - Tracker `Head` feeds `stateProberFor(...).onNewHead`, as `chainFollower.advance` does today.
  - Metric `erpc_chain_tracker_source_heads_total`.
- Delete:
  - `chainFollower` ticker, `advance`, `step`, `ingest`, `followerDefaultInterval`, `followerDefaultMaxPerTick`.
  - `integrity.reorgWindow`, `integrity.follow.interval`, `integrity.follow.maxBlocksPerTick`, at project, network and profile level.
- Rename: `erpc_integrity_follow_*`, `erpc_integrity_reorg_depth` → `erpc_chain_tracker_*`.
- Tests:
  - Delete `integrity_chainfollower_test.go`; its fork, gap and unreconciled scenarios move into the e2e tests below (Aram confirms in review).
  - ws e2e: wss source drops for 3 blocks, HTTP source keeps going; client gets every head in order.
  - ws e2e: a source on a minority fork pushes 2 blocks; the network answers the majority hash; the client gets no `Unwind`.
  - ws e2e: the network answer moves to the new branch; the client gets `Unwind` once, then the new heads.

## Slice 3: logs from receipts

- Add:
  - `architecture/evm/logfilter`: `LogsFilter`, `Matches`, bloom test; moved from `integrity/logfilter.go`.
  - Indexer logs filter registry and matching; `WantsLogs` from the bloom test.
  - Tracker: receipts fetch by hash, logs cursor, empty-root skip, per-network bloom proof, receipts kept in the window.
  - Indexer `Logs`, `Unwind` with `removed:true`, `Gap` per open question 2.
- Delete:
  - wsupstream logs subscriptions: logs `EnsureFilter` path, `stripFromBlockZero`, `isZeroBlockRef`.
  - `networks[].evm.stripSubscribeFromBlockZero`.
  - Logs case of `DedupKeyForFilter`, logs dedup windows, `logRemoved`.
  - `IngressSelector` and `reconcile` keep only pendingTx callers.
- Tests, extend `TestWebSocket_LogsSubscribeOnAllUpstreamsAndDedupe` (rename to match):
  - 2 filters, 3 blocks: one `eth_getBlockReceipts` per block, no upstream `eth_subscribe logs`.
  - Reorg of 1 block: removed logs of the old block, then logs of the new one.
  - Block with a bloom no filter matches: no receipts call.

## Slice 4: integrity on tracker data

- Add:
  - Header checks in `intakeHead` for every source while integrity is on.
  - An explicit exemption flag for resolver corroboration fetches.
- Change: hooks validate tracker fetches; `IsInternal` no longer skips integrity.
- Tests, extend `erpc/http_server_blocknumber_integrity_test.go` (its multi-upstream integrity setup):
  - One upstream serves receipts with a forged log: rejected by `receiptsRootRecompute`, served from the other upstream.
  - A pushed header with a wrong hash: dropped, `outcome="gated_integrity"`.

## Slice 5: gRPC StreamBlocks

- Add: `StreamBlocks` egress on the indexer, from `Head` events; reorg per open question 3.
- Delete: `blockStreamManager`, `blockStreamHub`, `fetchBlockHeader`, `blockStreamMaxBackfill`.
- Tests: extend `erpc/block_stream_test.go`: a stream gets every block once, in order, with no `eth_getBlockByNumber` per block.

## Slice 6: docs, deletions, verify, PR

- Docs:
  - `docs/pages/operation/websocket.mdx`: sources, HTTP polling, reorgs, logs from receipts, failure errors.
  - `docs/pages/config/projects/upstreams.mdx`: `subscriptionSource`.
  - `docs/pages/config/projects/networks.mdx`: `chainTracker`, deleted keys.
  - `docs/pages/config/failsafe/integrity.mdx`: follow section.
  - `docs/pages/reference/metrics.mdx`: new and renamed metrics.
- Delete: `indexer.canonicalChainDepth`.
- Specs: `specs/data-integrity/chain-view.md` points to this spec for window, follower and fork choice.
- Verify, once over all slices: `go build ./...`; `go test` for `./indexer/...`, `./architecture/evm/...`, `./erpc/...`; fix what fails.
- Smoke: local eRPC with an HTTP-only monad source; `wscat` `newHeads` and a Transfer `logs` filter for 5 minutes.
- PR to `erpc/erpc` `main`; Aram reviews; no merge by the agent.
