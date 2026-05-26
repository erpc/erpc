import type { UpstreamConfig } from "../generated";
import type { Duration } from "./generic";
/**
 * Snapshot of upstream metrics captured at the start of each
 * `selectionPolicy` eval tick. Matches spec §3.1.
 */
export type PolicyEvalUpstreamMetrics = {
    errorRate: number;
    errorsTotal: number;
    requestsTotal: number;
    throttledRate: number;
    misbehaviorRate: number;
    p50ResponseSeconds: number;
    p70ResponseSeconds: number;
    p90ResponseSeconds: number;
    p95ResponseSeconds: number;
    p99ResponseSeconds: number;
    /**
     * Latency at any quantile, in milliseconds. Accepts either a 0..1
     * fraction (`0.95`) or a 0..100 percentile (`95`).
     */
    latencyP(quantile: number): number;
    /**
     * Number of blocks this upstream is behind the network's highest known
     * block head. Block-number delta, NOT seconds — tolerances differ per
     * chain (e.g. 10 blocks ≈ 120s on Ethereum, ≈ 2.5s on Arbitrum).
     */
    blockHeadLag: number;
    /**
     * Number of finalized blocks this upstream is behind the network's
     * highest known finalized block. Block-number delta, not seconds.
     */
    finalizationLag: number;
    /**
     * Wall-clock seconds an upstream is behind the network's head, computed
     * as `blockHeadLag * <tracker's EMA block-time>`. Zero until the tracker
     * has enough samples to estimate block time (a few seconds after first
     * traffic on most chains).
     */
    blockHeadLagSeconds: number;
    /**
     * Wall-clock seconds an upstream is behind the network's finalized head.
     * Same caveat as `blockHeadLagSeconds`.
     */
    finalizationLagSeconds: number;
    /**
     * Reason an upstream is currently cordoned by external systems
     * (failsafe / circuit breaker). `null` when not cordoned. The eval
     * can choose to honor this via `.removeCordoned()`.
     */
    cordonedReason: string | null;
};
/**
 * Score-preset weight map used by `sortByScore`. Missing keys are treated
 * as zero. The built-in presets (`PREFER_FASTEST`, `PREFER_FRESHEST`,
 * `PREFER_LEAST_ERRORS`) are exposed as bare globals inside the eval.
 */
export type ScoreWeights = {
    errorRate?: number;
    respLatency?: number;
    throttledRate?: number;
    blockHeadLag?: number;
    finalizationLag?: number;
    misbehaviors?: number;
};
/**
 * Per-upstream score multiplier resolved for the current tick and exposed
 * as `u.scoreMultipliers`. Same metric keys as `ScoreWeights` plus
 * `overall` — the preference dial that scales the upstream's final score
 * (>1 prefers this upstream, <1 avoids it). Comes from the upstream's
 * `routing.scoreMultipliers` config; `sortByScore` combines it with the
 * base weights per its `multipliers` option.
 */
export type ScoreMultiplierWeights = ScoreWeights & {
    overall?: number;
};
/**
 * Per-upstream score breakdown attached by `sortByScore`.
 */
export type ScoreBreakdown = {
    errorRate: number;
    respLatency: number;
    throttledRate: number;
    blockHeadLag: number;
    finalizationLag: number;
    misbehaviors: number;
    overall: number;
};
/**
 * Upstream object passed into the eval. The chainable std-lib returns
 * new arrays rather than mutating in place. Std-lib steps may attach
 * `score`, `penaltyBreakdown`, and `annotations` for later steps + the
 * decision record.
 */
export type PolicyEvalUpstream = {
    readonly id: string;
    readonly vendor: string;
    readonly type: "evm" | string;
    readonly endpoint: string;
    readonly tags: readonly string[];
    readonly config: UpstreamConfig;
    readonly metrics: PolicyEvalUpstreamMetrics;
    /**
     * Per-method latency snapshot for this upstream — keyed by method
     * name, values are a minimal `{ requestsTotal, p50ms, …, p99ms }`
     * shape used by `latencyDeviationAbove` to compare apples-to-apples
     * per method. Only methods with ≥1 recorded sample appear. The
     * shape is intentionally light (no full PolicyEvalUpstreamMetrics
     * per entry) — at 50+ networks × dozens of methods, the per-tick
     * allocation cost would otherwise dominate the policy engine's CPU.
     * Most operators don't read this directly; the predicate handles it.
     */
    readonly metricsByMethod: {
        readonly [method: string]: {
            readonly requestsTotal: number;
            readonly p50ms: number;
            readonly p70ms: number;
            readonly p90ms: number;
            readonly p95ms: number;
            readonly p99ms: number;
        };
    };
    /**
     * Per-upstream score multipliers resolved from `routing.scoreMultipliers`
     * for THIS tick's (network, method, finality). Absent when the upstream
     * has no matching entry. `sortByScore` reads this automatically — you
     * rarely touch it directly.
     */
    readonly scoreMultipliers?: ScoreMultiplierWeights;
    readonly score?: number;
    readonly penaltyBreakdown?: ScoreBreakdown;
    readonly annotations?: string[];
    /** Tag check that reads better than `tags.includes(t)`. */
    hasTag(tag: string): boolean;
    /** Alias of `hasTag`. */
    is(tag: string): boolean;
};
/**
 * Eval-time context. The ONLY carrier of cross-tick state — the engine
 * stores nothing beyond what comes in and out through `ctx`, so evals
 * are testable as pure functions.
 */
export type PolicyEvalContext = {
    readonly network: string;
    readonly method: "*" | string;
    readonly finality: "realtime" | "unfinalized" | "finalized" | "unknown";
    readonly now: number;
    readonly previousOrder: readonly string[];
    readonly lastSwitchAt: number | null;
    readonly tickCount: number;
};
/** Back-compat alias kept for users who already typed against the old name. */
export type EvalContext = PolicyEvalContext;
/**
 * A predicate is a function that, given an upstream, returns true to
 * "trip" the rule (and drop the upstream when used with `excludeIf`).
 * Predicate factories like `errorRateAbove(0.5)` set the optional
 * `policyReason` so `excludeIf` can auto-label drops in diagnostics.
 */
export type PolicyEvalPredicate = ((u: PolicyEvalUpstream) => boolean) & {
    readonly policyReason?: string;
};
/** Glob pattern or array of patterns. `!`-prefix negates. */
export type TagPattern = string | readonly string[];
/** `tier:fallback`, `region:us-*`, `!cohort:beta`, ... */
export type Pattern = string | readonly string[];
/**
 * Options for `sortByScore`.
 *
 * - `multipliers` controls how each upstream's `routing.scoreMultipliers`
 *   (exposed as `u.scoreMultipliers`) combine with the base weights:
 *     - `'merge'` (default): per-upstream keys override the matching base
 *       keys; unset keys inherit the base. `overall` lifts the final score.
 *     - `'override'`: configured upstreams rank by THEIR weights only
 *       (base ignored); upstreams without config use the base.
 *     - `'off'`: ignore `u.scoreMultipliers` entirely; rank by base.
 * - `latencyQuantile` selects which response-time quantile the
 *   `respLatency` weight scales (default `p70`).
 * - `overall` is an extra multiplicative dial folded on top of the
 *   per-upstream `overall`; mainly for programmatic callers.
 */
export type SortByScoreOptions = {
    multipliers?: "merge" | "override" | "off";
    latencyQuantile?: "p50" | "p70" | "p90" | "p95" | "p99";
    overall?: (u: PolicyEvalUpstream) => number;
};
/** Options for `removeByLatency`. All thresholds in ms. */
export type RemoveByLatencyOptions = {
    p50Ms?: number;
    p70Ms?: number;
    p90Ms?: number;
    p95Ms?: number;
    p99Ms?: number;
};
/** Options for `removeByLag`. Both fields are block-count deltas. */
export type RemoveByLagOptions = {
    blockHead?: number;
    finalization?: number;
};
/** Options for `keepHealthy` — defaults to the conservative trip points. */
export type KeepHealthyOptions = {
    maxErrorRate?: number;
    maxBlockHeadLag?: number;
    maxP95Ms?: number;
    maxThrottledRate?: number;
};
/** Options for `preferTag` / `preferVendor`. */
export type PreferOptions = {
    minHealthy?: number;
    fallback?: TagPattern;
};
/** Options for `stickyPrimary`. */
export type StickyPrimaryOptions = {
    /**
     * Grouping grain for the cross-slot shared primary register. Slots
     * mapped to the same scope value share a primary and converge on it
     * via hysteresis + minSwitchInterval. Defaults to `NETWORK` — max
     * cohesion (all methods + finalities on a network share one primary).
     *
     * Pass one of the imported `NETWORK` / `NETWORK_METHOD` /
     * `NETWORK_FINALITY` / `NETWORK_METHOD_FINALITY` constants, or the
     * raw kebab-case string.
     */
    scope?: "network" | "network-method" | "network-finality" | "network-method-finality";
    hysteresis?: number;
    minSwitchInterval?: Duration;
};
/**
 * Options for `probeExcluded` — registers the per-network probe
 * subsystem to shadow-mirror sampled real requests against any
 * upstream currently in the excluded set. The mirrored calls feed the
 * SAME health-tracker counters as real traffic, so the upstream
 * re-enters rotation naturally once its metrics improve enough to
 * clear the chain's `excludeIf` predicates. There is no time-based
 * re-admission timer.
 *
 * Per-upstream opt-out: set `routing.probe: 'off'` on any upstream
 * config to keep it out of probe traffic entirely.
 */
export type ProbeExcludedOptions = {
    /**
     * Per-(request, excluded-upstream) probability of mirroring
     * (0.0–1.0). Default 0.1 — only 10% of incoming requests are probe
     * candidates. Throttles probe-traffic cost on high-RPS networks
     * while the `minSamples` floor below ensures low-RPS networks
     * aren't starved.
     */
    sampleRate?: number;
    /**
     * Per-upstream floor on probes within `minSamplesWindow`. While the
     * upstream has fewer than this many probes accumulated in the
     * rolling window, the prober bypasses `sampleRate` and considers
     * every incoming request — so even low-traffic networks accumulate
     * enough samples for re-admission. Default 10. Pair with the
     * chain's `samplesAbove(N)` guards: `minSamples` should be ≥ that
     * N so the re-admission criterion is reachable.
     */
    minSamples?: number;
    /**
     * Rolling window for `minSamples` (default `'60s'`, matches typical
     * `scoreMetricsWindowSize`).
     */
    minSamplesWindow?: Duration;
    /**
     * Concurrent in-flight probes per excluded upstream (default 4).
     * Bounds worst-case per-upstream probe RPS independently of
     * sampleRate. When at the cap, additional incoming requests are
     * dropped from the probe feed.
     */
    maxConcurrent?: number;
    /**
     * Per-probe deadline (default `'10s'`). Probes that overrun are
     * cancelled and counted as failures so a hung upstream registers
     * as bad in the tracker.
     */
    timeout?: Duration;
};
/**
 * Options for `latencyDeviationAbove` — controls how per-method
 * ratios are collapsed into a single trip/no-trip decision per
 * upstream. See the predicate's doc comment for the full rationale
 * (distribution-skew defense via per-method comparison).
 */
export type LatencyDeviationOptions = {
    /**
     * Quantile to compare (`50` | `70` | `90` | `95` | `99`, also
     * accepts 0..1 fractions). Default `70`.
     */
    quantile?: number;
    /**
     * Resolution mode when methods disagree:
     * - `'geomean'` (default) — geometric mean of per-method ratios.
     *   Trips when the typical ratio across methods is ≥ multiplier.
     *   Self-protective against single-method outliers.
     * - `'majority'` — trips when ≥50% of compared methods show the
     *   upstream as ≥ multiplier× slower.
     * - `'veto'` — trips when ANY single method shows the upstream as
     *   ≥ multiplier× slower. Most aggressive; false-positive prone
     *   on specialty methods.
     */
    mode?: "geomean" | "majority" | "veto";
    /**
     * Per-method sample floor. Methods with fewer recorded requests on
     * an upstream are skipped from BOTH the top-2 peer-baseline pool
     * AND the per-upstream ratio loop. Defaults to `50` — at that count
     * the p70 quantile's CI is tight enough (~±10%) for cross-upstream
     * comparison to be meaningful. Below that, bootstrap noise on rare
     * methods amplifies through the geomean and falsely-trips healthy
     * upstreams. Set to `0` to disable the gate entirely (only useful
     * for testing or when you've already gated upstream).
     */
    minMethodSamples?: number;
    /**
     * Exponential damping scale (ms) for per-method ratios. The
     * effective ratio that contributes to the collapse is:
     *
     *     effective_ratio = (my / peer) × (1 − exp(−my / dampingMs))
     *
     * — so methods where the candidate's latency is well below
     * `dampingMs` have their ratio damped toward zero, while methods at
     * many × `dampingMs` get full-weight ratios. The transition is
     * smooth: a slightly mis-tuned `dampingMs` degrades gracefully
     * rather than flipping the predicate.
     *
     * Defaults to `30` (ms) — at 30ms damping is 0.63 (significant);
     * at 100ms damping is 0.96 (raw ratio mostly passes through). The
     * 30ms choice keeps the predicate sensitive to truly slow
     * upstreams (anything ≥100ms gets near-full ratio) while
     * suppressing meaningless micro-differences (2ms vs 6ms damps to
     * ~0.13× raw). Pair with a high multiplier (10+) so the mid-range
     * 70-150ms tier still has breathing room. Set to `0` to disable
     * damping entirely (raw ratios at all latencies).
     */
    dampingMs?: number;
};
/** Options for `byFinality` — per-finality branch handlers. */
export type ByFinalityHandlers = {
    realtime?: (u: PolicyEvalUpstreamArray) => PolicyEvalUpstreamArray;
    unfinalized?: (u: PolicyEvalUpstreamArray) => PolicyEvalUpstreamArray;
    finalized?: (u: PolicyEvalUpstreamArray) => PolicyEvalUpstreamArray;
    unknown?: (u: PolicyEvalUpstreamArray) => PolicyEvalUpstreamArray;
};
/** Filter spec for `where` / `whereNot`. */
export type WhereFilter = {
    id?: Pattern;
    tag?: TagPattern;
    vendor?: Pattern;
    type?: Pattern;
};
/**
 * The chainable upstream array passed into the eval. All methods return
 * a NEW array (immutable-style) so the chain is side-effect-free. Order
 * is meaningful: position 0 is the primary, position N is the Nth
 * retry/hedge candidate, anything missing is excluded for that tick.
 *
 * Methods are installed on `Array.prototype` inside the sobek runtime
 * (see `internal/policy/stdlib/stdlib.js`) — this type is the surface
 * area the eval sees.
 */
export interface PolicyEvalUpstreamArray extends ReadonlyArray<PolicyEvalUpstream> {
    /** `true` iff `length === 0`. Property, not method. */
    readonly isEmpty: boolean;
    byId(id: Pattern): PolicyEvalUpstreamArray;
    excludeId(id: Pattern): PolicyEvalUpstreamArray;
    byTag(pat: TagPattern): PolicyEvalUpstreamArray;
    excludeTag(pat: TagPattern): PolicyEvalUpstreamArray;
    byVendor(v: Pattern): PolicyEvalUpstreamArray;
    excludeVendor(v: Pattern): PolicyEvalUpstreamArray;
    byType(t: Pattern): PolicyEvalUpstreamArray;
    where(f: WhereFilter): PolicyEvalUpstreamArray;
    whereNot(f: WhereFilter): PolicyEvalUpstreamArray;
    removeByErrorRate(max: number): PolicyEvalUpstreamArray;
    removeByThrottling(max: number): PolicyEvalUpstreamArray;
    removeByMisbehavior(max: number): PolicyEvalUpstreamArray;
    removeByLag(opts: RemoveByLagOptions): PolicyEvalUpstreamArray;
    removeByMinRequests(min: number): PolicyEvalUpstreamArray;
    removeCordoned(): PolicyEvalUpstreamArray;
    removeByLatency(opts: RemoveByLatencyOptions): PolicyEvalUpstreamArray;
    keepHealthy(opts?: KeepHealthyOptions): PolicyEvalUpstreamArray;
    excludeIf(predicate: PolicyEvalPredicate, reasonOverride?: string): PolicyEvalUpstreamArray;
    /**
     * Dry-run / observed-only counterpart of `excludeIf`. The predicate runs
     * for every upstream, but no upstream is actually dropped — instead, every
     * trip is surfaced via:
     *
     *  - `erpc_selection_shadow_exclusion_total{upstream, reason=<leaf slug>}`
     *    (same option-(c) leaf attribution as the real counter);
     *  - a `shadow:<reason>` annotation on the upstream this tick;
     *  - the per-tick step trail (when DEBUG / simulator step-log is on).
     *
     * Use when auditioning a new exclusion rule (or removal of an existing
     * one) in production: deploy with `shadowExcludeIf`, watch the shadow
     * counter for N days, then flip to `excludeIf` once the rate matches
     * expectations. The upstream stays in rotation and
     * `stickyPrimary` / `probeExcluded` are untouched.
     */
    shadowExcludeIf(predicate: PolicyEvalPredicate, reasonOverride?: string): PolicyEvalUpstreamArray;
    reject(fn: (u: PolicyEvalUpstream, i: number, a: PolicyEvalUpstream[]) => unknown): PolicyEvalUpstreamArray;
    partition(fn: (u: PolicyEvalUpstream) => unknown): [PolicyEvalUpstreamArray, PolicyEvalUpstreamArray];
    unique(keyFn?: (u: PolicyEvalUpstream) => string): PolicyEvalUpstreamArray;
    union(other: readonly PolicyEvalUpstream[]): PolicyEvalUpstreamArray;
    intersect(other: readonly PolicyEvalUpstream[]): PolicyEvalUpstreamArray;
    difference(other: readonly PolicyEvalUpstream[]): PolicyEvalUpstreamArray;
    /**
     * Rank upstreams best-first (HIGHER score wins):
     * `score(u) = overall(u) / (1 + Σ metricᵢ × weightᵢ)`.
     * `base` is the baseline weight map — a preset (PREFER_FASTEST,
     * PREFER_FRESHEST, PREFER_LEAST_ERRORS), a custom `ScoreWeights` object,
     * a `(u) => ScoreWeights` function, or omitted (defaults to
     * PREFER_FASTEST). Per-upstream `u.scoreMultipliers` combine with `base`
     * per `opts.multipliers` (default `'merge'`).
     */
    sortByScore(base?: ScoreWeights | ((u: PolicyEvalUpstream) => ScoreWeights), opts?: SortByScoreOptions): PolicyEvalUpstreamArray;
    sortBy(fn: (u: PolicyEvalUpstream) => number, opts?: {
        desc?: boolean;
    }): PolicyEvalUpstreamArray;
    sortByDesc(fn: (u: PolicyEvalUpstream) => number): PolicyEvalUpstreamArray;
    sortByLatency(quantile?: "p50" | "p70" | "p90" | "p95" | "p99"): PolicyEvalUpstreamArray;
    sortByErrorRate(): PolicyEvalUpstreamArray;
    sortByThrottling(): PolicyEvalUpstreamArray;
    sortByMisbehavior(): PolicyEvalUpstreamArray;
    sortByHeadLag(): PolicyEvalUpstreamArray;
    sortByFinalizationLag(): PolicyEvalUpstreamArray;
    shuffle(seed?: number): PolicyEvalUpstreamArray;
    rotateBy(n: number): PolicyEvalUpstreamArray;
    stickyPrimary(opts?: StickyPrimaryOptions): PolicyEvalUpstreamArray;
    preferTag(pat: TagPattern, opts?: PreferOptions): PolicyEvalUpstreamArray;
    preferVendor(name: Pattern, opts?: PreferOptions): PolicyEvalUpstreamArray;
    spreadAcrossTags(prefix: string): PolicyEvalUpstreamArray;
    pickTop(n: number): PolicyEvalUpstreamArray;
    pickBottom(n: number): PolicyEvalUpstreamArray;
    dropTop(n: number): PolicyEvalUpstreamArray;
    dropBottom(n: number): PolicyEvalUpstreamArray;
    take(n: number): PolicyEvalUpstreamArray;
    skip(n: number): PolicyEvalUpstreamArray;
    at_(i: number): PolicyEvalUpstream | null;
    /**
     * Register the per-network probe subsystem. When this step is in
     * the chain, sampled real requests are shadow-mirrored against any
     * upstream currently in the excluded set; the mirrored calls feed
     * the same health tracker counters as real traffic, so excluded
     * upstreams re-admit naturally when their metrics improve enough
     * to clear the chain's `excludeIf` predicates.
     *
     * This is a no-op transform on the upstream array — its real work
     * is in the Go-side prober. Omitting it disables shadow probing
     * (excluded upstreams stay excluded until structural signals like
     * head lag bring their counters back, or an operator intervenes).
     *
     * Per-upstream opt-out via `routing.probe: 'off'`.
     */
    probeExcluded(opts?: ProbeExcludedOptions): PolicyEvalUpstreamArray;
    forceInclude(idOrFn: Pattern | ((u: PolicyEvalUpstream) => unknown), position?: "head" | "tail"): PolicyEvalUpstreamArray;
    if(cond: boolean | ((arr: PolicyEvalUpstreamArray) => unknown), thenFn: (arr: PolicyEvalUpstreamArray) => PolicyEvalUpstreamArray, elseFn?: (arr: PolicyEvalUpstreamArray) => PolicyEvalUpstreamArray): PolicyEvalUpstreamArray;
    unless(cond: boolean | ((arr: PolicyEvalUpstreamArray) => unknown), fn: (arr: PolicyEvalUpstreamArray) => PolicyEvalUpstreamArray): PolicyEvalUpstreamArray;
    whenEmpty(fn: () => readonly PolicyEvalUpstream[]): PolicyEvalUpstreamArray;
    whenNotEmpty(fn: (arr: PolicyEvalUpstreamArray) => PolicyEvalUpstreamArray): PolicyEvalUpstreamArray;
    /**
     * Finality-conditional sub-chain. `mask` is a bitwise-OR of the
     * `REALTIME` / `UNFINALIZED` / `FINALIZED` / `UNKNOWN` flag constants
     * (each a power of two). When the request's `ctx.finality` matches
     * the mask, `fn(this)` runs and its result becomes the chain value;
     * otherwise the array passes through unchanged.
     *
     *   .when(REALTIME | UNFINALIZED | UNKNOWN,
     *     u => u.stickyPrimary({ scope: NETWORK }))
     *
     * Typical use: skip stickyPrimary for finalized reads (no consistency
     * requirement, so route freely on latency).
     */
    when(mask: number, fn: (arr: PolicyEvalUpstreamArray) => PolicyEvalUpstreamArray): PolicyEvalUpstreamArray;
    fallbackTo(arrOrFn: readonly PolicyEvalUpstream[] | ((ctx: PolicyEvalContext) => readonly PolicyEvalUpstream[])): PolicyEvalUpstreamArray;
    ensureMin(n: number, fn: (arr: PolicyEvalUpstreamArray) => PolicyEvalUpstreamArray): PolicyEvalUpstreamArray;
    byFinality(handlers: ByFinalityHandlers): PolicyEvalUpstreamArray;
    tap(fn: (arr: PolicyEvalUpstreamArray) => void): PolicyEvalUpstreamArray;
    label(name: string): PolicyEvalUpstreamArray;
    dump(level?: "trace" | "debug" | "info" | "warn" | "error" | "log"): PolicyEvalUpstreamArray;
}
/**
 * The selection-policy eval. Returns the ordered list of upstreams that
 * should serve traffic — order is law, missing means excluded.
 *
 * When written as a TypeScript function in `erpc.ts`, the eRPC loader
 * compiles your whole config module into a sobek program and runs it
 * INSIDE the policy runtime (the function is never stringified, so its
 * closures stay intact). It's there — where the chainable methods +
 * predicate factories declared below as ambient globals are installed on
 * `Array.prototype` / `globalThis` — that the eval actually executes.
 */
export type SelectionPolicyEvalFunction = (upstreams: PolicyEvalUpstreamArray, ctx: PolicyEvalContext) => readonly PolicyEvalUpstream[];
declare global {
    /**
     * Latency dominates (respLatency=15). Default for most request paths;
     * the `excludeIf` chain already drops broken upstreams, so the
     * ranking question is "which of the healthy ones answers first?".
     */
    const PREFER_FASTEST: ScoreWeights;
    /**
     * Block-head freshness dominates (blockHeadLag=15). Realtime reads
     * that can't tolerate a stale-head upstream.
     */
    const PREFER_FRESHEST: ScoreWeights;
    /**
     * Error rate dominates (errorRate=15). Use for write paths or
     * anything where a 5xx costs more than a slow response.
     */
    const PREFER_LEAST_ERRORS: ScoreWeights;
    function errorRateAbove(rate: number): PolicyEvalPredicate;
    function errorRateBelow(rate: number): PolicyEvalPredicate;
    function throttleRateAbove(rate: number): PolicyEvalPredicate;
    function throttleRateBelow(rate: number): PolicyEvalPredicate;
    function misbehaviorRateAbove(rate: number): PolicyEvalPredicate;
    function latencyAbove(ms: number, quantile?: number): PolicyEvalPredicate;
    function latencyDeviationAbove(multiplier: number, optsOrQuantile?: number | LatencyDeviationOptions): PolicyEvalPredicate;
    function blockNumberLagAbove(blocks: number): PolicyEvalPredicate;
    function finalizationLagAbove(blocks: number): PolicyEvalPredicate;
    function blockSecondsLagAbove(seconds: number): PolicyEvalPredicate;
    function finalizationSecondsLagAbove(seconds: number): PolicyEvalPredicate;
    function samplesBelow(n: number): PolicyEvalPredicate;
    function samplesAbove(n: number): PolicyEvalPredicate;
    /**
     * Compose predicates with AND. The composed predicate trips iff EVERY
     * input predicate trips for the upstream.
     */
    function all(...preds: PolicyEvalPredicate[]): PolicyEvalPredicate;
    /**
     * Compose predicates with OR. The composed predicate trips iff ANY
     * input predicate trips for the upstream.
     */
    function any(...preds: PolicyEvalPredicate[]): PolicyEvalPredicate;
    /** Negate a predicate. */
    function not(pred: PolicyEvalPredicate): PolicyEvalPredicate;
    /** True iff `ctx.method` matches the glob/pattern. */
    function methodMatches(pat: Pattern): boolean;
    /** True iff `ctx.finality === 'finalized'`. */
    function isFinalityRequest(): boolean;
    /** Convert a `Duration` to milliseconds. Installed by the engine. */
    function durationMs(d: Duration): number;
    /** One primary per network; max cohesion across methods + finalities. */
    const NETWORK: "network";
    /** One primary per `(network, method)`; finalities share. */
    const NETWORK_METHOD: "network-method";
    /** One primary per `(network, finality)`; methods share. */
    const NETWORK_FINALITY: "network-finality";
    /** One primary per slot — independent per `(network, method, finality)`. */
    const NETWORK_METHOD_FINALITY: "network-method-finality";
    /** Bit 0 — request is reading live tip data (e.g. eth_blockNumber). */
    const REALTIME: 1;
    /** Bit 1 — request is reading a recent-but-not-yet-finalized block. */
    const UNFINALIZED: 2;
    /** Bit 2 — request is reading a finalized (no-reorg) block. */
    const FINALIZED: 4;
    /** Bit 3 — finality couldn't be determined for this request. */
    const UNKNOWN: 8;
}
//# sourceMappingURL=policyEval.d.ts.map