export type {
  // Tygo generic replacement
  LogLevel,
  Duration,
  ByteSize,
  NetworkArchitecture,
  ConnectorDriverType,
  ConnectorConfig,
  UpstreamType,
  // Policy evaluation
  PolicyEvalUpstreamMetrics,
  PolicyEvalUpstream,
  SelectionPolicyEvalFunction,
} from "./types";
export {
  // Data finality const exports
  DataFinalityStateUnfinalized,
  DataFinalityStateFinalized,
  DataFinalityStateRealtime,
  DataFinalityStateUnknown,
  // Scope exports
  ScopeNetwork,
  ScopeUpstream,
  // Cache behavior exports
  CacheEmptyBehaviorIgnore,
  CacheEmptyBehaviorAllow,
  CacheEmptyBehaviorOnly,
  // Evm node type
  EvmNodeTypeFull,
  EvmNodeTypeArchive,
  EvmNodeTypeLight,
  // Architecture export
  ArchitectureEvm,
  // Upstream types const exprots
  UpstreamTypeEvm,
  // Auth types
  AuthTypeSecret,
  AuthTypeJwt,
  AuthTypeSiwe,
  AuthTypeNetwork,
} from "./generated";
export type {
  Config,
  ServerConfig,
  AdminConfig,
  DatabaseConfig,
  CacheConfig,
  CachePolicyConfig,
  MemoryConnectorConfig,
  RedisConnectorConfig,
  DynamoDBConnectorConfig,
  PostgreSQLConnectorConfig,
  AwsAuthConfig,
  ProjectConfig,
  CORSConfig,
  UpstreamConfig,
  RoutingConfig,
  ScoreMultiplierConfig,
  RateLimitAutoTuneConfig,
  JsonRpcUpstreamConfig,
  EvmUpstreamConfig,
  FailsafeConfig,
  RetryPolicyConfig,
  CircuitBreakerPolicyConfig,
  TimeoutPolicyConfig,
  HedgePolicyConfig,
  RateLimiterConfig,
  RateLimitBudgetConfig,
  RateLimitRuleConfig,
  HealthCheckConfig,
  NetworkConfig,
  EvmNetworkConfig,
  SelectionPolicyConfig,
  AuthStrategyConfig,
  SecretStrategyConfig,
  JwtStrategyConfig,
  SiweStrategyConfig,
  NetworkStrategyConfig,
  MetricsConfig,
} from "./generated";

import type { Config } from './generated'

export const createConfig = (
  cfg: Config
): Config => {
  return cfg;
};