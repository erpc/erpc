# SVM Support in eRPC: Architecture Design

**Status:** Superseded — historical. See [`DESIGN-MULTI-CHAIN-SOLANA_v0.5.md`](./DESIGN-MULTI-CHAIN-SOLANA_v0.5.md) for the design of record, and the shipped code for the authority.  
**Authors:** Andre Claro  
**Date:** 2026-04-21

> ### Corrections to this document
>
> This is the original Phase-1 proposal, kept for its rationale and decision log. Several
> technical claims below did **not** survive implementation and review. The list is here so a
> reader does not have to diff the doc against the code; each item is also corrected inline at
> the section named.
>
> 1. **`maxSlotsPerSignaturesQuery` was never implemented and has been dropped** — both the
>    guard and the config key (§8 config struct, §9.5, §12 example, §16 Q2). It was modelled on
>    EVM's `getLogs` range cap, but `getSignaturesForAddress` is bounded by *signature* cursors
>    (`before`/`until`) plus a `limit`, not by a slot range, so the key described a limit Solana
>    does not express. An early implementation used `minContextSlot` as a stand-in lower bound
>    on returned history; that reading is wrong — `minContextSlot` is the minimum bank slot at
>    which a request may be **evaluated** (a node-freshness floor), and it never restricts how
>    far back a query looks. The guard was deleted rather than rewritten and no field replaced
>    it. If you are migrating a config that still sets this key, remove it: `LoadConfig` runs
>    with `KnownFields(true)`, so an unknown key is a hard startup error, not a warning.
> 2. **`svm.commitment` has no default** (§8 claimed `"confirmed"`). When unset, nothing is
>    injected and each upstream's own server-side default governs.
> 3. **The poller field is `statePollerDebounce`, default 400 ms** (§8 claimed
>    `statePollerInterval: 500ms`). The ticker is fixed at 400 ms — one slot — and the config
>    value is a *gate* on the fan-out, not the ticker period.
> 4. **Cache finality is not a commitment-level fallback** (§9.3). The shipped rule is
>    per-method: never-cache → realtime; always-finalized → finalized; **slot-pinned**
>    (`getBlock`, `getTransaction`) → finalized only at `commitment: finalized`; **everything
>    else → realtime at every commitment level**. `commitment: finalized` is the latest
>    *rooted* slot, a head that advances every ~400 ms — not an immutability guarantee.
> 5. **The cache key is not commitment-and-poller-derived** (§9.4). Partition key is
>    `<networkId>:<slotRef>` where `slotRef` comes from the request's `minContextSlot` (or
>    `*`); the request key is a case-**preserving** hash, because base58 is case-sensitive.
> 6. **`getSlot` and `getBlockHeight` are different counters and are handled differently**
>    (§9.2 equated slot with EVM block number). Only `getSlot` is corrected to a tip;
>    `getBlockHeight` is passed through untouched.
> 7. **`maxFinalizedSlotLag` defaults to 100 slots, not 5** (§10), and an explicit `0`
>    disables the filter.
> 8. **The non-retryable write set includes `requestAirdrop`** (§9.5 listed only
>    `sendTransaction`/`sendRawTransaction`).
> 9. **Shred-insert lag is `maxShredInsertSlot − processedSlot`** — §9.2 stated this
>    correctly, and the implementation was later fixed to match the doc.
> 10. **The per-method file layout in §9.1 was not adopted.** The shipped package is
>     `hooks.go`, `handler.go`, `finality.go`, `json_rpc_cache.go`, `error_normalizer.go`,
>     `slot_lag.go`, `svm_state_poller.go`, `util.go`.
> 11. **`SvmUpstreamConfig` has no `slotAvailability` field** (§8 presented one as live). The
>     shipped struct is `chain` / `cluster` / `checkGenesisHash`. Operator-declared slot
>     windows were never built; the shipped mechanism is the network-level
>     `enforceBlockAvailability` guard, which compares against the pool's
>     `getMaxShredInsertSlot` frontier instead of declared bounds.
> 12. **The §12 and §10 YAML examples would not have loaded at all.** Independently of the
>     dropped key they used a network `id:` field (networks have none — identity comes from
>     `svm.chain`/`svm.cluster`), a network-level `cache:` block (SVM caching lives at
>     `database.svmJsonRpcCache`), `methods:`/`commitment:` cache-policy keys that do not
>     exist, a string-valued `auth.strategies[].secret`, and `consensus.requiredParticipants:
>     2` — which is a *list of tag quotas*, not a participant count. The count key is
>     `maxParticipants`, and `agreementThreshold` is a **count** (validated `<=
>     maxParticipants`), never a percentage. Both examples were rewritten against the shipped
>     schema and verified with `erpc validate`.
> 13. **Several §9.6 error rows named the wrong condition.** `-32002` is
>     `SendTransactionPreflightFailure` (not "tx already processed"), `-32003` is
>     `TransactionSignatureVerificationFailure` (not "blockhash not found"), `-32006` is
>     `TransactionPrecompileVerificationFailure` (not "node behind"), `-32013` is
>     `TransactionSignatureLenMismatch`, `-32014` is `BlockStatusNotAvailableYet`, and
>     `-32015` is `UnsupportedTransactionVersion`. The shipped normalizer also **preserves**
>     the native code and `error.data` rather than rewriting them, classifies HTTP 401/403
>     ahead of the JSON-RPC body, and treats a synthesized `-32700` as an upstream fault.
> 14. **§10 set `req.Finality` from the commitment level.** It does not — finality is
>     per-method (item 4). Upstream *routing* asks `IsFinalizedCommitment`, which is a
>     deliberately different question from cacheability.

---

## 1. Background & Motivation

eRPC is already deployed and battle-tested for EVM chains. Rather than building a new proxy, this document describes how to extend eRPC to support SVM-compatible chains (Solana first; Fogo, Eclipse, etc. later) through an `ArchitectureHandler` interface pattern.

The design goal is zero behavior change for EVM. All existing EVM logic stays intact. The interface is extracted from the shape of existing coupling points, so the core routing, failsafe, auth, rate-limiting, and cache-backend layers — which are already chain-agnostic — need no changes.

---

## 2. Solana Primer for EVM Engineers

| Concept | EVM equivalent | Notes |
|---|---|---|
| Slot | — (nearest: block number) | Monotonically increasing; ~400 ms each. **Not** a block height: a skipped slot advances the slot counter but not the block height, so `getSlot` and `getBlockHeight` are two different counters and diverge permanently. |
| Block | Block | A slot may be _skipped_ (no block produced) — hence the divergence above |
| Commitment level | Block **tag**, not finality depth | `processed` ≈ `latest`; `confirmed` has no EVM analogue; `finalized` = the latest **rooted** slot. Crucially, `finalized` is a *moving head* advancing every ~400 ms, not EVM's immutability horizon. |
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
- Shred-insert lag detection per upstream — nodes that receive shreds but fail to replay them are cordoned out of rotation
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

// CORRECTED — the shipped struct (common/config.go). See the corrections banner at
// the top of this document; the original proposal is preserved in the comment block
// that follows.
type SvmNetworkConfig struct {
    // Which SVM chain. Empty means "solana". Set it for forks (fogo, eclipse) so
    // NetworkId becomes "svm:<chain>:<cluster>" instead of "svm:<cluster>".
    Chain string `yaml:"chain,omitempty" json:"chain"`

    // Cluster the upstreams of this network serve. Network identity.
    Cluster string `yaml:"cluster,omitempty" json:"cluster"`

    // Default commitment injected into requests that omit one. One of
    // "processed", "confirmed", "finalized". NO DEFAULT: when unset nothing is
    // injected and each upstream's own server-side default governs.
    Commitment string `yaml:"commitment,omitempty" json:"commitment"`

    // Minimum interval between poll fan-outs. Default 400ms (one slot). This is a
    // GATE, not the ticker period — the ticker is pinned at 400ms.
    StatePollerDebounce Duration `yaml:"statePollerDebounce,omitempty" json:"statePollerDebounce"`

    // Slots an upstream's finalized slot may trail the pool reference before it is
    // excluded from consensus voting on finalized data. Pointer for tri-state:
    // nil => 100 (default), explicit 0 => filter disabled, >0 => that value.
    MaxFinalizedSlotLag *int64 `yaml:"maxFinalizedSlotLag,omitempty" json:"maxFinalizedSlotLag,omitempty"`

    // Gates the getBlock/getConfirmedBlock guard that short-circuits slots above
    // the pool's indexed frontier. nil => true.
    EnforceBlockAvailability *bool `yaml:"enforceBlockAvailability,omitempty" json:"enforceBlockAvailability,omitempty"`
}

// ---------------------------------------------------------------------------
// ORIGINAL PROPOSAL (not shipped) — kept for the record:
//
// type SvmNetworkConfig struct {
//     // Default commitment applied to requests that omit it. One of:
//     // "processed", "confirmed", "finalized". Default: "confirmed".
//     //   REVERSED: there is no default. An unset commitment means each upstream
//     //   applies its own server-side default.
//     Commitment string
//
//     // Slot polling interval. Default: 500ms (one Solana slot).
//     //   RENAMED to StatePollerDebounce; default 400ms; semantics changed from
//     //   "ticker period" to "gate on the fan-out".
//     StatePollerInterval *Duration
//
//     // Analogous to EVM's getLogs maxBlockRange. Default: 1000.
//     //   DELETED. getSignaturesForAddress is bounded by signature cursors
//     //   (before/until), not by a slot range, so this key expressed a limit the
//     //   Solana API does not have. The guard that read it used minContextSlot as a
//     //   lower bound on returned history, which is a misreading: minContextSlot is
//     //   the minimum bank slot at which a request may be EVALUATED, and never
//     //   restricts how far back a query looks.
//     MaxSlotsPerSignaturesQuery int64
//
//     // Per-upstream slot windows, mirroring EVM blockAvailability.
//     //   NOT SHIPPED in Phase 1. The shipped mechanism is the network-level
//     //   EnforceBlockAvailability guard above, which compares against the pool's
//     //   getMaxShredInsertSlot frontier rather than operator-declared bounds.
//     SlotAvailability *SvmSlotAvailabilityConfig
// }
//
// type SvmSlotAvailabilityConfig struct {
//     Lower *SvmSlotRef
// }
//
// type SvmSlotRef struct {
//     LatestSlotMinus *int64
//     Absolute        *int64
// }
// ---------------------------------------------------------------------------

type UpstreamConfig struct {
    // ... existing fields
    Evm *EvmUpstreamConfig `yaml:"evm,omitempty" json:"evm"`
    Svm *SvmUpstreamConfig `yaml:"svm,omitempty" json:"svm"`
}

// CORRECTED — the shipped struct. `SlotAvailability` was proposed here but never built
// (corrections item 11), and `Chain` was added for multi-chain SVM support.
type SvmUpstreamConfig struct {
    // Which SVM chain this upstream serves. Must match the network-level Chain.
    // Empty defaults to "solana".
    Chain string `yaml:"chain,omitempty" json:"chain"`

    // Cluster this upstream serves (e.g., "mainnet-beta", "devnet", "mainnet").
    Cluster string `yaml:"cluster,omitempty" json:"cluster"`

    // Opts an UNKNOWN cluster in to getGenesisHash validation at bootstrap. Known
    // clusters are validated regardless of this flag, and both a hash mismatch AND a
    // fetch failure fail the upstream, so a node mis-pointed at the wrong cluster
    // never joins the pool.
    CheckGenesisHash bool `yaml:"checkGenesisHash,omitempty" json:"checkGenesisHash"`
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

**CORRECTED — this layout was not adopted** (corrections item 10). Per-method files proved to
be the wrong cut: the commitment, finality and cache decisions are cross-method tables, not
per-method logic. The shipped package is `handler.go`, `hooks.go`, `finality.go`,
`json_rpc_cache.go`, `error_normalizer.go`, `slot_lag.go`, `svm_state_poller.go`, `util.go` —
and `commitment.go` never existed, its content living in `finality.go` and `hooks.go`.

### 9.2 State Poller: Slot Tracking

Mirrors `EvmStatePoller` in structure. Fans out up to **four RPC calls concurrently** per poll tick (~400 ms). ("Up to" because a later round added traffic-gated polling: when `context.slot` harvested from live responses has kept both slot views fresh inside the debounce window, the two `getSlot` calls are skipped — bounded at 4 consecutive skips, and `getHealth` / `getMaxShredInsertSlot` always run.)

```
getHealth                           → IsHealthy()           (node health)
getSlot {"commitment":"processed"}  → LatestSlot()          (processed tip)
getSlot {"commitment":"finalized"}  → FinalizedSlot()       (latest ROOTED slot)
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

**Shred-insert lag** (`maxShredInsertSlot − processedSlot`) detects nodes that receive block
shreds from the network but fail to replay them. The watermark is structurally ≥ the replayed
slot, so that subtraction *is* the silent-stale detector: shreds keep arriving while replay
stalls, so the watermark runs away from the processed slot while the node still answers
`getHealth` "ok". A lag exceeding `MaxShredInsertSlotLagThreshold` (100 slots) makes
`IsHealthy()` false.

**CORRECTED — "degraded" is not the shipped effect.** `IsHealthy()` needed a production
consumer, so the poller publishes its verdict through `Cordon("*")` / `Uncordon("*")` (the
default selection policy runs `.removeCordoned()`, and `EvmStatePoller.cordonForChainIdMismatch`
is the precedent). It is **edge-triggered**: the calls fire only on a transition, never once per
tick, because re-cordoning every 400 ms would restamp the reason, spawn a fresh
`erpc_upstream_cordoned` gauge series per tick, and reset the cordon-duration observation. A
cold poller cannot cordon — `healthy` starts true and the lag stays 0 until a real
`getMaxShredInsertSlot` sample lands, so "not yet observed" reads as healthy. Unknown is not
unhealthy.

**CORRECTED — slot is not block height** (corrections item 6, and §2's table). A skipped slot
advances the slot counter but not the block height, so the two counters diverge permanently.
Only `getSlot` is tip-corrected, and per commitment: `finalized` against the finalized tip,
`processed` (or unset) against the processed tip, and `confirmed` passed through
**uncorrected**, because no confirmed tip is tracked. `getBlockHeight` is a different counter
and is passed through untouched — correcting it against a slot tip would be a category error.

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

// MaxShredInsertSlotLagThreshold: above this lag the upstream is cordoned out of rotation.
const MaxShredInsertSlotLagThreshold int64 = 100
```

> **Q4 — Shared state key naming — Decided:** Use `svm/latestSlot/...` and `svm/finalizedSlot/...`. Architecture-prefixed keys are self-describing in a multi-architecture Redis namespace (e.g. `evm/latestBlock/...` alongside `svm/latestSlot/...`).

### 9.3 Finality Mapping

Lives in `architecture/svm/finality.go`. **CORRECTED — there is no commitment-level fallback**
(corrections item 4). The shipped rule is per-method; commitment only ever *promotes* a
slot-pinned read.

The load-bearing fact this proposal missed: `commitment: finalized` on Solana is the state at
the latest **rooted** slot, and the rooted slot advances roughly every 400 ms. It is a moving
head, not EVM's immutability horizon. So a read whose answer depends on where the head is —
`getBalance`, `getAccountInfo`, `getProgramAccounts`, `getTokenAccountBalance`, … — answers a
*different question* every slot even at `finalized`, exactly like EVM's `latest` tag (which
`erpc/networks.go` already maps to Realtime). Classifying those `Finalized` would be a
permanent-cache bug: `DataFinalityStateFinalized` is the zero value that a policy with no
explicit `finality` matches, and an unset TTL means "no expiry" in the connectors — so a cached
balance would never be invalidated by a later transfer.

Priority, as shipped:

```go
// architecture/svm/finality.go — GetFinality
//  1. neverCacheMethods      → Realtime  (the cache layer additionally hard-skips these)
//  2. alwaysFinalizedMethods → Finalized (immutable by construction)
//  3. slotPinnedMethods      → Finalized at commitment == finalized,
//                              otherwise Unfinalized (pinned, but fork-droppable)
//  4. everything else        → Realtime  (moving-head read)
```

| Bucket | Members | Result |
|---|---|---|
| `neverCacheMethods` | effectful: `sendTransaction`, `sendRawTransaction`, `simulateTransaction`, `requestAirdrop`; sub-slot snapshots: `getLatestBlockhash`, `getRecentBlockhash`, `getFeeForMessage`, `getSignatureStatuses`, `getVoteAccounts`, `getLeaderSchedule`, `getEpochInfo`, `getSlotLeaders`, `getRecentPerformanceSamples`, `getRecentPrioritizationFees` | `Realtime`, **and** hard-skipped in `Get`/`Set` so a stray `finality: realtime` policy cannot cache an effectful method |
| `alwaysFinalizedMethods` | `getInflationReward` (defined only over finalized epochs), `getBlockTime` (takes no commitment; stable once the slot exists) | `Finalized` |
| `slotPinnedMethods` | `getBlock`, `getTransaction`, plus the deprecated `getConfirmedBlock` / `getConfirmedTransaction` aliases | `Finalized` **only** at commitment `finalized`; `Unfinalized` below it |
| everything else | `getBalance`, `getAccountInfo`, `getProgramAccounts`, `getBlocks`, `getBlocksWithLimit`, `getSignaturesForAddress`, `getEpochSchedule`, … | `Realtime` at **every** commitment level |

Four entries the original tables got wrong:

- **`getBlock` / `getTransaction` are not unconditionally finalized.** They accept a commitment
  parameter and can return *confirmed*, not-yet-rooted data that a minority-fork switch can
  still drop. They are promoted only at `commitment: finalized`.
- **`getSignaturesForAddress` is not slot-pinned at all.** The signature list for an address
  *grows* as new transactions land, so it tracks the head even though it names an address.
- **`getBlocks` / `getBlocksWithLimit` are not slot-pinned.** They are range queries:
  `getBlocks(start)` with no end slot runs to the current head, and either form can name an
  upper bound the chain has not reached, returning a partial list that grows. A
  fully-in-the-past `getBlocks(start, end)` range *is* immutable and knowingly gets only
  realtime-TTL caching — promoting it would require comparing `end` against the poller's
  finalized slot, which would make finality depend on mutable poller state and therefore vary
  over time for an identical request. Not worth it: `getBlocks` is cheap next to `getBlock`.
- **`getEpochSchedule` is not never-cache.** Epoch-schedule constants (`slotsPerEpoch`,
  `leaderScheduleSlotOffset`, …) change only at epoch boundaries (~432,000 slots / ~2 days), so
  it falls through to the moving-head bucket and is cached under the realtime policy's TTL.

`minContextSlot` is **not** a promotion signal either: it is the minimum bank slot at which the
request may be *evaluated*, so `getBalance(pubkey, {minContextSlot: 1})` still answers at the
current head.

Step 3 resolves the commitment with `resolveCommitment` — the *same* predicate the injection
hook uses — so finality reflects the commitment that actually reaches the upstream, not merely
whether a network default exists. When injection legitimately skips a request (legacy
encoding-string form, missing args, non-injectable method), no default reaches the upstream and
the response is `Unfinalized` rather than wrongly trusting the network default. Because the
predicate reads request shape plus config and not mutation state, this holds whether finality is
computed before or after injection.

`IsFinalizedCommitment` is a **separate** predicate, for upstream *routing*: "which slot does
the node evaluate this at". Conflating it with `GetFinality` ("is this response immutable enough
to cache") is the trap to avoid — `getBalance` at `commitment: finalized` is `Realtime` for
caching but finalized for routing.

There is no hardcoded per-commitment TTL table. Operators set TTLs per finality bucket under
`database.svmJsonRpcCache.policies[*]`; on a `realtime` policy the TTL is the *only* staleness
bound, because SVM has no block-timestamp age guard to catch an over-long value.

> **Q6 — `DataFinalityStateRealtime` — Decided:** Add in Phase 1. Cache policy for `Realtime` = always skip. EVM never uses it; no regression risk.

### 9.4 Cache Key Strategy

**CORRECTED — the key is neither commitment- nor poller-derived** (corrections item 5).
`architecture/svm/json_rpc_cache.go` produces two parts, matching the shared `data.Connector`
contract:

```
partition key : <networkId>:<slotRef>
request key   : <method>:<type- and structure-delimited, case-PRESERVING digest of params>
```

- **`slotRef`** is the request's own `minContextSlot` when present, else the literal `*`. It is
  derived from params, never from the poller — so a given `(method, params)` tuple always yields
  the same partition key on `Set` and on `Get`. Deriving it from live poller state would move
  the key under a request every 400 ms and guarantee a permanent miss. Because of that,
  `ConnectorMainIndex` is always the right index for SVM; the reverse-index wildcard fallback
  that EVM needs for its bespoke `blockRef` dimension cannot matter here.
- **The request key is case-PRESERVING.** It deliberately does *not* use the shared
  `req.CacheHash()`, which lowercases string params — right for EVM hex, catastrophic for
  Solana, where base58 pubkeys and signatures are case-**sensitive** and `So111…` ≠ `so111…`.
  Collapsing case would serve one account's data under another's key. The digest is also type-
  and structure-delimited, so `["1"]` and `[1]`, or `[[a],[b]]` and `[a,b]`, cannot collide.
- **`networkId` prefixes the partition key**, so `svm:mainnet-beta`, `svm:fogo:mainnet` and
  `evm:1` can share one Redis or DynamoDB connector without collision — the intended deployment
  for mixed projects.
- **The commitment is not a key dimension.** It reaches the key indirectly, through the params
  the injector has already written: injection runs in `HandleProjectPreForward`, *before* the
  cache read, so `Get` and `Set` see identical post-injection params.

There is no per-commitment TTL rule here either. Cacheability is the finality classification of
§9.3; the TTL is whatever the matching `database.svmJsonRpcCache` policy sets.

### 9.5 Hooks

Most hooks return `(false, nil, nil)` (no-op). The meaningful ones:

**`HandleProjectPreForward`**
- `getGenesisHash` — short-circuit with the hardcoded value from `knownSvmClusters` (analogous to EVM's `eth_chainId` short-circuit)
- **Not shipped:** the proposed optional `getClusterNodes` / `getVersion` short-circuits from config-provided values were never built. `getGenesisHash` is the only short-circuit — and it is skipped when the request carries a skip-cache-read directive, so an operator can always force a real round-trip.

**`HandleNetworkPreForward`**
- All methods — inject the default commitment from `SvmNetworkConfig.Commitment` when params omit one. **CORRECTED — this moved, and it does not touch `req.Finality`.** Injection runs in `HandleProjectPreForward`, *before* the network-layer cache read, so `Get` and `Set` key on identical post-injection params; doing it here left `Get` keyed on pre-injection params and `Set` on post-injection params — a permanent cache miss for every request that relied on the network default. Nor is finality assigned from the resolved commitment: it is computed per-method by `GetFinality` (§9.3, corrections items 4 and 14). What actually remains in `HandleNetworkPreForward` is per-method validation gates plus the consensus slot-lag pre-filter (§10).
- `getSignaturesForAddress` — **no guard.** The proposed slot-range validation was dropped (corrections item 1): Solana paginates this method by *signature* cursors (`before`, `until`) plus a `limit`, so there is no slot range for eRPC to validate. Requests pass through untouched, and clients page by feeding the last signature of a batch back as `before`. Auto-pagination remains a Phase-2 idea (§16 Q2).

**`HandleUpstreamPostForward`**
- All responses — extract `context.slot` from the response (present on most read methods) and feed the upstream's poller through `SuggestLatestSlot()` / `SuggestFinalizedSlot()`, keeping the slot view fresh between ticks. **CORRECTED — `resp.Finality` is not set from the original request's commitment**; response finality is the per-method classification of §9.3.
- Non-retryable write errors — **the set is `sendTransaction`, `sendRawTransaction` *and* `requestAirdrop`** (corrections item 8), centralized in `IsNonRetryableWriteMethod` (`architecture/svm/util.go`) so call sites gate on the helper rather than re-listing names. Errors from these are wrapped as `ClientSideException` with `WithRetryableTowardNetwork(false)` so the failsafe loop cannot re-submit to a second upstream: a resubmitted transaction may double-broadcast once the original propagates through the cluster, and `requestAirdrop` *mints* per call, so a failover after an effective first attempt mints twice. They are excluded from hedging for the same reason. `simulateTransaction` is deliberately **not** in the set — it is read-only and safe to retry or hedge (though it is still never cached). Note: this required a prerequisite fix in `upstream/failsafe.go` — the network-scope retry predicate's non-retryable branch fell through to the default "err != nil → retry" rule without an explicit `return false`.

**`HandleUpstreamPreForward`**
- No-op for Phase 1

### 9.6 Error Normalization

SVM errors carry the actionable half of their payload in `error.data` rather than in the message
string:

```json
{"code": -32002, "message": "Transaction simulation failed",
 "data": {"err": "InsufficientFundsForFee", "logs": ["..."], "unitsConsumed": 0}}
```

**CORRECTED** (corrections item 13). Several rows of the original table named the wrong
condition, and the shipped normalizer's central guarantee is the *opposite* of what that table
implied. The authoritative per-code table now lives in `docs/pages/reference/errors.mdx` →
"SVM (Solana) error contract"; what follows is only what changed relative to this proposal.

**The native code and `error.data` reach the client unchanged.** This is the opposite of the EVM
path's normalize-the-number behavior. Solana clients dispatch on the exact number — `@solana/kit`
maps `-32002`/`-32005`/`-32016`/… to named error classes, and `@solana/web3.js` raises
`SendTransactionError` with populated `.logs` only when it sees `-32002` — so rewriting the code
to an eRPC constant silently breaks them. eRPC's routing verdict lives entirely in the **outer
`StandardError` class** (retryable / capacity / client-vs-server), never in the wire number.

That has a consequence worth stating: `common.JsonRpcErrorNumber` reuses `-32005` for
`CapacityExceeded` and `-32016` for `Unauthorized`, while Solana assigns them to `NodeUnhealthy`
and `MinContextSlotNotReached`. The invariant keeping them apart is that on an SVM path eRPC
never *synthesizes* either number — its own capacity verdict goes out as HTTP 429 with `-32000`,
and its auth verdict as HTTP 401/403 with `-32600`.

Codes this proposal mislabelled (all now map 1:1 to agave's `RpcCustomError`, `-32001`…`-32019`):

| Code | Actual condition | This document said | Shipped class |
|---|---|---|---|
| `-32002` | `SendTransactionPreflightFailure` | "tx already processed" | `ClientSideException`, not retryable |
| `-32003` | `TransactionSignatureVerificationFailure` | "blockhash not found" | `ClientSideException`, not retryable |
| `-32006` | `TransactionPrecompileVerificationFailure` | "node behind / not impl", retryable | `ClientSideException`, **not** retryable |
| `-32013` | `TransactionSignatureLenMismatch` | "unsupported tx version" | `ClientSideException`, not retryable |
| `-32014` | `BlockStatusNotAvailableYet` | absent from the table | `MissingData`, retryable |
| `-32015` | `UnsupportedTransactionVersion` | "block status not available", retryable `MissingData` | `ClientSideException`, **not** retryable |
| `-32016` | `MinContextSlotNotReached` | "QuickNode variant" | `ServerSideException`, retryable — a standard agave code, not a vendor extension |

Three rules this proposal did not have at all:

1. **HTTP 401/403 outrank the JSON-RPC body.** An auth failure is a verdict about the
   credential, not about the RPC call, so it is classified from the status even when a JSON-RPC
   error body is present. Helius and QuickNode return 401/403 *with* an error object;
   classifying from the body first sent expired API keys down whichever generic path their code
   happened to map to and never reached eRPC's unauthorized/billing handling. The codeless
   fallback is deliberately `-32600`, not eRPC's `-32016`, which an SVM client would decode as
   `MinContextSlotNotReached` and answer by retrying against a fresher node forever instead of
   fixing its key.
2. **A synthesized `-32700` is an UPSTREAM fault, not a caller error.** eRPC serializes the
   outbound request itself, so a parse error never means "the caller sent bad JSON" — it means
   eRPC could not parse what *this* upstream sent back (the parse layer synthesizes `-32700` for
   any unparseable body). On a failing HTTP status it is classified from the status; otherwise it
   stays a retryable `ServerSideException`. Grouping it with the caller-side client errors is
   what turned a plaintext or HTML 429 from a CDN in front of a provider into a hard
   non-retryable parse error — no failover, no capacity signal for rate-limit auto-tune, and a
   caller who saw a parse error instead of a rate limit.
3. **`permanent` and `terminal` are separate flags on missing-data errors.** `-32007`
   (`SlotSkipped`) is permanent but still retryable across upstreams — eRPC sweeps every
   provider once and then stops, because waiting cannot un-skip a slot. `-32009`
   (`LongTermStorageSlotSkipped`) is permanent **and** terminal: the node already consulted
   long-term storage, so the verdict is cluster-wide and eRPC does not fail over at all.
   `-32019` (`LongTermStorageUnreachable`) is a statement about *this node's* backend and stays
   retryable.

**Mixed causes resolve deterministically.** When every upstream fails, `orderCauses` imposes a
total order over the causes — retryable-toward-network first, then by upstream id — before
`TranslateToJsonRpcException` picks the dominant code. So one upstream reporting `-32004
BlockNotAvailable` and another `-32007 SlotSkipped` for the same slot always shows the client the
retryable `-32004`: reporting the terminal `-32007` could make an indexer permanently skip a slot
that does exist, while reporting `-32004` only costs a retry.

**SVM `ClientSideException` is non-retryable toward the network**, via an explicit
`WithRetryableTowardNetwork(false)` on every client-side branch. That opt-out is scoped to SVM:
EVM's `ClientSideException` stays retryable by default, because there one node may simply lack a
capability another has.

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

eRPC's consensus engine operates on response hashes and is triggered by `MatchFinality` on the failsafe policy. It is already chain-agnostic. **CORRECTED — the SVM adapter's job is not "to set `req.Finality`".** It supplies `GetFinality` (§9.3), which the pipeline calls to classify each request per method, and `IsFinalizedCommitment` for routing decisions. The policy activates on that classification.

### Rules

**Write methods** — never consensus-eligible. The shipped non-retryable write set is
`sendTransaction`, `sendRawTransaction` and `requestAirdrop` (`IsNonRetryableWriteMethod`); there
is no `sendVersionedTransaction` RPC method — versioned transactions travel through
`sendTransaction`. These are excluded from hedging as well, which is what prevents a double-send
during consensus.

**CORRECTED — the adapter does not set `req.Finality` from the commitment level** (corrections
items 4 and 14). Finality is per-method (§9.3), so `commitment: finalized` alone does not make a
response `Finalized`: only a slot-pinned read (`getBlock`, `getTransaction`) at that commitment
does. Consensus therefore activates for slot-pinned finalized reads, and a
`matchFinality: [finalized]` policy stays inactive for moving-head reads at *any* commitment.

That inactivity is the desired outcome, and it subsumes the original `confirmed` rule below. Two
honest nodes at adjacent rooted slots legitimately disagree about a balance — the rooted head
moves every ~400 ms — so voting on `getBalance` would false-positive constantly no matter which
commitment was asked for. The original rules reached the right answer for `confirmed` by the
wrong route, and the wrong answer for `finalized` moving-head reads.

Routing asks a different question from caching. `IsFinalizedCommitment` answers "which slot does
the node evaluate this at" and *is* true for `getBalance` at `commitment: finalized`; that is the
predicate the slot-lag pre-filter uses. `GetFinality` answers "is this response immutable enough
to cache". Conflating the two is the trap.

### Slot-Lag Pre-Filter

Even among upstreams serving finalized data, one at finalized slot 100 and one at 110 answer
differently for state that changed in between. `architecture/svm/slot_lag.go` excludes upstreams
whose `FinalizedSlot()` trails a reference slot by more than `maxFinalizedSlotLag`.

**CORRECTED — the default is 100 slots (≈40 s), not 5** (corrections item 7). The field is
`*int64` precisely so three states stay distinguishable: omitted → 100, explicit `0` → filter
**disabled**, `>0` → that value.

Two properties the original sketch lacked:

- **The reference is not the raw pool max.** Plain pool-max is poisonable: one upstream reporting
  a wildly inflated finalized slot becomes the reference, every honest upstream then trails it by
  more than `maxLag`, and the filter can shrink the pool to just the liar. `ReferenceFinalizedSlot`
  clamps — when the leader outruns the runner-up by more than `maxLag`, the runner-up becomes the
  reference. The leader still passes (the filter only drops trailers) but can no longer drag the
  bar above the honest pack. This defends a single liar, not colluding upstreams, which would
  need a majority/vote-based baseline.
- **It never empties the pool.** If every upstream is filtered out, `FilterByFinalizedSlotLag`
  returns the original list: serving potentially-stale data beats deadlocking the request, and the
  failsafe consensus policy is the correct layer to detect divergence. An upstream with no
  finalized-slot sample yet also passes — unknown is not the same as trailing.

> **Q7 — Slot-lag filter — Decided:** Hard exclude for consensus-eligible requests. Score penalty already handles general lag for all other requests.

The filter runs only when a consensus policy is active **and** the request's resolved finality is
`Finalized`.

### Config

No new consensus config is needed — users configure it the same way as EVM:

```yaml
networks:
  # No `id` field: identity comes from svm.chain + svm.cluster.
  - architecture: svm
    svm:
      cluster: mainnet-beta
    failsafe:
      - matchMethod: "*"
        matchFinality: [finalized]
        consensus:
          # Both are COUNTS; validation enforces agreementThreshold <= maxParticipants.
          # "every participant must agree" is threshold == maxParticipants, not 100.
          maxParticipants: 2
          agreementThreshold: 2
```

---

## 11. Vendor Support (Phase 2)

Vendor-specific adapters (Helius, Alchemy, QuickNode, Triton) are deferred to Phase 2. Phase 1 uses plain HTTPS endpoints with `type: svm`.

Phase 2 will follow the same pattern as `thirdparty/alchemy.go` — implement `SupportsNetwork` checking `strings.HasPrefix(networkId, "svm:")` and `GenerateConfigs` building the vendor URL from the cluster name. Register in `thirdparty/vendors_registry.go`.

> **Q5 — Network ID format + genesis hash validation — Decided:** Use `svm:<cluster>` (e.g., `svm:mainnet-beta`, `svm:fogo-mainnet`), mirroring EVM's `evm:<chainId>` pattern. Genesis hashes for well-known clusters are hardcoded in `knownSvmClusters` — bootstrap validates automatically with no RPC call. For unknown clusters, `CheckGenesisHash: true` triggers a `getGenesisHash` call at bootstrap.

---

## 12. Configuration Example

Verified to load with `erpc validate` against the shipped schema — see corrections item 12 for
what the original example got wrong.

```yaml
projects:
  - id: my-project
    auth:
      strategies:
        - type: secret
          secret:
            value: ${PROJECT_SECRET}
    networks:
      # Networks carry no `id` field. Identity is derived from svm.chain +
      # svm.cluster: chain omitted => "solana" => networkId "svm:mainnet-beta".
      - architecture: svm
        svm:
          cluster: mainnet-beta
          commitment: confirmed
          statePollerDebounce: 400ms
          maxFinalizedSlotLag: 100
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
              # Counts, not percentages: agreementThreshold <= maxParticipants.
              maxParticipants: 2
              agreementThreshold: 2

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

      - id: triton-mainnet
        endpoint: https://${TRITON_HOST}.rpcpool.com/${TRITON_KEY}
        type: svm
        svm:
          cluster: mainnet-beta

# SVM caching is a root-level `database` block, not a per-network one.
database:
  svmJsonRpcCache:
    connectors:
      - id: memory
        driver: memory
        memory:
          maxItems: 100000
      - id: redis
        driver: redis
        redis:
          uri: redis://${REDIS_HOST}:6379/0
    policies:
      # Immutable: getBlock/getTransaction pinned to a rooted slot, plus
      # getInflationReward/getBlockTime. Safe to keep forever.
      - connector: redis
        network: "svm:mainnet-beta"
        method: "*"
        finality: finalized
        ttl: 0
      # Pinned to a slot but still fork-droppable: below `finalized` commitment.
      - connector: memory
        network: "svm:mainnet-beta"
        method: "*"
        finality: unfinalized
        ttl: 3s
      # Every other read is moving-head (getBalance, getAccountInfo, ...). This
      # TTL is the ONLY staleness bound — SVM has no block-timestamp age guard.
      - connector: memory
        network: "svm:mainnet-beta"
        method: "*"
        finality: realtime
        ttl: 2s
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
- `architecture/svm/finality_test.go` — finality state mapping (the proposed `commitment_test.go` never existed; see corrections item 10)

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
| **4 — Cache & Finality** | `neverCacheMethods` / `alwaysFinalizedMethods` / `slotPinnedMethods` tables, commitment injection, cache key design, `DataFinalityStateRealtime` | `architecture/svm/finality.go`, `architecture/svm/json_rpc_cache.go`, `common/defaults.go` | 1 day |
| **5 — Hooks** | All hook implementations, `getGenesisHash` short-circuit, non-retryable write guard (`sendTransaction`, `sendRawTransaction`, `requestAirdrop`), slot-lag filter for consensus | `architecture/svm/hooks.go`, `architecture/svm/slot_lag.go` | 2 days |
| **6 — Vendors** | _(Phase 2 — deferred)_ Helius, Alchemy, QuickNode, Triton | `thirdparty/helius.go`, etc. | — |
| **7 — Tests** | 14 integration tests (see §14), unit tests, gock helpers with host guard | `*_test.go`, `util/test_helpers_svm.go` | 2 days |
| **8 — Docs & Config** | YAML example, README | — | 0.5 day |
| **Total** | | | ~11.5 days |

---

## 16. Open Questions

| # | Question | Recommendation | Status |
|---|---|---|---|
| **Q1** | `UpstreamConfig.Type` vs `NetworkConfig.Architecture` — unify or keep separate? | Keep separate: `type` (e.g., `svm`, `svm+helius` in Phase 2) controls vendor client construction; `architecture` (`svm`) controls handler. Both use `svm` as the prefix — validation rejects configs where the `type` prefix doesn't match the network `architecture`. | Decided |
| **Q2** | `getSignaturesForAddress` auto-pagination? | Phase 2. **The Phase-1 half of this answer was wrong** and has been dropped (corrections item 1): there is no slot range to cap, because Solana pages this method by signature cursors (`before`/`until`) plus a `limit`. Any Phase-2 work is cursor-following, not range-splitting, and it is not analogous to EVM's `getLogs` pre-split guard. | Deferred |
| **Q3** | EVM stubs on `Network` interface — needed for SVM? | No. `ArchitectureHandler` approach uses a single `Network` struct for all architectures; a `SvmNetwork` type is never created. Extract `EvmNetwork` sub-interface in a follow-up. | Decided |
| **Q4** | Rename shared state keys (`latestBlock/` → `statePoller/latest/`)? | Yes, rename in Phase 1. No Redis migration needed — this is a fresh deployment; keys do not exist yet. | Decided |
| **Q5** | Cluster name vs genesis hash as network ID? | Use `svm:<cluster>`. Genesis hashes for known clusters are hardcoded in `knownSvmClusters` — validated at bootstrap with no RPC call. `checkGenesisHash: true` triggers a `getGenesisHash` call only for unknown/custom clusters. | Decided |
| **Q6** | `DataFinalityStateRealtime` for `processed`? | Add it in Phase 1. Cache policy for `Realtime` = always skip. EVM never uses it; no regression risk. | Decided |
| **Q7** | Slot-lag consensus pre-filter — hard exclude or score penalty? | Hard exclude for consensus-eligible requests (`finalized` + consensus policy active). Score penalty already handles general lag for all other requests. | Decided |
| **Q8** | Contribute `ArchitectureHandler` interface upstream to eRPC OSS or maintain a fork? | Propose Phase 0 (interface extraction only) as a PR to eRPC upstream first — it is non-breaking, EVM behavior is unchanged, and the Bitcoin/Aptos stubs in the codebase signal the maintainers intended multi-chain. If rejected, fork. The SVM adapter follows in a second PR. | Pending — needs maintainer conversation |
