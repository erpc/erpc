import type { UpstreamConfig } from "../generated";
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
     * Reason an upstream is currently cordoned by external systems
     * (failsafe / circuit breaker). `null` when not cordoned. The eval
     * can choose to honor this via `.removeCordoned()`.
     */
    cordonedReason: string | null;
};
/**
 * Score-preset weight map used by `sortByScore`. Missing keys are treated
 * as zero. The built-in presets (`BALANCED`, `PREFER_FASTER`,
 * `PREFER_FEWER_ERRORS`, `PREFER_FRESHER_HEAD`, `PREFER_LESS_THROTTLED`,
 * `PREFER_CHEAP`) are exposed as bare globals inside the eval.
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
    readonly config: UpstreamConfig;
    readonly metrics: PolicyEvalUpstreamMetrics;
    readonly score?: number;
    readonly penaltyBreakdown?: ScoreBreakdown;
    readonly annotations?: string[];
};
/**
 * Eval-time context. The ONLY carrier of cross-tick state — the engine
 * stores nothing beyond what comes in and out through `ctx`, so evals
 * are testable as pure functions.
 */
export type EvalContext = {
    network: string;
    method: "*" | string;
    finality: "realtime" | "unfinalized" | "finalized" | "unknown";
    now: number;
    previousOrder: string[];
    previousExcluded: string[];
    lastSwitchAt: number | null;
    excludedSince: {
        [id: string]: number;
    };
    tickCount: number;
};
/**
 * The selection-policy eval. Returns the ordered list of upstreams that
 * should serve traffic — order is law, missing means excluded.
 */
export type SelectionPolicyEvalFunction = (upstreams: PolicyEvalUpstream[], ctx: EvalContext) => PolicyEvalUpstream[];
//# sourceMappingURL=policyEval.d.ts.map