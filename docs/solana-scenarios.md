# Solana Scenario Catalog

Catalog of edge-case and real-world scenarios that the Solana support is
verified against. Each scenario maps to a test in
`erpc/solana_scenarios_test.go`, `erpc/solana_e2e_test.go`, or the unit
suites under `architecture/solana/`.

## Production scenarios covered

### Transaction submission safety
- `sendTransaction` failure must not retry across upstreams (deterministic
  errors agree on every node, retry causes double-broadcast).
  - Simulation failure (`-32002`) — `TestSolana_SimulationError_NotRetried`
  - Generic non-retryable wrapper — `TestSolanaSendTransactionNotRetried`
  - Blockhash expired — `TestSolanaScenarios_BlockhashExpired_NotRetried`
  - Blockhash not found — `TestSolanaScenarios_BlockhashNotFound_NotRetried`
- Burst of 10 concurrent `sendTransaction` calls, each non-retryable
  → exactly 10 upstream hits, no doubles —
  `TestSolanaScenarios_SendTransactionBurst_NoneRetried`

### Upstream health & failover
- HTTP 5xx from primary → failover to secondary —
  `TestSolanaHTTP500_FailsoverToSecondUpstream`
- HTTP 429 rate limit → failover —
  `TestSolanaHTTP429_FailsoverToSecondUpstream`
- `-32000` rate limit text-pattern in JSON body → capacity-exceeded routing —
  `TestSolana_32000_RateLimit_InJsonBody_FailsoverToSecondUpstream`,
  `TestSolanaScenarios_RateLimit_RoutesToFailover`
- `-32601` method not found → unsupported routing —
  `TestSolanaMethodNotFound_FailsoverToSecondUpstream`
- `-32008` no snapshot → missing-data routing —
  `TestSolanaNoSnapshot_FailsoverToSecondUpstream`
- `-32011/-32016` min context slot not reached → failover to caught-up node —
  `TestSolanaMinContextSlot_SucceedsOnSecondUpstream`,
  `TestSolanaScenarios_MinContextSlotNotReached_RoutesToFailover`
- Provider quirk: `-32010 / -32009` "excluded from secondary indexes" → unsupported routing —
  `TestSolanaScenarios_AccountSecondaryIndexes_RoutesToFailover`
- Health poller marks unhealthy on `-32005` —
  `TestSolanaHealthPollerUnhealthy`
- Shred-insert lag (>100 slots ahead of processed) marks node degraded —
  `TestMaxShredInsertSlotLag_AboveThreshold_MarksNodeDegraded` (unit),
  `TestSolanaScenarios_ShredInsertLag_DegradesNode` (e2e)

### Slot tracking
- Highest slot reflects max across upstreams —
  `TestSolanaHighestSlotReflectsMultipleUpstreams`
- Finalized slot tracked via shared state —
  `TestSolanaFinalizedSlotTracked`
- State poller bootstrap and ticking — `TestSolanaStatePoller`
- Concurrent CAS updates — `TestStubSlotVar_ConcurrentUpdates`
- Large rollback callback fires at threshold —
  `TestTrackerIntegration_LargeRollbackCallbackFires`

### Cluster / network identity
- Genesis hash mismatch (devnet upstream behind mainnet-beta config) →
  bootstrap fails fast —
  `TestSolanaGenesisHashMismatch`
- Testnet upstream behind mainnet-beta config → bootstrap fails fast —
  `TestSolanaScenarios_ClusterMismatch_TestnetUpstream`
- HTML auth wall on `getGenesisHash` → bootstrap fails fast (task-fatal) —
  `TestSolanaScenarios_GenesisHash_HtmlAuthWall_FailsBootstrap`

### Cache / finality classification
- Never-cache methods stay realtime —
  `TestGetFinality_NeverCacheMethods`
- Always-finalized methods stay finalized —
  `TestGetFinality_AlwaysFinalizedMethods`
- Commitment-driven finality (processed/confirmed/finalized) —
  `TestGetFinality_Commitment*`
- Case-insensitive commitment parsing —
  `TestGetFinality_CommitmentCaseInsensitive`
- Never-cache wins over always-finalized —
  `TestGetFinality_NeverCacheBeatsAlwaysFinalized`

### Architecture interop
- Solana and EVM networks coexist — `TestSolanaAndEvmInSameProject`
- EVM still works after Solana support added —
  `TestEvmStillWorksAfterSolana`
- Request ID preservation across architectures (string IDs) —
  `TestSolanaRequestID_StringPreserved`

### Concurrency
- Race-stress: 20 goroutines × 25 iterations of mixed forwards —
  `TestSolanaScenarios_RaceStress_ConcurrentForwards`
- Rapid `getHealth` polling (web3.js Connection pattern) —
  `TestSolanaScenarios_RapidGetHealth_DoesNotPanic`

### Error normalization
- Per-vendor `-32000` disambiguation (rate-limit, simulation, generic) —
  `TestJsonRpcErrorExtractor_*`
- HTTP-level error mapping (429/401/403/5xx) —
  `TestJsonRpcErrorExtractor_HTTP*`
- Non-retryable client-side scoping for Solana-specific deterministic codes
  (-32002/-32003/-32013, -32000 sub-cases) —
  `TestSolana_SimulationError_NotRetried`,
  `TestHandleUpstreamPostForward_*`

## Real-world patterns NOT YET covered (future work)

- Cache TTL precision: `confirmed` vs `finalized` write/read cycle.
  Needs cache wired in test setup.
- `getRecentPrioritizationFees` never cached at e2e level (unit covered).
- `sendAndConfirmTransaction` flow (send + repeated `getSignatureStatuses` poll).
- Anchor / `@solana/web3.js` Connection healthcheck pattern (fast loop of
  many getHealth calls).
- Indexer pattern: sequential `getBlock` for every slot in order.
- DEX aggregator pattern: parallel `getProgramAccounts` queries with
  different filters.
- Address Lookup Tables (ALT) and versioned transactions — proxy passthrough
  smoke test.
- Compute unit budget exceeded — `-32002` with specific message.
- `getProgramAccounts` with very large response — streaming behaviour.
- Geographic provider failover (latency-based scoring).
- Reorg / fork choice scenarios (rare on Solana but possible).
