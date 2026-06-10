# KB: Shadow upstreams

> status: complete
> source-dirs: erpc/shadow.go, erpc/projects.go, upstream/registry.go, upstream/upstream.go, common/config.go (ShadowUpstreamConfig), common/errors.go, health/tracker.go, telemetry/metrics.go, docs/kb/_census/metrics.md

## L1 — Capability (CTO view)

Shadow upstreams mirror live production traffic to a secondary (shadow) upstream without affecting response latency or reliability — the shadow request fires asynchronously after the primary response is already committed to the client. eRPC compares the shadow response to the primary response by content hash, logs any divergence as an error, and increments Prometheus counters for identical/mismatch/error outcomes. This enables dark-launch validation of new RPC providers, chain configurations, or endpoint versions.

## L2 — Mechanics (staff-engineer view)

**Registration.** An upstream becomes a shadow upstream by setting `shadow.enabled: true` in its config. The upstream registry (`upstream/registry.go:L574-L584`) routes it into `networkShadowUpstreams[networkId]` instead of `networkUpstreams[networkId]`. Shadow upstreams are never offered to the selection policy, never appear in `GetNetworkUpstreams`, and are never routed to by the normal forwarding path.

**Trigger point.** After `p.doForward` returns in `PreparedProject.Forward` (`erpc/projects.go:L135-L163`), if there are shadow upstreams and the primary response is non-nil, the response is cloned (JSON-RPC response deep-copy + a new `NormalizedResponse` wrapper), and `executeShadowRequests` is launched as a goroutine. The primary response is already committed to the caller; the shadow goroutine runs entirely in the background.

**Goroutine safety.** The response clone is done before the goroutine launch. The shadow goroutine receives the cloned response, not the live one. The original request body is similarly byte-copied into a new `NormalizedRequest` so that mutations in the shadow goroutine cannot race with the original.

**Per-upstream sample rate.** Before each shadow request, if `shadow.sampleRate < 1.0`, a uniform-random draw from `[0.0, 1.0)` is compared to `sampleRate`. If `rand.Float64() >= sampleRate`, the shadow request is skipped for that upstream (debug log emitted). Default `sampleRate` is unset (nil), treated as `1.0` (always fire).

**Method filtering.** `ups.ShouldHandleMethod(method)` is called first. If the shadow upstream's allow/ignore method lists would reject this method, the request is skipped with a debug log. This prevents shadow noise for methods the shadow upstream does not implement.

**Forwarding with bypass.** `ups.Forward(ctx, shadowReq, true, false)` sets `byPassMethodExclusion=true` (`upstream/upstream.go:L410`). This allows the shadow call to bypass eRPC's internal `shouldSkip` check (which would otherwise check the shadow flag again in the selection engine). Method exclusion from the upstream config is still enforced by the pre-call `ShouldHandleMethod` check in the shadow layer itself (`erpc/shadow.go:L54-L61`).

**Comparison algorithm:**
1. Compute `originalSize` = `resp.Size(ctx)` (byte count of the serialized response).
2. Compute `shadowSize` = `shadowResp.Size(shadowCtx)`.
3. If `ignoreFields` is configured for this method, compute both hashes via `HashWithIgnoredFields`; otherwise use `Hash` on both.
4. Check `IsResultEmptyish` on both responses.
5. If `shadowHash == expectedHash` OR (both emptyish) → increment `MetricShadowResponseIdenticalTotal`, log at Trace.
6. Otherwise → increment `MetricShadowResponseMismatchTotal` (with `finality`, `emptyish`, `larger` labels), log at Error with full request/response objects.

**Ignore fields.** `shadow.ignoreFields` is a `map[string][]string` keyed by method name. For `eth_getBlockByNumber` you might ignore a `"timestamp"` field that differs between RPC providers due to clock skew. The hash is recalculated for both the original and shadow response with those JSON keys masked before comparison — ensuring the diff is meaningful.

**Context lifecycle.** The shadow goroutine creates its own `context.WithCancel` from `p.networksRegistry.appCtx` (NOT from the inbound request context, which may be cancelled by the time the goroutine runs), so shadow requests are not aborted when the client disconnects.

**Panic recovery.** A `recover()` deferred in `executeShadowRequests` catches any panic, logs it as an error, and increments `MetricUnexpectedPanicTotal` with label `shadow-upstreams`.

## L3 — Exhaustive reference (agent view)

### Config fields

All under `projects[*].upstreams[*].shadow` in YAML (within an `UpstreamConfig` that also has `shadow.enabled: true`).

| # | YAML path | Type | Default | Behavior / notes | Source |
|---|-----------|------|---------|------------------|--------|
| 1 | `upstreams[*].shadow.enabled` | `bool` | `false` | When `true`, this upstream is registered as a shadow upstream and excluded from normal routing | `common/config.go:L983`, `upstream/registry.go:L574` |
| 2 | `upstreams[*].shadow.sampleRate` | `*float64` | `nil` → treated as `1.0` (always fire) | Fraction of eligible shadow requests to fire; `0.5` = 50%; applied per-upstream per-request via `rand.Float64() >= sampleRate` check | `common/config.go:L984`, `erpc/shadow.go:L65-L75` |
| 3 | `upstreams[*].shadow.ignoreFields` | `map[string][]string` | `nil` (no fields ignored) | Keys are JSON-RPC method names; values are JSON field names to mask from both the original and shadow responses before hash comparison; enables fair comparison when providers legitimately differ on volatile fields | `common/config.go:L985`, `erpc/shadow.go:L167-L191` |

**Total config fields documented: 3**

### Behaviors & algorithms

**Registry registration (shadow vs normal split)** (`upstream/registry.go:L574-L603`):
- `isShadow = ups.Config().Shadow != nil && ups.Config().Shadow.Enabled`
- Shadow: appended to `networkShadowUpstreams[networkId]` (deduped by id).
- Normal: appended to `networkUpstreams[networkId]`; atomic snapshot refreshed.
- A shadow upstream is NEVER in `networkUpstreams`; the registry enforces the split.

**`Network.ShadowUpstreams()` accessor** (`erpc/networks.go:L302-L303`): delegates to `upstreamsRegistry.GetNetworkShadowUpstreams(n.networkId)`.

**Trigger in `PreparedProject.Forward`** (`erpc/projects.go:L137-L163`):
1. `shadowUpstreams := network.ShadowUpstreams()`.
2. If empty or `resp == nil`, skip entirely.
3. Parse primary response JSON-RPC object; if parsing fails, log and skip.
4. Clone: `jrr.Clone()` + wrap in a new `NormalizedResponse`; copy upstream, fromCache, attempts, retries, hedges, evmBlockRef, evmBlockNumber.
5. `go p.executeShadowRequests(ctx, network, shadowUpstreams, cloneResp)`.

**Request cloning in `executeShadowRequests`** (`erpc/shadow.go:L86-L118`):
- If `origReq.Body()` is non-nil: `append([]byte(nil), body...)` (byte copy).
- Else: marshal JSON-RPC object to bytes, create new `NormalizedRequest`, pre-populate parsed request.
- Copy directives (cloned) and HTTP context.
- Set network reference.

**Per-upstream loop** (`erpc/shadow.go:L53-L264`):
1. `ShouldHandleMethod` check.
2. Sample rate check (`rand.Float64() >= sampleRate` → skip).
3. Launch goroutine with new app-context-derived context.
4. Forward with `byPassMethodExclusion=true, isHedgeAttempt=false`.
5. On error or nil response → increment `MetricShadowResponseErrorTotal` with appropriate error label, log debug.
6. Compute sizes, hashes (with or without ignored fields).
7. Check emptyish status on both.
8. Emit identical or mismatch metric + log.
9. `shadowResp.Release()`.

**Error fingerprinting.** `common.ErrorFingerprint(errForward)` is used as the `error` label on `MetricShadowResponseErrorTotal` — errors are normalized to a short stable string (e.g. the error type name), preventing label cardinality explosion.

**Hash comparison emptyish shortcut.** `isShadowEmpty && isOriginalEmpty` is treated as identical regardless of hash — both returning `null`/`""`/`[]`/`{}` is a match even if the serialized forms differ slightly.

**`isShadowLarger` flag.** `shadowSize > originalSize` is captured and passed as a label (`larger="true"/"false"`) on `MetricShadowResponseMismatchTotal`, enabling identification of cases where the shadow returns more data (e.g. extra fields in receipts).

### Observability

| Metric | Type | Labels | Fires when | Source |
|--------|------|--------|-----------|--------|
| `erpc_shadow_response_identical_total` | counter | `project`, `vendor`, `network`, `upstream`, `category` | Shadow response content-hash matches primary (or both emptyish) | `erpc/shadow.go:L218-L224`, `telemetry/metrics.go:L640-L644` |
| `erpc_shadow_response_mismatch_total` | counter | `project`, `vendor`, `network`, `upstream`, `category`, `finality`, `emptyish`, `larger` | Shadow response hash differs from primary | `erpc/shadow.go:L238-L248`, `telemetry/metrics.go:L646-L650` |
| `erpc_shadow_response_error_total` | counter | `project`, `vendor`, `network`, `upstream`, `category`, `error` | Shadow request errored, returned nil, or hash computation failed | `erpc/shadow.go:L121-L127,L141-L147,L194-L200`, `telemetry/metrics.go:L652-L656` |
| `erpc_unexpected_panic_total` | counter | `type=shadow-upstreams`, `context`, `fingerprint` | Panic recovered in `executeShadowRequests` | `erpc/shadow.go:L20-L26` |

**Label values:**
- `vendor` = `ups.VendorName()` (e.g. `"alchemy"`, `"quicknode"`).
- `category` = JSON-RPC method name (e.g. `"eth_getBlockByNumber"`).
- `finality` = `shadowResp.Finality(shadowCtx).String()` on mismatch (e.g. `"finalized"`, `"unfinalized"`, `"n/a"`).
- `emptyish` = `"true"/"false"` — whether shadow returned empty-ish result.
- `larger` = `"true"/"false"` — whether shadow response byte size exceeded primary.
- `error` = `common.ErrorFingerprint(err)` — stable short error type string.

**OTel trace span:** `"Project.executeShadowRequest"` (detail span) is created per shadow goroutine (`erpc/shadow.go:L82`); uses `common.StartDetailSpan`.

**Log messages:**
- Error: `"shadow response hash mismatch"` with full `request`, `originalResponse`, `shadowResponse` objects, `expectedHash`, `shadowHash`, `ignoredFields` (`erpc/shadow.go:L253-L261`).
- Debug: `"shadow request returned error"` (`erpc/shadow.go:L129-L137`).
- Debug: `"shadow request returned nil response"` (`erpc/shadow.go:L148-L157`).
- Debug: `"shadow request skipped due to sampling"` (`erpc/shadow.go:L69-L75`).
- Debug: `"method not allowed for shadow upstream"` (`erpc/shadow.go:L60-L62`).
- Trace: `"shadow response identical to primary response"` (`erpc/shadow.go:L225-L231`).

### Error codes

**`ErrUpstreamShadowing` (`ErrCodeUpstreamShadowing`)** — `common/errors.go:L1416-L1430`

- **When it fires:** `Upstream.shouldSkip` (`upstream/upstream.go:L1505-L1508`) returns this error as the `reason` any time a request arrives at a shadow-enabled upstream through the normal forward path (i.e., when `byPassMethodExclusion=false`). This is the very first check in `shouldSkip`, before syncing state or method filters. The caller at `upstream/upstream.go:L444-L449` wraps it in `ErrUpstreamRequestSkipped(reason, upstreamId)`.
- **Why shadow upstreams produce it:** Shadow upstreams are intentionally excluded from the normal routing pool (they live in `networkShadowUpstreams`, not `networkUpstreams`). In the rare case that a shadow upstream ends up in a normal forwarding path — which should not happen in correct configuration — `shouldSkip` fires this error as a hard guard.
- **Normal bypass:** The shadow goroutine itself calls `ups.Forward(ctx, shadowReq, true, false)` with `byPassMethodExclusion=true` (`erpc/shadow.go`, `upstream/upstream.go:L444`). This skips the `shouldSkip` block entirely, so `ErrUpstreamShadowing` is **never raised** during legitimate shadow execution.
- **HTTP status:** None. `ErrUpstreamShadowing` has no `ErrorStatusCode()` method. It is not a client-facing error; it is an internal guard that surfaces only in the `ErrUpstreamRequestSkipped` wrapper. The wire response for `ErrUpstreamRequestSkipped` uses eRPC's default handling (HTTP 200 with JSON-RPC error body, or retried on a different upstream).
- **Health-tracker exemption:** `health/tracker.go:L970-L983` explicitly lists `ErrCodeUpstreamShadowing` in the set of error codes that do **not** increment `ErrorsTotal` for the upstream. A shadow guard-trip does not degrade the upstream's error-rate score. `health/tracker_test.go:L765` encodes this as a test case asserting `ErrorsTotal` stays zero.
- **Details field:** `upstreamId` string — the ID of the shadow upstream that was incorrectly reached via the normal path.

### Edge cases & gotchas

1. **Shadow upstreams never participate in routing.** They are in a completely separate registry slice from `networkUpstreams`. No selection policy, no health scoring, no failsafe retry applies to them. `GetNetworkUpstreams` never returns them.

2. **Shadow fires even when primary response came from cache.** `p.doForward` returns the cached response, shadow sees `resp != nil` and fires anyway. If the shadow upstream has the same data as the cache, you will see identical matches; if not, you will see mismatches for every cache-served request.

3. **Context is from appCtx, not request ctx.** If the client disconnects mid-request, the primary response may be from a completed forward with the request ctx already cancelled. The shadow goroutine uses `p.networksRegistry.appCtx` so it runs to completion regardless. Shadow requests are never cancelled by client disconnect.

4. **sampleRate=0.0 suppresses all shadow traffic.** `rand.Float64()` always returns `>= 0.0`, so `rand.Float64() >= 0.0` is always true → skip. Setting `sampleRate: 0` is equivalent to disabling shadow without removing the config.

5. **sampleRate=1.0 (or nil) fires every request.** Explicitly setting `sampleRate: 1.0` or leaving it unset both result in always firing.

6. **Both hashes are recalculated when ignoreFields is set.** This prevents false mismatches where the original and shadow differ only on an ignored field. Both sides are hashed with the same mask, ensuring fair comparison.

7. **Hash error increments `error_total` with `error="hash_error"`.** If hash computation fails (e.g., response body not JSON-parseable), the comparison is skipped entirely and the error counter is incremented. No mismatch is recorded.

8. **Shadow upstreams respect method allow/ignore lists.** The `ShouldHandleMethod` call at `erpc/shadow.go:L54` checks the upstream's configured method filters. A shadow upstream with `allowMethods: [eth_call]` will only shadow `eth_call` requests.

9. **Shadow upstreams bypass `shouldSkip` (selection engine exclusion).** `byPassMethodExclusion=true` in `Forward` skips the `shouldSkip` check, which would otherwise include selection-engine cordon/score checks that are meaningless for a shadow upstream that is not in the normal pool.

10. **`ErrUpstreamShadowing` is a guard, not a user-visible error.** It fires inside `shouldSkip` only when a shadow upstream is reached through the normal forward path. Because shadow upstreams are exclusively in `networkShadowUpstreams` (never in `networkUpstreams`), this should never occur in normal operation. The health tracker exempts it from `ErrorsTotal` (`health/tracker.go:L975`; test: `health/tracker_test.go:L765`), so a misconfiguration that routes normal traffic to a shadow upstream would not appear as upstream degradation in scoring — it would only manifest as `ErrUpstreamRequestSkipped` retries.

11. **Panic in shadow goroutine is isolated.** The `defer recover()` in `executeShadowRequests` ensures a shadow panic never propagates to the caller goroutine. The primary response is already returned to the client before the goroutine is even spawned.

### Source map

- `erpc/shadow.go` — `executeShadowRequests`: request cloning, per-upstream loop, sample rate check, method check, forward, hash comparison, metric emit; called from `erpc/projects.go`.
- `erpc/projects.go:L135-L163` — `PreparedProject.Forward`: trigger point after `doForward`, response cloning, goroutine launch.
- `upstream/registry.go:L574-L603` — shadow vs. normal upstream registration split; `GetNetworkShadowUpstreams` accessor at `L295-L298`.
- `erpc/networks.go:L302-L303` — `Network.ShadowUpstreams()` accessor delegating to registry.
- `common/config.go:L982-L986` — `ShadowUpstreamConfig` struct definition.
- `common/errors.go:L1416-L1430` — `ErrUpstreamShadowing` / `ErrCodeUpstreamShadowing` definition; no `ErrorStatusCode()` method (no HTTP override).
- `upstream/upstream.go:L1505-L1508` — `shouldSkip` first check: returns `NewErrUpstreamShadowing` for any shadow-enabled upstream reached outside the bypass path.
- `health/tracker.go:L970-L983` — exemption list: `ErrCodeUpstreamShadowing` does not count as an upstream failure in `ErrorsTotal`.
- `health/tracker_test.go:L765` — test asserting `ErrUpstreamShadowing` keeps `ErrorsTotal=0` and `ErrorRate=0`.
- `telemetry/metrics.go:L640-L656` — `MetricShadowResponseIdenticalTotal`, `MetricShadowResponseMismatchTotal`, `MetricShadowResponseErrorTotal` definitions.
- `upstream/upstream.go:L410-L450` — `Forward` method showing `byPassMethodExclusion` parameter.
