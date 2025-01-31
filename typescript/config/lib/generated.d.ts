import type { LogLevel, Duration, ByteSize, ConnectorDriverType as TsConnectorDriverType, ConnectorConfig as TsConnectorConfig, UpstreamType as TsUpstreamType, NetworkArchitecture as TsNetworkArchitecture, AuthType as TsAuthType, AuthStrategyConfig as TsAuthStrategyConfig, SelectionPolicyEvalFunction } from "./types";
export declare const UpstreamTypeEvm: UpstreamType;
export type EvmUpstream = Upstream;
export type EvmNodeType = string;
export declare const EvmNodeTypeFull: EvmNodeType;
export declare const EvmNodeTypeArchive: EvmNodeType;
export declare const EvmNodeTypeLight: EvmNodeType;
export type EvmSyncingState = number;
export declare const EvmSyncingStateUnknown: EvmSyncingState;
export declare const EvmSyncingStateSyncing: EvmSyncingState;
export declare const EvmSyncingStateNotSyncing: EvmSyncingState;
export type EvmStatePoller = any;
export type CacheDAL = any;
export interface MockCacheDal {
    mock: any;
}
/**
 * Config represents the configuration of the application.
 */
export interface Config {
    logLevel: LogLevel;
    server?: ServerConfig;
    admin?: AdminConfig;
    database?: DatabaseConfig;
    projects: (ProjectConfig | undefined)[];
    rateLimiters?: RateLimiterConfig;
    metrics?: MetricsConfig;
    proxyPools?: (ProxyPoolConfig | undefined)[];
}
export interface ServerConfig {
    listenV4?: boolean;
    httpHostV4?: string;
    listenV6?: boolean;
    httpHostV6?: string;
    httpPort?: number;
    maxTimeout?: string;
    readTimeout?: string;
    writeTimeout?: string;
    enableGzip?: boolean;
    tls?: TLSConfig;
    aliasing?: AliasingConfig;
}
export interface AdminConfig {
    auth?: AuthConfig;
    cors?: CORSConfig;
}
export interface AliasingConfig {
    rules: (AliasingRuleConfig | undefined)[];
}
export interface AliasingRuleConfig {
    matchDomain: string;
    serveProject: string;
    serveArchitecture: string;
    serveChain: string;
}
export interface DatabaseConfig {
    evmJsonRpcCache?: CacheConfig;
}
export interface CacheConfig {
    connectors?: TsConnectorConfig[];
    policies?: (CachePolicyConfig | undefined)[];
    methods?: {
        [key: string]: CacheMethodConfig | undefined;
    };
}
export interface CacheMethodConfig {
    reqRefs: any[][];
    respRefs: any[][];
    finalized: boolean;
    realtime: boolean;
}
export interface CachePolicyConfig {
    connector: string;
    network?: string;
    method?: string;
    params?: any[];
    finality?: DataFinalityState;
    empty?: CacheEmptyBehavior;
    minItemSize?: ByteSize;
    maxItemSize?: ByteSize;
    ttl?: number;
}
export type ConnectorDriverType = string;
export declare const DriverMemory: ConnectorDriverType;
export declare const DriverRedis: ConnectorDriverType;
export declare const DriverPostgreSQL: ConnectorDriverType;
export declare const DriverDynamoDB: ConnectorDriverType;
export interface ConnectorConfig {
    id: string;
    driver: TsConnectorDriverType;
    memory?: MemoryConnectorConfig;
    redis?: RedisConnectorConfig;
    dynamodb?: DynamoDBConnectorConfig;
    postgresql?: PostgreSQLConnectorConfig;
}
export interface MemoryConnectorConfig {
    maxItems: number;
}
export interface TLSConfig {
    enabled: boolean;
    certFile: string;
    keyFile: string;
    caFile?: string;
    insecureSkipVerify?: boolean;
}
export interface RedisConnectorConfig {
    addr: string;
    db: number;
    tls?: TLSConfig;
    connPoolSize: number;
    initTimeout?: number;
    getTimeout?: number;
    setTimeout?: number;
}
export interface DynamoDBConnectorConfig {
    table: string;
    region: string;
    endpoint: string;
    auth?: AwsAuthConfig;
    partitionKeyName: string;
    rangeKeyName: string;
    reverseIndexName: string;
    ttlAttributeName: string;
    initTimeout?: number;
    getTimeout?: number;
    setTimeout?: number;
}
export interface PostgreSQLConnectorConfig {
    connectionUri: string;
    table: string;
    minConns?: number;
    maxConns?: number;
    initTimeout?: number;
    getTimeout?: number;
    setTimeout?: number;
}
export interface AwsAuthConfig {
    mode: 'file' | 'env' | 'secret';
    credentialsFile: string;
    profile: string;
    accessKeyID: string;
    secretAccessKey: string;
}
export interface ProjectConfig {
    id: string;
    auth?: AuthConfig;
    cors?: CORSConfig;
    providers?: (ProviderConfig | undefined)[];
    upstreamDefaults?: UpstreamConfig;
    upstreams?: (UpstreamConfig | undefined)[];
    networkDefaults?: NetworkDefaults;
    networks?: (NetworkConfig | undefined)[];
    rateLimitBudget?: string;
    healthCheck?: HealthCheckConfig;
}
export interface NetworkDefaults {
    rateLimitBudget?: string;
    failsafe?: FailsafeConfig;
    selectionPolicy?: SelectionPolicyConfig;
    directiveDefaults?: DirectiveDefaultsConfig;
    evm?: EvmNetworkConfig;
}
export interface CORSConfig {
    allowedOrigins: string[];
    allowedMethods: string[];
    allowedHeaders: string[];
    exposedHeaders: string[];
    allowCredentials?: boolean;
    maxAge: number;
}
export type VendorSettings = {
    [key: string]: any;
};
export interface ProviderConfig {
    id?: string;
    vendor: string;
    settings?: VendorSettings;
    onlyNetworks?: string[];
    upstreamIdTemplate?: string;
    overrides?: {
        [key: string]: UpstreamConfig | undefined;
    };
}
export interface UpstreamConfig {
    id?: string;
    type?: TsUpstreamType;
    group?: string;
    vendorName?: string;
    endpoint: string;
    evm?: EvmUpstreamConfig;
    jsonRpc?: JsonRpcUpstreamConfig;
    ignoreMethods?: string[];
    allowMethods?: string[];
    autoIgnoreUnsupportedMethods?: boolean;
    failsafe?: FailsafeConfig;
    rateLimitBudget?: string;
    rateLimitAutoTune?: RateLimitAutoTuneConfig;
    routing?: RoutingConfig;
}
export interface RoutingConfig {
    scoreMultipliers: (ScoreMultiplierConfig | undefined)[];
}
export interface ScoreMultiplierConfig {
    network: string;
    method: string;
    overall: number;
    errorRate: number;
    p90latency: number;
    totalRequests: number;
    throttledRate: number;
    blockHeadLag: number;
    finalizationLag: number;
}
export type Alias = UpstreamConfig;
export interface RateLimitAutoTuneConfig {
    enabled?: boolean;
    adjustmentPeriod: Duration;
    errorRateThreshold: number;
    increaseFactor: number;
    decreaseFactor: number;
    minBudget: number;
    maxBudget: number;
}
export interface JsonRpcUpstreamConfig {
    supportsBatch?: boolean;
    batchMaxSize?: number;
    batchMaxWait?: string;
    enableGzip?: boolean;
    headers?: {
        [key: string]: string;
    };
    proxyPool?: string;
}
export interface EvmUpstreamConfig {
    chainId: number;
    nodeType?: EvmNodeType;
    statePollerInterval?: string;
    statePollerDebounce?: string;
    maxAvailableRecentBlocks?: number;
    getLogsMaxBlockRange?: number;
}
export interface FailsafeConfig {
    retry?: RetryPolicyConfig;
    circuitBreaker?: CircuitBreakerPolicyConfig;
    timeout?: TimeoutPolicyConfig;
    hedge?: HedgePolicyConfig;
    consensus?: ConsensusPolicyConfig;
}
export interface RetryPolicyConfig {
    maxAttempts: number;
    delay: string;
    backoffMaxDelay: string;
    backoffFactor: number;
    jitter: string;
}
export interface CircuitBreakerPolicyConfig {
    failureThresholdCount: number;
    failureThresholdCapacity: number;
    halfOpenAfter: string;
    successThresholdCount: number;
    successThresholdCapacity: number;
}
export interface TimeoutPolicyConfig {
    duration: Duration;
}
export interface HedgePolicyConfig {
    delay: string;
    maxCount: number;
    quantile: number;
    minDelay: Duration;
    maxDelay: Duration;
}
export type ConsensusFailureBehavior = string;
export declare const ConsensusFailureBehaviorReturnError: ConsensusFailureBehavior;
export declare const ConsensusFailureBehaviorAcceptAnyValidResult: ConsensusFailureBehavior;
export declare const ConsensusFailureBehaviorPreferBlockHeadLeader: ConsensusFailureBehavior;
export declare const ConsensusFailureBehaviorOnlyBlockHeadLeader: ConsensusFailureBehavior;
export type ConsensusLowParticipantsBehavior = string;
export declare const ConsensusLowParticipantsBehaviorReturnError: ConsensusLowParticipantsBehavior;
export declare const ConsensusLowParticipantsBehaviorAcceptAnyValidResult: ConsensusLowParticipantsBehavior;
export declare const ConsensusLowParticipantsBehaviorPreferBlockHeadLeader: ConsensusLowParticipantsBehavior;
export declare const ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader: ConsensusLowParticipantsBehavior;
export type ConsensusDisputeBehavior = string;
export declare const ConsensusDisputeBehaviorReturnError: ConsensusDisputeBehavior;
export declare const ConsensusDisputeBehaviorAcceptAnyValidResult: ConsensusDisputeBehavior;
export declare const ConsensusDisputeBehaviorPreferBlockHeadLeader: ConsensusDisputeBehavior;
export declare const ConsensusDisputeBehaviorOnlyBlockHeadLeader: ConsensusDisputeBehavior;
export interface ConsensusPolicyConfig {
    requiredParticipants: number;
    agreementThreshold?: number;
    failureBehavior?: ConsensusFailureBehavior;
    disputeBehavior?: ConsensusDisputeBehavior;
    lowParticipantsBehavior?: ConsensusLowParticipantsBehavior;
    punishMisbehavior?: PunishMisbehaviorConfig;
}
export interface PunishMisbehaviorConfig {
    disputeThreshold: number;
    sitOutPenalty?: string;
}
export interface RateLimiterConfig {
    budgets: RateLimitBudgetConfig[];
}
export interface RateLimitBudgetConfig {
    id: string;
    rules: RateLimitRuleConfig[];
}
export interface RateLimitRuleConfig {
    method: string;
    maxCount: number;
    period: Duration;
    waitTime: Duration;
}
export interface ProxyPoolConfig {
    id: string;
    urls: string[];
}
export interface HealthCheckConfig {
    scoreMetricsWindowSize: string;
}
export interface NetworkConfig {
    architecture: TsNetworkArchitecture;
    rateLimitBudget?: string;
    failsafe?: FailsafeConfig;
    evm?: EvmNetworkConfig;
    selectionPolicy?: SelectionPolicyConfig;
    directiveDefaults?: DirectiveDefaultsConfig;
}
export interface DirectiveDefaultsConfig {
    retryEmpty?: boolean;
    retryPending?: boolean;
    skipCacheRead?: boolean;
    useUpstream?: string;
}
export interface EvmNetworkConfig {
    chainId: number;
    fallbackFinalityDepth?: number;
    fallbackStatePollerDebounce?: string;
    integrity?: EvmIntegrityConfig;
}
export interface EvmIntegrityConfig {
    enforceHighestBlock?: boolean;
    enforceGetLogsBlockRange?: boolean;
}
export interface SelectionPolicyConfig {
    evalInterval?: number;
    evalFunction?: SelectionPolicyEvalFunction | undefined;
    evalPerMethod?: boolean;
    resampleExcluded?: boolean;
    resampleInterval?: number;
    resampleCount?: number;
}
export type AuthType = string;
export declare const AuthTypeSecret: AuthType;
export declare const AuthTypeJwt: AuthType;
export declare const AuthTypeSiwe: AuthType;
export declare const AuthTypeNetwork: AuthType;
export interface AuthConfig {
    strategies: TsAuthStrategyConfig[];
}
export interface AuthStrategyConfig {
    ignoreMethods?: string[];
    allowMethods?: string[];
    rateLimitBudget?: string;
    type: TsAuthType;
    network?: NetworkStrategyConfig;
    secret?: SecretStrategyConfig;
    jwt?: JwtStrategyConfig;
    siwe?: SiweStrategyConfig;
}
export interface SecretStrategyConfig {
    value: string;
}
export interface JwtStrategyConfig {
    allowedIssuers: string[];
    allowedAudiences: string[];
    allowedAlgorithms: string[];
    requiredClaims: string[];
    verificationKeys: {
        [key: string]: string;
    };
}
export interface SiweStrategyConfig {
    allowedDomains: string[];
}
export interface NetworkStrategyConfig {
    allowedIPs: string[];
    allowedCIDRs: string[];
    allowLocalhost: boolean;
    trustedProxies: string[];
}
export interface MetricsConfig {
    enabled?: boolean;
    listenV4?: boolean;
    hostV4?: string;
    listenV6?: boolean;
    hostV6?: string;
    port?: number;
}
export type DataFinalityState = number;
/**
 * Finalized gets 0 intentionally so that when user has not specified finality,
 * it defaults to finalized, which is safest sane default for caching.
 * This attribute will be calculated based on extracted block number (from request and/or response)
 * and comparing to the upstream (one that returned the response) 'finalized' block (fetch via evm state poller).
 */
export declare const DataFinalityStateFinalized: DataFinalityState;
/**
 * When we CAN determine the block number, and it's after the upstream 'finalized' block, we consider the data unfinalized.
 */
export declare const DataFinalityStateUnfinalized: DataFinalityState;
/**
 * Certain methods points are meant to be realtime and updated with every new block (e.g. eth_gasPrice).
 * These can be cached with short TTLs to improve performance.
 */
export declare const DataFinalityStateRealtime: DataFinalityState;
/**
 * When we CANNOT determine the block number (e.g some trace by hash calls), we consider the data unknown.
 * Most often it is safe to cache this data for longer as they're access when block hash is provided directly.
 */
export declare const DataFinalityStateUnknown: DataFinalityState;
export type CacheEmptyBehavior = number;
export declare const CacheEmptyBehaviorIgnore: CacheEmptyBehavior;
export declare const CacheEmptyBehaviorAllow: CacheEmptyBehavior;
export declare const CacheEmptyBehaviorOnly: CacheEmptyBehavior;
export declare const DefaultEvmFinalityDepth = 1024;
export declare const DefaultEvmStatePollerDebounce = "5s";
export declare const DefaultPolicyFunction = "\n\t(upstreams, method) => {\n\t\tconst defaults = upstreams.filter(u => u.config.group !== 'fallback')\n\t\tconst fallbacks = upstreams.filter(u => u.config.group === 'fallback')\n\t\t\n\t\tconst maxErrorRate = parseFloat(process.env.ROUTING_POLICY_MAX_ERROR_RATE || '0.7')\n\t\tconst maxBlockHeadLag = parseFloat(process.env.ROUTING_POLICY_MAX_BLOCK_HEAD_LAG || '10')\n\t\tconst minHealthyThreshold = parseInt(process.env.ROUTING_POLICY_MIN_HEALTHY_THRESHOLD || '1')\n\t\t\n\t\tconst healthyOnes = defaults.filter(\n\t\t\tu => u.metrics.errorRate < maxErrorRate && u.metrics.blockHeadLag < maxBlockHeadLag\n\t\t)\n\t\t\n\t\tif (healthyOnes.length >= minHealthyThreshold) {\n\t\t\treturn healthyOnes\n\t\t}\n\n\t\tif (fallbacks.length > 0) {\n\t\t\tlet healthyFallbacks = fallbacks.filter(\n\t\t\t\tu => u.metrics.errorRate < maxErrorRate && u.metrics.blockHeadLag < maxBlockHeadLag\n\t\t\t)\n\t\t\t\n\t\t\tif (healthyFallbacks.length > 0) {\n\t\t\t\treturn healthyFallbacks\n\t\t\t}\n\t\t}\n\n\t\t// The reason all upstreams are returned is to be less harsh and still consider default nodes (in case they have intermittent issues)\n\t\t// Order of upstreams does not matter as that will be decided by the upstream scoring mechanism\n\t\treturn upstreams\n\t}\n";
export type NetworkArchitecture = string;
export declare const ArchitectureEvm: NetworkArchitecture;
export type Network = any;
export type QuantileTracker = any;
export type TrackedMetrics = any;
export type Scope = string;
/**
 * Policies must be created with a "network" in mind,
 * assuming there will be many upstreams e.g. Retry might endup using a different upstream
 */
export declare const ScopeNetwork: Scope;
/**
 * Policies must be created with one only "upstream" in mind
 * e.g. Retry with be towards the same upstream
 */
export declare const ScopeUpstream: Scope;
export type UpstreamType = string;
export type Upstream = any;
export interface FakeUpstream {
}
export type Vendor = any;
//# sourceMappingURL=generated.d.ts.map