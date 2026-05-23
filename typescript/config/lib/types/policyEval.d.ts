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
    readonly previousExcluded: readonly string[];
    readonly lastSwitchAt: number | null;
    readonly excludedSince: {
        readonly [id: string]: number;
    };
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
    hysteresis?: number;
    minSwitchInterval?: Duration;
};
/** Options for `readmitExcluded` (alias `probeExcluded`). */
export type ReadmitExcludedOptions = {
    reAdmitAfter?: Duration;
    maxConcurrent?: number;
    position?: "head" | "tail" | "random";
    longestFirst?: boolean;
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
     * expectations. The upstream stays in rotation and `excludedSince` /
     * `stickyPrimary` / `readmitExcluded` are untouched.
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
    readmitExcluded(opts?: ReadmitExcludedOptions): PolicyEvalUpstreamArray;
    /** Back-compat alias of `readmitExcluded`. */
    probeExcluded(opts?: ReadmitExcludedOptions): PolicyEvalUpstreamArray;
    forceInclude(idOrFn: Pattern | ((u: PolicyEvalUpstream) => unknown), position?: "head" | "tail"): PolicyEvalUpstreamArray;
    cooldown(duration: Duration): PolicyEvalUpstreamArray;
    if(cond: boolean | ((arr: PolicyEvalUpstreamArray) => unknown), thenFn: (arr: PolicyEvalUpstreamArray) => PolicyEvalUpstreamArray, elseFn?: (arr: PolicyEvalUpstreamArray) => PolicyEvalUpstreamArray): PolicyEvalUpstreamArray;
    unless(cond: boolean | ((arr: PolicyEvalUpstreamArray) => unknown), fn: (arr: PolicyEvalUpstreamArray) => PolicyEvalUpstreamArray): PolicyEvalUpstreamArray;
    whenEmpty(fn: () => readonly PolicyEvalUpstream[]): PolicyEvalUpstreamArray;
    whenNotEmpty(fn: (arr: PolicyEvalUpstreamArray) => PolicyEvalUpstreamArray): PolicyEvalUpstreamArray;
    fallbackTo(arrOrFn: readonly PolicyEvalUpstream[] | ((ctx: PolicyEvalContext) => readonly PolicyEvalUpstream[])): PolicyEvalUpstreamArray;
    ensureMin(n: number, fn: (arr: PolicyEvalUpstreamArray) => PolicyEvalUpstreamArray): PolicyEvalUpstreamArray;
    byFinality(handlers: ByFinalityHandlers): PolicyEvalUpstreamArray;
    tap(fn: (arr: PolicyEvalUpstreamArray) => void): PolicyEvalUpstreamArray;
    label(name: string): PolicyEvalUpstreamArray;
    annotate(fn: (u: PolicyEvalUpstream) => string): PolicyEvalUpstreamArray;
    mark(predicate: (u: PolicyEvalUpstream) => unknown, note: string): PolicyEvalUpstreamArray;
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
    function latencyAbove(quantile: number, ms: number): PolicyEvalPredicate;
    function latencyP50Above(ms: number): PolicyEvalPredicate;
    function latencyP70Above(ms: number): PolicyEvalPredicate;
    function latencyP90Above(ms: number): PolicyEvalPredicate;
    function latencyP95Above(ms: number): PolicyEvalPredicate;
    function latencyP99Above(ms: number): PolicyEvalPredicate;
    function latencyDeviationAbove(quantile: number, ms: number): PolicyEvalPredicate;
    function latencyDeviationPctAbove(quantile: number, pct: number): PolicyEvalPredicate;
    function p50DeviationMsAbove(ms: number): PolicyEvalPredicate;
    function p70DeviationMsAbove(ms: number): PolicyEvalPredicate;
    function p90DeviationMsAbove(ms: number): PolicyEvalPredicate;
    function p95DeviationMsAbove(ms: number): PolicyEvalPredicate;
    function p99DeviationMsAbove(ms: number): PolicyEvalPredicate;
    function p50DeviationPctAbove(pct: number): PolicyEvalPredicate;
    function p70DeviationPctAbove(pct: number): PolicyEvalPredicate;
    function p90DeviationPctAbove(pct: number): PolicyEvalPredicate;
    function p95DeviationPctAbove(pct: number): PolicyEvalPredicate;
    function p99DeviationPctAbove(pct: number): PolicyEvalPredicate;
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
}
//# sourceMappingURL=policyEval.d.ts.map