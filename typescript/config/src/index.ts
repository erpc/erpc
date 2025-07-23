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
  EvmNetworkConfigForDefaults,
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
  EvmNodeTypeUnknown,
  // Evm syncing type
  EvmSyncingStateUnknown,
  EvmSyncingStateSyncing,
  EvmSyncingStateNotSyncing,
  // Architecture export
  ArchitectureEvm,
  // Upstream types const exprots
  UpstreamTypeEvm,
  // Auth types
  AuthTypeSecret,
  AuthTypeJwt,
  AuthTypeSiwe,
  AuthTypeNetwork,
  // Consensus related
  ConsensusLowParticipantsBehaviorReturnError,
  ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
  ConsensusLowParticipantsBehaviorPreferBlockHeadLeader,
  ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader,
  ConsensusDisputeBehaviorReturnError,
  ConsensusDisputeBehaviorAcceptMostCommonValidResult,
  ConsensusDisputeBehaviorPreferBlockHeadLeader,
  ConsensusDisputeBehaviorOnlyBlockHeadLeader,
} from "./generated";
export type {
  Config,
  ProjectConfig,
  HealthCheckConfig,
  // Provider related
  ProviderConfig,
  VendorSettings,
  // Upstream related
  UpstreamConfig,
  EvmUpstreamConfig,
  RoutingConfig,
  ScoreMultiplierConfig,
  RateLimitAutoTuneConfig,
  JsonRpcUpstreamConfig,
  // Failsafe related
  FailsafeConfig,
  RetryPolicyConfig,
  CircuitBreakerPolicyConfig,
  HedgePolicyConfig,
  TimeoutPolicyConfig,
  ConsensusPolicyConfig,
  // Network related
  NetworkConfig,
  EvmNetworkConfig,
  EvmIntegrityConfig,
  SelectionPolicyConfig,
  DirectiveDefaultsConfig,
  // DB related
  DatabaseConfig,
  CacheConfig,
  DataFinalityState,
  CacheEmptyBehavior,
  CachePolicyConfig,
  MemoryConnectorConfig,
  RedisConnectorConfig,
  DynamoDBConnectorConfig,
  AwsAuthConfig,
  PostgreSQLConnectorConfig,
  // Auth related
  AuthStrategyConfig,
  SecretStrategyConfig,
  JwtStrategyConfig,
  SiweStrategyConfig,
  NetworkStrategyConfig,
  // Rate limits related
  RateLimiterConfig,
  RateLimitBudgetConfig,
  RateLimitRuleConfig,
  // Server config related
  ServerConfig,
  CORSConfig,
  MetricsConfig,
  AdminConfig,
  AliasingConfig,
  AliasingRuleConfig,
  TLSConfig,
  // Proxy pools related
  ProxyPoolConfig,
} from "./generated";

import type { Config } from './generated'

export const createConfig = (
  cfg: Config
): Config => {
  return cfg;
};