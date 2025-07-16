import type { LogLevel, Duration, ByteSize, ConnectorDriverType as TsConnectorDriverType, ConnectorConfig as TsConnectorConfig, UpstreamType as TsUpstreamType, NetworkArchitecture as TsNetworkArchitecture, AuthType as TsAuthType, AuthStrategyConfig as TsAuthStrategyConfig, EvmNetworkConfigForDefaults as TsEvmNetworkConfigForDefaults, SelectionPolicyEvalFunction } from "./types";
export declare const UpstreamTypeEvm: UpstreamType;
export type EvmUpstream = Upstream;
export type AvailbilityConfidence = number;
export declare const AvailbilityConfidenceBlockHead: AvailbilityConfidence;
export declare const AvailbilityConfidenceFinalized: AvailbilityConfidence;
export type EvmNodeType = string;
export declare const EvmNodeTypeUnknown: EvmNodeType;
export declare const EvmNodeTypeFull: EvmNodeType;
export declare const EvmNodeTypeArchive: EvmNodeType;
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
    logLevel?: LogLevel;
    clusterKey?: string;
    server?: ServerConfig;
    healthCheck?: HealthCheckConfig;
    admin?: AdminConfig;
    database?: DatabaseConfig;
    projects?: (ProjectConfig | undefined)[];
    rateLimiters?: RateLimiterConfig;
    metrics?: MetricsConfig;
    proxyPools?: (ProxyPoolConfig | undefined)[];
    tracing?: TracingConfig;
}
export interface ServerConfig {
    listenV4?: boolean;
    httpHostV4?: string;
    listenV6?: boolean;
    httpHostV6?: string;
    httpPort?: number;
    maxTimeout?: Duration;
    readTimeout?: Duration;
    writeTimeout?: Duration;
    enableGzip?: boolean;
    tls?: TLSConfig;
    aliasing?: AliasingConfig;
    waitBeforeShutdown?: Duration;
    waitAfterShutdown?: Duration;
    includeErrorDetails?: boolean;
}
export interface HealthCheckConfig {
    mode?: HealthCheckMode;
    auth?: AuthConfig;
    defaultEval?: string;
}
export type HealthCheckMode = string;
export declare const HealthCheckModeSimple: HealthCheckMode;
export declare const HealthCheckModeVerbose: HealthCheckMode;
export declare const EvalAnyInitializedUpstreams = "any:initializedUpstreams";
export declare const EvalAnyErrorRateBelow90 = "any:errorRateBelow90";
export declare const EvalAllErrorRateBelow90 = "all:errorRateBelow90";
export declare const EvalAnyErrorRateBelow100 = "any:errorRateBelow100";
export declare const EvalAllErrorRateBelow100 = "all:errorRateBelow100";
export declare const EvalEvmAnyChainId = "any:evm:eth_chainId";
export declare const EvalEvmAllChainId = "all:evm:eth_chainId";
export declare const EvalAllActiveUpstreams = "all:activeUpstreams";
export type TracingProtocol = string;
export declare const TracingProtocolHttp: TracingProtocol;
export declare const TracingProtocolGrpc: TracingProtocol;
export interface TracingConfig {
    enabled?: boolean;
    endpoint?: string;
    protocol?: TracingProtocol;
    sampleRate?: number;
    detailed?: boolean;
    tls?: TLSConfig;
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
    sharedState?: SharedStateConfig;
}
export interface SharedStateConfig {
    clusterKey?: string;
    connector?: ConnectorConfig;
    fallbackTimeout?: Duration;
    lockTtl?: Duration;
}
export interface CacheConfig {
    connectors?: TsConnectorConfig[];
    policies?: (CachePolicyConfig | undefined)[];
    compression?: CompressionConfig;
}
export interface CompressionConfig {
    enabled?: boolean;
    algorithm?: string;
    zstdLevel?: string;
    threshold?: number;
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
    ttl?: Duration;
}
export type ConnectorDriverType = string;
export declare const DriverMemory: ConnectorDriverType;
export declare const DriverRedis: ConnectorDriverType;
export declare const DriverPostgreSQL: ConnectorDriverType;
export declare const DriverDynamoDB: ConnectorDriverType;
export interface ConnectorConfig {
    id?: string;
    driver: TsConnectorDriverType;
    memory?: MemoryConnectorConfig;
    redis?: RedisConnectorConfig;
    dynamodb?: DynamoDBConnectorConfig;
    postgresql?: PostgreSQLConnectorConfig;
}
export interface MemoryConnectorConfig {
    maxItems: number;
    maxTotalSize: string;
    emitMetrics?: boolean;
}
export interface MockConnectorConfig {
    memoryconnectorconfig: MemoryConnectorConfig;
    getdelay: number;
    setdelay: number;
    geterrorrate: number;
    seterrorrate: number;
}
export interface TLSConfig {
    enabled: boolean;
    certFile: string;
    keyFile: string;
    caFile?: string;
    insecureSkipVerify?: boolean;
}
export interface RedisConnectorConfig {
    addr?: string;
    username?: string;
    db?: number;
    tls?: TLSConfig;
    connPoolSize?: number;
    uri: string;
    initTimeout?: Duration;
    getTimeout?: Duration;
    setTimeout?: Duration;
    lockRetryInterval?: Duration;
}
export interface DynamoDBConnectorConfig {
    table?: string;
    region?: string;
    endpoint?: string;
    auth?: AwsAuthConfig;
    partitionKeyName?: string;
    rangeKeyName?: string;
    reverseIndexName?: string;
    ttlAttributeName?: string;
    initTimeout?: Duration;
    getTimeout?: Duration;
    setTimeout?: Duration;
    maxRetries?: number;
    statePollInterval?: Duration;
    lockRetryInterval?: Duration;
}
export interface PostgreSQLConnectorConfig {
    connectionUri: string;
    table: string;
    minConns?: number;
    maxConns?: number;
    initTimeout?: Duration;
    getTimeout?: Duration;
    setTimeout?: Duration;
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
    scoreMetricsWindowSize?: Duration;
    healthCheck?: DeprecatedProjectHealthCheckConfig;
}
export interface NetworkDefaults {
    rateLimitBudget?: string;
    failsafe?: (FailsafeConfig | undefined)[];
    selectionPolicy?: SelectionPolicyConfig;
    directiveDefaults?: DirectiveDefaultsConfig;
    evm?: TsEvmNetworkConfigForDefaults;
}
/**
 * Define a type alias to avoid recursion
 */
/**
 * If that fails, try the old format with single failsafe object
 */
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
    ignoreNetworks?: string[];
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
    endpoint?: string;
    evm?: EvmUpstreamConfig;
    jsonRpc?: JsonRpcUpstreamConfig;
    ignoreMethods?: string[];
    allowMethods?: string[];
    autoIgnoreUnsupportedMethods?: boolean;
    failsafe?: (FailsafeConfig | undefined)[];
    rateLimitBudget?: string;
    rateLimitAutoTune?: RateLimitAutoTuneConfig;
    routing?: RoutingConfig;
    shadow?: ShadowUpstreamConfig;
}
/**
 * Define a type alias to avoid recursion
 */
/**
 * If that fails, try the old format with single failsafe object
 */
export interface ShadowUpstreamConfig {
    enabled: boolean;
    ignoreFields?: {
        [key: string]: string[];
    };
}
export interface RoutingConfig {
    scoreMultipliers: (ScoreMultiplierConfig | undefined)[];
    scoreLatencyQuantile?: number;
}
export interface ScoreMultiplierConfig {
    network: string;
    method: string;
    overall?: number;
    errorRate?: number;
    respLatency?: number;
    totalRequests?: number;
    throttledRate?: number;
    blockHeadLag?: number;
    finalizationLag?: number;
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
    batchMaxWait?: Duration;
    enableGzip?: boolean;
    headers?: {
        [key: string]: string;
    };
    proxyPool?: string;
}
export interface EvmUpstreamConfig {
    chainId: number;
    nodeType?: EvmNodeType;
    statePollerInterval?: Duration;
    statePollerDebounce?: Duration;
    maxAvailableRecentBlocks?: number;
    getLogsAutoSplittingRangeThreshold?: number;
    getLogsMaxAllowedRange?: number;
    getLogsMaxAllowedAddresses?: number;
    getLogsMaxAllowedTopics?: number;
    getLogsSplitOnError?: boolean;
    skipWhenSyncing?: boolean;
}
export interface FailsafeConfig {
    matchMethod?: string;
    matchFinality?: DataFinalityState[];
    retry?: RetryPolicyConfig;
    circuitBreaker?: CircuitBreakerPolicyConfig;
    timeout?: TimeoutPolicyConfig;
    hedge?: HedgePolicyConfig;
    consensus?: ConsensusPolicyConfig;
}
export interface RetryPolicyConfig {
    maxAttempts: number;
    delay?: Duration;
    backoffMaxDelay?: Duration;
    backoffFactor?: number;
    jitter?: Duration;
    emptyResultConfidence?: AvailbilityConfidence;
    emptyResultIgnore?: string[];
}
export interface CircuitBreakerPolicyConfig {
    failureThresholdCount: number;
    failureThresholdCapacity: number;
    halfOpenAfter?: Duration;
    successThresholdCount: number;
    successThresholdCapacity: number;
}
export interface TimeoutPolicyConfig {
    duration?: Duration;
}
export interface HedgePolicyConfig {
    delay?: Duration;
    maxCount: number;
    quantile?: number;
    minDelay?: Duration;
    maxDelay?: Duration;
}
export type ConsensusLowParticipantsBehavior = string;
export declare const ConsensusLowParticipantsBehaviorReturnError: ConsensusLowParticipantsBehavior;
export declare const ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult: ConsensusLowParticipantsBehavior;
export declare const ConsensusLowParticipantsBehaviorPreferBlockHeadLeader: ConsensusLowParticipantsBehavior;
export declare const ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader: ConsensusLowParticipantsBehavior;
export type ConsensusDisputeBehavior = string;
export declare const ConsensusDisputeBehaviorReturnError: ConsensusDisputeBehavior;
export declare const ConsensusDisputeBehaviorAcceptMostCommonValidResult: ConsensusDisputeBehavior;
export declare const ConsensusDisputeBehaviorPreferBlockHeadLeader: ConsensusDisputeBehavior;
export declare const ConsensusDisputeBehaviorOnlyBlockHeadLeader: ConsensusDisputeBehavior;
export interface ConsensusPolicyConfig {
    requiredParticipants: number;
    agreementThreshold?: number;
    disputeBehavior?: ConsensusDisputeBehavior;
    lowParticipantsBehavior?: ConsensusLowParticipantsBehavior;
    punishMisbehavior?: PunishMisbehaviorConfig;
    disputeLogLevel?: string;
    ignoreFields?: {
        [key: string]: string[];
    };
}
export interface PunishMisbehaviorConfig {
    disputeThreshold: number;
    disputeWindow?: Duration;
    sitOutPenalty?: Duration;
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
export interface DeprecatedProjectHealthCheckConfig {
    scoreMetricsWindowSize: Duration;
}
export interface MethodsConfig {
    preserveDefaultMethods?: boolean;
    definitions?: {
        [key: string]: CacheMethodConfig | undefined;
    };
}
export interface NetworkConfig {
    architecture: TsNetworkArchitecture;
    rateLimitBudget?: string;
    failsafe?: (FailsafeConfig | undefined)[];
    evm?: EvmNetworkConfig;
    selectionPolicy?: SelectionPolicyConfig;
    directiveDefaults?: DirectiveDefaultsConfig;
    alias?: string;
    methods?: MethodsConfig;
}
/**
 * Define a type alias to avoid recursion
 */
/**
 * If that fails, try the old format with single failsafe object
 */
export interface DirectiveDefaultsConfig {
    retryEmpty?: boolean;
    retryPending?: boolean;
    skipCacheRead?: boolean;
    useUpstream?: string;
}
export interface EvmNetworkConfig {
    chainId: number;
    fallbackFinalityDepth?: number;
    fallbackStatePollerDebounce?: Duration;
    integrity?: EvmIntegrityConfig;
}
export interface EvmIntegrityConfig {
    enforceHighestBlock?: boolean;
    enforceGetLogsBlockRange?: boolean;
}
export interface SelectionPolicyConfig {
    evalInterval?: Duration;
    evalFunction?: SelectionPolicyEvalFunction | undefined;
    evalPerMethod?: boolean;
    resampleExcluded?: boolean;
    resampleInterval?: Duration;
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
export type LabelMode = string;
export declare const ErrorLabelModeVerbose: LabelMode;
export declare const ErrorLabelModeCompact: LabelMode;
export interface MetricsConfig {
    enabled?: boolean;
    listenV4?: boolean;
    hostV4?: string;
    listenV6?: boolean;
    hostV6?: string;
    port?: number;
    errorLabelMode?: LabelMode;
    histogramBuckets?: string;
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
//# sourceMappingURL=generated.d.ts.map