# Chain tracker: plan

Spec: [feature.md](feature.md). One slice per commit; each ships on its own. At each slice's end: one `go build ./...` and the tests that slice extends; no builds between edits. Unit tests of code a slice deletes go with it; review confirms each list.

```
slice 0  skeleton: wss heads → intake → tracker → chain feed → tap → ws newHeads
slice 1  sources: HTTP poll, reconciler from network init, liveness, opt-in, idle stop, expiring switches; prewarm gone
slice 2  walk, settle, re-anchor, fill, Read, scheduler; follower ticker gone; chaos test
slice 3  logs from receipts, fallback, bloom check; upstream logs subscriptions gone
slice 4  mempool feed; indexer ingress, egress and ws buffers gone
slice 5  integrity on tracker data
slice 6  gRPC StreamBlocks as a resumable transport; block stream hub gone
slice 7  docs, last deletions, one build and test run, PR
```

One test proves convergence across slices: `TestChainFeed_ConvergesUnderFaults` in `erpc/chain_feed_test.go`, added in slice 2 and extended in slices 3, 5 and 6.

- Real eRPC; 4 mocked vendors serve one simulated chain: 2 wss (`mockWsUpstream`) and 2 HTTP (`httptest`).
- A seeded fault schedule drives the vendors; 4 fixed seeds; time constants are package variables, so each seed runs in under 30s.
- Each client applies what it receives: a repeated number replaces that number and above; `removed:true` removes a log.
- Assert per client: its final chain and logs equal the simulated canonical chain and its logs, from its first block on; or it got its transport's end signal, and only for a fault that calls for one.

## Slice 0: skeleton

Boundaries are final here; each component starts simple.

- Add:
  - `indexer`: `Cursor`, `Block`, `Batch`, `ReadOpts`, `ChainFeed` (`Head`, `Read`, `Want`), `ErrOutOfRange`, `Taps` (`NewTaps`, `RunChain`, `Demand`), `Intake`, `Source`, `Topic`.
  - `ChainTracker` in `architecture/evm/chain_tracker.go`, moved from `chainView`, with integrity interfaces unchanged:
    - One goroutine with a mailbox under `recover`: results and ticks never drop; headers keep the newest per source. A message's changes apply in one step; a panic drops the message (`erpc_unexpected_panic_total`, outcome `malformed`); 3 in a row reset the tracker (`erpc_chain_tracker_reset_total`).
    - `Observe` takes every input. `observeBlockView` and `resolveHeaderKind` answers arrive without a vote and stop writing pins.
    - Spec §6 rules 3 and 4, and the root part of rule 1: an empty tracker holds the first vote as a root; a vote whose parent is not held becomes a root. Walks and fill come in slice 2.
    - `Head`; `Read` for a pinned cursor. Any other cursor restarts after the head.
  - Config `chainTracker.reorgWindow` (moved from `integrity.reorgWindow`, at project, network and profile level) and `chainTracker.maxBackfill`.
  - Block time from the pins; the live bound; liveness against the newest known block.
  - Network intake: `Header` → parse (check 0) → poller `ObserveHead` (identity, counter, on the `SuggestLatestBlock` code path) → accepted → liveness → `authoritative(u)`, without the fallback hysteresis → trackers.
  - Network-wide tracker pin change: `NoteObservedLatestBlock`, then taps wake.
  - ws `newHeads`: one tap per subscription; projection from `architecture/evm`, once per block; more than `reorgWindow` behind, with nothing Reverted, restarts after the head.
  - Metrics `erpc_chain_tracker_source_heads_total` (`voted`, `duplicate`, `malformed`, `gated_identity`, `gated_authority`), `erpc_source_live`, `erpc_chain_tracker_reanchor_total`, `erpc_chain_tracker_reset_total`. `erpc_network_subscription_last_head_timestamp_seconds` is set on each head change of the network-wide tracker.
- Change:
  - `wsupstream.handleNewHeads` calls `Intake.Header`.
  - `enrichStatePoller` keeps moving the head counter with `eth_getBlockByNumber(latest)` answers, and passes them to the trackers without a vote.
- Delete:
  - `NetworkHandle.SuggestLatestBlock`, `networkHandle.heads`, `headSource`, `deliversHeadsFrom`, wsupstream `HeadsLive`; `erpc_subscription_withheld_heads_total`.
  - Indexer `headMarker`, `lastHead`, `headMu`, the `KindNewHead` branch of `Ingest`, `NetworkHandle.HeadIndexed`; `kind="newHeads"` of the dedup counter and of `erpc_subscription_undelivered_total`.
- Tests:
  - `TestWebSocket_Subscriptions`, `TestWebSocket_SubscriptionDedup`, `TestWebSocket_EveryUpstreamHeadReachesItsPoller`, `TestWebSocket_LastHeadTimestampCarriesProjectAndNetwork`, `TestWebSocket_InstanceSubscriptionCap` stay green.
  - `networks_ws_tip_test.go`: the 4 `TestNetworkHandle_SuggestLatestBlock_*` and `TestNetworkHandle_FallbackHeadsHeldWhilePrimariesStream` call the network intake; add: an upstream the policy excludes never reaches clients.
  - Deleted with their code: 6 `TestIndexer_NewHead_*`; the head half of `TestIndexer_ReportsIndexedHeadsAndUndeliveredEvents`; the `HeadsLive` assertion of `TestAdapterGivesUpNewHeadsWhenUnsupported`.

## Slice 1: sources

- Add:
  - `upstreams[].evm.subscriptionSource` (`*bool`): defaults, validation, TS config type.
  - The 60s fallback hysteresis in `authoritative(u)` (spec §5.2).
  - Source reconciler per network, on one goroutine under `recover`, started at network init whether or not a WebSocket or gRPC server runs. Wakes: the `NewTaps` callback, liveness changes, `Intake.Subscribed`, a timer at the live bound. wss `newHeads` always; poll `newHeads` while demanded and authoritative; idle stop after 60s.
  - wss source: `Set` keeps the `newHeads` subscription and `watchHeads`, as today, and reports each subscribe answer through `Intake.Subscribed`. A `newHeads` refused as unsupported is asked again on each reconnect and every 10 min; meanwhile the reconciler polls that upstream under the poll rule.
  - Poll source: `EvmStatePoller` polls at block time × 0.7, at most 12s, while its topics include `newHeads`; `statePollerDebounce` overrides. After errors it backs off up to 60s and never stops; `skipLatestBlockCheck` does not apply to it.
  - Expiries: the chain-identity cordon is checked again every 5 min and lifted on a match (`time.AfterFunc` + `Uncordon`, as `consensus/executor.go:1468`); `skipLatestBlockCheck` expires after 10 min.
  - `Head` while no source is live: waits for the next pin change.
- Change:
  - `fetchBlock("latest")` returns the header bytes; the poll path calls `Intake.Header`.
- Delete:
  - `PrewarmUpstreamSubscriptions`, `prewarmInterval` and their start in `erpc/init.go`.
  - `bootstrapNetwork`, `SubscriptionManager.networks`, `bootstrapMu`; until slice 4 the reconciler registers each network's wss adapters as indexer ingresses.
- Rename: `ErrNoWsUpstreamAvailable` → `ErrNoSubscriptionSource`.
- Tests, extend `erpc/ws_server_test.go`:
  - HTTP-only network, `subscriptionSource: true`: the client gets polled heads in order, projected (no `transactions`).
  - Same network without the flag: `eth_subscribe` fails with `ErrNoSubscriptionSource`.
  - wss + HTTP on one network: each head reaches the client once.
  - Failover on, primary wss + fallback HTTP: the fallback polls only while no primary has been live for 60s; client heads stay contiguous.
  - The last client leaves: polls stop after the idle delay; a new client then gets only heads after it subscribed.
  - WebSocket server off, 2 projects define one chain: each project's network subscribes `newHeads` on its wss upstream at init, and HTTP `eth_blockNumber` follows the pushes.
  - A wss upstream refuses `newHeads` as unsupported: the client gets its heads by poll; after the re-ask interval, a package variable, it is asked again once.
  - Deleted with their code: `erpc/http_server_prewarm_test.go` (3 tests); the first scenario above covers 2 projects on one chain and inverts `_SkippedWhenWebSocketDisabled`.

## Slice 2: walk, settle, re-anchor, fill, Read, scheduler

- Add, in the tracker and chain feed:
  - Spec §6 rule 1 walks (at most `reorgWindow` blocks in class 1), rule 2 fill for every root, rule 5 settles by `latest` from up to 3 upstreams that do not vote, rule 6 no-source asks to the first authoritative upstream in selection order, every block time, at most every 12s.
  - `ReconfirmPin` over votes; finalized anchors only below the window; a majority branch below the finalized height is followed and logged.
  - Window (spec §8): header timestamps, floor of `reorgWindow` blocks, 4 orphans per height; fill joins trees.
  - `Read` for every cursor case (spec §9.1); batches of at most `reorgWindow` blocks.
  - Scheduler (spec §7): 16 slots, 3 classes of at least 1 slot each; halving when a whole sweep ends in capacity errors; one sweep without hedge or network retry; failsafe config, an `internal` consensus policy included, does not apply; upstreams with an open breaker skipped, and no breaker fed; timeout 10s; no client rate limits; upstream permits as clients take them for classes 1 and 2, only when free for class 3; cache reads only by hash; backoff; single-flight keyed by (selector, upstream, method, params), without the selector when one upstream is named; a queue of one request per key; the tracker waits on every fetch it submits, and a Read's own queued fetch drops when its last Read leaves.
  - `IsInternal` split: request kind `internal` for failsafe matching; the retry, hedge and breaker bypass for subscription calls, state-poller and chainId probes only.
  - Group trackers: `reorgWindow` blocks; their upstreams' headers vote before the authority gate; selector-scoped answers go only to them.
  - Integrity follow tap per tracker: demand while `integrity.follow.enabled`; feeds `stateProberFor(...).onNewHead`; restarts after the head on out of range.
  - Metrics `erpc_chain_tracker_settle_total`, `_fetch_total{kind="header"}`, `_fetch_slots`, `erpc_taps`, `erpc_tap_out_of_range_total`. `erpc_websocket_subscription_notifications_dropped_total` keeps counting the buffers that remain until slice 4 deletes them.
- Delete:
  - `chainFollower` ticker, `advance`, `step`, `ingest`, its retry of an unreconciled block, `followerDefaultInterval`, `followerDefaultMaxPerTick`; aux kind `chain_follow`.
  - `integrity.follow.interval`, `integrity.follow.maxBlocksPerTick`, at project, network and profile level; `erpc_integrity_follow_stall_total`.
- Rename: `erpc_integrity_follow_head`, `_lag`, `erpc_integrity_reorg_depth` → `erpc_chain_tracker_*`.
- Tests:
  - New `TestChainFeed_ConvergesUnderFaults` with ws `newHeads` clients. Faults:
    - reorgs of 1–3 blocks; one vendor on a minority fork for 20 blocks; one vendor 20 blocks ahead on its own fork; a reorg deeper than the window;
    - a vendor silent with its socket open; a primary 40 blocks behind that keeps advancing; a vendor down for 50 blocks; every vendor down for 2 minutes; no blocks for 3 minutes;
    - 429 bursts on every vendor; answers 2 block times late; client load that uses every upstream's whole rate budget for 30s;
    - a 1-vs-1 split settled by the third vendor; 2 of 4 votes on a branch that does not link; a restart where a minority header arrives first;
    - a client answer for an orphan cached by number; a burst that fills the mailbox; a header that makes the tracker panic, once and 3 times in a row.
  - Also assert: no client waits more than 3 live bounds for a head while a vendor serves; the minority fork never reaches a client; a single panic leaves the pins unchanged; the quiet chain reconnects nothing; tracker fetches alone open no breaker.
  - Deleted with their code: `integrity_chainfollower_test.go` (4 tests); the chaos schedule covers their fork, gap and unreconciled scenarios.
  - `TestUpstreamExecutor_InternalRequestsSkipBreaker` stays green: subscription calls keep the breaker bypass.

## Slice 3: logs from receipts

- Add:
  - `architecture/evm/logfilter`: `LogsFilter`, `Matches`, bloom test; moved from `integrity/logfilter.go`.
  - Chain feed: `ChainFeed.LogsHead`; the `Want` registry; receipts fetched at pin time (class 2) and held within `reorgWindow` of the head; answer check; bloom check on every answer, counted per upstream, refetched from another vendor, off for the network when at least 2 vendors mismatch on 2 blocks in a row and none matches, or after 8 in a row on a one-vendor network (the mismatching blocks fetched again), on again after 8 matching blocks in a row; skips after 8 matching blocks with transactions in a row, only on headers whose hash recomputes; chain-profile switch for the check and both skips; receipts index by address and topic0; matched logs as positions; `_fetch_total{kind="receipts"|"logs"}`.
  - Fallback with integrity off: `eth_getLogs` by `blockHash` after 5s of failed receipts on every upstream.
  - ws `logs`: one tap per subscription; matches once per block and filter; `removed:true` from the same receipts for Reverted, fetched by hash when not held; out of range past `reorgWindow` blocks behind the logs head, or after 60s on one block's receipts; closes with `1013` spread over 60s.
  - ws `logs` subscribe parses its filter with `LogsFilter`; one that does not parse fails with `-32602` before any fetch.
  - `logs` subscribe fails with `ErrNoSubscriptionSource` when no upstream serves a usable method, and while every fetch of each usable method has failed for 60s; during that refusal the head block's receipts are fetched every block time, at most every 12s, until one succeeds.
  - `IgnoreMethod` entries expire after 10 min.
- Delete:
  - wsupstream logs subscriptions: the logs `EnsureFilter` path, `stripFromBlockZero`, `isZeroBlockRef`; `networks[].evm.stripSubscribeFromBlockZero`.
  - Logs case of `DedupKeyForFilter`, logs dedup windows, `logRemoved`; `kind="logs"` of `erpc_upstream_subscriptions_active` and `erpc_subscription_dedup_dropped_total`; logs windows in `erpc_subscription_dedup_entries`.
- Tests:
  - Rewrite `TestWebSocket_LogsSubscribeOnAllUpstreamsAndDedupe` as the no-fault baseline: 2 filters, 12 blocks with transactions, one `eth_getBlockReceipts` per block, no upstream `eth_subscribe logs`; after the 8th, a block no filter may match makes no receipts call.
  - Same test: no upstream serves either method: `eth_subscribe logs` fails with `ErrNoSubscriptionSource`.
  - Same test: a filter with a malformed `address` fails with `-32602`, with no upstream call.
  - wsupstream tests that subscribe a `logs` filter (`TestAdapterEnsureFilterKeepsStructuredError`, `TestAdapterGivesUpNewHeadsWhenUnsupported`) use a `newPendingTransactions` filter.
  - Extend `TestChainFeed_ConvergesUnderFaults` with ws `logs` clients on 2 filters. Faults: null receipts; receipts that drop the last 10 receipts, from 2 upstreams of one vendor; 429s; receipts 2 block times late; one vendor without `eth_getBlockReceipts`; receipts failing on every vendor for 13s; one block failing on every vendor for 60s; every receipts and logs fetch failing for 90s.
  - Assert logs as above. Only the 60s block and the 90s outage close clients with `1013`; after 60s of the outage, a new `logs` subscribe fails with `ErrNoSubscriptionSource`.
  - Deleted with their code: `TestIsZeroBlockRef`, `TestStripFromBlockZero`, 3 `TestDedupKeyForFilter_Logs*`, 6 `TestIndexer_Log_*`.

## Slice 4: mempool feed

- Add:
  - `indexer/mempool`: `MempoolFeed` (`Tail`, `Read`); one ring per (network, variant) of 10,000 entries; dedup over 60s, at most 100,000 hashes per network; wakes at most every 50 ms; `Taps.RunMempool`.
  - At most 16 variants per network; the 17th subscribe fails with `ErrSubscriptionLimitExceeded`.
  - Reconciler topic `newPendingTransactions:<variant>` for push sources; `Intake.PendingTx` with `authoritative(u)`; a silent pending-transaction topic is subscribed again after 60s, counted in `erpc_source_resubscribe_total`.
  - A subscribe that opens a new variant waits up to 10s for its sources' first `Intake.Subscribed` answers. Every authoritative push source refusing it with an error other than a capacity error fails the subscribe with the first refusal; capacity errors, connection errors and timeouts open it.
  - ws `newPendingTransactions`: one tap per subscription; out of range skips to the newest; a network with no push source refuses it with `ErrNoSubscriptionSource`.
  - `erpc_subscription_dedup_entries` counts the mempool; its help text says so.
- Delete:
  - Indexer `Sink`, `StreamEvent`, `IndexedEvent`, `EventIngress`, `EventEgress`, `IngressSelector`, `NetworkHandle`; `EnsureFilter`, `ReleaseFilter`, `subscribe`, `reconcile`, `partitionIngresses`, `Ingest`, `dedupe`, `fanOut`, `erpc_subscription_undelivered_total`; `nullingress`; the `indexer` config block.
  - `subIngressSelector`, `tierWsIngresses`; wsupstream `EnsureFilter`, `RemoveFilter`, `FilterLive`.
  - wsclient `enqueue`, `isLossy`, `runWriter` and per-subscription buffers; `WsConnection.NotificationDropped`; `server.webSocket.subscriptionBufferSize`; `erpc_websocket_subscription_notifications_dropped_total`, `erpc_websocket_send_queue_length`.
  - `erpc_websocket_subscriptions_active`: every ws subscription is a tap now, counted in `erpc_taps`.
- Tests:
  - Extend `TestWebSocket_SubscriptionDedup`: 2 wss sources push the same pending transaction; the client gets it once. Both sources refuse a new variant with `-32602`: the client gets `-32602`. Both answer `-32005`: the subscribe opens, and a retry delivers.
  - Extend the slice 1 failover test: the fallback subscribes `newPendingTransactions` only while it is authoritative.
  - wsupstream tests call `Set` instead of `Start`, `EnsureFilter` and `RemoveFilter`; same scenarios. `TestAdapterEnsureFilterKeepsStructuredError` goes: the dedup test covers its refusal end to end.
  - Deleted with their code: 16 `TestIndexer_*` (filter, selector, reconcile, undelivered), `TestIntegration_NullIngress_EndToEnd`, `TestTierWsIngresses`, 4 wsclient buffer tests in `indexer/adapters/wsclient/adapter_test.go` (`TestAdapter_BurstWithinBufferIsDelivered`, `TestAdapter_NewHeadsOverflowDropsOldest`, `TestAdapter_LogsOverflowIsReportedNotEvicted`, `TestAdapter_RemovedSubOverflowIsNotReported`).

## Slice 5: integrity on tracker data

- Add:
  - Header checks in the network intake for every source while integrity is on, before `ObserveHead`, so a forged header never moves the head counter; outcome `gated_integrity`.
  - Consecutive checks find the parent by parentHash (`parentInSegment`); a parent that is not held skips.
  - Unanimous failure by height: no header at 2 heights in a row passes within the live bound → the check is suspended; after 100 blocks it runs observe-only and enforces again after 2 passing heights; `erpc_integrity_check_suspended_total`.
- Change: hooks validate tracker fetches; a check on a tracker fetch reads only held data; `IsInternal` no longer skips integrity.
- Tests:
  - Run `TestChainFeed_ConvergesUnderFaults` with integrity on (checks config as in `erpc/integrity_network_test.go` `buildIntegrityNetwork`). Added faults: one vendor serves receipts with a forged log; one vendor pushes headers with a wrong hash, first at 2 heights in a row; a 3-block reorg. Assert as above; the forged data never reaches a client; no check is suspended; `erpc_integrity_violation_total` rises only for those 2 vendors.
  - Same test, 2 runs: every vendor, then a network with one vendor, serves headers whose base fee breaks `baseFeeDerivation`. The check is suspended, heads keep flowing, and 100 blocks later it stays suspended without dropping a header.
  - A client `eth_getLogs` for a block a logs tap fetched: `getLogsCompleteness` passes with no `canonical_receipts` aux fetch.

## Slice 6: gRPC StreamBlocks

- Proto, `blockchain-data-standards/manifesto` PR first: `evm/stream.proto` reorg contract comments and the 2 resume fields; bump the module here.
- Add: `StreamBlocks` transport, one tap per stream: resume (spec §9.3); resend from the fork point; out of range, including a cursor the pod cannot place, ends the stream with `OUT_OF_RANGE`; any Read wait of 60s, or one `Send` of 10s, ends it with `UNAVAILABLE`; auth and rate limits once at open.
- Delete: `blockStreamManager`, `blockStreamHub`, `fetchBlockHeader`, `blockStreamMaxBackfill`, `ProcessBlockStream`.
- Tests: delete `erpc/block_stream_test.go` (4 hub unit tests). Extend `TestChainFeed_ConvergesUnderFaults` with 2 eRPC instances and gRPC streams on the `queryGrpc` server:
  - Streams disconnect at random and resume on the other instance; assert headers as above, with no `eth_getBlockByNumber` per block.
  - A resume with a header older than the window, or an orphan no vendor serves: `OUT_OF_RANGE`.
  - A resume above a fresh instance's head waits, then continues.

## Slice 7: docs, deletions, verify, PR

- Docs:
  - `docs/pages/operation/websocket.mdx`: sources, polling, votes, reorgs, logs from receipts, the window, signals and wait bounds per transport, the failure model; `newHeads` from network init, with or without the WebSocket server; unsupported `newHeads` polled; `subscriptionBufferSize` removed.
  - `docs/pages/operation/production.mdx`: `subscriptionBufferSize` and the `indexer` block removed.
  - `docs/pages/config/projects/upstreams.mdx`: `subscriptionSource`.
  - `docs/pages/config/projects/networks.mdx`: `chainTracker`, deleted keys.
  - `docs/pages/config/failsafe/integrity.mdx`: follow section; unanimous-failure suspension; `matchRequestKind: internal` no longer covers tracker fetches.
  - `docs/pages/config/failsafe/circuit-breaker.mdx`: tracker fetches skip open breakers and never feed them.
  - `docs/pages/reference/metrics.mdx`: new, renamed and deleted metrics.
- Specs: `specs/data-integrity/chain-view.md` points to this spec for window, follower and fork choice.
- Verify, once over all slices: `go build ./...`; `go test` for `./common/...`, `./indexer/...`, `./architecture/evm/...`, `./erpc/...`; fix what fails.
- Smoke: local eRPC with an HTTP-only monad source for 5 minutes: `wscat` `newHeads` and a Transfer `logs` filter; `grpcurl` `StreamBlocks`, reconnecting with a resume cursor; kill the source for 1 minute, then check that both clients continue gap-free.
- PR to `erpc/erpc` `main`.
