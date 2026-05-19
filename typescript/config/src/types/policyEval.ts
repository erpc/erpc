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
  readonly excludedSince: { readonly [id: string]: number };
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
 * Options for `sortByScore`. `latencyQuantile` selects which quantile
 * the `respLatency` weight scales; `overall` is a per-upstream
 * multiplier hook used by the legacy `scoreMultipliers` translator.
 */
export type SortByScoreOptions = {
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
export interface PolicyEvalUpstreamArray
  extends ReadonlyArray<PolicyEvalUpstream> {
  /** `true` iff `length === 0`. Property, not method. */
  readonly isEmpty: boolean;

  // ─── 4.2 Identity & label selection ───────────────────────────────────
  byId(id: Pattern): PolicyEvalUpstreamArray;
  excludeId(id: Pattern): PolicyEvalUpstreamArray;
  byTag(pat: TagPattern): PolicyEvalUpstreamArray;
  excludeTag(pat: TagPattern): PolicyEvalUpstreamArray;
  byVendor(v: Pattern): PolicyEvalUpstreamArray;
  excludeVendor(v: Pattern): PolicyEvalUpstreamArray;
  byType(t: Pattern): PolicyEvalUpstreamArray;
  where(f: WhereFilter): PolicyEvalUpstreamArray;
  whereNot(f: WhereFilter): PolicyEvalUpstreamArray;

  // ─── 4.3 Health filters ───────────────────────────────────────────────
  removeByErrorRate(max: number): PolicyEvalUpstreamArray;
  removeByThrottling(max: number): PolicyEvalUpstreamArray;
  removeByMisbehavior(max: number): PolicyEvalUpstreamArray;
  removeByLag(opts: RemoveByLagOptions): PolicyEvalUpstreamArray;
  removeByMinRequests(min: number): PolicyEvalUpstreamArray;
  removeCordoned(): PolicyEvalUpstreamArray;
  removeByLatency(opts: RemoveByLatencyOptions): PolicyEvalUpstreamArray;
  keepHealthy(opts?: KeepHealthyOptions): PolicyEvalUpstreamArray;

  // ─── 4.3a Predicate-driven exclusion ──────────────────────────────────
  excludeIf(
    predicate: PolicyEvalPredicate,
    reasonOverride?: string,
  ): PolicyEvalUpstreamArray;

  // ─── 4.4 Generic functional ───────────────────────────────────────────
  reject(
    fn: (u: PolicyEvalUpstream, i: number, a: PolicyEvalUpstream[]) => unknown,
  ): PolicyEvalUpstreamArray;
  partition(
    fn: (u: PolicyEvalUpstream) => unknown,
  ): [PolicyEvalUpstreamArray, PolicyEvalUpstreamArray];
  unique(keyFn?: (u: PolicyEvalUpstream) => string): PolicyEvalUpstreamArray;
  union(other: readonly PolicyEvalUpstream[]): PolicyEvalUpstreamArray;
  intersect(other: readonly PolicyEvalUpstream[]): PolicyEvalUpstreamArray;
  difference(other: readonly PolicyEvalUpstream[]): PolicyEvalUpstreamArray;

  // ─── 4.5 Sorting ──────────────────────────────────────────────────────
  sortByScore(
    weightsOrFnOrPreset:
      | ScoreWeights
      | ((u: PolicyEvalUpstream) => ScoreWeights),
    opts?: SortByScoreOptions,
  ): PolicyEvalUpstreamArray;
  sortBy(
    fn: (u: PolicyEvalUpstream) => number,
    opts?: { desc?: boolean },
  ): PolicyEvalUpstreamArray;
  sortByDesc(fn: (u: PolicyEvalUpstream) => number): PolicyEvalUpstreamArray;
  sortByLatency(
    quantile?: "p50" | "p70" | "p90" | "p95" | "p99",
  ): PolicyEvalUpstreamArray;
  sortByErrorRate(): PolicyEvalUpstreamArray;
  sortByThrottling(): PolicyEvalUpstreamArray;
  sortByMisbehavior(): PolicyEvalUpstreamArray;
  sortByHeadLag(): PolicyEvalUpstreamArray;
  sortByFinalizationLag(): PolicyEvalUpstreamArray;

  // ─── 4.6 Randomization & rotation ─────────────────────────────────────
  shuffle(seed?: number): PolicyEvalUpstreamArray;
  rotateBy(n: number): PolicyEvalUpstreamArray;

  // ─── 4.7 Stability (cross-tick) ───────────────────────────────────────
  stickyPrimary(opts?: StickyPrimaryOptions): PolicyEvalUpstreamArray;

  // ─── 4.8 Grouping & multi-tier ────────────────────────────────────────
  preferTag(pat: TagPattern, opts?: PreferOptions): PolicyEvalUpstreamArray;
  preferVendor(name: Pattern, opts?: PreferOptions): PolicyEvalUpstreamArray;
  spreadAcrossTags(prefix: string): PolicyEvalUpstreamArray;

  // ─── 4.9 Slicing & limits ─────────────────────────────────────────────
  pickTop(n: number): PolicyEvalUpstreamArray;
  pickBottom(n: number): PolicyEvalUpstreamArray;
  dropTop(n: number): PolicyEvalUpstreamArray;
  dropBottom(n: number): PolicyEvalUpstreamArray;
  take(n: number): PolicyEvalUpstreamArray;
  skip(n: number): PolicyEvalUpstreamArray;
  at_(i: number): PolicyEvalUpstream | null;

  // ─── 4.10 Probing & forced inclusion ──────────────────────────────────
  readmitExcluded(opts?: ReadmitExcludedOptions): PolicyEvalUpstreamArray;
  /** Back-compat alias of `readmitExcluded`. */
  probeExcluded(opts?: ReadmitExcludedOptions): PolicyEvalUpstreamArray;
  forceInclude(
    idOrFn: Pattern | ((u: PolicyEvalUpstream) => unknown),
    position?: "head" | "tail",
  ): PolicyEvalUpstreamArray;

  // ─── 4.11 Cooldown & warmup ───────────────────────────────────────────
  cooldown(duration: Duration): PolicyEvalUpstreamArray;

  // ─── 4.12 Combinators ─────────────────────────────────────────────────
  if(
    cond: boolean | ((arr: PolicyEvalUpstreamArray) => unknown),
    thenFn: (arr: PolicyEvalUpstreamArray) => PolicyEvalUpstreamArray,
    elseFn?: (arr: PolicyEvalUpstreamArray) => PolicyEvalUpstreamArray,
  ): PolicyEvalUpstreamArray;
  unless(
    cond: boolean | ((arr: PolicyEvalUpstreamArray) => unknown),
    fn: (arr: PolicyEvalUpstreamArray) => PolicyEvalUpstreamArray,
  ): PolicyEvalUpstreamArray;
  whenEmpty(
    fn: () => readonly PolicyEvalUpstream[],
  ): PolicyEvalUpstreamArray;
  whenNotEmpty(
    fn: (arr: PolicyEvalUpstreamArray) => PolicyEvalUpstreamArray,
  ): PolicyEvalUpstreamArray;
  fallbackTo(
    arrOrFn:
      | readonly PolicyEvalUpstream[]
      | ((ctx: PolicyEvalContext) => readonly PolicyEvalUpstream[]),
  ): PolicyEvalUpstreamArray;
  ensureMin(
    n: number,
    fn: (arr: PolicyEvalUpstreamArray) => PolicyEvalUpstreamArray,
  ): PolicyEvalUpstreamArray;
  byFinality(handlers: ByFinalityHandlers): PolicyEvalUpstreamArray;

  // ─── 4.13 Annotations & debug ─────────────────────────────────────────
  tap(fn: (arr: PolicyEvalUpstreamArray) => void): PolicyEvalUpstreamArray;
  label(name: string): PolicyEvalUpstreamArray;
  annotate(
    fn: (u: PolicyEvalUpstream) => string,
  ): PolicyEvalUpstreamArray;
  mark(
    predicate: (u: PolicyEvalUpstream) => unknown,
    note: string,
  ): PolicyEvalUpstreamArray;
  dump(level?: "trace" | "debug" | "info" | "warn" | "error" | "log"): PolicyEvalUpstreamArray;
}

/**
 * The selection-policy eval. Returns the ordered list of upstreams that
 * should serve traffic — order is law, missing means excluded.
 *
 * When written as a TypeScript function in `erpc.ts`, the eRPC loader
 * stringifies it via `Function.prototype.toString()` at config load time,
 * compiles the source into a sobek program, and re-evaluates it inside
 * the policy runtime — where the chainable methods + predicate factories
 * declared below as ambient globals are actually installed on
 * `Array.prototype` / `globalThis`.
 */
export type SelectionPolicyEvalFunction = (
  upstreams: PolicyEvalUpstreamArray,
  ctx: PolicyEvalContext,
) => readonly PolicyEvalUpstream[];

/* ───────────────────────── Ambient globals ─────────────────────────────
 * The factories and presets below are installed on `globalThis` by the
 * stdlib at policy-runtime load time. Declaring them as ambient globals
 * here lets the TypeScript checker see them inside `evalFunc` bodies
 * without making the user import from the package (which would not
 * survive Function.prototype.toString() round-tripping into sobek).
 * ──────────────────────────────────────────────────────────────────── */

declare global {
  // Score presets ─────────────────────────────────────────────────────
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

  // Rate-based predicate factories (0..1 fractions) ───────────────────
  function errorRateAbove(rate: number): PolicyEvalPredicate;
  function errorRateBelow(rate: number): PolicyEvalPredicate;
  function throttleRateAbove(rate: number): PolicyEvalPredicate;
  function throttleRateBelow(rate: number): PolicyEvalPredicate;
  function misbehaviorRateAbove(rate: number): PolicyEvalPredicate;

  // Latency predicate factories (ms thresholds) ───────────────────────
  function latencyAbove(quantile: number, ms: number): PolicyEvalPredicate;
  function latencyP50Above(ms: number): PolicyEvalPredicate;
  function latencyP70Above(ms: number): PolicyEvalPredicate;
  function latencyP90Above(ms: number): PolicyEvalPredicate;
  function latencyP95Above(ms: number): PolicyEvalPredicate;
  function latencyP99Above(ms: number): PolicyEvalPredicate;

  // Latency-deviation predicate factories (compare against pool median) ─
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

  // Lag predicate factories ───────────────────────────────────────────
  function blockNumberLagAbove(blocks: number): PolicyEvalPredicate;
  function finalizationLagAbove(blocks: number): PolicyEvalPredicate;
  function blockSecondsLagAbove(seconds: number): PolicyEvalPredicate;
  function finalizationSecondsLagAbove(seconds: number): PolicyEvalPredicate;

  // Sample-size guards ────────────────────────────────────────────────
  function samplesBelow(n: number): PolicyEvalPredicate;
  function samplesAbove(n: number): PolicyEvalPredicate;

  // Logical combinators ───────────────────────────────────────────────
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

  // Module-level helpers ──────────────────────────────────────────────
  /** True iff `ctx.method` matches the glob/pattern. */
  function methodMatches(pat: Pattern): boolean;
  /** True iff `ctx.finality === 'finalized'`. */
  function isFinalityRequest(): boolean;
  /** Convert a `Duration` to milliseconds. Installed by the engine. */
  function durationMs(d: Duration): number;
}
