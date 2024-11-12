import type { UpstreamConfig } from "./generated";
export type Duration = `${number}ms` | `${number}s` | `${number}m` | `${number}h`;
export type LogLevel = 'trace' | 'debug' | 'info' | 'warn' | 'error' | 'disabled' | undefined;
export type ConnectorDriverType = 'memory' | 'redis' | 'postgres' | 'dynamodb';
export type Upstream = {
    id: string;
    config: UpstreamConfig;
    metrics: UpstreamMetrics;
};
export type UpstreamMetrics = {
    errorRate: number;
    errorsTotal: number;
    requestsTotal: number;
    throttledRate: number;
    p90LatencySecs: number;
    blockHeadLag: number;
    finalizationLag: number;
};
export type SelectionPolicyEvalFunction = (upstreams: Upstream[], method: '*' | string) => Upstream[];
//# sourceMappingURL=types.d.ts.map