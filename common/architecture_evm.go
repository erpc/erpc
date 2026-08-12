package common

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	UpstreamTypeEvm UpstreamType = "evm"
)

type EvmUpstream interface {
	Upstream
	EvmGetChainId(ctx context.Context) (string, error)
	EvmIsBlockFinalized(ctx context.Context, blockNumber int64, forceFreshIfStale bool) (bool, error)
	EvmAssertBlockAvailability(ctx context.Context, forMethod string, confidence AvailbilityConfidence, forceFreshIfStale bool, blockNumber int64) (bool, error)
	EvmSyncingState() EvmSyncingState
	EvmStatePoller() EvmStatePoller
	// EvmEffectiveLatestBlock returns the latest block adjusted for the upstream's upper availability bound.
	// If the upstream has a blockAvailability.upper config (e.g., latestBlockMinus: 5), this returns
	// min(latestBlock, upperBound) instead of the raw latest block.
	EvmEffectiveLatestBlock() int64
	// EvmEffectiveFinalizedBlock returns the finalized block adjusted for the upstream's upper availability bound.
	// If the upstream has a blockAvailability.upper config, this returns min(finalizedBlock, upperBound).
	EvmEffectiveFinalizedBlock() int64
	// EvmBlockAvailabilityBounds returns the resolved [min, max] block range this upstream
	// is configured to serve. Returns (math.MinInt64, math.MaxInt64) for unbounded sides.
	EvmBlockAvailabilityBounds() (int64, int64)
}

// EvmStateProvenReader is the OPTIONAL, separately-asserted surface for the
// state-proven boundary (see the integrity state prober). Deliberately NOT part
// of EvmUpstream: that interface is implemented outside this repo, and widening
// it broke every existing implementor — the chainId suggest-gate silently
// degraded when its upstream stopped satisfying the assertion. Optional
// capabilities are asserted narrowly, never added to the core interface.
type EvmStateProvenReader interface {
	// EvmStateProvenBlock is the highest block for which this upstream has
	// PROVEN it holds the state trie. 0 = never proven (probe disabled,
	// warming up, or unsupported) — callers fall back to the claimed head.
	EvmStateProvenBlock() int64
}

// EvmStateProvenWriter is the prober-facing half.
type EvmStateProvenWriter interface {
	// EvmSetStateProvenBlock records a successful state proof at a height.
	// Monotonic: a lower value than the current one is ignored.
	EvmSetStateProvenBlock(int64)
}

type AvailbilityConfidence int

const (
	AvailbilityConfidenceBlockHead AvailbilityConfidence = 1
	AvailbilityConfidenceFinalized AvailbilityConfidence = 2
	// AvailbilityConfidenceStateProven gates on the state-PROVEN head rather
	// than the claimed head: the highest block for which the integrity state
	// probe verified the upstream truly executes in that block's context /
	// holds its state trie. Nodes sometimes answer state queries (eth_call,
	// eth_getBalance, ...) from OLDER state while their reported head is
	// current; this confidence exists so routing for state methods can refuse
	// to outrun proof. Falls back to blockHead while nothing is proven yet.
	AvailbilityConfidenceStateProven AvailbilityConfidence = 3
)

func (c AvailbilityConfidence) String() string {
	switch c {
	case AvailbilityConfidenceStateProven:
		return "stateProven"
	case AvailbilityConfidenceBlockHead:
		return "blockHead"
	case AvailbilityConfidenceFinalized:
		return "finalizedBlock"
	default:
		return fmt.Sprintf("unknown(%d)", c)
	}
}

func (c AvailbilityConfidence) MarshalYAML() (interface{}, error) {
	return c.String(), nil
}

func (c AvailbilityConfidence) MarshalJSON() ([]byte, error) {
	return SonicCfg.Marshal(c.String())
}

func (c *AvailbilityConfidence) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}

	switch strings.ToLower(s) {
	case "blockhead", "1":
		*c = AvailbilityConfidenceBlockHead
		return nil
	case "finalizedblock", "2":
		*c = AvailbilityConfidenceFinalized
		return nil
	}

	return fmt.Errorf("invalid availability confidence: %s", s)
}

type EvmNodeType string

const (
	EvmNodeTypeUnknown EvmNodeType = "unknown"
	EvmNodeTypeFull    EvmNodeType = "full"
	EvmNodeTypeArchive EvmNodeType = "archive"
)

type EvmSyncingState int

const (
	EvmSyncingStateUnknown EvmSyncingState = iota
	EvmSyncingStateSyncing
	EvmSyncingStateNotSyncing
)

func (s EvmSyncingState) String() string {
	switch s {
	case EvmSyncingStateSyncing:
		return "syncing"
	case EvmSyncingStateNotSyncing:
		return "not_syncing"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

type EvmStatePoller interface {
	Bootstrap(ctx context.Context) error
	Poll(ctx context.Context) error
	PollLatestBlockNumber(ctx context.Context) (int64, error)
	PollFinalizedBlockNumber(ctx context.Context) (int64, error)
	PollEarliestBlockNumber(ctx context.Context, probe EvmAvailabilityProbeType, staleness time.Duration) (int64, error)
	SyncingState() EvmSyncingState
	SetSyncingState(state EvmSyncingState)
	LatestBlock() int64
	FinalizedBlock() int64
	IsBlockFinalized(blockNumber int64) (bool, error)
	SuggestFinalizedBlock(blockNumber int64)
	SuggestLatestBlock(blockNumber int64)
	SetNetworkConfig(cfg *NetworkConfig)
	IsObjectNull() bool
	EarliestBlock(probe EvmAvailabilityProbeType) int64
	GetDiagnostics() *EvmStatePollerDiagnostics
}

// EvmStatePollerDiagnostics contains diagnostic information about the state poller
// including block bounds, probe status, and any detection issues.
type EvmStatePollerDiagnostics struct {
	Enabled bool `json:"enabled"`

	// Block head information
	LatestBlock    int64 `json:"latestBlock"`
	FinalizedBlock int64 `json:"finalizedBlock"`

	// Syncing state
	SyncingState      string `json:"syncingState"`
	SkipSyncingCheck  bool   `json:"skipSyncingCheck,omitempty"`
	SyncingCheckError string `json:"syncingCheckError,omitempty"`

	// Latest block detection status
	SkipLatestBlockCheck      bool   `json:"skipLatestBlockCheck,omitempty"`
	LatestBlockFailureCount   int    `json:"latestBlockFailureCount,omitempty"`
	LatestBlockSuccessfulOnce bool   `json:"latestBlockSuccessfulOnce,omitempty"`
	LatestBlockDetectionIssue string `json:"latestBlockDetectionIssue,omitempty"`

	// Finalized block detection status
	SkipFinalizedCheck           bool   `json:"skipFinalizedCheck,omitempty"`
	FinalizedBlockFailureCount   int    `json:"finalizedBlockFailureCount,omitempty"`
	FinalizedBlockSuccessfulOnce bool   `json:"finalizedBlockSuccessfulOnce,omitempty"`
	FinalizedBlockDetectionIssue string `json:"finalizedBlockDetectionIssue,omitempty"`

	// Earliest block bounds per probe type
	EarliestByProbe map[EvmAvailabilityProbeType]*EvmProbeEarliestInfo `json:"earliestByProbe,omitempty"`
}

// EvmProbeEarliestInfo contains information about earliest block detection for a specific probe type
type EvmProbeEarliestInfo struct {
	ProbeType        EvmAvailabilityProbeType `json:"probeType"`
	EarliestBlock    int64                    `json:"earliestBlock"`
	SchedulerRunning bool                     `json:"schedulerRunning,omitempty"`
}

// IsEvmStateQueryMethod reports whether a method reads the STATE TRIE at a
// block (as opposed to chain data like blocks/receipts/logs). These are the
// methods a node can silently answer from older state, so they are the ones
// the state-proven boundary applies to (architecture/evm/hooks.go).
//
// Membership is a two-part test, and BOTH parts must hold:
//
//  1. the method's answer is a function of the state trie at the requested
//     block (a balance, a slot, code, an account proof, or an EVM execution in
//     that block's context) — a node with stale state answers it wrongly while
//     reporting a current head; and
//  2. the requested block is resolvable from the request through the method's
//     ReqRefs (common/defaults.go), because a boundary that cannot name a
//     height cannot enforce one.
//
// This list is an enumeration over an open set, so its unmatched path is the
// one that matters: an unlisted state method is simply NOT gated (fail-open,
// exactly today's behavior for it). Adding a method here can only ever refuse
// or divert traffic the proven head does not cover, so extend it whenever parts
// 1 and 2 both hold. Known state-reading methods still absent:
//
//   - eth_createAccessList, debug_traceCallMany — no method config at all, so
//     part 2 fails and gating them would be inert until they get one.
//   - trace_call — its block parameter is the second OR third argument
//     depending on the client, and on the variant where trace types occupy the
//     second argument the extraction errors out, so the gate would be inert for
//     exactly those requests. Pin the position first.
//   - the arbtrace_call family — parts 1 and 2 both hold; left out only to keep
//     this change to the methods the gap review named.
func IsEvmStateQueryMethod(methodLower string) bool {
	switch methodLower {
	case "eth_call", "eth_getbalance", "eth_getcode", "eth_getstorageat",
		"eth_gettransactioncount", "eth_estimategas",
		// eth_getProof IS the state trie (params[2] is the block); eth_simulateV1
		// (params[1]) and debug_traceCall (params[1]) execute the EVM against the
		// state at the requested block exactly like eth_call does.
		"eth_getproof", "eth_simulatev1", "debug_tracecall":
		return true
	}
	return false
}
