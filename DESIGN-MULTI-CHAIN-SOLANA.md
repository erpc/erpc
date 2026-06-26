# SVM Support in eRPC: Architecture Design

**Status:** Draft  
**Authors:** Andre Claro  
**Date:** 2026-04-21

---

## 1. Background & Motivation

eRPC is already deployed and battle-tested for EVM chains. Rather than building a new proxy, this document describes how to extend eRPC to support SVM-compatible chains (Solana first; Fogo, Eclipse, etc. later) through an `ArchitectureHandler` interface pattern.

The design goal is zero behavior change for EVM. All existing EVM logic stays intact. The interface is extracted from the shape of existing coupling points, so the core routing, failsafe, auth, rate-limiting, and cache-backend layers — which are already chain-agnostic — need no changes.

---

## 2. Solana Primer for EVM Engineers

| Concept | EVM equivalent | Notes |
|---|---|---|
| Slot | Block number | Monotonically increasing; ~400 ms each |
| Block | Block | A slot may be _skipped_ (no block produced) |
| Commitment level | Finality | `processed` ≈ latest; `confirmed` ≈ safe; `finalized` ≈ finalized |
| Transaction signature | Tx hash | Base-58, 88 chars |
| Account | Contract / EOA | Programs are accounts with `executable=true` |
| Cluster | Network | `mainnet-beta`, `devnet`, `testnet` — not numeric IDs |
| JSON-RPC | JSON-RPC 2.0 | Same wire format, different method namespace |
| WebSocket | WebSocket | Subscription model similar to `eth_subscribe` |

Transport is identical (JSON-RPC 2.0 over HTTP). The differences are in network identification, block/slot references, method names, error codes, finality semantics, and state polling.

---

## 3. Scope

### Phase 1 — MVP (this document)

- HTTP JSON-RPC proxy for `mainnet-beta`, `devnet`, `testnet`
- Network ID format: `svm:<cluster>` (e.g., `svm:mainnet-beta`, `svm:fogo-mainnet`)
- Commitment-level-aware caching and finality
- Slot-number state poller per upstream (`getSlot`, `getHealth`, `getMaxShredInsertSlot` polled concurrently)
- Shred-insert lag detection per upstream — nodes that receive shreds but fail to process them are marked degraded
- Error normalization for common SVM error codes
- Score-based upstream selection and failover (same algorithm as EVM)
- Config structs: `SvmNetworkConfig`, `SvmUpstreamConfig`
- Vendor support: generic HTTPS only (`type: svm`)

### Phase 2+ (out of scope here)

- Vendor-specific adapters: Helius, Alchemy (SVM), QuickNode (SVM), Triton (`thirdparty/` package)
- WebSocket / subscription forwarding — requires significant transport changes
- gRPC query server for SVM
- `getSignaturesForAddress` auto-pagination (see §9 Q2)
- Transaction simulation caching
- Archive node / slot-availability lower-bound detection

---

## 4. Problem: Where EVM Is Hard-Coded Today

There are exactly **8 integration points** in production code where the pipeline calls `architecture/evm` directly:

| # | File | Line | Call | Layer |
|---|------|------|------|-------|
| 1 | `erpc/projects.go` | 258 | `evm.HandleProjectPreForward(...)` | Project, before cache |
| 2 | `erpc/projects.go` | 259, 265 | `evm.HandleNetworkPostForward(...)` | Network, after response |
| 3 | `erpc/networks.go` | 377 | `evm.HandleNetworkPreForward(...)` | Network, after upstream selection |
| 4 | `erpc/networks.go` | 1035 | `evm.HandleUpstreamPreForward(...)` | Upstream, before forward |
| 5 | `erpc/networks.go` | 1036, 1042 | `evm.HandleUpstreamPostForward(...)` | Upstream, after forward |
| 6 | `upstream/upstream.go` | 171 | `evm.NewEvmStatePoller(...)` | Upstream bootstrap |
| 7 | `erpc/init.go` | 63 | `evm.NewEvmJsonRpcCache(...)` | Init, cache setup |
| 8 | `upstream/registry.go` | 139 | `evm.NewJsonRpcErrorExtractor()` | Error classification |

Everything else — failsafe, auth, rate limiting, multiplexing, health scoring, tracing, cache backends — is already chain-agnostic. The blast radius of this change is narrow.

---

## 5. Solution: `ArchitectureHandler` Interface

Introduce one interface in `common/` that captures all 8 coupling points. The EVM package gets a thin wrapper behind this interface — no logic moves. New architectures implement the same interface and plug in without touching the pipeline.

```go
// common/architecture.go  (new file)

// ArchitectureHandler is implemented once per chain architecture (evm, svm, etc.).
// It is resolved from config.Architecture at init time and stored on Network and Upstream.
// All methods must be safe for concurrent use.
type ArchitectureHandler interface {
    // HandleProjectPreForward is called at project level, before cache read and upstream
    // selection. Use for transformations that affect the cache key or that can
    // short-circuit without knowing which upstream will be used.
    // (handled=true, resp, nil) → return resp, skip pipeline.
    // (handled=true, nil, err)  → return err, skip pipeline.
    // (handled=false, nil, nil) → continue normal pipeline.
    HandleProjectPreForward(
        ctx context.Context,
        network Network,
        req *NormalizedRequest,
    ) (handled bool, resp *NormalizedResponse, err error)

    // HandleNetworkPreForward is called after upstream selection but before the
    // failsafe loop. The selected upstreams slice is passed for availability-aware logic
    // (e.g., computing effective thresholds from the live upstream set).
    HandleNetworkPreForward(
        ctx context.Context,
        network Network,
        upstreams []Upstream,
        req *NormalizedRequest,
    ) (handled bool, resp *NormalizedResponse, err error)

    // HandleNetworkPostForward is called after every response (success or error) at
    // the network level, wrapping both the short-circuit and normal paths in projects.go.
    HandleNetworkPostForward(
        ctx context.Context,
        network Network,
        req *NormalizedRequest,
        resp *NormalizedResponse,
        err error,
    ) (*NormalizedResponse, error)

    // HandleUpstreamPreForward is called per upstream, immediately before the
    // HTTP/gRPC call. skipCacheRead mirrors the directive on the request.
    HandleUpstreamPreForward(
        ctx context.Context,
        network Network,
        upstream Upstream,
        req *NormalizedRequest,
        skipCacheRead bool,
    ) (handled bool, resp *NormalizedResponse, err error)

    // HandleUpstreamPostForward is called per upstream, immediately after the
    // HTTP/gRPC call, including when err != nil.
    HandleUpstreamPostForward(
        ctx context.Context,
        network Network,
        upstream Upstream,
        req *NormalizedRequest,
        resp *NormalizedResponse,
        err error,
        skipCacheRead bool,
    ) (*NormalizedResponse, error)

    // NewStatePoller returns a StatePoller for the given upstream, started in the
    // background by Bootstrap(). Return nil if polling is not needed.
    NewStatePoller(
        projectId string,
        appCtx context.Context,
        logger *zerolog.Logger,
        upstream Upstream,
        tracker HealthTracker,
        sharedState data.SharedStateRegistry,
    ) StatePoller

    // NewCacheDAL returns the cache data-access layer for this architecture.
    // Called once per project at init time.
    NewCacheDAL(
        ctx context.Context,
        logger *zerolog.Logger,
        cfg *CacheConfig,
    ) (CacheDAL, error)

    // NewJsonRpcErrorExtractor returns an error classifier that maps provider-specific
    // error codes to eRPC's internal error codes.
    NewJsonRpcErrorExtractor() JsonRpcErrorExtractor
}

// StatePoller is the minimal interface the upstream bootstrap depends on.
// Architecture-specific pollers (EvmStatePoller, SvmStatePoller) extend this.
type StatePoller interface {
    Bootstrap(ctx context.Context) error
    IsObjectNull() bool
}

// ArchitectureRegistry maps architecture names to their handlers.
// Populated at program start via init() in each architecture package.
var ArchitectureRegistry = map[NetworkArchitecture]ArchitectureHandler{}

func RegisterArchitecture(name NetworkArchitecture, h ArchitectureHandler) {
    ArchitectureRegistry[name] = h
}

func GetArchitectureHandler(arch NetworkArchitecture) (ArchitectureHandler, error) {
    h, ok := ArchitectureRegistry[arch]
    if !ok {
        return nil, NewErrUnknownNetworkArchitecture(arch)
    }
    return h, nil
}
```

`EvmStatePoller` already satisfies `StatePoller` — it already has both `Bootstrap` and `IsObjectNull`.

---

## 6. Integration: What Changes in Each File

### 6.1 `architecture/evm/handler.go` — new file, wraps existing functions

```go
package evm

func init() {
    common.RegisterArchitecture(common.ArchitectureEvm, &EvmArchitectureHandler{})
}

type EvmArchitectureHandler struct{}

// Every method delegates to the existing package-level function — zero logic moves.
func (h *EvmArchitectureHandler) HandleProjectPreForward(ctx context.Context, network common.Network, req *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
    return HandleProjectPreForward(ctx, network, req)
}
func (h *EvmArchitectureHandler) HandleNetworkPreForward(ctx context.Context, network common.Network, upstreams []common.Upstream, req *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
    return HandleNetworkPreForward(ctx, network, upstreams, req)
}
func (h *EvmArchitectureHandler) HandleNetworkPostForward(ctx context.Context, network common.Network, req *common.NormalizedRequest, resp *common.NormalizedResponse, err error) (*common.NormalizedResponse, error) {
    return HandleNetworkPostForward(ctx, network, req, resp, err)
}
func (h *EvmArchitectureHandler) HandleUpstreamPreForward(ctx context.Context, network common.Network, upstream common.Upstream, req *common.NormalizedRequest, skipCacheRead bool) (bool, *common.NormalizedResponse, error) {
    return HandleUpstreamPreForward(ctx, network, upstream, req, skipCacheRead)
}
func (h *EvmArchitectureHandler) HandleUpstreamPostForward(ctx context.Context, network common.Network, upstream common.Upstream, req *common.NormalizedRequest, resp *common.NormalizedResponse, err error, skipCacheRead bool) (*common.NormalizedResponse, error) {
    return HandleUpstreamPostForward(ctx, network, upstream, req, resp, err, skipCacheRead)
}
func (h *EvmArchitectureHandler) NewStatePoller(projectId string, appCtx context.Context, logger *zerolog.Logger, upstream common.Upstream, tracker common.HealthTracker, sharedState data.SharedStateRegistry) common.StatePoller {
    return NewEvmStatePoller(projectId, appCtx, logger, upstream, tracker, sharedState)
}
func (h *EvmArchitectureHandler) NewCacheDAL(ctx context.Context, logger *zerolog.Logger, cfg *common.CacheConfig) (common.CacheDAL, error) {
    return NewEvmJsonRpcCache(ctx, logger, cfg)
}
func (h *EvmArchitectureHandler) NewJsonRpcErrorExtractor() common.JsonRpcErrorExtractor {
    return NewJsonRpcErrorExtractor()
}
```

### 6.2 `erpc/projects.go` — 3-line change

```go
// Before (lines 258-265):
if handled, resp, err := evm.HandleProjectPreForward(ctx, network, nq); handled {
    return evm.HandleNetworkPostForward(ctx, network, nq, resp, err)
}
return evm.HandleNetworkPostForward(ctx, network, nq, resp, err)

// After:
h := network.ArchitectureHandler()
if handled, resp, err := h.HandleProjectPreForward(ctx, network, nq); handled {
    return h.HandleNetworkPostForward(ctx, network, nq, resp, err)
}
return h.HandleNetworkPostForward(ctx, network, nq, resp, err)
```

Remove the `architecture/evm` import from `projects.go`.

### 6.3 `erpc/networks.go` — 3 call-sites, same pattern

```go
// Line 377:
// Before:  evm.HandleNetworkPreForward(ctx, n, upsList, req)
// After:   n.architectureHandler.HandleNetworkPreForward(ctx, n, upsList, req)

// Lines 1035-1042:
// Before:
if handled, resp, err := evm.HandleUpstreamPreForward(execSpanCtx, n, u, req, skipCacheRead); handled {
    return evm.HandleUpstreamPostForward(execSpanCtx, n, u, req, resp, err, skipCacheRead)
}
return evm.HandleUpstreamPostForward(execSpanCtx, n, u, req, resp, err, skipCacheRead)

// After:
h := n.architectureHandler
if handled, resp, err := h.HandleUpstreamPreForward(execSpanCtx, n, u, req, skipCacheRead); handled {
    return h.HandleUpstreamPostForward(execSpanCtx, n, u, req, resp, err, skipCacheRead)
}
return h.HandleUpstreamPostForward(execSpanCtx, n, u, req, resp, err, skipCacheRead)
```

Add `architectureHandler common.ArchitectureHandler` to the `Network` struct and populate it in the networks registry from `cfg.Architecture`.

### 6.4 `upstream/upstream.go` — state poller generalization

```go
// Before (line 171):
if u.config.Type == common.UpstreamTypeEvm {
    u.evmStatePoller = evm.NewEvmStatePoller(u.ProjectId, u.appCtx, u.logger, u, u.metricsTracker, u.sharedStateRegistry)
}

// After:
if h, err := common.GetArchitectureHandler(common.NetworkArchitecture(u.config.Type)); err == nil {
    if sp := h.NewStatePoller(u.ProjectId, u.appCtx, u.logger, u, u.metricsTracker, u.sharedStateRegistry); sp != nil {
        u.statePoller = sp
        if err := sp.Bootstrap(ctx); err != nil {
            u.logger.Error().Err(err).Msg("failed on initial bootstrap of state poller (will retry in background)")
        }
    }
}
```

Replace the `evmStatePoller common.EvmStatePoller` field with `statePoller common.StatePoller`. Preserve the existing `EvmStatePoller()` accessor via a type assertion — all EVM callers already guard against nil:

```go
func (u *Upstream) EvmStatePoller() common.EvmStatePoller {
    sp, _ := u.statePoller.(common.EvmStatePoller)
    return sp // nil for non-EVM upstreams; all callers already guard on nil
}
```

> **Q1 — `type` vs `architecture` — Decided:** Keep separate: `type` (e.g., `svm`, `svm+helius` in Phase 2) controls vendor client construction; `architecture` (`svm`) controls handler. Both use `svm` as the prefix — validation rejects configs where the `type` prefix doesn't match the network `architecture`.

### 6.5 `upstream/registry.go` — composite error extractor

`NewClientRegistry` currently takes a single `JsonRpcErrorExtractor` for the whole project. A project with both EVM and SVM networks needs per-architecture extraction.

**Decision: composite extractor.** Build a `CompositeJsonRpcErrorExtractor` that tries each registered architecture's extractor in order, returning the first non-nil classification. Composite is sufficient because error shapes don't overlap across architectures.

```go
// upstream/composite_error_extractor.go  (new file)
type CompositeJsonRpcErrorExtractor struct {
    extractors []common.JsonRpcErrorExtractor
}
func (c *CompositeJsonRpcErrorExtractor) Extract(resp *common.NormalizedResponse) error {
    for _, e := range c.extractors {
        if err := e.Extract(resp); err != nil {
            return err
        }
    }
    return nil
}
```

`init.go` builds the composite from all registered architectures present in the project config.

### 6.6 `erpc/init.go` — per-architecture cache DAL

```go
// Before (line 63):
evmJsonRpcCache, err = evm.NewEvmJsonRpcCache(appCtx, &logger, cfg.Database.EvmJsonRpcCache)

// After: build one CacheDAL per architecture in the project, wrap in a CompositeCache.
```

Add `erpc/composite_cache.go`:

```go
type CompositeCache struct {
    handlers map[string]common.CacheDAL // keyed by architecture prefix: "evm", "svm"
}

func (c *CompositeCache) Get(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
    if dal, ok := c.handlers[architectureFromNetworkId(req.NetworkId())]; ok {
        return dal.Get(ctx, req)
    }
    return nil, nil
}

func (c *CompositeCache) Set(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) error {
    if dal, ok := c.handlers[architectureFromNetworkId(req.NetworkId())]; ok {
        return dal.Set(ctx, req, resp)
    }
    return nil
}
```

EVM uses its existing `EvmJsonRpcCache`; SVM uses `SvmJsonRpcCache`. Zero behavior change for EVM.

---

## 7. `common/network.go` — Architecture Constants and Network Interface

```go
const (
    ArchitectureEvm NetworkArchitecture = "evm" // existing
    ArchitectureSvm NetworkArchitecture = "svm" // new — covers Solana, Fogo, Eclipse, and any SVM-compatible chain
)

func IsValidArchitecture(a string) bool {
    switch a {
    case string(ArchitectureEvm), string(ArchitectureSvm):
        return true
    }
    return false
}

func IsValidNetwork(network string) bool {
    if strings.HasPrefix(network, "evm:") { ... }  // unchanged
    if strings.HasPrefix(network, "svm:") {
        return IsValidSvmCluster(strings.TrimPrefix(network, "svm:"))
    }
    return false
}

// knownSvmClusters maps cluster name → immutable genesis hash.
// Genesis hashes are the hash of block 0 and never change.
// Known clusters are always validated at upstream bootstrap without an extra RPC call.
// Add new SVM-compatible chain clusters here as they are onboarded.
var knownSvmClusters = map[string]string{
    "mainnet-beta": "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d", // Solana mainnet
    "devnet":       "EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG",  // Solana devnet
    "testnet":      "4uhcVJyU9pJkvQyS88uRDiswHXSCkY3zQawwpjk2NsNY",  // Solana testnet
    "fogo-mainnet": "",                                                // TODO: add Fogo genesis hash on onboarding
}

func IsValidSvmCluster(cluster string) bool {
    _, ok := knownSvmClusters[cluster]
    return ok
}

// KnownGenesisHash returns the hardcoded genesis hash for a cluster, or "" if unknown.
func KnownGenesisHash(cluster string) string {
    return knownSvmClusters[cluster]
}
```

The `Network` interface currently has three EVM-specific methods (`EvmHighestLatestBlockNumber`, `EvmHighestFinalizedBlockNumber`, `EvmLeaderUpstream`). These must stay on the interface to avoid breaking EVM callers in the short term.

> **Q3 — EVM stubs on `Network` interface — Decided:** The `ArchitectureHandler` approach avoids ever creating a `SvmNetwork` type, so the stubs problem never arises — the single `Network` struct in `erpc/networks.go` is used for all architectures. EVM-specific callers already guard on `Architecture() == ArchitectureEvm`. No change needed in Phase 1. Extract an `EvmNetwork` sub-interface in a follow-up once it is clear which callers need to be updated.

---

## 8. `common/config.go` — Config Structs

Add `SvmNetworkConfig` and `SvmUpstreamConfig` alongside the existing EVM equivalents:

```go
type NetworkConfig struct {
    // ... existing fields
    Evm *EvmNetworkConfig `yaml:"evm,omitempty" json:"evm"`
    Svm *SvmNetworkConfig `yaml:"svm,omitempty" json:"svm"`
}

type SvmNetworkConfig struct {
    // Default commitment applied to requests that omit it. One of:
    // "processed", "confirmed", "finalized". Default: "confirmed".
    Commitment string `yaml:"commitment,omitempty" json:"commitment"`

    // Slot polling interval. Default: 500ms (one Solana slot).
    StatePollerInterval *Duration `yaml:"statePollerInterval,omitempty" json:"statePollerInterval"`

    // Analogous to EVM's getLogs maxBlockRange. Default: 1000.
    MaxSlotsPerSignaturesQuery int64 `yaml:"maxSlotsPerSignaturesQuery,omitempty" json:"maxSlotsPerSignaturesQuery"`

    SlotAvailability *SvmSlotAvailabilityConfig `yaml:"slotAvailability,omitempty" json:"slotAvailability"`
}

type SvmSlotAvailabilityConfig struct {
    Lower *SvmSlotRef `yaml:"lower,omitempty" json:"lower"`
}

type SvmSlotRef struct {
    LatestSlotMinus *int64 `yaml:"latestSlotMinus,omitempty" json:"latestSlotMinus"`
    Absolute        *int64 `yaml:"absolute,omitempty" json:"absolute"`
}

type UpstreamConfig struct {
    // ... existing fields
    Evm *EvmUpstreamConfig `yaml:"evm,omitempty" json:"evm"`
    Svm *SvmUpstreamConfig `yaml:"svm,omitempty" json:"svm"`
}

type SvmUpstreamConfig struct {
    // Cluster this upstream serves (e.g., "mainnet-beta", "devnet", "fogo-mainnet").
    // For known clusters, genesis hash is validated at bootstrap against the hardcoded
    // table — no RPC call needed. For unknown clusters, set CheckGenesisHash: true to
    // validate via getGenesisHash on bootstrap.
    Cluster          string                     `yaml:"cluster,omitempty"          json:"cluster"`
    CheckGenesisHash bool                       `yaml:"checkGenesisHash,omitempty" json:"checkGenesisHash"`
    SlotAvailability *SvmSlotAvailabilityConfig `yaml:"slotAvailability,omitempty" json:"slotAvailability"`
}
```

---

## 9. SVM Architecture Handler

### 9.1 Package Structure

```
architecture/svm/
  handler.go           — ArchitectureHandler implementation + init() registration
  hooks.go             — HandleProjectPreForward, HandleNetwork*, HandleUpstream*
  svm_state_poller.go  — slot/epoch poller (implements common.StatePoller)
  json_rpc_cache.go    — commitment-aware cache (implements common.CacheDAL)
  error_normalizer.go  — SVM error code → eRPC error code mapping
  commitment.go        — commitment string → DataFinalityState + TTL defaults
  getSlot.go           — handler for getSlot
  getBlock.go          — handler for getBlock
  sendTransaction.go   — handler (never cached; idempotency notes)
  getTransaction.go    — handler for getTransaction
  getAccountInfo.go    — handler for getAccountInfo
  common.go            — shared constants (commitment level type, cluster map)
```

### 9.2 State Poller: Slot Tracking

Mirrors `EvmStatePoller` in structure. Fans out **four RPC calls concurrently** per poll tick (~400 ms):

```
getHealth                           → IsHealthy()           (node health)
getSlot {"commitment":"processed"}  → LatestSlot()          (≈ EVM latest block)
getSlot {"commitment":"finalized"}  → FinalizedSlot()       (≈ EVM finalized block)
getMaxShredInsertSlot               → shred-insert lag      (silent stale detection)
```

Concurrency eliminates 3× RTT per tick vs. sequential polling. Request bodies are pre-computed static byte slices — no `fmt.Sprintf` allocation per tick:

```go
// architecture/svm/svm_state_poller.go
var (
    reqGetHealth             = []byte(`{"jsonrpc":"2.0","id":1,"method":"getHealth","params":[]}`)
    reqGetSlotProcessed      = []byte(`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"processed"}]}`)
    reqGetSlotFinalized      = []byte(`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"finalized"}]}`)
    reqGetMaxShredInsertSlot = []byte(`{"jsonrpc":"2.0","id":1,"method":"getMaxShredInsertSlot","params":[]}`)
)
```

**Shred-insert lag** (`maxShredInsertSlot − processedSlot`) detects nodes that receive block shreds from the network but fail to process them. These nodes pass `getHealth` but serve stale state. A lag exceeding `MaxShredInsertSlotLagThreshold` (100 slots) marks the upstream degraded.

Slot state is stored via `common.SlotSharedVariable` (a narrow interface on `data.CounterInt64SharedVariable`) keyed by `UniqueUpstreamKey`, so horizontal replicas converge without extra polling. Shared state keys use the architecture prefix:

```
svm/latestSlot/<upstreamKey>
svm/finalizedSlot/<upstreamKey>
```

```go
// common/architecture_svm.go  (new file, mirrors architecture_evm.go)
type SvmStatePoller interface {
    Bootstrap(ctx context.Context) error
    IsObjectNull() bool
    Poll(ctx context.Context)
    LatestSlot() int64
    FinalizedSlot() int64
    MaxShredInsertSlotLag() int64  // maxShredInsertSlot − processedSlot; 0 = unknown
    IsHealthy() bool
    SuggestLatestSlot(slot int64)
    SuggestFinalizedSlot(slot int64)
}

// MaxShredInsertSlotLagThreshold: upstream is degraded above this lag.
const MaxShredInsertSlotLagThreshold int64 = 100
```

> **Q4 — Shared state key naming — Decided:** Use `svm/latestSlot/...` and `svm/finalizedSlot/...`. Architecture-prefixed keys are self-describing in a multi-architecture Redis namespace (e.g. `evm/latestBlock/...` alongside `svm/latestSlot/...`).

### 9.3 Finality Mapping

Lives in `architecture/svm/finality.go`. Method-level tables take **highest priority**; commitment level is the fallback.

```go
// architecture/svm/finality.go

// neverCacheMethods are always ephemeral regardless of commitment.
var neverCacheMethods = map[string]bool{
    // Blockhash — changes every ~400 ms; caching causes tx rejections
    "getLatestBlockhash":          true,
    "getRecentBlockhash":          true, // deprecated alias
    "getFeeForMessage":            true,
    // Transaction submission — side-effecting; must never be cached or deduplicated
    "sendTransaction":             true,
    "sendRawTransaction":          true,
    "simulateTransaction":         true,
    // Real-time statuses — meaningful only for in-flight transactions
    "getSignatureStatuses":        true,
    "getSignatureStatus":          true, // deprecated alias
    // Cluster / validator state — changes every epoch/slot
    "getVoteAccounts":             true,
    "getLeaderSchedule":           true,
    "getEpochInfo":                true,
    "getEpochSchedule":            true,
    "getSlotLeaders":              true,
    "getRecentPerformanceSamples": true,
    "getRecentPrioritizationFees": true,
    // Airdrops — side-effecting
    "requestAirdrop":              true,
}

// alwaysFinalizedMethods are immutable once finalized — cache indefinitely.
var alwaysFinalizedMethods = map[string]bool{
    "getBlock":                true,
    "getTransaction":          true,
    "getConfirmedBlock":       true, // deprecated alias
    "getConfirmedTransaction": true, // deprecated alias
    "getInflationReward":      true,
    "getBlocks":               true,
    "getBlockTime":            true,
    "getSignaturesForAddress": true, // historical sigs don't change once finalized
}

type SvmCommitment string

const (
    CommitmentProcessed SvmCommitment = "processed"
    CommitmentConfirmed SvmCommitment = "confirmed"
    CommitmentFinalized SvmCommitment = "finalized"
)

// GetFinality maps a request to a DataFinalityState for cache decisions.
// Priority: neverCacheMethods > alwaysFinalizedMethods > commitment level.
func GetFinality(ctx context.Context, _ common.Network, req *common.NormalizedRequest, _ *common.NormalizedResponse) common.DataFinalityState {
    method, err := req.Method()
    if err != nil {
        return common.DataFinalityStateUnknown
    }
    if neverCacheMethods[strings.ToLower(method)] {
        return common.DataFinalityStateRealtime
    }
    if alwaysFinalizedMethods[strings.ToLower(method)] {
        return common.DataFinalityStateFinalized
    }
    switch SvmCommitment(strings.ToLower(extractCommitment(req))) {
    case CommitmentProcessed:
        return common.DataFinalityStateRealtime    // no cache
    case CommitmentConfirmed:
        return common.DataFinalityStateUnfinalized // short TTL
    case CommitmentFinalized:
        return common.DataFinalityStateFinalized   // cache forever
    default:
        return common.DataFinalityStateUnfinalized
    }
}
```

Default cache TTLs (added to `common/defaults.go`):

| Rule | TTL |
|---|---|
| `neverCacheMethods` | no cache |
| `alwaysFinalizedMethods` | 0 (forever) |
| commitment `finalized` | 0 (forever) |
| commitment `confirmed` | 3 s |
| commitment `processed` | no cache |

> **Q6 — `DataFinalityStateRealtime` — Decided:** Add in Phase 1. Cache policy for `Realtime` = always skip. EVM never uses it; no regression risk.

### 9.4 Cache Key Strategy

SVM uses slot number derived from the commitment level as the primary cache key dimension:

```go
// architecture/svm/json_rpc_cache.go
func buildCacheKey(req *NormalizedRequest, state *SvmChainState) string {
    commitment := extractCommitment(req)              // from params or network default
    slot       := resolveSlot(req, state, commitment) // finalized/latest slot from poller
    return fmt.Sprintf("%s:%s:%s:%d:%s",
        req.NetworkId(), req.Method(), commitment, slot, paramsHash(req))
}
```

For `finalized` data the slot is immutable — TTL 0. For `confirmed` the slot may advance — TTL 3 s. Methods without a slot dimension (`getHealth`, `getVersion`, `getClusterNodes`) use method-only keys with a fixed TTL.

Cache key format: `svm:<cluster>:<method>:<commitment>:<slot>:<params_hash>`

### 9.5 Hooks

Most hooks return `(false, nil, nil)` (no-op). The meaningful ones:

**`HandleProjectPreForward`**
- `getGenesisHash` — short-circuit with the hardcoded value from `knownSvmClusters` (analogous to EVM's `eth_chainId` short-circuit)
- `getClusterNodes`, `getVersion` — optional short-circuit from config-provided values

**`HandleNetworkPreForward`**
- All methods — inject default commitment from `SvmNetworkConfig.Commitment` if params omit it; set `req.Finality` from the resolved commitment so the failsafe consensus policy activates correctly (see §10)
- `getSignaturesForAddress` — validate slot range; return error if range exceeds `MaxSlotsPerSignaturesQuery` (Phase 2: auto-split)

**`HandleUpstreamPostForward`**
- All responses — extract `context.slot` from the response (present on most read methods); call `SuggestLatestSlot()` on the poller; set `resp.Finality` from the commitment in the original request
- `sendTransaction` / `sendRawTransaction` errors — wrap as `ClientSideException` with `WithRetryableTowardNetwork(false)` to prevent the failsafe retry loop from re-submitting the same transaction to a different upstream (double-spend risk). Note: this requires a prerequisite fix in `upstream/failsafe.go` — the network-scope retry predicate's non-retryable branch was falling through to the default "err != nil → retry" rule without an explicit `return false`.

**`HandleUpstreamPreForward`**
- No-op for Phase 1

### 9.6 Error Normalization

SVM errors use a JSON object in `error.data.err` rather than a numeric code at the top level:

```json
{"code": -32002, "message": "Transaction simulation failed", "data": {"err": "InsufficientFundsForFee"}}
```

Key mappings for `error_normalizer.go`:

| SVM Error | eRPC Code | Retried? | Notes |
|---|---|---|---|
| `-32005` `NodeBehind` | `ErrCodeEndpointServerSideException` | Yes (failover) | Node is behind; try another |
| `-32006` (node behind / not impl) | `ErrCodeEndpointServerSideException` | Yes (failover) | |
| `-32004` (block not available) | `ErrCodeEndpointMissingData` | Yes | |
| `-32007` (slot skipped) | `ErrCodeEndpointMissingData` | Yes | Null result passthrough |
| `-32008` (no snapshot) | `ErrCodeEndpointMissingData` | Yes | |
| `-32009` (slot not available) | `ErrCodeEndpointMissingData` | Yes | |
| `-32015` (block status not available) | `ErrCodeEndpointMissingData` | Yes | |
| `-32016` (min context slot not reached) | `ErrCodeEndpointServerSideException` | Yes (failover) | QuickNode variant |
| `-32601` (method not found) | `ErrCodeEndpointUnsupported` | Yes (failover) | Try upstream that supports it |
| `-32002` (tx already processed) | `ErrCodeEndpointClientSideException` | No | Deterministic; don't retry |
| `-32003` (blockhash not found) | `ErrCodeEndpointClientSideException` | No | Client must re-sign |
| `-32013` (unsupported tx version) | `ErrCodeEndpointClientSideException` | No | |
| `-32000` w/ simulation/blockhash msg | `ErrCodeEndpointClientSideException` | No | Disambiguate by message text |
| `-32000` w/ rate-limit msg | `ErrCodeEndpointCapacityExceeded` | Auto-tunes rate limiter | |
| `-32000` (other) | `ErrCodeEndpointServerSideException` | Yes (failover) | |
| `SendTransactionPreflightFailure` | `ErrCodeEndpointExecutionException` | No | |
| `InsufficientFundsForFee` | `ErrCodeEndpointExecutionException` | No | |
| `TransactionSignatureVerificationFailure` | `ErrCodeEndpointClientSideException` | No | |
| HTTP 429 | `ErrCodeEndpointThrottled` | Auto-tunes rate limiter | |
| HTTP 500/502/503/504 | `ErrCodeEndpointServerSideException` | Yes (failover) | |

**Important:** `ClientSideException` errors for SVM (e.g. `-32002`, `-32003`) must be marked non-retryable **toward the network** via `WithRetryableTowardNetwork(false)` in `HandleUpstreamPostForward`. EVM callers rely on `ClientSideException` being retryable across upstreams (one node may lack a capability); the SVM non-retry guard must be scoped to Solana only.

### 9.7 Genesis Hash Validation

Done in `upstream.go` (same layer as EVM's `eth_chainId` check) during `Bootstrap`. For clusters in `knownSvmClusters` the hash is compared locally — no RPC call. For unknown clusters with `CheckGenesisHash: true`, `getGenesisHash` is called.

The validation must handle several failure modes that would otherwise produce cryptic errors:

```go
// upstream/upstream.go
func (u *Upstream) svmVerifyGenesisHash(ctx context.Context, cluster string) error {
    expectedHash, ok := common.KnownGenesisHash(cluster)
    if !ok {
        return nil // unknown cluster (e.g. localnet) — skip
    }

    resp, err := u.Forward(ctx, genesisHashRequest, true)
    if err != nil {
        // HTTP 401/403 → clear auth diagnostic instead of "empty JSON input"
        if common.HasErrorCode(err, common.ErrCodeEndpointClientSideException) {
            return common.NewErrUpstreamClientInitialization(
                &common.BaseError{Code: "ErrSvmGenesisHashFetchFailed",
                    Cause: fmt.Errorf("HTTP error fetching genesis hash (check auth/endpoint): %w", err)}, u)
        }
        return common.NewErrUpstreamClientInitialization(
            &common.BaseError{Code: "ErrSvmGenesisHashFetchFailed", Cause: err}, u)
    }

    jrr, err := resp.JsonRpcResponse()
    if err != nil {
        return common.NewErrUpstreamClientInitialization(
            &common.BaseError{Code: "ErrSvmGenesisHashParseFailed", Cause: err}, u)
    }

    // Detect non-JSON-RPC bodies (HTML error pages, gateway auth walls).
    if jrr.Error == nil && len(jrr.GetResultBytes()) == 0 {
        return common.NewErrUpstreamClientInitialization(
            &common.BaseError{Code: "ErrSvmGenesisHashFetchFailed",
                Cause: fmt.Errorf("upstream returned non-JSON-RPC body (check auth/endpoint URL)")}, u)
    }

    if jrr.Error != nil {
        return common.NewErrUpstreamClientInitialization(
            &common.BaseError{Code: "ErrSvmGenesisHashRpcError",
                Cause: fmt.Errorf("RPC error %d: %s", jrr.Error.Code, jrr.Error.Message)}, u)
    }

    var genesisHash string
    if err := sonic.Unmarshal(jrr.GetResultBytes(), &genesisHash); err != nil {
        return common.NewErrUpstreamClientInitialization(
            &common.BaseError{Code: "ErrSvmGenesisHashUnmarshalFailed", Cause: err}, u)
    }
    if genesisHash != expectedHash {
        return common.NewErrUpstreamClientInitialization(
            &common.BaseError{Code: "ErrSvmClusterMismatch",
                Cause: fmt.Errorf("genesis hash mismatch: got %q, cluster %q expects %q",
                    genesisHash, cluster, expectedHash)}, u)
    }
    return nil
}
```

`ErrUpstreamClientInitialization` must be marked task-fatal so the bootstrap initializer does not retry a permanent mismatch — the network detects all-upstreams-failed immediately rather than blocking for 30 s.

---

## 10. Consensus for SVM Chains

eRPC's consensus engine operates on response hashes and is triggered by `MatchFinality` on the failsafe policy. It is already chain-agnostic. The SVM adapter's job is to set `req.Finality` correctly so the policy activates at the right time.

### Rules

**`sendTransaction` / `sendVersionedTransaction`** — never consensus-eligible. Write methods are already excluded from hedging (`upstream/failsafe.go` skips hedge for non-retryable writes), which also prevents double-send in consensus.

**`confirmed` commitment** — do not use consensus. Upstreams legitimately disagree at the `confirmed` level — two nodes can be at different confirmed slots at the same instant. Consensus would false-positive constantly. The adapter sets `req.Finality = DataFinalityStateUnfinalized`; the failsafe policy must use `MatchFinality: [finalized]` to stay inactive.

**`finalized` commitment** — consensus is valid. All honest nodes agree on finalized data. The adapter sets `req.Finality = DataFinalityStateFinalized`; a network-level consensus policy with `MatchFinality: [finalized]` activates automatically.

### Slot-Lag Pre-Filter

Even for `finalized`, upstreams can disagree if they are at different finalized slots. An upstream at finalized slot 100 and one at finalized slot 110 will return different `getAccountInfo` results for an account that changed between those slots. The EVM analogy is consensus across upstreams at different finalized block numbers.

**Recommendation:** add a pre-filter step in the SVM adapter's `HandleNetworkPreForward` that excludes upstreams whose `FinalizedSlot()` is more than N slots behind the network's highest known finalized slot (default N = 5, configurable via `SvmNetworkConfig`). This is equivalent to the EVM block-lag penalty but applied as a hard exclude rather than a score penalty, specifically for consensus-eligible requests.

```go
// architecture/svm/hooks.go
func filterByFinalizedSlotLag(upstreams []Upstream, maxLag int64, networkFinalizedSlot int64) []Upstream {
    var eligible []Upstream
    for _, u := range upstreams {
        if sp, ok := u.StatePoller().(SvmStatePoller); ok {
            if networkFinalizedSlot-sp.FinalizedSlot() <= maxLag {
                eligible = append(eligible, u)
            }
        }
    }
    return eligible
}
```

> **Q7 — Slot-lag filter — Decided:** Hard exclude for consensus-eligible requests (`finalized` + consensus policy active). Score penalty already handles general lag for all other requests.

This filter is only applied when `commitment == "finalized"` and consensus is enabled on the network.

### Config

No new consensus config is needed — users configure it the same way as EVM:

```yaml
networks:
  - id: svm:mainnet-beta
    architecture: svm
    failsafe:
      - matchMethod: "*"
        matchFinality: [finalized]
        consensus:
          requiredParticipants: 2
          agreementThreshold: 100      # 100% agreement required for finalized data
```

---

## 11. Vendor Support (Phase 2)

Vendor-specific adapters (Helius, Alchemy, QuickNode, Triton) are deferred to Phase 2. Phase 1 uses plain HTTPS endpoints with `type: svm`.

Phase 2 will follow the same pattern as `thirdparty/alchemy.go` — implement `SupportsNetwork` checking `strings.HasPrefix(networkId, "svm:")` and `GenerateConfigs` building the vendor URL from the cluster name. Register in `thirdparty/vendors_registry.go`.

> **Q5 — Network ID format + genesis hash validation — Decided:** Use `svm:<cluster>` (e.g., `svm:mainnet-beta`, `svm:fogo-mainnet`), mirroring EVM's `evm:<chainId>` pattern. Genesis hashes for well-known clusters are hardcoded in `knownSvmClusters` — bootstrap validates automatically with no RPC call. For unknown clusters, `CheckGenesisHash: true` triggers a `getGenesisHash` call at bootstrap.

---

## 12. Configuration Example

```yaml
projects:
  - id: my-project
    auth:
      strategies:
        - type: secret
          secret: ${PROJECT_SECRET}
    networks:
      - architecture: svm
        id: svm:mainnet-beta
        svm:
          commitment: confirmed
          statePollerInterval: 500ms
          maxSlotsPerSignaturesQuery: 1000
        failsafe:
          - matchMethod: "*"
            timeout:
              duration: 10s
            retry:
              maxAttempts: 3
              delay: 200ms
          - matchMethod: "*"
            matchFinality: [finalized]
            consensus:
              requiredParticipants: 2
              agreementThreshold: 100
        cache:
          policies:
            - methods: ["getAccountInfo", "getBalance", "getTokenAccountBalance", "getBlock"]
              commitment: finalized
              ttl: 0
              connector: redis
            - methods: ["getAccountInfo", "getBalance"]
              commitment: confirmed
              ttl: 3s
              connector: memory

    upstreams:
      - id: helius-mainnet
        endpoint: https://mainnet.helius-rpc.com/?api-key=${HELIUS_KEY}
        type: svm
        svm:
          cluster: mainnet-beta
        failsafe:
          - matchMethod: "*"
            timeout:
              duration: 8s

      - id: alchemy-mainnet
        endpoint: https://solana-mainnet.g.alchemy.com/v2/${ALCHEMY_KEY}
        type: svm
        svm:
          cluster: mainnet-beta
```

---

## 13. What Does Not Change

- **All EVM logic** — `architecture/evm/*.go` untouched
- **Core pipeline** — `erpc/networks.go` routing, failsafe, multiplex, cache-read/write
- **Failsafe policies** — retry, hedge, circuit breaker, consensus are already chain-agnostic
- **Auth / rate limiting** — fully chain-agnostic
- **Health scoring** — block-lag metric becomes slot-lag for SVM; same weighted formula
- **Cache backends** — Redis, PostgreSQL, DynamoDB, Memory connectors untouched
- **Observability** — Prometheus metrics and OpenTelemetry tracing chain-agnostic; add `architecture` label to relevant metrics

---

## 14. Testing Strategy

Following the repo's existing patterns (`util.ResetGock()`, `util.SetupMocksForEvmStatePoller()`):

### Unit tests (per package)

- `architecture/svm/error_normalizer_test.go` — mapping correctness
- `architecture/svm/json_rpc_cache_test.go` — cache key generation, commitment injection, TTL
- `architecture/svm/svm_state_poller_test.go` — polling lifecycle with gock mocks
- `architecture/svm/commitment_test.go` — finality state mapping

### Integration tests

- `erpc/svm_network_test.go` — full stack with gocked SVM upstreams: cache hits, failover, retry, consensus activation on finalized vs confirmed

### Integration test cases (from PR #799)

Minimum coverage required to ship Phase 1:

| Test | What it verifies |
|---|---|
| `TestSvmBasicProxy` | JSON-RPC passthrough |
| `TestSvmFailover` | upstream failover on error |
| `TestSvmStatePoller` | slot tracking via shared state |
| `TestSvmHealthPollerUnhealthy` | `-32005` flips `IsHealthy` to false, upstream deprioritised |
| `TestSvmGenesisHashMismatch` | wrong-cluster upstream rejected at bootstrap |
| `TestSvmCacheFinalityMapping` | finality classification for SVM methods |
| `TestSvmSendTransactionNotRetried` | `sendTransaction` error stops after exactly one upstream (double-spend guard) |
| `TestSvmHighestSlotReflectsMultipleUpstreams` | network-level slot aggregation |
| `TestSvmAndEvmInSameProject` | both architectures coexist in one project |
| `TestSvmFinalizedSlotTracked` | finalized slot populated and < latest slot |
| `TestSvmGetBlock_SkippedSlotNullPassthrough` | null result for skipped slots passes through |
| `TestSvmMinContextSlot_SucceedsOnSecondUpstream` | `-32016` triggers failover |
| `TestSvmSignatureStatuses_NullEntriesPassthrough` | null entries in array responses pass through |
| `TestSvmHTTP500_FailsoverToSecondUpstream` | HTTP 5xx triggers failover |

### Gock helper

```go
// util/test_helpers_svm.go  (new file)
func SetupMocksForSvmStatePoller(upstreamUrl string, latestSlot, finalizedSlot int64) {
    gock.New(upstreamUrl).Post("").
        MatchType("json").
        BodyString(`"getSlot"`).
        Reply(200).JSON(map[string]interface{}{"jsonrpc": "2.0", "result": latestSlot, "id": 1})
    // also mock getHealth and getMaxShredInsertSlot
}
```

**Gock host guard (required for multi-upstream tests):** gock evaluates every registered mock's filter callback _before_ URL matching. In tests with two upstreams, a filter for upstream A fires when upstream B is called — corrupting counter assertions. Every filter function must guard on `r.URL.Host`:

```go
// WRONG — both upstreams trigger this filter
gock.New(sol1Url).Post("").Filter(func(r *http.Request) bool {
    totalCalls++
    return true
})

// CORRECT
gock.New(sol1Url).Post("").Filter(func(r *http.Request) bool {
    if r.URL.Host != sol1Host { return false }
    totalCalls++
    return true
})
```

---

## 15. Implementation Plan

| Phase | Work | Files | Est. |
|---|---|---|---|
| **0 — Interface** | `common/architecture.go`, `architecture/evm/handler.go`, wire handler into `Network`/`Upstream`, replace 8 hard-coded calls, all EVM tests pass. **Migrate the 7 EVM methods currently in `upstream/upstream.go`** (`EvmGetChainId`, `EvmIsBlockFinalized`, `EvmSyncingState`, `EvmLatestBlock`, `EvmFinalizedBlock`, `EvmStatePoller`, `EvmAssertBlockAvailability`) into `architecture/evm/` — they are already flagged `// TODO move to evm package`. Replace `evmStatePoller` field with generic `statePoller common.StatePoller`. **Also fix `upstream/failsafe.go` non-retryable short-circuit** (missing `return false` in network-scope retry predicate). | `common/`, `erpc/projects.go`, `erpc/networks.go`, `erpc/init.go`, `upstream/upstream.go`, `upstream/registry.go`, `upstream/failsafe.go`, `architecture/evm/` | 2 days |
| **1 — Foundation** | `ArchitectureSvm` constant, config structs, validation helpers, composite cache, composite error extractor | `common/network.go`, `common/config.go`, `erpc/composite_cache.go`, `upstream/composite_error_extractor.go` | 1 day |
| **2 — State Poller** | `common/architecture_svm.go`, `architecture/svm/svm_state_poller.go` with concurrent 4-call polling (`getHealth`, `getSlot(processed)`, `getSlot(finalized)`, `getMaxShredInsertSlot`), shred-insert lag detection, bootstrap integration, `svmVerifyGenesisHash` with all edge cases | `architecture/svm/`, `upstream/upstream.go` | 2 days |
| **3 — Error Normalizer** | Full SVM error table (see §9.6), retryability classification, `-32000` message-text disambiguation | `architecture/svm/error_normalizer.go` | 1 day |
| **4 — Cache & Finality** | `neverCacheMethods`/`alwaysFinalizedMethods` tables, commitment injection, cache key design, TTL defaults, `DataFinalityStateRealtime` | `architecture/svm/finality.go`, `architecture/svm/json_rpc_cache.go`, `common/defaults.go` | 1 day |
| **5 — Hooks** | All hook implementations, `getGenesisHash` short-circuit, `sendTransaction` non-retry guard, slot-lag filter for consensus | `architecture/svm/hooks.go`, per-method files | 2 days |
| **6 — Vendors** | _(Phase 2 — deferred)_ Helius, Alchemy, QuickNode, Triton | `thirdparty/helius.go`, etc. | — |
| **7 — Tests** | 14 integration tests (see §14), unit tests, gock helpers with host guard | `*_test.go`, `util/test_helpers_svm.go` | 2 days |
| **8 — Docs & Config** | YAML example, README | — | 0.5 day |
| **Total** | | | ~11.5 days |

---

## 16. Open Questions

| # | Question | Recommendation | Status |
|---|---|---|---|
| **Q1** | `UpstreamConfig.Type` vs `NetworkConfig.Architecture` — unify or keep separate? | Keep separate: `type` (e.g., `svm`, `svm+helius` in Phase 2) controls vendor client construction; `architecture` (`svm`) controls handler. Both use `svm` as the prefix — validation rejects configs where the `type` prefix doesn't match the network `architecture`. | Decided |
| **Q2** | `getSignaturesForAddress` auto-pagination? | Phase 2. Document the 1000-slot limit in Phase 1 config and return an explicit error if exceeded, same as EVM's `getLogs` pre-split guard. | Deferred |
| **Q3** | EVM stubs on `Network` interface — needed for SVM? | No. `ArchitectureHandler` approach uses a single `Network` struct for all architectures; a `SvmNetwork` type is never created. Extract `EvmNetwork` sub-interface in a follow-up. | Decided |
| **Q4** | Rename shared state keys (`latestBlock/` → `statePoller/latest/`)? | Yes, rename in Phase 1. No Redis migration needed — this is a fresh deployment; keys do not exist yet. | Decided |
| **Q5** | Cluster name vs genesis hash as network ID? | Use `svm:<cluster>`. Genesis hashes for known clusters are hardcoded in `knownSvmClusters` — validated at bootstrap with no RPC call. `checkGenesisHash: true` triggers a `getGenesisHash` call only for unknown/custom clusters. | Decided |
| **Q6** | `DataFinalityStateRealtime` for `processed`? | Add it in Phase 1. Cache policy for `Realtime` = always skip. EVM never uses it; no regression risk. | Decided |
| **Q7** | Slot-lag consensus pre-filter — hard exclude or score penalty? | Hard exclude for consensus-eligible requests (`finalized` + consensus policy active). Score penalty already handles general lag for all other requests. | Decided |
| **Q8** | Contribute `ArchitectureHandler` interface upstream to eRPC OSS or maintain a fork? | Propose Phase 0 (interface extraction only) as a PR to eRPC upstream first — it is non-breaking, EVM behavior is unchanged, and the Bitcoin/Aptos stubs in the codebase signal the maintainers intended multi-chain. If rejected, fork. The SVM adapter follows in a second PR. | Pending — needs maintainer conversation |
