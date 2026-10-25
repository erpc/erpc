# Chain tracker: plan

Spec: [feature.md](feature.md). One step per commit; each ships on its own. Before each step, its real-world scenarios (spec §19, by ID) run red for the reason the step fixes; after it, they and every earlier step's scenarios pass. Build and test once per step, at its end. Unit tests of code a step deletes go with it.

## Done

| Slice | Built |
|---|---|
| 0 | skeleton: wss heads → intake → tracker → chain feed → tap → ws `newHeads` |
| 1 | sources: HTTP poll, reconciler from network init, opt-in `subscriptionSource`, idle stop, expiring switches; prewarm deleted |
| 2 | walk, settle, re-anchor, fill, `Read`, scheduler; follower ticker deleted; `TestChainFeed_ConvergesUnderFaults` |
| 3 | logs from receipts, fallback, bloom check; upstream `logs` subscriptions deleted |
| 4 | mempool feed; indexer ingress, egress and ws buffers deleted |
| 5 | integrity on tracker data, suspension |
| 6 | gRPC `StreamBlocks` as a resumable transport; block stream hub deleted |

## Remaining, in order

```
H0  delete the delivered-head floor
H1  freshness, voters, reports, confirmed head, block time
H2  Head, refusal rule, lanes, finality, per-method tags
H3  fetch schedule
H4  suspension parameters, evidence quorum
H5  tag-state head and gate
H6  signals
HS  shared counters: receive-time freshness, rollback write
T   the rest of the real-world catalogue
7   docs, last deletions, one verification, smoke
```

## H0: delete the delivered-head floor

- Delete: `Network.NoteObservedLatestBlock`, `deliveredLatestBlock`, the network counter `latestBlockShared` (key `network/<project>/<network>/latestBlock`) and its setup, `applyDeliveredHeadFloor` and its call in the served tip, the tracker's call on each pin change. No replacement floor.
- Change:
  - Taps deliver a pin only when its number ≤ the network-wide lane's `Latest` and the vote need is met (spec §6.5, until H1 replaces the need).
  - Catch-up (spec §6.5): while `Latest` holds back a pin and a tap is open, the voters whose head is below it, guarantee subsets included, refresh once per bt, at most 1s apart; every head-counter move wakes the tracker.
  - A `latest` header the tracker fetches (no-source ask, settle answer) moves the answering upstream's counter (spec §6.3).
  - 3 panics of one message kind, with no clean one of that kind between them, reset the tracker: the extra wake-ups must not end the count. Calls into the network's head code recover on their own.
- Tests:
  - Red first: 3 sources at 1005, 1001, 1000, q = 2 → `eth_blockNumber` and `eth_getBlockByNumber("latest")` = 1001; ws `newHeads` and `StreamBlocks` never above 1001; a 2nd source at 1005 moves both to 1005.
  - Red first: 1 wss source + 1 HTTP non-source polled every 30s, and 3 wss sources + an HTTP non-source named in `guaranteedFor` → each pushed head reaches a `newHeads` client within 3s.
  - Real-world: X3, K2, K6 (guarantee clamp).
  - Deleted with their code: `erpc/networks_tip_floor_test.go`, the floor cases of `erpc/networks_ws_tip_test.go`, `erpc/networks_served_tip_guaranteedfor_test.go`.

## H1: freshness, voters, reports, confirmed head

- Add:
  - Number age and hash age against the lag reference, in `health/tracker.go` (spec §6.1); W.
  - Bench threshold `max(30s, 3 × backed-off cadence)`, read through an injected reader of the poller backoff.
  - `voter(u)` and topic admission (spec §6.2); the reconciler and mempool admission read topic admission.
  - Hash votes only from tracker and poller fetches that skip cache reads, and pushes (spec §6.3).
  - Minority-fork demotion: off the pins for longer than W → no number vote, last in routing.
  - Confirmed head = highest pin with `q.Of(hash voters)` hash votes and number ≤ the lane's `Latest`; cold window W; a use-upstream connection gates on its lane (spec §6.5).
  - `PinFresh` = confirmed ∧ evidence quorum ∧ undisputed; `ReconfirmPin` returns it only then.
  - `erpc_upstream_report_age_seconds`.
  - Block time (spec §6.7): the EMA samples accepted source headers; one default, 1s, until 4 samples, the poll cadence included; `fallbackStatePollerDebounce` loses its 5s default (X9 needs it at cold start).
- Change:
  - Tap start and the gRPC stall rule read hash voters; the reconciler timer, suspension window, cold window and every re-arm read W.
  - Group trackers count the voters of their lane.
- Delete: live bound, `live`, `SourceLive`, `newestKnown`, `authoritative(u)`, `authoritativeIn`, `EvmAuthoritativeUpstreams`, `EvmDeliveryQuorum`, `votingSources`, `SendTip`, the `max(2, need)` floor; `erpc_source_live`; outcome `gated_authority` → `gated_voter`; the tracker's pins-mean block time and its 12s default.
- Tests, real-world: X9 (1 wss + 2 https non-sources, 3 instances), U3, U3-lone, N3, N4-lone, N5, U10, U11, U18, K6 (tiered network, no deadlock), C9 (block time at cold start).

## H2: Head, refusal rule, lanes, finality, per-method tags

- Add:
  - `Head` per (network, lane), published atomically; triggers: head change (local or remote), bench change, eligible-set change, 1s timer for every lane. `Leader` within `Latest + maxRetryableBlockDistance`; ceiling with `OldestReportAt` (spec §6.4).
  - One `Head` read per request before the multiplexer and cache (spec §7).
  - Refusal rule in `tryShortCircuitFutureBlock`, `EmptyResultBeyondConfidence`, the `eth_query` range check: band up to the ceiling; the retry waits for the band's Ask, then orders head ≥ N first; reports older than 1 bt Asked before "future".
  - By-hash answers above `Latest` wait up to 1 bt.
  - Per-method interpolation: absent params, EIP-1898 leaves, method config for `eth_callMany` and `eth_createAccessList`, `erigon_blockNumber` = `Latest`; `eth_getBlockByNumber(latest|finalized)` interpolated with a pre-forward refresh.
  - `enforceNonNullTaggedBlocks` reads the original tag; one head predicate with `headLagToleranceBlocks`.
  - Finality split: reads `Head.Finalized`; permanent writes also the serving upstream's finalized; the cache write guard reads the network-wide lane.
  - Lane taps read their lane's `Head`; the 50ms re-read H1 left for them is deleted.
- Delete: per-request served-tip evaluation; post-forward raise, `pickHighestBlock`, `enforceHighestBlock` for `eth_getBlockByNumber`; raw-max consensus leader; `EvmLowestFinalizedBlockNumber`; `pinLatestTag` re-read per attempt; the cache key on the word `latest`.
- Tests, real-world: U12, U14, P5, P6, P7, P9, P10, K7, C12, C13. Extend `erpc/networks_served_tip_invariants_test.go` (assertions unchanged) and the existing interpolation tests.

## H3: fetch schedule

- Add (spec §6.8):
  - One backoff per upstream (cadence doubled per failure, at most 60s); the bench reads it.
  - Ask: single-flight per upstream, outcome to every waiter; it joins a fetch already in flight and waits for its answer; debounced only by a fetch sent within the debounce; number need met by a fresh shared counter, hash need only by a local fetch.
  - Evidence time (spec §6.4 `OldestReportAt`): every report carries when its evidence was taken (an answer's request send time, a push's receive time) through `ObserveHead`, `SuggestLatestBlock` and the intake, with no window where an answer looks fresher.
  - Poll source = Asked once per cadence while demanded and admitted.
  - Settle = Ask with a hash need: voters without a fresh hash vote, then non-voters, up to 3; no-hash-voter Asks.
  - Settle rounds (spec §6.5): keyed on the pinned head and the conflicting branch; a changed key never reports `decided` from votes alone; a client ctx ending with answers pending gets `PinRateLimited`; a deferred re-check does not start the reconfirm cooldown.
  - A panic in an Ask fails its waiters.
- Delete: tracker-owned settle and no-source fetches in the scheduler; class 1 keeps walks only.
- Tests, real-world: U1, U16, U17, C4, C7, D8 (Ask permits), P1 (the other instance within 1 Ask; the reorg settle before any demotion), X2 (slow links, no shared state).

## H4: suspension parameters, evidence quorum

- Add:
  - Evidence quorum predicate (spec §6.10), read by anchors, suspension, the integrity-on `eth_getLogs` fallback, the bloom suspension, `PinFresh`.
  - One suspension mechanism with per-check parameters: headers 2 heights, bloom 8 blocks, re-armed after 8 matches; reads the pins (spec §6.9).
- Delete: `bloomGate`'s own state machine; the five separate "2 vendors" rules.
- Tests, real-world: U8 (integrity on), U9 and its variants (one vendor, then 2 vendors; truncated receipts; wrong bloom), K5.

## H5: tag-state head and gate

- Add (spec §6.11): tag-state head per upstream from the state prober and Multicall3 evidence; skip in `checkEvmUpstreamBlockAvailability` for family-3 tag requests, decided once per request; tolerance W with sample age credited; readmit after 3 fresh samples or a W without stale evidence; all stale → highest tag-state head, counted; `-32603` only past 60s; probe from network start without `follow`, at most every 2s per upstream, re-probe on each skip; one family-3 map for the pin, the shadow pin and the gate.
- Delete: #1303's misbehavior record for stale `latest` state; `latestTagParamIndex` and the prober's own "tag reaches u" predicate.
- Tests, real-world: P11.

## H6: signals

- Add (spec §17): `erpc_chain_tracker_invariant_violations_total` (`latest_regressed` only below a block this instance sent), `erpc_head_refusal_total` with `future_after_refresh`, `erpc_head_ask_total`, `erpc_chain_tracker_confirm_lag_blocks`, `erpc_tap_delivery_seconds`, `erpc_served_tip_voters`, `erpc_served_tip_quorum`, `erpc_upstream_tag_state_stale`, `erpc_upstream_tag_state_all_stale_total`; structured transition logs; spans for tracker fetches, Asks and tap reads.
- Tests, real-world: every scenario asserts invariant violations 0 and `retried_missing` 0; X7, X8.

## HS: shared counters

- Change (spec §14): a higher remote value wins whatever its timestamp (#1243, open on `main`: merge it, never rewrite it); timestamps decide only rollbacks past 1024 blocks; freshness = local receive time; `erpc_shared_counter_remote_rejected_total`.
- Add: rollback write when the pins re-anchor below a voter's counter and its hash vote is on the new branch.
- Tests, real-world: X5, C5, N6.

## T: the rest of the catalogue

- Add every spec §19 scenario not yet run: U2, U4–U7, U13, U15, N1, N2, N4, N5, N7, C1–C3, C6, C8, C10, C11, C14, D1–D7, D9, X1, X4, X6, K1–K4, P2–P4, P8.
- Change: `erpc/chain_feed_test.go` uses `test/chainsim`; it stays the fast CI test.
- Tests: all 75 scenarios pass on `make test-realworld`; the chaos test passes 3 runs under `-race`.

## Slice 7: docs, last deletions, verify, smoke

- Docs, current behaviour only:
  - `docs/pages/operation/websocket.mdx`: sources, voters and the confirmed head, reorgs, logs from receipts, the window, signals and wait bounds per transport, the failure model.
  - `docs/pages/operation/production.mdx`: `subscriptionBufferSize` and the `indexer` block removed.
  - `docs/pages/config/projects/upstreams.mdx`: `subscriptionSource`.
  - `docs/pages/config/projects/networks.mdx`: `chainTracker`, deleted keys, per-method tags and `translateLatestTag`.
  - `docs/pages/config/failsafe/integrity.mdx`: follow tap; suspension; evidence quorum; `matchRequestKind: internal` no longer covers tracker fetches.
  - `docs/pages/config/failsafe/circuit-breaker.mdx`: tracker fetches skip open breakers and never feed them.
  - `docs/pages/reference/metrics.mdx`: spec §17.
  - The gRPC `StreamBlocks` page: resume fields, reorg resend, end codes.
- Delete: every symbol, config key and metric spec §16, §17 and §20 delete; nothing left outside `specs/` and changelogs. Rebuild `typescript/config/lib`.
- Verify once: `go build ./...`; `go vet ./...`; `go test -race` over `./...`; `make test-realworld`.
- Smoke: local eRPC against a public RPC through the fault proxy, ws `newHeads`, ws `logs` and resuming `StreamBlocks` clients for 5 minutes with a 1-minute outage; the oracle holds.
