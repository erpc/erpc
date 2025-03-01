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
  blockHeadLag: number;
  finalizationLag: number;

  // @deprecated
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
export type SelectionPolicyEvalFunction = (
  upstreams: PolicyEvalUpstream[],
  method: "*" | string,
) => PolicyEvalUpstream[];
