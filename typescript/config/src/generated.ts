// Code generated by tygo. DO NOT EDIT.
export * from "./types"
import * as types from "./types"

//////////
// source: config.go

/**
 * Config represents the configuration of the application.
 */
export interface Config {
  logLevel: types.LogLevel;
  server?: ServerConfig;
  admin?: AdminConfig;
  database?: DatabaseConfig;
  projects: (ProjectConfig | undefined)[];
  rateLimiters?: RateLimiterConfig;
  metrics?: MetricsConfig;
}
export interface ServerConfig {
  listenV4?: boolean;
  httpHostV4?: string;
  listenV6?: boolean;
  httpHostV6?: string;
  httpPort?: number /* int */;
  maxTimeout?: string;
  enableGzip?: boolean;
}
export interface AdminConfig {
  auth?: AuthConfig;
  cors?: CORSConfig;
}
export interface DatabaseConfig {
  evmJsonRpcCache?: ConnectorConfig;
}
export type ConnectorDriverType = string;
export const DriverMemory: ConnectorDriverType = "memory";
export const DriverRedis: ConnectorDriverType = "redis";
export const DriverPostgres: ConnectorDriverType = "postgres";
export const DriverDynamoDB: ConnectorDriverType = "dynamodb";
export interface ConnectorConfig {
  driver: types.ConnectorDriverType;
  memory?: MemoryConnectorConfig;
  redis?: RedisConnectorConfig;
  dynamodb?: DynamoDBConnectorConfig;
  postgresql?: PostgreSQLConnectorConfig;
}
export interface MemoryConnectorConfig {
  maxItems: number /* int */;
}
export interface TLSConfig {
  enabled: boolean;
  certFile: string;
  keyFile: string;
  caFile: string;
  insecureSkipVerify: boolean;
}
export interface RedisConnectorConfig {
  addr: string;
  db: number /* int */;
  tls?: TLSConfig;
}
export interface DynamoDBConnectorConfig {
  table: string;
  region: string;
  endpoint: string;
  auth?: AwsAuthConfig;
  partitionKeyName: string;
  rangeKeyName: string;
  reverseIndexName: string;
}
export interface PostgreSQLConnectorConfig {
  connectionUri: string;
  table: string;
}
export interface AwsAuthConfig {
  mode: string; // "file", "env", "secret"
  credentialsFile: string;
  profile: string;
  accessKeyID: string;
  secretAccessKey: string;
}
export interface ProjectConfig {
  id: string;
  auth?: AuthConfig;
  cors?: CORSConfig;
  upstreams: (UpstreamConfig | undefined)[];
  networks?: (NetworkConfig | undefined)[];
  rateLimitBudget?: string;
  healthCheck?: HealthCheckConfig;
}
export interface CORSConfig {
  allowedOrigins: string[];
  allowedMethods: string[];
  allowedHeaders: string[];
  exposedHeaders: string[];
  allowCredentials?: boolean;
  maxAge: number /* int */;
}
export interface UpstreamConfig {
  id?: string;
  type?: UpstreamType;
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
  overall: number /* float64 */;
  errorRate: number /* float64 */;
  p90latency: number /* float64 */;
  totalRequests: number /* float64 */;
  throttledRate: number /* float64 */;
  blockHeadLag: number /* float64 */;
  finalizationLag: number /* float64 */;
}
export type Alias = UpstreamConfig;
export interface RateLimitAutoTuneConfig {
  enabled?: boolean;
  adjustmentPeriod: string;
  errorRateThreshold: number /* float64 */;
  increaseFactor: number /* float64 */;
  decreaseFactor: number /* float64 */;
  minBudget: number /* int */;
  maxBudget: number /* int */;
}
export interface JsonRpcUpstreamConfig {
  supportsBatch?: boolean;
  batchMaxSize?: number /* int */;
  batchMaxWait?: string;
  enableGzip?: boolean;
}
export interface EvmUpstreamConfig {
  chainId: number /* int */;
  nodeType?: EvmNodeType;
  statePollerInterval?: string;
}
export interface FailsafeConfig {
  retry?: RetryPolicyConfig;
  circuitBreaker?: CircuitBreakerPolicyConfig;
  timeout?: TimeoutPolicyConfig;
  hedge?: HedgePolicyConfig;
}
export interface RetryPolicyConfig {
  maxAttempts: number /* int */;
  delay: string;
  backoffMaxDelay: string;
  backoffFactor: number /* float32 */;
  jitter: string;
}
export interface CircuitBreakerPolicyConfig {
  failureThresholdCount: number /* uint */;
  failureThresholdCapacity: number /* uint */;
  halfOpenAfter: string;
  successThresholdCount: number /* uint */;
  successThresholdCapacity: number /* uint */;
}
export interface TimeoutPolicyConfig {
  duration: string;
}
export interface HedgePolicyConfig {
  delay: string;
  maxCount: number /* int */;
}
export interface RateLimiterConfig {
  budgets: (RateLimitBudgetConfig | undefined)[];
}
export interface RateLimitBudgetConfig {
  id: string;
  rules: (RateLimitRuleConfig | undefined)[];
}
export interface RateLimitRuleConfig {
  method: string;
  maxCount: number /* uint */;
  period: string;
  waitTime: string;
}
export interface HealthCheckConfig {
  scoreMetricsWindowSize: string;
}
export interface NetworkConfig {
  architecture: 'evm';
  rateLimitBudget?: string;
  failsafe?: FailsafeConfig;
  evm?: EvmNetworkConfig;
  selectionPolicy?: SelectionPolicyConfig;
}
export interface EvmNetworkConfig {
  chainId: number /* int64 */;
  finalityDepth?: number /* int64 */;
}
export interface SelectionPolicyConfig {
  evalInterval?: types.Duration;
  evalFunction?: types.SelectionPolicyEvalFunction | undefined;
  evalPerMethod?: boolean;
  resampleExcluded?: boolean;
  resampleInterval?: types.Duration;
  resampleCount?: number /* int */;
}
export type AuthType = string;
export const AuthTypeSecret: AuthType = "secret";
export const AuthTypeJwt: AuthType = "jwt";
export const AuthTypeSiwe: AuthType = "siwe";
export const AuthTypeNetwork: AuthType = "network";
export interface AuthConfig {
  strategies: (AuthStrategyConfig | undefined)[];
}
export interface AuthStrategyConfig {
  ignoreMethods?: string[];
  allowMethods?: string[];
  rateLimitBudget?: string;
  type: AuthType;
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
  verificationKeys: { [key: string]: string};
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
  port?: number /* int */;
}

//////////
// source: defaults.go

export const DefaultEvmFinalityDepth = 1024;
export const DefaultPolicyFunction = `
	(upstreams, method) => {
		const defaults = upstreams.filter(u => u.config.group !== 'fallback')
		const fallbacks = upstreams.filter(u => u.config.group === 'fallback')
		
		const maxErrorRate = parseFloat(process.env.ROUTING_POLICY_MAX_ERROR_RATE || '0.7')
		const maxBlockHeadLag = parseFloat(process.env.ROUTING_POLICY_MAX_BLOCK_HEAD_LAG || '10')
		const minHealthyThreshold = parseInt(process.env.ROUTING_POLICY_MIN_HEALTHY_THRESHOLD || '1')
		
		const healthyOnes = defaults.filter(
			u => u.metrics.errorRate < maxErrorRate && u.metrics.blockHeadLag < maxBlockHeadLag
		)
		
		if (healthyOnes.length >= minHealthyThreshold) {
			return healthyOnes
		}

		return [...fallbacks, ...healthyOnes]
	}
`;

//////////
// source: evm.go

export type EvmNodeType = string;
export const EvmNodeTypeFull: EvmNodeType = "full";
export const EvmNodeTypeArchive: EvmNodeType = "archive";
export type EvmStatePoller = any;

//////////
// source: network.go

export type NetworkArchitecture = string;
export const ArchitectureEvm: NetworkArchitecture = "evm";
export type Network = any;

//////////
// source: upstream.go

export type Scope = string;
/**
 * Policies must be created with a "network" in mind,
 * assuming there will be many upstreams e.g. Retry might endup using a different upstream
 */
export const ScopeNetwork: Scope = "network";
/**
 * Policies must be created with one only "upstream" in mind
 * e.g. Retry with be towards the same upstream
 */
export const ScopeUpstream: Scope = "upstream";
export type UpstreamType = string;
export const UpstreamTypeEvm: UpstreamType = "evm";
export const UpstreamTypeEvmAlchemy: UpstreamType = "evm+alchemy";
export const UpstreamTypeEvmDrpc: UpstreamType = "evm+drpc";
export const UpstreamTypeEvmBlastapi: UpstreamType = "evm+blastapi";
export const UpstreamTypeEvmEnvio: UpstreamType = "evm+envio";
export const UpstreamTypeEvmPimlico: UpstreamType = "evm+pimlico";
export const UpstreamTypeEvmThirdweb: UpstreamType = "evm+thirdweb";
export const UpstreamTypeEvmEtherspot: UpstreamType = "evm+etherspot";
export const UpstreamTypeEvmInfura: UpstreamType = "evm+infura";
export type EvmSyncingState = number /* int */;
export const EvmSyncingStateUnknown: EvmSyncingState = 0;
export const EvmSyncingStateSyncing: EvmSyncingState = 1;
export const EvmSyncingStateNotSyncing: EvmSyncingState = 2;
export type Upstream = any;
