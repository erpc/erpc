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
  PolicyEvalUpstreamArray,
  PolicyEvalContext,
  PolicyEvalPredicate,
  EvalContext,
  SelectionPolicyEvalFunction,
  ScoreWeights,
  ScoreBreakdown,
  TagPattern,
  Pattern,
  SortByScoreOptions,
  RemoveByLatencyOptions,
  RemoveByLagOptions,
  KeepHealthyOptions,
  PreferOptions,
  StickyPrimaryOptions,
  ProbeExcludedOptions,
  LatencyDeviationOptions,
  ByFinalityHandlers,
  WhereFilter,
  PolicyEvalArrayCondition,
  IncludeIfTarget,
  EvmNetworkConfigForDefaults,
  SvmNetworkConfigForDefaults,
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
  // Selection-policy evalScope + stickyPrimary scope constants (canonical)
  EvalScopeNetwork,
  EvalScopeNetworkMethod,
  EvalScopeNetworkFinality,
  EvalScopeNetworkMethodFinality,
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
  // Rate limiter periods
  RateLimitPeriodSecond,
  RateLimitPeriodMinute,
  RateLimitPeriodHour,
  RateLimitPeriodDay,
  RateLimitPeriodWeek,
  RateLimitPeriodMonth,
  RateLimitPeriodYear,
} from "./generated";
// Short-name re-exports for selection-policy evalScope / stickyPrimary
// scope (CAPITAL_SNAKE_CASE matching the JS ambient globals installed
// by the policy stdlib) and the finality bit-flags for `when(mask, ...)`.
export {
  NETWORK,
  NETWORK_METHOD,
  NETWORK_FINALITY,
  NETWORK_METHOD_FINALITY,
  REALTIME,
  UNFINALIZED,
  FINALIZED,
  UNKNOWN,
} from "./constants";
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
  EvmQueryShimConfig,
  UpstreamIntegrityConfig,
  UpstreamIntegrityEthGetBlockReceiptsConfig,
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
  EvalScope,
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
