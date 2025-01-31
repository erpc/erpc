"use strict";
var __defProp = Object.defineProperty;
var __getOwnPropDesc = Object.getOwnPropertyDescriptor;
var __getOwnPropNames = Object.getOwnPropertyNames;
var __hasOwnProp = Object.prototype.hasOwnProperty;
var __export = (target, all) => {
  for (var name in all)
    __defProp(target, name, { get: all[name], enumerable: true });
};
var __copyProps = (to, from, except, desc) => {
  if (from && typeof from === "object" || typeof from === "function") {
    for (let key of __getOwnPropNames(from))
      if (!__hasOwnProp.call(to, key) && key !== except)
        __defProp(to, key, { get: () => from[key], enumerable: !(desc = __getOwnPropDesc(from, key)) || desc.enumerable });
  }
  return to;
};
var __toCommonJS = (mod) => __copyProps(__defProp({}, "__esModule", { value: true }), mod);

// src/index.ts
var src_exports = {};
__export(src_exports, {
  ArchitectureEvm: () => ArchitectureEvm,
  AuthTypeJwt: () => AuthTypeJwt,
  AuthTypeNetwork: () => AuthTypeNetwork,
  AuthTypeSecret: () => AuthTypeSecret,
  AuthTypeSiwe: () => AuthTypeSiwe,
  CacheEmptyBehaviorAllow: () => CacheEmptyBehaviorAllow,
  CacheEmptyBehaviorIgnore: () => CacheEmptyBehaviorIgnore,
  CacheEmptyBehaviorOnly: () => CacheEmptyBehaviorOnly,
  ConsensusDisputeBehaviorAcceptAnyValidResult: () => ConsensusDisputeBehaviorAcceptAnyValidResult,
  ConsensusDisputeBehaviorOnlyBlockHeadLeader: () => ConsensusDisputeBehaviorOnlyBlockHeadLeader,
  ConsensusDisputeBehaviorPreferBlockHeadLeader: () => ConsensusDisputeBehaviorPreferBlockHeadLeader,
  ConsensusDisputeBehaviorReturnError: () => ConsensusDisputeBehaviorReturnError,
  ConsensusFailureBehaviorAcceptAnyValidResult: () => ConsensusFailureBehaviorAcceptAnyValidResult,
  ConsensusFailureBehaviorOnlyBlockHeadLeader: () => ConsensusFailureBehaviorOnlyBlockHeadLeader,
  ConsensusFailureBehaviorPreferBlockHeadLeader: () => ConsensusFailureBehaviorPreferBlockHeadLeader,
  ConsensusFailureBehaviorReturnError: () => ConsensusFailureBehaviorReturnError,
  ConsensusLowParticipantsBehaviorAcceptAnyValidResult: () => ConsensusLowParticipantsBehaviorAcceptAnyValidResult,
  ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader: () => ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader,
  ConsensusLowParticipantsBehaviorPreferBlockHeadLeader: () => ConsensusLowParticipantsBehaviorPreferBlockHeadLeader,
  ConsensusLowParticipantsBehaviorReturnError: () => ConsensusLowParticipantsBehaviorReturnError,
  DataFinalityStateFinalized: () => DataFinalityStateFinalized,
  DataFinalityStateRealtime: () => DataFinalityStateRealtime,
  DataFinalityStateUnfinalized: () => DataFinalityStateUnfinalized,
  DataFinalityStateUnknown: () => DataFinalityStateUnknown,
  EvmNodeTypeArchive: () => EvmNodeTypeArchive,
  EvmNodeTypeFull: () => EvmNodeTypeFull,
  EvmNodeTypeLight: () => EvmNodeTypeLight,
  EvmSyncingStateNotSyncing: () => EvmSyncingStateNotSyncing,
  EvmSyncingStateSyncing: () => EvmSyncingStateSyncing,
  EvmSyncingStateUnknown: () => EvmSyncingStateUnknown,
  ScopeNetwork: () => ScopeNetwork,
  ScopeUpstream: () => ScopeUpstream,
  UpstreamTypeEvm: () => UpstreamTypeEvm,
  createConfig: () => createConfig
});
module.exports = __toCommonJS(src_exports);

// src/generated.ts
var UpstreamTypeEvm = "evm";
var EvmNodeTypeFull = "full";
var EvmNodeTypeArchive = "archive";
var EvmNodeTypeLight = "light";
var EvmSyncingStateUnknown = 0;
var EvmSyncingStateSyncing = 1;
var EvmSyncingStateNotSyncing = 2;
var ConsensusFailureBehaviorReturnError = "returnError";
var ConsensusFailureBehaviorAcceptAnyValidResult = "acceptAnyValidResult";
var ConsensusFailureBehaviorPreferBlockHeadLeader = "preferBlockHeadLeader";
var ConsensusFailureBehaviorOnlyBlockHeadLeader = "onlyBlockHeadLeader";
var ConsensusLowParticipantsBehaviorReturnError = "returnError";
var ConsensusLowParticipantsBehaviorAcceptAnyValidResult = "acceptAnyValidResult";
var ConsensusLowParticipantsBehaviorPreferBlockHeadLeader = "preferBlockHeadLeader";
var ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader = "onlyBlockHeadLeader";
var ConsensusDisputeBehaviorReturnError = "returnError";
var ConsensusDisputeBehaviorAcceptAnyValidResult = "acceptAnyValidResult";
var ConsensusDisputeBehaviorPreferBlockHeadLeader = "preferBlockHeadLeader";
var ConsensusDisputeBehaviorOnlyBlockHeadLeader = "onlyBlockHeadLeader";
var AuthTypeSecret = "secret";
var AuthTypeJwt = "jwt";
var AuthTypeSiwe = "siwe";
var AuthTypeNetwork = "network";
var DataFinalityStateFinalized = 0;
var DataFinalityStateUnfinalized = 1;
var DataFinalityStateRealtime = 2;
var DataFinalityStateUnknown = 3;
var CacheEmptyBehaviorIgnore = 0;
var CacheEmptyBehaviorAllow = 1;
var CacheEmptyBehaviorOnly = 2;
var ArchitectureEvm = "evm";
var ScopeNetwork = "network";
var ScopeUpstream = "upstream";

// src/index.ts
var createConfig = (cfg) => {
  return cfg;
};
// Annotate the CommonJS export names for ESM import in node:
0 && (module.exports = {
  ArchitectureEvm,
  AuthTypeJwt,
  AuthTypeNetwork,
  AuthTypeSecret,
  AuthTypeSiwe,
  CacheEmptyBehaviorAllow,
  CacheEmptyBehaviorIgnore,
  CacheEmptyBehaviorOnly,
  ConsensusDisputeBehaviorAcceptAnyValidResult,
  ConsensusDisputeBehaviorOnlyBlockHeadLeader,
  ConsensusDisputeBehaviorPreferBlockHeadLeader,
  ConsensusDisputeBehaviorReturnError,
  ConsensusFailureBehaviorAcceptAnyValidResult,
  ConsensusFailureBehaviorOnlyBlockHeadLeader,
  ConsensusFailureBehaviorPreferBlockHeadLeader,
  ConsensusFailureBehaviorReturnError,
  ConsensusLowParticipantsBehaviorAcceptAnyValidResult,
  ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader,
  ConsensusLowParticipantsBehaviorPreferBlockHeadLeader,
  ConsensusLowParticipantsBehaviorReturnError,
  DataFinalityStateFinalized,
  DataFinalityStateRealtime,
  DataFinalityStateUnfinalized,
  DataFinalityStateUnknown,
  EvmNodeTypeArchive,
  EvmNodeTypeFull,
  EvmNodeTypeLight,
  EvmSyncingStateNotSyncing,
  EvmSyncingStateSyncing,
  EvmSyncingStateUnknown,
  ScopeNetwork,
  ScopeUpstream,
  UpstreamTypeEvm,
  createConfig
});
//# sourceMappingURL=index.js.map
