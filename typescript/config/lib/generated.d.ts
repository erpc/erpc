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
    httpPortV4?: number;
    httpPortV6?: number;
    maxTimeout?: Duration;
    readTimeout?: Duration;
    writeTimeout?: Duration;
    enableGzip?: boolean;
    tls?: TLSConfig;
    aliasing?: AliasingConfig;
    waitBeforeShutdown?: Duration;
    waitAfterShutdown?: Duration;
    includeErrorDetails?: boolean;
    trustedIPForwarders?: string[];
    trustedIPHeaders?: string[];
    responseHeaders?: {
        [key: string]: string;
    };
}
export interface HealthCheckConfig {
    mode?: HealthCheckMode;
    auth?: AuthConfig;
    defaultEval?: string;
}
export type HealthCheckMode = string;
export declare const HealthCheckModeSimple: HealthCheckMode;
export declare const HealthCheckModeNetworks: HealthCheckMode;
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
    serviceName?: string;
    headers?: {
        [key: string]: string;
    };
    tls?: TLSConfig;
    resourceAttributes?: {
        [key: string]: string;
    };
    /**
     * ForceTraceMatchers defines conditions for force-tracing requests.
     * Each matcher can specify network and/or method patterns.
     * Multiple patterns can be separated by "|" (OR within field).
     * Both network and method must match if both are specified (AND between fields).
     * If only one field is specified, only that field is checked.
     */
    forceTraceMatchers?: (ForceTraceMatcher | undefined)[];
}
/**
 * ForceTraceMatcher defines a condition for force-tracing requests.
 */
export interface ForceTraceMatcher {
    /**
     * Network patterns to match (e.g., "evm:1", "evm:1|evm:42161", "evm:*")
     * Multiple patterns separated by "|" act as OR conditions.
     */
    network?: string;
    /**
     * Method patterns to match (e.g., "eth_call", "debug_*|trace_*")
     * Multiple patterns separated by "|" act as OR conditions.
     */
    method?: string;
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
    /**
     * ClusterKey identifies the logical group for shared counters across replicas (multi-tenant friendly)
     */
    clusterKey?: string;
    /**
     * Connector contains the storage driver configuration (redis, postgresql, dynamodb, memory)
     */
    connector?: ConnectorConfig;
    /**
     * FallbackTimeout is the timeout for remote storage operations (get/set/publish).
     * It is a seconds-scale network timeout and NOT a foreground latency budget.
     */
    fallbackTimeout?: Duration;
    /**
     * LockTtl is the expiration for the distributed lock key in the backing store.
     * Should comfortably exceed the expected duration of remote writes.
     */
    lockTtl?: Duration;
    /**
     * LockMaxWait caps how long the foreground path will wait to acquire the lock
     * before proceeding locally and deferring the remote write to background.
     */
    lockMaxWait?: Duration;
    /**
     * UpdateMaxWait caps how long the foreground path will spend computing a new value
     * (e.g., polling latest block) before returning the current local value.
     */
    updateMaxWait?: Duration;
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
    stateful?: boolean;
    /**
     * TranslateLatestTag controls whether the method-level tag translation should convert "latest" to a concrete hex block number.
     * When nil or true, translation is enabled by default.
     */
    translateLatestTag?: boolean;
    /**
     * TranslateFinalizedTag controls whether the method-level tag translation should convert "finalized" to a concrete hex block number.
     * When nil or true, translation is enabled by default.
     */
    translateFinalizedTag?: boolean;
    /**
     * EnforceBlockAvailability controls whether per-upstream block availability bounds (upper/lower)
     * are enforced for this method at the network level. When nil or true, enforcement is enabled.
     */
    enforceBlockAvailability?: boolean;
}
export interface CachePolicyConfig {
    connector: string;
    network?: string;
    method?: string;
    params?: any[];
    finality?: DataFinalityState;
    empty?: CacheEmptyBehavior;
    appliesTo?: 'get' | 'set' | 'both';
    minItemSize?: ByteSize;
    maxItemSize?: ByteSize;
    ttl?: Duration;
}
export type ConnectorDriverType = string;
export declare const DriverMemory: ConnectorDriverType;
export declare const DriverRedis: ConnectorDriverType;
export declare const DriverPostgreSQL: ConnectorDriverType;
export declare const DriverDynamoDB: ConnectorDriverType;
export declare const DriverGrpc: ConnectorDriverType;
export interface ConnectorConfig {
    id?: string;
    driver: TsConnectorDriverType;
    memory?: MemoryConnectorConfig;
    redis?: RedisConnectorConfig;
    dynamodb?: DynamoDBConnectorConfig;
    postgresql?: PostgreSQLConnectorConfig;
    grpc?: GrpcConnectorConfig;
}
export interface GrpcConnectorConfig {
    bootstrap?: string;
    servers?: string[];
    headers?: {
        [key: string]: string;
    };
    getTimeout?: Duration;
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
    scoreRefreshInterval?: Duration;
    /**
     * ScoreMetricsMode controls label cardinality for upstream score metrics for this project.
     * Allowed values:
     * - "compact": emit compact series by setting upstream and category labels to 'n/a'
     * - "detailed": emit full project/vendor/network/upstream/category series
     */
    scoreMetricsMode?: string;
    healthCheck?: DeprecatedProjectHealthCheckConfig;
    /**
     * Configure user agent tracking at the project level
     */
    userAgentMode?: UserAgentTrackingMode;
}
/**
 * UserAgentTrackingMode controls how user agents are recorded for metrics/labels
 */
export type UserAgentTrackingMode = string;
/**
 * UserAgentTrackingModeSimplified lowers cardinality by bucketing common user agents
 */
export declare const UserAgentTrackingModeSimplified: UserAgentTrackingMode;
/**
 * UserAgentTrackingModeRaw records the user agent string as-is (high cardinality)
 */
export declare const UserAgentTrackingModeRaw: UserAgentTrackingMode;
export interface NetworkDefaults {
    rateLimitBudget?: string;
    failsafe?: (FailsafeConfig | undefined)[];
    selectionPolicy?: SelectionPolicyConfig;
    directiveDefaults?: DirectiveDefaultsConfig;
    evm?: TsEvmNetworkConfigForDefaults;
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
export interface ShadowUpstreamConfig {
    enabled: boolean;
    ignoreFields?: {
        [key: string]: string[];
    };
}
export interface UpstreamIntegrityConfig {
    eth_getBlockReceipts?: UpstreamIntegrityEthGetBlockReceiptsConfig;
}
export interface UpstreamIntegrityEthGetBlockReceiptsConfig {
    enabled?: boolean;
    checkLogIndexStrictIncrements?: boolean;
    checkLogsBloom?: boolean;
}
export interface RoutingConfig {
    scoreMultipliers: (ScoreMultiplierConfig | undefined)[];
    scoreLatencyQuantile?: number;
}
export interface ScoreMultiplierConfig {
    network: string;
    method: string;
    finality?: DataFinalityState[];
    overall?: number;
    errorRate?: number;
    respLatency?: number;
    totalRequests?: number;
    throttledRate?: number;
    blockHeadLag?: number;
    finalizationLag?: number;
    misbehaviors?: number;
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
    statePollerInterval?: Duration;
    statePollerDebounce?: Duration;
    blockAvailability?: EvmBlockAvailabilityConfig;
    getLogsAutoSplittingRangeThreshold?: number;
    skipWhenSyncing?: boolean;
    integrity?: UpstreamIntegrityConfig;
    /**
     * @deprecated: use blockAvailability bounds instead; kept for config back-compat only
     */
    nodeType?: EvmNodeType;
    /**
     * @deprecated: should be removed in a future release
     */
    maxAvailableRecentBlocks?: number;
}
/**
 * EvmBlockAvailability defines optional lower/upper block availability expressions for an upstream.
 * Presence of lower/upper implies the feature is active. When both are nil, it's effectively off
 */
export interface EvmBlockAvailabilityConfig {
    lower?: EvmAvailabilityBoundConfig;
    upper?: EvmAvailabilityBoundConfig;
}
/**
 * EvmBound represents a single bound definition.
 * Exactly one of ExactBlock, LatestMinus, EarliestPlus should be set.
 * UpdateRate only applies to earliestBlockPlus bounds: 0 means freeze at first evaluation; >0 means recompute on that cadence.
 * For latestBlockMinus, updateRate is ignored: bounds are computed on-demand using the continuously-updated latest block from evmStatePoller.
 */
export type EvmAvailabilityProbeType = string;
export declare const EvmProbeBlockHeader: EvmAvailabilityProbeType;
export declare const EvmProbeEventLogs: EvmAvailabilityProbeType;
export declare const EvmProbeCallState: EvmAvailabilityProbeType;
export declare const EvmProbeTraceData: EvmAvailabilityProbeType;
export interface EvmAvailabilityBoundConfig {
    exactBlock?: number;
    latestBlockMinus?: number;
    earliestBlockPlus?: number;
    probe?: EvmAvailabilityProbeType;
    updateRate?: Duration;
}
export interface FailsafeConfig {
    matchMethod?: string;
    matchFinality?: DataFinalityState[];
    matchUpstreamGroup?: string;
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
    /**
     * EmptyResultMaxAttempts limits total attempts when retries are triggered due to empty responses.
     */
    emptyResultMaxAttempts?: number;
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
    maxParticipants: number;
    agreementThreshold?: number;
    disputeBehavior?: ConsensusDisputeBehavior;
    lowParticipantsBehavior?: ConsensusLowParticipantsBehavior;
    punishMisbehavior?: PunishMisbehaviorConfig;
    disputeLogLevel?: string;
    ignoreFields?: {
        [key: string]: string[];
    };
    preferNonEmpty?: boolean;
    preferLargerResponses?: boolean;
    misbehaviorsDestination?: MisbehaviorsDestinationConfig;
}
export type MisbehaviorsDestinationType = string;
export declare const MisbehaviorsDestinationTypeFile: MisbehaviorsDestinationType;
export declare const MisbehaviorsDestinationTypeS3: MisbehaviorsDestinationType;
export interface MisbehaviorsDestinationConfig {
    /**
     * Type of destination: "file" or "s3"
     */
    type: 'file' | 's3';
    /**
     * Path for file destination, or S3 URI (s3://bucket/prefix/) for S3 destination
     */
    path: string;
    /**
     * Pattern for generating file names. Supports placeholders:
     * {dateByHour} - formatted as 2006-01-02-15
     * {dateByDay} - formatted as 2006-01-02
     * {method} - the RPC method name
     * {networkId} - the network ID with : replaced by _
     * {instanceId} - unique instance identifier
     */
    filePattern?: string;
    /**
     * S3-specific settings for bulk flushing
     */
    s3?: S3FlushConfig;
}
export interface S3FlushConfig {
    /**
     * Maximum number of records to buffer before flushing (default: 100)
     */
    maxRecords?: number;
    /**
     * Maximum size in bytes to buffer before flushing (default: 1MB)
     */
    maxSize?: number;
    /**
     * Maximum time to wait before flushing buffered records (default: 60s)
     */
    flushInterval?: Duration;
    /**
     * AWS region for S3 bucket (defaults to AWS_REGION env var)
     */
    region?: string;
    /**
     * AWS credentials config (optional). If not specified, uses standard AWS credential chain:
     * 1. Environment variables (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY)
     * 2. IAM role (for EC2/ECS/EKS)
     * 3. Shared credentials file (~/.aws/credentials)
     * Supported modes: "env", "file", "secret"
     */
    credentials?: AwsAuthConfig;
    /**
     * Content type for uploaded files (default: "application/jsonl")
     */
    contentType?: string;
}
export interface PunishMisbehaviorConfig {
    disputeThreshold: number;
    disputeWindow?: Duration;
    sitOutPenalty?: Duration;
}
export interface RateLimiterConfig {
    store?: RateLimitStoreConfig;
    budgets: RateLimitBudgetConfig[];
}
export interface RateLimitBudgetConfig {
    id: string;
    rules: RateLimitRuleConfig[];
}
export interface RateLimitRuleConfig {
    method: string;
    maxCount: number;
    /**
     * Period is the canonical period selector. Supported: second, minute, hour, day, week, month, year
     */
    period: RateLimitPeriod;
    waitTime?: Duration;
    perIP?: boolean;
    perUser?: boolean;
    perNetwork?: boolean;
}
/**
 * RateLimitPeriod enumerates supported periods for rate limiting.
 * It is an int enum to enable strong typing in TypeScript generation, while
 * marshaling to JSON/YAML as human-readable strings like "second", "minute", etc.
 */
export type RateLimitPeriod = number;
export declare const RateLimitPeriodSecond: RateLimitPeriod;
export declare const RateLimitPeriodMinute: RateLimitPeriod;
export declare const RateLimitPeriodHour: RateLimitPeriod;
export declare const RateLimitPeriodDay: RateLimitPeriod;
export declare const RateLimitPeriodWeek: RateLimitPeriod;
export declare const RateLimitPeriodMonth: RateLimitPeriod;
export declare const RateLimitPeriodYear: RateLimitPeriod;
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
export interface DirectiveDefaultsConfig {
    retryEmpty?: boolean;
    retryPending?: boolean;
    skipCacheRead?: boolean;
    useUpstream?: string;
    skipInterpolation?: boolean;
    /**
     * Validation: Block Integrity
     */
    enforceHighestBlock?: boolean;
    enforceGetLogsBlockRange?: boolean;
    enforceNonNullTaggedBlocks?: boolean;
    /**
     * Validation: Header Field Lengths
     */
    validateHeaderFieldLengths?: boolean;
    /**
     * Validation: Transactions (for eth_getBlockByNumber/Hash with full txs)
     */
    validateTransactionFields?: boolean;
    validateTransactionBlockInfo?: boolean;
    /**
     * Validation: Receipts & Logs
     */
    enforceLogIndexStrictIncrements?: boolean;
    validateTxHashUniqueness?: boolean;
    validateTransactionIndex?: boolean;
    validateLogFields?: boolean;
    /**
     * Validation: Bloom Filter (simplified to 2 checks)
     * ValidateLogsBloomEmptiness: if logs exist, bloom must not be zero; if bloom is non-zero, logs must exist
     */
    validateLogsBloomEmptiness?: boolean;
    /**
     * ValidateLogsBloomMatch: recalculate bloom from logs and verify it matches the provided bloom
     */
    validateLogsBloomMatch?: boolean;
    /**
     * Validation: Receipt-to-Transaction Cross-Validation (requires GroundTruthTransactions in library-mode)
     */
    validateReceiptTransactionMatch?: boolean;
    validateContractCreation?: boolean;
    /**
     * Validation: numeric checks
     */
    receiptsCountExact?: number;
    receiptsCountAtLeast?: number;
    /**
     * Validation: Expected Ground Truths
     */
    validationExpectedBlockHash?: string;
    validationExpectedBlockNumber?: number;
}
export interface EvmNetworkConfig {
    chainId: number;
    fallbackFinalityDepth?: number;
    fallbackStatePollerDebounce?: Duration;
    integrity?: EvmIntegrityConfig;
    getLogsMaxAllowedRange?: number;
    getLogsMaxAllowedAddresses?: number;
    getLogsMaxAllowedTopics?: number;
    getLogsSplitOnError?: boolean;
    getLogsSplitConcurrency?: number;
    /**
     * EnforceBlockAvailability controls whether the network should enforce per-upstream
     * block availability bounds (upper/lower) for methods by default. Method-level config may override.
     * When nil or true, enforcement is enabled.
     */
    enforceBlockAvailability?: boolean;
    /**
     * MaxRetryableBlockDistance controls the maximum block distance for which an upstream
     * block unavailability error is considered retryable. If the requested block is within
     * this distance from the upstream's latest block, the error is retryable (upstream may catch up).
     * If the distance is larger, the error is not retryable (upstream is too far behind).
     * Default: 128 blocks.
     */
    maxRetryableBlockDistance?: number;
    /**
     * MarkEmptyAsErrorMethods lists methods for which an empty/null result from an upstream
     * should be treated as a "missing data" error, triggering retry on other upstreams.
     * This is useful for point-lookups (blocks, transactions, receipts, traces) where an
     * empty result likely means the upstream hasn't indexed that data yet.
     * Default includes common point-lookup methods like eth_getBlockByNumber, eth_getTransactionByHash, etc.
     */
    markEmptyAsErrorMethods?: string[];
}
/**
 * EvmIntegrityConfig is deprecated. Use DirectiveDefaultsConfig for validation settings.
 */
export interface EvmIntegrityConfig {
    /**
     * @deprecated: use DirectiveDefaults.EnforceHighestBlock
     */
    enforceHighestBlock?: boolean;
    /**
     * @deprecated: use DirectiveDefaults.EnforceGetLogsBlockRange
     */
    enforceGetLogsBlockRange?: boolean;
    /**
     * @deprecated: use DirectiveDefaults.EnforceNonNullTaggedBlocks
     */
    enforceNonNullTaggedBlocks?: boolean;
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
export declare const AuthTypeDatabase: AuthType;
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
    database?: DatabaseStrategyConfig;
    jwt?: JwtStrategyConfig;
    siwe?: SiweStrategyConfig;
}
export interface SecretStrategyConfig {
    id: string;
    value: string;
    /**
     * RateLimitBudget, if set, is applied to the authenticated user from this strategy
     */
    rateLimitBudget?: string;
}
export interface DatabaseStrategyConfig {
    connector?: ConnectorConfig;
    cache?: DatabaseStrategyCacheConfig;
    retry?: DatabaseRetryConfig;
    failOpen?: DatabaseFailOpenConfig;
    maxWait?: Duration;
}
export interface DatabaseStrategyCacheConfig {
    ttl?: number;
    maxSize?: number;
    maxCost?: number;
    numCounters?: number;
}
export interface DatabaseRetryConfig {
    maxAttempts?: number;
    baseBackoff?: Duration;
}
export interface DatabaseFailOpenConfig {
    enabled: boolean;
    userId?: string;
    rateLimitBudget?: string;
}
export interface JwtStrategyConfig {
    allowedIssuers: string[];
    allowedAudiences: string[];
    allowedAlgorithms: string[];
    requiredClaims: string[];
    verificationKeys: {
        [key: string]: string;
    };
    /**
     * RateLimitBudgetClaimName is the JWT claim name that, if present,
     * will be used to set the per-user RateLimitBudget override.
     * Defaults to "rlm".
     */
    rateLimitBudgetClaimName?: string;
}
export interface SiweStrategyConfig {
    allowedDomains: string[];
    /**
     * RateLimitBudget, if set, is applied to the authenticated user
     */
    rateLimitBudget?: string;
}
export interface NetworkStrategyConfig {
    allowedIPs: string[];
    allowedCIDRs: string[];
    allowLocalhost: boolean;
    trustedProxies: string[];
    /**
     * RateLimitBudget, if set, is applied to the authenticated user (client IP)
     */
    rateLimitBudget?: string;
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
/**
 * RateLimitStoreConfig defines where rate limit counters are stored
 */
export interface RateLimitStoreConfig {
    driver: string;
    redis?: RedisConnectorConfig;
    cacheKeyPrefix?: string;
    nearLimitRatio?: number;
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
/**
 * CachePolicyAppliesTo controls whether a cache policy applies to get, set, or both operations.
 */
export type CachePolicyAppliesTo = string;
export declare const CachePolicyAppliesToBoth: CachePolicyAppliesTo;
export declare const CachePolicyAppliesToGet: CachePolicyAppliesTo;
export declare const CachePolicyAppliesToSet: CachePolicyAppliesTo;
/**
 * JsonRpcErrorExtractor allows callers to inject architecture-specific
 * JSON-RPC error normalization logic into HTTP clients without creating
 * package import cycles.
 */
export type JsonRpcErrorExtractor = any;
/**
 * JsonRpcErrorExtractorFunc is an adapter to allow normal functions to be used
 * as JsonRpcErrorExtractor implementations.
 * Similar to http.HandlerFunc style adapters.
 */
export type JsonRpcErrorExtractorFunc = any;
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
/**
 * HealthTracker is an interface for tracking upstream health metrics
 */
export type HealthTracker = any;
export type Upstream = any;
export interface User {
    id: string;
    ratelimitbudget: string;
}
//# sourceMappingURL=generated.d.ts.map