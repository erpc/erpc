# Chain tracker: plan

Spec: [feature.md](feature.md). One slice per commit; each ships on its own. At each slice's end: one `go build ./...` and the tests that slice extends; no builds between edits. Unit tests of code a slice deletes go with it; Aram confirms each list in review.

```
slice 0  skeleton: wss heads → intake → tracker → indexer → clients
slice 1  HTTP subscription sources
slice 2  linkage, gap fill, reorgs; follower ticker gone
slice 3  logs from receipts; vendor log subscriptions gone
slice 4  integrity on tracker data
slice 5  gRPC StreamBlocks as indexer egress; block_stream hub gone
slice 6  docs, last deletions, one build and test run, PR
```

## Slice 0: skeleton

Boundaries are final here; each component starts simple.

- Add:
  - `ChainTracker` in `architecture/evm/chain_tracker.go`, moved from `chainView`; integrity interfaces unchanged.
  - `indexer.ChainSink`: `Head` only does work; `WantsLogs`, `Logs`, `Unwind`, `Gap` are wired, never called yet.
  - `EvmStatePoller.ObserveHead(raw)`: parse header; gates 1–2 on the same code path as `SuggestLatestBlock`; accepted headers go to `OnHeader`.
  - Tracker header projection (spec §11), applied to every source.
  - `Network` registers `OnHeader` per upstream at bootstrap → `Network.intakeHead`.
  - `intakeHead`: gate 4 moved unchanged from `deliversHeadsFrom` (liveness: wss adapter `HeadsLive`); gate 3 new, `tipCandidateUpstreams("*")` (spec §5.1).
  - Tracker `Head` calls `NoteObservedLatestBlock` before the sink.
  - Metric `erpc_chain_tracker_source_heads_total`; outcomes appear as their gates land.
- Change:
  - `wsupstream.handleNewHeads` calls `ObserveHead` instead of `sink.Ingest`.
  - `enrichStatePoller` passes `eth_getBlockByNumber(latest)` responses to `ObserveHead`.
  - Tracker adopts a header only when it is higher than the head (linkage comes in slice 2).
- Delete:
  - `NetworkHandle.SuggestLatestBlock`, `networkHandle.heads`, `headSource`, `deliversHeadsFrom`.
  - Indexer `headMarker`, `lastHead`, `headMu`, and the `KindNewHead` branch of `Ingest`; `kind="newHeads"` of the dedup counter.
- Tests:
  - `TestWebSocket_Subscriptions`, `TestWebSocket_SubscriptionDedup`, `TestWebSocket_EveryUpstreamHeadReachesItsPoller` stay green.
  - `networks_ws_tip_test.go` gate tests call `intakeHead`; add: an upstream the policy excludes never reaches clients.
  - Deleted with their code: 6 `TestIndexer_NewHead_*` in `indexer/indexer_test.go`.

## Slice 1: HTTP subscription sources

- Add:
  - `upstreams[].evm.subscriptionSource` (`*bool`): defaults, validation, TS config type.
  - Network demand refcount: client subscription, `integrity.follow.enabled`.
  - Poller fast timer in its existing goroutine at `resolveDebounce`, on while demanded.
  - Source liveness for gate 4: wss subscription live, or HTTP poll ok within 2 periods.
  - Idle stop: sources stop 60s after the last consumer leaves.
- Change:
  - `fetchBlock("latest")` returns the header bytes; the poll path calls `ObserveHead`.
  - wss `newHeads` subscription starts and stops with demand.
  - `bootstrapNetwork` accepts any subscription source.
- Rename: `ErrNoWsUpstreamAvailable` → `ErrNoSubscriptionSource`.
- Tests, extend `erpc/ws_server_test.go`:
  - HTTP-only network, `subscriptionSource: true`: client gets polled heads in order, projected as wss heads (no `transactions`).
  - Same network without the flag: `eth_subscribe` fails with `ErrNoSubscriptionSource`.
  - wss + HTTP on one network: each head reaches the client once.

## Slice 2: linkage, gap fill, reorgs

- Add, in the tracker:
  - Spec §6 rules 1–9: parent linkage, gap fill by number, ancestor walk by hash, push confirmation via `ReconfirmPin`.
  - `Unwind` and `Gap` (`GapHeads`) events; the indexer ignores them for heads.
  - `ObserveTarget(served tip)` while no source is live.
  - Config `chainTracker.reorgWindow`, `chainTracker.maxBackfill`.
  - Tracker `Head` feeds `stateProberFor(...).onNewHead`, as `chainFollower.advance` does today.
  - Metric `erpc_chain_tracker_fetch_total` (`kind="header"`).
- Delete:
  - `chainFollower` ticker, `advance`, `step`, `ingest`, `followerDefaultInterval`, `followerDefaultMaxPerTick`; aux kind `chain_follow`.
  - `integrity.reorgWindow`, `integrity.follow.interval`, `integrity.follow.maxBlocksPerTick`, at project, network and profile level.
- Rename: `erpc_integrity_follow_*`, `erpc_integrity_reorg_depth` → `erpc_chain_tracker_*`.
- Tests:
  - Delete `integrity_chainfollower_test.go` (4 tests); its fork, gap and unreconciled scenarios move into the e2e tests below.
  - ws e2e: wss source drops for 3 blocks, HTTP source keeps going; client gets every head in order.
  - ws e2e: a source on a minority fork pushes 2 blocks; the network answers the majority hash; the client gets no `Unwind`.
  - ws e2e: the network answer moves to the new branch; the client gets `Unwind` once, then the new heads.

## Slice 3: logs from receipts

- Add:
  - `architecture/evm/logfilter`: `LogsFilter`, `Matches`, bloom test; moved from `integrity/logfilter.go`.
  - Indexer logs filter registry and matching; `WantsLogs` from the bloom test.
  - Tracker: receipts fetch by hash, answer check (spec §7), logs cursor, empty-root skip, per-network bloom proof, receipts kept in the window, `Gap` (`GapLogs`), `erpc_chain_tracker_fetch_total{kind="receipts"}`.
  - Indexer `Logs`, `Unwind` with `removed:true`; `Gap` closes each connection with a `logs` subscription on the network with `1013`.
- Delete:
  - wsupstream logs subscriptions: logs `EnsureFilter` path, `stripFromBlockZero`, `isZeroBlockRef`.
  - `networks[].evm.stripSubscribeFromBlockZero`.
  - Logs case of `DedupKeyForFilter`, logs dedup windows, `logRemoved`.
  - `kind="logs"` of `erpc_upstream_subscriptions_active` and `erpc_subscription_dedup_dropped_total`.
  - `IngressSelector` (`subIngressSelector`), `reconcile`, `partitionIngresses` keep only pendingTx callers.
- Tests, extend `TestWebSocket_LogsSubscribeOnAllUpstreamsAndDedupe` (rename to match):
  - 2 filters, 3 blocks: one `eth_getBlockReceipts` per block, no upstream `eth_subscribe logs`.
  - Reorg of 1 block: removed logs of the old block, then logs of the new one.
  - Block with a bloom no filter matches: no receipts call.
  - Receipts answer null once, then served: the block's logs arrive once, in order.
  - Receipts keep failing until the block leaves the window: the client connection closes with `1013`.
- Deleted with their code: `TestIsZeroBlockRef`, `TestStripFromBlockZero`, 3 `TestDedupKeyForFilter_Logs*`, 6 `TestIndexer_Log_*`.

## Slice 4: integrity on tracker data

- Add:
  - Header checks in `intakeHead` for every source while integrity is on.
  - An explicit exemption flag for resolver corroboration fetches.
- Change: hooks validate tracker fetches; `IsInternal` no longer skips integrity.
- Tests, extend the slice 3 logs test with integrity on (checks config as in `erpc/integrity_network_test.go` `buildIntegrityNetwork`):
  - One upstream serves receipts with a forged log: rejected by `receiptsRootRecompute`; the client gets the logs from the other upstream.
  - A pushed header with a wrong hash: dropped, `outcome="gated_integrity"`, `erpc_integrity_violation_total` up by 1 for its upstream.

## Slice 5: gRPC StreamBlocks

- Add:
  - `StreamBlocks` egress on the indexer, from `Head` events; a stream counts as source demand (spec §5.2).
  - Reorg: resend from ancestor + 1 (spec §6.1). `GapHeads` ends the stream with `DATA_LOSS`; `GapLogs` leaves it open (spec §6).
  - Resume from `fromBlock` and `fromHash` (spec §6.1): replay, then live, in one ordered sequence.
- Proto, `blockchain-data-standards/manifesto` PR first: `evm/stream.proto` reorg contract comments and the 2 resume fields; bump the module here.
- Delete: `blockStreamManager`, `blockStreamHub`, `fetchBlockHeader`, `blockStreamMaxBackfill`.
- Tests: delete `erpc/block_stream_test.go` (4 hub unit tests). New `TestGrpc_StreamBlocks` in `erpc/networks_query_test.go`, on the `queryGrpc` real gRPC server with a mocked HTTP poll source:
  - A stream gets every block once, in order, with no `eth_getBlockByNumber` per block.
  - A 1-block reorg: the stream gets the new block again at the same number.
  - Reconnect with the last header before a 1-block reorg: replay from `fromBlock − reorgWindow`, ending on the new branch.
  - Reconnect with a header older than `maxBackfill`: `OUT_OF_RANGE`.
  - A head gap above `maxBackfill` ends the stream with `DATA_LOSS`; a logs gap leaves it open.

## Slice 6: docs, deletions, verify, PR

- Docs:
  - `docs/pages/operation/websocket.mdx`: sources, HTTP polling, reorgs, logs from receipts, failure errors.
  - `docs/pages/config/projects/upstreams.mdx`: `subscriptionSource`.
  - `docs/pages/config/projects/networks.mdx`: `chainTracker`, deleted keys.
  - `docs/pages/config/failsafe/integrity.mdx`: follow section.
  - `docs/pages/reference/metrics.mdx`: new and renamed metrics.
- Delete: `indexer.canonicalChainDepth`.
- Specs: `specs/data-integrity/chain-view.md` points to this spec for window, follower and fork choice.
- Verify, once over all slices: `go build ./...`; `go test` for `./common/...`, `./indexer/...`, `./architecture/evm/...`, `./erpc/...`; fix what fails.
- Smoke: local eRPC with an HTTP-only monad source; `wscat` `newHeads` and a Transfer `logs` filter for 5 minutes.
- PR to `erpc/erpc` `main`; Aram reviews; no merge by the agent.
