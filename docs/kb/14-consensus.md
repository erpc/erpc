# KB: Consensus policy (multi-upstream agreement)

> status: complete
> source-dirs: consensus/consensus.go, consensus/executor.go, consensus/analysis.go, consensus/rules.go, consensus/policy.go, consensus/quota.go, consensus/export.go, consensus/export_s3.go, consensus/export_utils.go, consensus/utils.go, consensus/types.go, consensus/specs.md, consensus/executor_test.go, consensus/analysis_test.go, consensus/rules_sendrawtx_test.go, consensus/quota_test.go, consensus/wait_cap_test.go, erpc/network_executor.go, erpc/networks_consensus_test.go, erpc/networks_consensus_quota_test.go, erpc/consensus_dsl_test.go, erpc/consensus_largest_response_test.go, erpc/http_server_consensus_test.go, erpc/skip_consensus_directive_test.go, common/config.go, common/defaults.go, telemetry/metrics.go

## L1 — Capability (CTO view)

Consensus is eRPC's multi-upstream agreement policy: it fans out each request to N upstreams simultaneously, groups identical responses by canonical hash, and selects a winner according to configurable threshold, preference, and dispute rules. When upstreams disagree, operators can choose to accept the majority, return an error, defer to the block-head leader upstream, or apply advanced preferences (prefer non-empty, prefer largest response, prefer highest numeric value). Misbehaving upstreams—those whose response differs from the consensus group—are tracked, optionally penalized by cordoning them for a configurable sit-out period, and their disagreements can be exported to NDJSON files or S3 for offline analysis.

## L2 — Mechanics (staff-engineer view)

**Integration point.** Consensus sits in `networkExecutor.Run` (`erpc/network_executor.go:L183-L188`). When a `ConsensusPolicyConfig` is present on the network and the `SkipConsensus` request directive is false, the executor calls `consensus.Run(ctx, req, slotInner)` where `slotInner` is `retry(hedge(tryOneUpstream))`. Without consensus the executor calls `retry(hedge(runUpstreamSweep))` directly. Composition: `consensus(retry(hedge(tryOneUpstream)))`.

**Participant selection.** `Consensus.Run` binds the request to context, creates an `executor`, and calls `executor.Run`. Before spawning goroutines, if `requiredParticipants` is configured, `reorderForParticipantQuota` front-loads tag-matching upstreams so the first `maxParticipants` drawn satisfy every `{tag, minParticipants}` entry (`consensus/quota.go:L27-L79`). Tag matching supports exact equality and glob wildcards (`*`, `?`) via `upstreamMatchesTag` (`consensus/quota.go:L84-L101`). The executor then spawns exactly `maxParticipants` goroutines (`executor.go:L224-L246`), each calling `slotInner` independently. The participant count defaults to 5 (`common/defaults.go:L2390-L2392`).

**Collection and analysis loop.** The `runAnalyzer` goroutine reads responses from a buffered channel. After each response it calls `newConsensusAnalysis` (classify, hash, group) and `determineWinner` (apply rules). If `shouldShortCircuit` returns true, it sends the outcome to `outcomeCh` immediately and either cancels remaining requests (normal mode) or lets them run in background (fire-and-forget mode). After all `maxParticipants` responses (or a wait-cap deadline fires), it sends the final outcome. The caller's select receives from `outcomeCh` or `ctx.Done()` (`executor.go:L265-L291`).

**Wait caps.** `maxWaitOnResult` (adaptive p50, default 5ms-1s) is armed when the first non-empty response arrives. `maxWaitOnEmpty` (adaptive p90, default 50ms-2s) is armed on the very first response of any kind. When the timer fires the analyzer resolves with whatever responses it has collected, optionally cancelling remaining in-flight requests (`executor.go:L449-L483`). `AdaptiveDuration` resolves to a concrete duration by looking up per-method p-quantile latency from the upstream tracker; returns 0 (no cap) when no data is available.

**Response classification.** Each response is classified into one of four types (`executor.go:L59-L78`): `ResponseTypeNonEmpty` (successful, non-null/non-empty result), `ResponseTypeEmpty` (successful but null/emptyish result), `ResponseTypeConsensusError` (JSON-RPC execution exception, client-side exception, unsupported, or missing-data error), `ResponseTypeInfrastructureError` (all other errors including `ErrUpstreamsExhausted`). Infrastructure errors are excluded from `validParticipants`. Only consensus-valid errors participate in agreement groups (`analysis.go:L333-L344`). `ErrUpstreamsExhausted` is always infra even when it wraps execution exceptions via the shared `ErrorsByUpstream` map—the special check at `analysis.go:L380-L386` guards against misclassification.

**Hashing.** Non-error responses are hashed via `JsonRpcResponse.CanonicalHash()` or `CanonicalHashWithIgnoredFields(fields)` when `ignoreFields` is configured for the method. Errors are hashed via `errorToConsensusHash`: JSON-RPC exceptions hash as `"jsonrpc:<normalized_code>"`; standard errors hash as their code; unknown errors hash as `"error:generic"` (`analysis.go:L347-L360`).

**Default `ignoreFields`.** On first consensus setup, `IgnoreFields` is populated with `eth_getLogs: ["*.blockTimestamp"]`, `eth_getTransactionReceipt: ["blockTimestamp", "logs.*.blockTimestamp"]`, `eth_getBlockReceipts: ["*.blockTimestamp", "*.logs.*.blockTimestamp"]` (`common/defaults.go:L2405-L2419`). These prevent spurious disagreements caused by timestamp fields that may differ between nodes.

**Rule evaluation.** `determineWinner` walks `consensusRules` in priority order (`rules.go:L24-L840`). Rules that match first win. Detailed rule ordering and semantics are in the L3 reference below.

**Short-circuit rules.** `shouldShortCircuit` walks `shortCircuitRules` (`rules.go:L842-L943`). Three rules: (1) `eth_sendRawTransaction`: short-circuit on any single non-empty response; (2) consensus-valid error meets threshold (blocked when `preferNonEmpty` or `preferLargerResponses` active, or when `preferHighestValueFor` is configured for that method); (3) non-empty winner meets threshold with unassailable lead (blocked when `preferLargerResponses` active, when `preferNonEmpty` active and leader is empty, or when `preferHighestValueFor` is configured).

**Leader selection.** Leader is the upstream with the highest EVM block number according to `net.EvmLeaderUpstream(ctx)`. Leader-based behaviors (`OnlyBlockHeadLeader`, `PreferBlockHeadLeader`) use `getLeaderGroupNonError()` (non-error result) or `getLeaderGroupAny()` (any valid result) to find the leader's group. `getLeaderFirstErrorIncludingInfra()` includes infrastructure errors for `OnlyBlockHeadLeader` strict mode.

**Largest-response preference within a group.** Within each response group, the group tracks its `LargestResult` (updated whenever a new member has a larger byte size). When returning the winner, `group.LargestResult` is used, guaranteeing the most complete response is selected even among upstreams that agreed on the same canonical hash (`analysis.go:L102-L117`, `erpc/consensus_largest_response_test.go`).

**Misbehavior tracking and punishment.** After all responses are in, `trackAndPunishMisbehavingUpstreams` identifies groups that differ from the consensus group. Data disagreements (non-empty vs non-empty, empty vs non-empty) where the consensus group is also data are counted as misbehavior. Error disagreements are tracked separately via `MetricConsensusUpstreamErrors` and are NOT treated as misbehavior. For each misbehaving upstream: records `MetricConsensusMisbehaviorDetected`, calls `tracker.RecordUpstreamMisbehavior`, and optionally applies punishment if `shouldPunishUpstream` returns true. Punishment requires a clear majority (consensus group count > 50% of valid participants) and uses a token-bucket rate limiter (`disputeThreshold` tokens per `disputeWindow`); when the limiter denies, the upstream is cordoned for `sitOutPenalty` (`executor.go:L784-L1229`). Cordoning calls `upstream.Cordon("*", "misbehaving in consensus")` and an `AfterFunc` timer calls `upstream.Uncordon("*", "end of consensus penalty")`.

**Misbehavior export.** When `misbehaviorsDestination` is configured, each misbehavior event (any round with at least one misbehaving upstream) is serialized as a JSONL record containing timestamp, project/network/method/finality, full policy snapshot, request body, winner snapshot, analysis snapshot (groups, counts, hashes), and per-participant snapshot (upstream ID, vendor, response type, hash, size, full response body or error string). For file export: records are written immediately to `path/<resolved-filename>` with open file handles cached per resolved name. For S3 export: records are buffered in memory per file name and flushed asynchronously when `maxRecords`, `maxSize`, or `flushInterval` threshold fires (`export.go`, `export_s3.go`).

**Fire-and-forget mode.** When `fireAndForget: true`, the executor uses `context.WithoutCancel(ctx)` as base context so participant goroutines continue even after the HTTP response is sent. On short-circuit the caller receives the result immediately but remaining in-flight requests are NOT cancelled. The canonical use-case is `eth_sendRawTransaction` broadcasting: return the first accepted tx hash to the client but continue broadcasting to all other nodes.

**SkipConsensus directive.** Any request can bypass consensus by setting `X-ERPC-Skip-Consensus: true` (header), `?skip-consensus=true` (query param), or via `directiveDefaults.skipConsensus: true` in the network/project config. When set, the executor falls through to the standard retry+hedge path. Retry, hedge, and timeout still apply. Explicit `false` keeps consensus active. (`erpc/network_executor.go:L174-L188`, `common/request.go:L44,L70,L746-L832`).

**Caller-abandoned handling.** If the HTTP client disconnects before consensus completes, `ctx.Done()` fires in the caller's select. A drain goroutine is spawned to wait for `analyzerDone` (ensuring the analyzer finishes its reads), drain the `outcomeCh`, and release the winner's response buffer. This prevents memory leaks from orphaned responses. Metrics record `caller_abandoned` outcome (`executor.go:L287-L338`).

**Panic safety.** Both `executeParticipant` and `runAnalyzer` have `recover()` defers. A panic in a participant sends `errPanicInConsensus` on `responseChan`. A panic in the analyzer sends `errPanicInConsensus` on `outcomeCh` so the caller's select never deadlocks. `MetricConsensusPanics` is incremented on any recovery (`executor.go:L642-L655`, `executor.go:L381-L397`).

## L3 — Exhaustive reference (agent view)

### Config fields

All under `networks[].failsafe[].consensus.` (or project-level `failsafe[].consensus.`). `Duration` accepts Go duration strings (`"200ms"`, `"2s"`). `*bool` fields: unset → default applied by `SetDefaults`.

| # | YAML path | Type | Default | Behavior |
|---|-----------|------|---------|----------|
| 1 | `maxParticipants` | int | `5` (`common/defaults.go:L2390-L2392`) | Number of upstreams to fan-out to. `maxToSpawn` in the executor; if ≤0 clamped to 1 (`executor.go:L223-L225`). |
| 2 | `agreementThreshold` | int | `2` (`common/defaults.go:L2393-L2395`) | Minimum number of participants that must agree on the same response hash to declare consensus. Used by all rules and by `isLowParticipants` (`validParticipants < threshold`). |
| 3 | `disputeBehavior` | string enum | `"returnError"` (`common/defaults.go:L2396-L2398`) | Action when no valid group meets threshold and `validParticipants >= agreementThreshold`. Values: `"returnError"` (emit `ErrConsensusDispute`), `"acceptMostCommonValidResult"` (pick best valid group by count), `"preferBlockHeadLeader"` (prefer leader non-error group if available, else fall back to acceptMostCommon), `"onlyBlockHeadLeader"` (strictly return leader result or leader error, or dispute). |
| 4 | `lowParticipantsBehavior` | string enum | `"acceptMostCommonValidResult"` (`common/defaults.go:L2399-L2401`) | Action when `validParticipants < agreementThreshold`. "Low participants" means fewer upstreams produced valid (non-infrastructure-error) responses than `agreementThreshold` before the wait caps fired. Same four values as `disputeBehavior`. Under `acceptMostCommonValidResult`: prefer non-empty > empty > error; ties among non-empty groups → dispute. Distinction from `disputeBehavior`: `lowParticipantsBehavior` fires when there are not enough valid participants to reach threshold (`validParticipants < agreementThreshold`, `analysis.go:L267-L268`); `disputeBehavior` fires when there ARE enough valid participants (`validParticipants >= agreementThreshold`) but they disagree (no group meets threshold). |
| 5 | `disputeLogLevel` | string | `"warn"` (`common/defaults.go:L2402-L2404`) | Zerolog level for the misbehavior log event. Values: `"trace"`, `"debug"`, `"info"`, `"warn"`, `"error"`. Logging only fires when `misbehavingCount > 0` AND logger level ≤ this value (`executor.go:L829-L831`). |
| 6 | `ignoreFields` | map[string][]string | Built-in per-method defaults (`common/defaults.go:L2405-L2419`): `{"eth_getLogs": ["*.blockTimestamp"], "eth_getTransactionReceipt": ["blockTimestamp", "logs.*.blockTimestamp"], "eth_getBlockReceipts": ["*.blockTimestamp", "*.logs.*.blockTimestamp"]}` | Map from JSON-RPC method name to dot-path field patterns to exclude from canonical hash. Supports wildcard segments (`*`). **Set replacement — no merge**: if you set ANY value (even for a different method), the ENTIRE default map is replaced; only your entries survive. Setting to `{}` explicitly disables all defaults. (`analysis.go:L442-L453`) |
| 7 | `preferNonEmpty` | *bool | `true` (`common/defaults.go:L2420-L2422`) | Under `acceptMostCommon`: pick non-empty over empty or consensus-error even when those meet threshold. Disables short-circuit when empty leads. Exact semantics per rules 5-8 and 12-13 in the rule table below. |
| 8 | `preferLargerResponses` | *bool | `true` (`common/defaults.go:L2423-L2425`) | Prefer larger response bodies. Under `acceptMostCommon`: pick largest non-empty when no group meets threshold; under `ReturnError`: when smaller group meets threshold but larger exists → dispute. Disables short-circuit entirely when active. |
| 9 | `preferHighestValueFor` | map[string][]string | `nil` (disabled) | Map from method name to ordered list of field paths. Responses are grouped by numeric value (not hash) and the highest value that meets `agreementThreshold` wins. Supports `"result"` (direct scalar) or named object fields (e.g. `"nonce"`). Multiple fields act as ordered tie-breakers. Bypasses all other rules. Disables short-circuit for that method. (`rules.go:L62-L155`, `utils.go:L13-L168`) |
| 10 | `fireAndForget` | bool | `false` (Go zero value; intentionally no `SetDefaults` entry) | When `true`, remaining in-flight participant requests are NOT cancelled after short-circuit. The executor calls `context.WithoutCancel(ctx)` to produce `baseCtx` (`executor.go:L200-L201`), stripping the HTTP-request cancellation signal. Participant goroutines then inherit `cancellableCtx` (derived from `baseCtx`) and therefore survive the HTTP response being sent. They continue until their upstream call completes, a wait-cap timer fires and `cancelRemaining()` is called, or all participants finish normally. There is no process-shutdown "appCtx" wired here — goroutines are bounded only by the upstream call completing, the wait cap, or process exit. In `false` mode (default), `cancelRemaining()` is called in the `defer` block at `executor.go:L215-L218`, cancelling all remaining participants when `executeConsensus` returns. Best practice for `eth_sendRawTransaction` broadcasting. **Design note**: there is intentionally NO `SetDefaults` entry for this field; the Go zero-value `false` is the correct default and requires no action (`common/config.go:L1608-L1612`). |
| 11 | `maxWaitOnResult` | *AdaptiveDuration | adaptive: quantile=0.5, min=5ms, max=1s (`common/defaults.go:L2432-L2438`) | Cap on how long to wait for additional participants AFTER the first non-empty response arrives. Resolved per-request via per-method p50 latency quantile. Value `0` or nil (configured) means no cap. **Cold-start**: when no latency data exists yet, `GetQuantile` returns `0` (`health/quantile.go:L156`); `Resolve` then falls back to `min` (5ms for the default); so on cold start the effective cap is `min`, NOT uncapped. If you configure `{quantile:0.5, max:1s}` without a min, cold start returns 0 (no cap until data accumulates). |
| 12 | `maxWaitOnEmpty` | *AdaptiveDuration | adaptive: quantile=0.9, min=50ms, max=2s (`common/defaults.go:L2439-L2445`) | Cap on how long to wait AFTER the first response of any kind (empty, error, or non-empty). Resolved per-request via per-method p90 latency quantile. Same cold-start behavior as `maxWaitOnResult`: falls back to `min` (50ms for default) when no data available. |
| 13 | `requiredParticipants` | []*ConsensusRequiredParticipant | `[]` (disabled) | List of `{tag, minParticipants}` entries. Each entry ensures at least `minParticipants` upstreams matching `tag` are front-loaded into the participant set. Tag matching uses exact equality then glob. Best-effort: shortfall falls through to `lowParticipantsBehavior`. (`consensus/quota.go:L27-L79`) |
| 14 | `requiredParticipants[].tag` | string | **Required** (no default) | Glob pattern matched against `upstream.tags[]`. Exact match checked first; glob wildcards `*`/`?` applied on mismatch. Validation rejects an empty or missing `tag` with error `"consensus.requiredParticipants[N].tag is required"` (`common/validation.go:L1089-L1090`). |
| 15 | `requiredParticipants[].minParticipants` | int | **No default; Go zero value is `0`** | Minimum number of upstreams matching `tag` that must be included. Validation requires `> 0` (`common/validation.go:L1092-L1093`) and `<= maxParticipants` (`common/validation.go:L1095-L1096`). **Critical**: there is no `SetDefaults` entry for this field. If you declare a `requiredParticipants` entry without explicitly setting `minParticipants`, Go leaves it as `0`. The validator immediately rejects `0` with `"consensus.requiredParticipants[N].minParticipants must be greater than 0"` — this is a **startup validation error**, not a silent no-op. There is no way to get a valid configuration with `minParticipants: 0` or an omitted `minParticipants`. |
| 16 | `punishMisbehavior` | *PunishMisbehaviorConfig | `nil` (disabled) | Enables upstream cordon punishment for persistent misbehavior. |
| 17 | `punishMisbehavior.disputeThreshold` | uint | **Required** >0 (no `SetDefaults` entry) | Number of disputes allowed before punishment fires. Token bucket: `threshold` tokens per `disputeWindow`. Validation requires `> 0` (`common/validation.go:L1172-L1173`); omitting this field when `punishMisbehavior` block is present is a validation error. There is no `SetDefaults` entry — the value MUST be explicitly set by the operator. |
| 18 | `punishMisbehavior.disputeWindow` | Duration | **Required** >0 | Window for the token bucket. Validation requires `> 0` (`common/validation.go:L1175-L1176`). Additionally, at runtime there is an independent guard: if `DisputeWindow.Duration() <= 0` the punishment is silently disabled with a debug log `"punishment disabled: DisputeWindow is zero or negative"` (`executor.go:L1189-L1192`), preventing invalid rate-limiter creation. Both the validation error and the runtime guard protect against invalid windows. |
| 19 | `punishMisbehavior.sitOutPenalty` | Duration | — | Duration to cordon the misbehaving upstream. After expiry, `upstream.Uncordon` is called and the sitout timer entry is removed. |
| 20 | `misbehaviorsDestination` | *MisbehaviorsDestinationConfig | `nil` (disabled) | Export target for misbehavior JSONL records. |
| — | **`misbehaviorsDestination.s3` auto-creation** | — | When `type: "s3"` is set, `SetDefaults` auto-creates `s3` as `&S3FlushConfig{}` if it is nil (`common/defaults.go:L2468-L2471`). You do not need to provide an empty `s3:` block — all s3 sub-fields then receive their individual defaults. |
| 21 | `misbehaviorsDestination.type` | string | `"file"` (`common/defaults.go:L2460-L2462`) | `"file"` or `"s3"`. |
| 22 | `misbehaviorsDestination.path` | string | — | For `file`: absolute directory path (created with `0o750` if missing; must be absolute). For `s3`: S3 URI `s3://bucket-name/optional/prefix/`. |
| 23 | `misbehaviorsDestination.filePattern` | string | `"{timestampMs}-{method}-{networkId}"` (`common/defaults.go:L2464-L2466`) | File name template. Supports: `{dateByHour}` (UTC `2006-01-02-15`), `{dateByDay}` (UTC `2006-01-02`), `{method}`, `{networkId}`, `{instanceId}`, `{timestampMs}`. Instance ID derives from `INSTANCE_ID` → `POD_NAME` → `HOSTNAME` → SHA256 hash of `now+pid` (`export_utils.go:L26-L49`). Always appends `.jsonl` if no recognized extension. |
| 24 | `misbehaviorsDestination.s3.maxRecords` | int | `100` (`common/defaults.go:L2472-L2474`) | Flush when any per-key buffer has ≥ this many records. |
| 25 | `misbehaviorsDestination.s3.maxSize` | int64 | `1048576` (1 MiB) (`common/defaults.go:L2475-L2477`) | Flush when any per-key buffer reaches this byte count. |
| 26 | `misbehaviorsDestination.s3.flushInterval` | Duration | `"60s"` (`common/defaults.go:L2478-L2480`) | Periodic flush interval regardless of threshold. Background worker runs a ticker at this interval. |
| 27 | `misbehaviorsDestination.s3.region` | string | `""` → SDK default chain (env/IMDS) | AWS region for the S3 bucket. |
| 28 | `misbehaviorsDestination.s3.contentType` | string | `"application/jsonl"` (`common/defaults.go:L2481-L2483`) | `Content-Type` header for S3 `PutObject` uploads. |
| 29 | `misbehaviorsDestination.s3.credentials.mode` | string | `""` → SDK default chain | `"secret"` (static access key+secret via `credentials.NewStaticCredentials`), `"file"` (credentials file+profile via `credentials.NewSharedCredentials`), `"env"` (explicit env-var provider via `credentials.NewEnvCredentials`: reads `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`), `""` or any other value (SDK default credential chain: env vars → shared credentials file (`~/.aws/credentials`) → EC2/ECS/EKS IMDS/instance-profile IAM role). The `""` path falls through to the `default:` switch case which sets no explicit provider, leaving the SDK to walk its full chain (`consensus/export_s3.go:L89-L96`). There is no separate IMDS-only mode; IMDS is the last resort in the default chain. |
| 30 | `misbehaviorsDestination.s3.credentials.accessKeyID` | string | — | Used when `mode: "secret"`. **YAML key is `accessKeyID` (capital D)**, matching the Go struct tag `yaml:"accessKeyID"` at `common/config.go:L483`. Using `accessKeyId` (lowercase d) will NOT be recognized by the YAML parser; the field will silently be empty and AWS SDK will fall back to its default credential chain. |
| 31 | `misbehaviorsDestination.s3.credentials.secretAccessKey` | string | — | Used when `mode: "secret"`. |
| 32 | `misbehaviorsDestination.s3.credentials.credentialsFile` | string | — | Used when `mode: "file"`. |
| 33 | `misbehaviorsDestination.s3.credentials.profile` | string | — | Used when `mode: "file"`. |

### Behaviors & algorithms

#### Consensus rule evaluation order (priority 1 = highest)

Rules are evaluated in the order they appear in `consensusRules` slice (`rules.go:L24`). First matching rule wins.

| Priority | Rule description | Triggers when | Action |
|----------|-----------------|---------------|--------|
| 1 | `eth_sendRawTransaction` special | Method is `eth_sendRawTransaction` AND at least one non-empty response exists | Return first non-empty (tx hash). Ignores threshold entirely. |
| 2 | `preferHighestValueFor` | Method has `preferHighestValueFor` configured AND at least one valid group has extractable numeric values | Group responses by numeric value, find highest value with count ≥ threshold, return it. If no value meets threshold → dispute. |
| 3 | `onlyBlockHeadLeader` on dispute | `disputeBehavior == onlyBlockHeadLeader` AND no valid group meets threshold | Return leader non-error result if available; else return leader's error (including infra errors); else dispute. |
| 4 | `onlyBlockHeadLeader` on low participants | `lowParticipantsBehavior == onlyBlockHeadLeader` AND `validParticipants < threshold` | Return leader non-error result if available; else return leader's error (non-infra); else low-participants error. |
| 5 | `preferBlockHeadLeader` on dispute/low | (`disputeBehavior == preferBlockHeadLeader`) OR (`lowParticipantsBehavior == preferBlockHeadLeader` AND low participants) AND no valid group meets threshold AND leader non-error exists | Return leader non-error result. If leader missing → dispute. |
| 6 | `preferLarger + acceptMostCommon` below threshold | `preferLargerResponses == true` AND `acceptMostCommon` active AND best group below threshold | Return largest non-empty by response size (`getBestBySize`). |
| 7 | `acceptMostCommon + preferNonEmpty` above threshold (empty/error leads) | `preferNonEmpty == true` AND `acceptMostCommon` AND best group meets threshold AND best is empty OR consensus-error AND at least one non-empty exists | Return best non-empty (by count, then size). |
| 8 | Tie at/above threshold without preference (non-error groups) | No `preferNonEmpty`, no `preferLargerResponses`; best group meets threshold; multiple valid non-error groups share best count | Dispute. |
| 9 | `acceptMostCommon` below threshold: prefer non-empty over empty | `preferNonEmpty == true` AND `acceptMostCommon` AND below threshold AND exactly 1 non-empty group AND at least 1 empty group | Return the single non-empty group. |
| 10 | `acceptMostCommon` below threshold: prefer non-empty over consensus error | `preferNonEmpty == true` AND `acceptMostCommon` AND below threshold AND leader by count is consensus-error AND non-empty exists | Return best non-empty (by count, then size). |
| 11 | `returnError + preferNonEmpty` (empty threshold winner) | `disputeBehavior == returnError` AND `preferNonEmpty == true` AND best by count is empty AND meets threshold AND non-empty exists | Dispute (do not return the empty winner). |
| 12 | `acceptMostCommon + preferNonEmpty` above threshold (non-empty and error both qualify) | `preferNonEmpty == true` AND `acceptMostCommon` AND best meets threshold AND both non-empty and consensus-error groups ≥ threshold | Return best non-empty (by count, then size). |
| 13 | Tie above threshold without preference (general) | No `preferNonEmpty`, no `preferLargerResponses`; multiple valid groups share best count ≥ threshold | Dispute. |
| 14 | `preferLarger`: above threshold, multiple valid groups | `preferLargerResponses == true` AND best meets threshold AND > 1 valid group ≥ threshold | Return largest non-empty by size. |
| 15 | `preferLarger + acceptMostCommon`: smaller wins threshold but larger exists | `preferLargerResponses == true` AND `acceptMostCommon` AND best meets threshold AND best is non-empty AND a larger non-empty exists | Return largest non-empty by size. |
| 16 | `returnError + preferLarger`: smaller meets threshold but larger exists (below threshold) | `disputeBehavior == returnError` AND `preferLargerResponses == true` AND best meets threshold AND best is non-empty AND larger non-empty exists below threshold | Dispute. |
| 17 | `acceptMostCommon` below threshold with unique leader | `acceptMostCommon` active AND best count < threshold AND best count > second-best count | Return best valid group (non-empty or empty → result; consensus-error → error). |
| 18 | `lowParticipants + acceptMostCommon` | `lowParticipantsBehavior == acceptMostCommonValidResult` AND `validParticipants < threshold` AND valid groups exist | Priority: best non-empty (if not tied) → best empty → best error → low-participants error. If best non-empty is tied → dispute. |
| 19 | Threshold winner (generic) | Any valid group meets threshold | Return result or consensus-error. Highest count wins. |
| 20 | Dispute (multiple groups, none at threshold) | Best count < threshold AND > 1 valid group | Dispute error. |
| 21 | All infrastructure errors meet threshold | `validParticipants == 0` AND infra error group meets threshold | Return agreed infra error. |
| 22 | `lowParticipants + returnError` | `lowParticipantsBehavior == returnError` AND `validParticipants < threshold` | Low-participants error. |
| 23 | No responses | `len(groups) == 0` | Low-participants error. |
| 24 | Fallback | Catch-all | Low-participants error. |

#### Short-circuit rule evaluation order

| Priority | Reason key | When fires | Short-circuit blocked when |
|----------|-----------|-----------|---------------------------|
| 1 | `sendrawtx_first_success` | Method is `eth_sendRawTransaction` AND any non-empty group with count ≥ 1 | — |
| 2 | `consensus_error_threshold` | Best group is `ResponseTypeConsensusError` AND count ≥ threshold | `preferHighestValueFor` configured for method; OR `acceptMostCommon` active AND (`preferNonEmpty` OR `preferLargerResponses`). |
| 3 | `unassailable_lead` | Best group is `ResponseTypeNonEmpty` AND count ≥ threshold AND lead over second ≥ remaining participants | `preferHighestValueFor` configured for method; OR `preferLargerResponses` active; OR `preferNonEmpty` active AND best is empty; OR any of these when `hasRemaining()`. |

#### Response classification detail

- `ErrUpstreamsExhausted` → always `ResponseTypeInfrastructureError`, hash `"error:exhausted"` regardless of wrapped inner errors (`analysis.go:L376-L386`).
- `ErrCodeEndpointExecutionException` (EVM revert, code 3) → `ResponseTypeConsensusError`.
- `ErrCodeEndpointClientSideException`, `ErrCodeEndpointUnsupported`, `ErrCodeEndpointMissingData` → `ResponseTypeConsensusError` (agreed-upon errors).
- All other errors (timeout, network, server 500, etc.) → `ResponseTypeInfrastructureError`.
- Successful response: `IsResultEmptyish(ctx)` → `ResponseTypeEmpty`; else `ResponseTypeNonEmpty`.
- Nil JSON-RPC response on a successful response object → `ResponseTypeInfrastructureError`, hash `"error:generic"`.

#### Misbehavior record JSONL schema

Written to file or S3 as one JSON object per line. Top-level fields:

| Field | Type | Description |
|-------|------|-------------|
| `ts` | int64 | Unix millisecond timestamp |
| `projectId` | string | |
| `networkId` | string | |
| `method` | string | JSON-RPC method |
| `finality` | string | Data finality state string |
| `policy` | object | Snapshot of `maxParticipants`, `agreementThreshold`, `disputeBehavior`, `lowParticipantsBehavior`, `preferNonEmpty`, `preferLargerResponses`, `ignoreFields` |
| `request` | object | `{jsonrpc, id, method, params}` |
| `winner` | object | `{responseType, hash, size, upstreamId}` |
| `analysis` | object | `{totalParticipants, validParticipants, bestByCount, groups[]}` each group has `{hash, count, isTie, responseType, responseSize}` |
| `participants` | array | Per upstream: `{upstreamId, vendor, responseType, responseHash, responseSize, response, error}` |

#### SkipConsensus directive

- Header: `X-ERPC-Skip-Consensus: true` (case-insensitive string comparison for `"true"`)
- Query param: `skip-consensus=true`
- Config: `directiveDefaults.skipConsensus: true` in network or project config
- Effect: bypasses the consensus branch entirely; falls through to `retry(hedge(runUpstreamSweep))`. Retry, hedge, circuit breaker, and timeout still apply.
- Only `"true"` activates bypass; `"false"` or any other value keeps consensus active (`common/request.go:L746-L747`, `erpc/skip_consensus_directive_test.go:L125-L153`).

#### Largest-response-within-group preference

Within each hash group, `LargestResult` tracks the member with the highest `CachedResponseSize`. When returning the winner result, `group.LargestResult` is used (not the first response or any other member). This ensures that among upstreams that agree on the same content hash, the most complete representation is returned (`analysis.go:L102-L117`).

#### `preferHighestValueFor` field extraction

- Field path `"result"` extracts the direct scalar result (hex string or number).
- Any other path extracts `resultObj[fieldName]` from the parsed result object.
- Multiple fields are compared as ordered chains: first field is primary, subsequent fields are tie-breakers.
- Numeric parsing handles: hex strings (`0x...`), decimal strings, `json.Number`, `float64` (precision limited to ~15 significant digits), `int64`, `int` (`utils.go:L64-L119`).
- If any field returns nil, the entire response is excluded from value grouping.
- Responses are regrouped by value (not hash), allowing different formatting of the same numeric value to match.

### Observability

#### Prometheus metrics

| Metric | Type | Labels | When fired |
|--------|------|--------|-----------|
| `erpc_consensus_total` | Counter | `project`, `network`, `category`, `outcome`, `finality` | Every `Run` completion. `outcome` values: `success`, `consensus_on_error`, `dispute`, `low_participants`, `generic_error`, `caller_abandoned`. (`executor.go:L1344-L1346`) |
| `erpc_consensus_duration_seconds` | Histogram | same as total | Duration of each `Run` from start to `recordMetricsAndTracing`. Buckets: global defaults `[0.05, 0.5, 5, 30]`. (`executor.go:L1345-L1347`) |
| `erpc_consensus_errors_total` | Counter | `project`, `network`, `category`, `error`, `finality` | When result is an error. `error` values same as `outcome` error sub-types. (`executor.go:L1360-L1366`) |
| `erpc_consensus_agreement_count` | Histogram | `project`, `network`, `category`, `finality` | When best group count > 0; records highest group count. Buckets: linear 1-10. (`executor.go:L1348-L1352`) |
| `erpc_consensus_responses_collected` | Histogram | `project`, `network`, `category`, `vendors`, `short_circuited`, `finality` | After all responses collected; records count. `vendors` is comma-joined sorted vendor names. Buckets: linear 1-10. (`executor.go:L562-L574`) |
| `erpc_consensus_short_circuit_total` | Counter | `project`, `network`, `category`, `reason`, `finality` | When short-circuit fires. `reason` values: `sendrawtx_first_success`, `consensus_error_threshold`, `unassailable_lead`. (`executor.go:L573-L580`) |
| `erpc_consensus_wait_capped_total` | Counter | `project`, `network`, `category`, `trigger`, `finality` | When `maxWaitOnResult` or `maxWaitOnEmpty` timer fires. `trigger`: `"result"` (had non-empty) or `"empty"` (only empties/errors). (`executor.go:L581-L598`) |
| `erpc_consensus_misbehavior_detected_total` | Counter | `project`, `network`, `upstream`, `category`, `finality`, `response_type`, `larger_than_consensus` | Per misbehaving upstream per round. `response_type` is the misbehaving group's type. `larger_than_consensus` is `"true"/"false"`. (`executor.go:L967-L978`) |
| `erpc_consensus_upstream_punished_total` | Counter | `project`, `network`, `upstream` | When upstream is cordoned. (`executor.go:L1216`) |
| `erpc_consensus_upstream_errors_total` | Counter | `project`, `network`, `upstream`, `category`, `finality`, `response_type`, `error_code` | Per upstream per round when upstream has a consensus or infra error that disagrees with consensus group. (`executor.go:L944-L956`) |
| `erpc_consensus_cancellations_total` | Counter | `project`, `network`, `category`, `phase`, `finality` | `phase` values: `before_execution` (cancelled before `inner` call), `after_execution` (cancelled but result still used), `caller_abandoned` (HTTP client disconnected). (`executor.go:L655-L670`, `executor.go:L327-L332`) |
| `erpc_consensus_panics_total` | Counter | `project`, `network`, `category`, `finality` | Per panic recovery in participant or analyzer. (`executor.go:L648-L651`, `executor.go:L388-L395`) |

#### OTel trace spans

| Span name | Attributes | Notes |
|-----------|-----------|-------|
| `Consensus.Run` | `network.id`, `request.method` | Top-level span for the entire consensus operation. Set on `consensus.outcome`, `consensus.achieved`, `consensus.low_participants`, `consensus.dispute`, `participants.total`, `participants.valid` (`executor.go:L1336-L1342`). |
| `Consensus.CollectResponses` | `short_circuited` (bool), `wait_capped` (bool), `responses.collected` (int) | Child span from first goroutine spawn to all-responses-collected. Owned by `runAnalyzer`; ended in its deferred cleanup (`executor.go:L189`, `executor.go:L549-L553`). |

#### Notable log messages

- `"consensus rule matched"` — debug, fires for each rule match in `determineWinner`.
- `"consensus misbehavior detected - upstreams differ from consensus"` — at `disputeLogLevel` (default warn), fires when `misbehavingCount > 0` and logging is enabled. Includes full participant breakdown with numbered keys (`upstream1`, `responseType1`, `agreesWithConsensus1`, etc.).
- `"fire-and-forget mode: remaining requests complete in background"` — debug, fires on short-circuit when `fireAndForget: true`.
- `"misbehaviour limit exhausted, punishing upstream"` — warn, fires when a upstream's rate limiter denies.
- `"upstream already in sitout, skipping"` — debug, fires when punishment attempted but upstream already cordoned.
- `"consensus caller abandoned; analysis continues in background"` — warn, fires when ctx cancelled before outcome sent.
- `"panic in consensus analyzer"` — error, fires on `recover()` in `runAnalyzer`.
- `"Panic in consensus participant"` — error, fires on `recover()` in `executeParticipant`.
- `"failed to append misbehavior record"` — warn, fires when exporter `AppendWithMetadata` returns error.
- `"failed to initialize S3 misbehavior exporter; export disabled"` — error, fires when S3 setup fails (init-time only).
- `"uploaded misbehavior records to S3"` — info, fires after each successful S3 PutObject.
- `"failed to upload misbehavior records to S3"` — error, fires on S3 PutObject failure.

### Edge cases & gotchas

1. **`ErrUpstreamsExhausted` wrapping foreign consensus errors.** When a consensus slot exhausts its upstreams, the returned error carries a `ErrorsByUpstream` map containing errors from other slots (e.g., execution reverts from participant-A and participant-B). Without a guard, `HasErrorCode` traversal would find those execution reverts and misclassify the exhausted error as a `ResponseTypeConsensusError`, creating phantom voting groups. The explicit `ErrCodeUpstreamsExhausted` check at `analysis.go:L376-L386` prevents this. (`consensus/analysis_test.go:TestErrUpstreamsExhausted_NotMisclassifiedAsConsensusError`)

2. **`eth_sendRawTransaction` bypasses threshold, even when majority errors.** Rule 1 (`rules.go:L29-L59`) fires as soon as any single non-empty response exists, even when 2 of 3 participants returned consensus errors. The first valid tx hash always wins. Short-circuit rule 1 also fires immediately so remaining requests are resolved ASAP (or continue in background under `fireAndForget`). (`consensus/rules_sendrawtx_test.go:TestSendRawTransaction_RulePriority`)

3. **`preferNonEmpty` + `ReturnError` combination.** Under `disputeBehavior: returnError`, if empty meets threshold but a non-empty minority exists AND `preferNonEmpty: true`, the result is a dispute (rule 11), NOT the empty winner and NOT the non-empty minority. The non-empty preference escalates to a dispute under `ReturnError`. (`erpc/networks_consensus_test.go:return_error_two_empty_vs_one_non_empty_prefer_non_empty_enabled_dispute`)

4. **`preferLargerResponses` disables ALL short-circuit.** The `unassailable_lead` short-circuit rule refuses to fire when `preferLargerResponses: true` (`rules.go:L913-L915`). A later response with a larger body could change the winner even if the current leader is above threshold. This increases latency for the common case. Consider disabling `preferLargerResponses` in low-latency scenarios.

5. **Tie among non-empty at/above threshold without preference → dispute.** Rule 8 fires before the generic threshold-winner rule 19. If 2 upstreams agree on value-A and 2 upstreams agree on value-B (both at or above `agreementThreshold`) with no preference enabled, the result is a dispute. Enabling `preferLargerResponses` resolves ties by size.

6. **S3 exporter validates bucket access at startup.** `newS3MisbehaviorExporter` calls `HeadBucket` synchronously. If the bucket is unreachable, S3 export is disabled and an error is logged — consensus continues normally without export (`policy.go:L155-L168`).

7. **File export requires absolute path.** `newFileMisbehaviorExporter` rejects relative paths with an error (`export.go:L37-L41`). The process must have write access to the directory.

8. **`requiredParticipants` is best-effort.** If a required tag group has fewer healthy upstreams than `minParticipants`, the quota is partially satisfied and consensus runs with what's available. The shortfall is handled by the normal `lowParticipantsBehavior` path — there is no special error for quota shortfall. (`consensus/quota.go:L17-L19`)

9. **Tie flag among non-empty groups tracks per-type, per-count frequency.** `responseGroup.IsTie` is true when at least two groups of the same `ResponseType` share the same `Count` (`analysis.go:L121-L138`). Infrastructure error groups always have `IsTie = false`. This flag is used by the `lowParticipants + acceptMostCommon` rule to escalate non-empty ties to dispute.

10. **Race-free analysis struct.** `newConsensusAnalysis` pre-populates all cached accessor fields before returning. After the analyzer goroutine sends the outcome to the caller via `outcomeCh`, both goroutines may read the analysis concurrently (caller reads for metrics; analyzer reads for misbehavior tracking). Lazy-init under concurrent reads would be a data race (`analysis.go:L141-L153`).

11. **Misbehavior punishment only when majority consensus.** `shouldPunishUpstream` requires `consensusGroup.Count > analysis.validParticipants / 2` (strict majority). When only `agreementThreshold` of 3 (e.g., 2) agree but total is 3 (2 > 3/2 = 1), punishment is allowed. But with 2 of 5 agreeing (2 > 5/2 = 2 fails), punishment is blocked. This avoids punishing upstreams when the consensus group itself is a minority of participants (`executor.go:L1196`).

12. **Context cancellation after all executions does NOT produce LowParticipants.** When the parent context is cancelled after every participant has completed but before the analyzer delivers the outcome, the executor's non-blocking try-receive (`executor.go:L271-L279`) picks up the outcome and returns it rather than treating it as abandonment. (`consensus/executor_test.go:TestConsensus_ContextCancelAfterExecution_DoesNotReturnLowParticipants`)

13. **`DisputeLogLevel` zero value defaults to warn.** If `disputeLogLevel` is set to zerolog level 0 (which is `TraceLevel` in zerolog), the builder coerces it to `WarnLevel` because zerolog's zero value is Trace, not the user's intent (`policy.go:L115-L118`). Always set `disputeLogLevel` explicitly to `"trace"` if trace-level logging is desired.

14. **`preferHighestValueFor` can be combined with other methods.** The map allows different handling per method — e.g., `eth_getTransactionCount: ["result"]` uses highest-value for that method while `eth_call` falls through to normal hash-based consensus. The rule only matches when at least one valid group has extractable numeric values; if no values are extractable it falls through to subsequent rules.

15. **S3 content is always overwritten on flush.** The S3 exporter uses `PutObject` (not multipart append) with the buffer content. A flush replaces the object at `prefix/<filename>`. With small `flushInterval` and `{timestampMs}` in `filePattern` each flush creates a new object; with `{dateByHour}` pattern subsequent flushes within the same hour overwrite the previous object.

16. **`ErrConsensusDispute` error contract.** Error code `ErrConsensusDispute` (constant `"ErrConsensusDispute"`, `common/errors.go:L2559`). HTTP method-level status: `409 Conflict` (`ErrorStatusCode() int` returns `http.StatusConflict`, `common/errors.go:L2587-L2588`). Wire HTTP status for POST JSON-RPC: `200` (consensus errors fall through the transport-level switch at `erpc/http_server.go:L1473-L1491` without a match; the error is translated to JSON-RPC via `TranslateToJsonRpcException` and returned as `{"error":{"code":-32603,...}}` — JSON-RPC code `-32603` (server-side exception, `common/json_rpc.go:L1623-L1629`)). The error's `Cause` is `errors.Join(causes...)` where `causes` are the per-participant errors; `Errors()` unwraps them (`common/errors.go:L2574-L2584`). Retryability: `IsRetryableTowardNetwork` uses the multi-error children rule: if ANY child error is retryable, the whole dispute error is retryable (`common/errors.go:L2397-L2411`).

17. **`ErrConsensusLowParticipants` error contract.** Error code `ErrConsensusLowParticipants` (constant `"ErrConsensusLowParticipants"`, `common/errors.go:L2593`). HTTP method-level status: `412 Precondition Failed` (`ErrorStatusCode()` returns `http.StatusPreconditionFailed`, `common/errors.go:L2621-L2622`). Wire HTTP status for POST JSON-RPC: `200` (same reason as `ErrConsensusDispute`; translated to JSON-RPC `-32603`). Same `errors.Join` cause and multi-error children retryability rule as `ErrConsensusDispute`. `DeepestMessage()` emits `"ErrConsensusLowParticipants: <msg>: <participant summary>"` including per-participant info (`common/errors.go:L2625-L2626`).

18. **`ignoreFields` is a complete SET REPLACEMENT, not a merge.** Row 6 in the config table documents this, but the operational consequence is severe enough to warrant a dedicated callout: if you want to ADD a method to the default `ignoreFields` map (e.g., add `eth_getBlockByNumber: ["timestamp"]`), you MUST also include all the default entries (`eth_getLogs`, `eth_getTransactionReceipt`, `eth_getBlockReceipts`) in your YAML. Omitting them silently removes them, causing spurious consensus disputes on `blockTimestamp` fields across those methods. Setting `ignoreFields: {}` explicitly disables ALL timestamp-ignore defaults. There is no additive/merge syntax. (`analysis.go:L442-L453`, `common/defaults.go:L2405-L2419`)

19. **Omitting `minParticipants` in a `requiredParticipants` entry is a startup validation error, not a silent zero.** A user who writes a `requiredParticipants` block without `minParticipants` (e.g., `{tag: "archive"}` only) gets a startup validation failure: `"consensus.requiredParticipants[N].minParticipants must be greater than 0"`. Go leaves unset `int` fields at their zero value `0`, which immediately fails the `> 0` check at `common/validation.go:L1092-L1093`. The error is not deferred to request time. The server will refuse to start.

20. **`requiredParticipants` shortfall is silently best-effort.** If a `{tag, minParticipants}` entry specifies 3 upstreams but only 1 healthy upstream matches the tag, `reorderForParticipantQuota` front-loads that 1 upstream and continues. There is no warning, no error, and no special metric for quota shortfall. The request proceeds with whatever participants are available, and the `lowParticipantsBehavior` path handles the resulting low participant count. Operators cannot distinguish tag-quota shortfall from general upstream unavailability in current observability output. (`consensus/quota.go:L17-L19`, `consensus/quota.go:L27-L79`)

21. **`fireAndForget` goroutines are bounded by upstream calls, not by process shutdown.** In fire-and-forget mode, participant goroutines use `context.WithoutCancel(ctx)` as their base, meaning process-level signals (e.g., SIGTERM) that cancel the application context do NOT propagate to these goroutines unless the upstream's own network call is blocked. In practice, a graceful shutdown that drains in-flight HTTP requests may still have fire-and-forget participants running against upstreams. Operators should account for this in shutdown timeout budgets. (`executor.go:L195-L219`)

22. **`OPENAI_API_KEY` env var is test-only, not runtime.** The `OPENAI_API_KEY` environment variable is read exclusively by `util/dsl.go:L161-L164` in the consensus DSL test-data generation harness (`callOpenAIToGenerateDSL`). It is NOT read anywhere in the production server runtime. If the key is absent and the cached DSL results are stale, DSL-based consensus tests will `t.Fatalf("OPENAI_API_KEY not set and no up-to-date cache")`. Setting this env var in a production container has no effect on consensus or any other runtime behavior.

### Source map

- `consensus/consensus.go` — `Consensus` struct and `NewConsensus`; entry point `Run` that binds request to context and delegates to `executor.Run`.
- `consensus/executor.go` — Core orchestration: `executeConsensus` (fan-out, wait caps, context racing), `runAnalyzer` (collection loop, short-circuit, misbehavior), `executeParticipant` (per-slot goroutine), `trackAndPunishMisbehavingUpstreams`, `createRateLimiter`, `handleMisbehavingUpstream`, metrics and tracing.
- `consensus/analysis.go` — `consensusAnalysis` struct and `newConsensusAnalysis` (classify+hash+group all responses, pre-populate caches), `responseGroup`, all `getX()` cached accessors, `classifyAndHashResponse`, `resultOrErrorToHash`, `isConsensusValidError`, `isAgreedUponError`, `errorToConsensusHash`.
- `consensus/rules.go` — `consensusRules` slice (24 rules, priority-ordered) and `shortCircuitRules` slice (3 rules). All decision logic lives here.
- `consensus/policy.go` — `consensusPolicy` runtime struct, `config` struct, `builder` API, `NewConsensusPolicyBuilder` (test helper), `createMisbehaviorExporter`.
- `consensus/types.go` — `slotResult` (response+error pair for one slot).
- `consensus/quota.go` — `reorderForParticipantQuota` (front-loads tag-matching upstreams), `upstreamMatchesTag` (glob matching).
- `consensus/utils.go` — `extractFieldValues`, `parseNumericValue`, `valuesToKey`, `compareValueChains` for `preferHighestValueFor` logic.
- `consensus/export.go` — `misbehaviorExporter` interface, `fileMisbehaviorExporter`, JSONL record structs (`misbehaviorRecord`, `policySnapshot`, `winnerSnapshot`, `analysisSnapshot`, `groupSnapshot`, `participantSnapshot`).
- `consensus/export_s3.go` — `s3MisbehaviorExporter`: buffered per-key S3 uploads with background flush worker.
- `consensus/export_utils.go` — `resolveFilePattern` / `resolveFilePatternWithDefaults`, `getInstanceID` (cached singleton), `sanitizeForFilename`, `parseS3Path`.
- `consensus/specs.md` — Internal specification document. Verified against code; some implementation details diverge slightly (the code is authoritative).
- `erpc/network_executor.go` — Integration point: `networkExecutor.Run` routes to `consensus.Run(ctx, req, slotInner)` or `retry(hedge(runUpstreamSweep))` based on config and `SkipConsensus` directive.
- `common/config.go:L1591-L1658` — `ConsensusPolicyConfig`, `ConsensusRequiredParticipant`, `MisbehaviorsDestinationConfig`, `S3FlushConfig`, `PunishMisbehaviorConfig`, behavior enum types.
- `common/defaults.go:L2389-L2487` — `ConsensusPolicyConfig.SetDefaults` and `MisbehaviorsDestinationConfig.SetDefaults`.
- `common/request.go:L44,L70,L161,L746-L832` — `SkipConsensus` directive parsing from header and query param.
- `telemetry/metrics.go:L665-L722,L844-L863` — All `erpc_consensus_*` metric definitions with label sets.
