import type { UpstreamConfig } from "../generated";
/**
 * Metrics that will be passed to the selection policy evaluation function
 */
export type PolicyEvalUpstreamMetrics = {
    errorRate: number;
    errorsTotal: number;
    requestsTotal: number;
    throttledRate: number;
    p90ResponseSeconds: number;
    p95ResponseSeconds: number;
    p99ResponseSeconds: number;
    /**
     * Number of blocks this upstream is behind the network's highest known block head.
     * This is a block-number delta, not seconds — tolerances differ per chain
     * (e.g. 10 blocks ≈ 120s on Ethereum, ≈ 2.5s on Arbitrum).
     */
    blockHeadLag: number;
    /**
     * Number of finalized blocks this upstream is behind the network's highest known
     * finalized block. Block-number delta, not seconds.
     */
    finalizationLag: number;
    p90LatencySecs: number;
    p95LatencySecs: number;
    p99LatencySecs: number;
};
/**
 * Upstream that will be passed to the selection policy evaluation function
 */
export type PolicyEvalUpstream = {
    id: string;
    config: UpstreamConfig;
    metrics: PolicyEvalUpstreamMetrics;
};
/**
 * The selection policy evaluation function
 */
export type SelectionPolicyEvalFunction = (upstreams: PolicyEvalUpstream[], method: "*" | string) => PolicyEvalUpstream[];
//# sourceMappingURL=policyEval.d.ts.map