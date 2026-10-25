package common

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	UpstreamTypeEvm UpstreamType = "evm"
)

// HeadNeed is what a head Ask must produce (spec §6.8).
type HeadNeed uint8

const (
	// HeadNeedNumber: a fresh number; a shared counter whose report is
	// younger than the cadence meets it.
	HeadNeedNumber HeadNeed = iota
	// HeadNeedHash: a hash vote; only a local fetch sent within the
	// cadence meets it.
	HeadNeedHash
	// HeadNeedFork: a hash need whose answer is fresh evidence of the
	// node's head, whatever its earlier votes say (spec §6.3 fork poll).
	HeadNeedFork
)

// AskReason is a head Ask's caller, the reason label of
// erpc_head_ask_total (spec §6.8, §17).
type AskReason string

const (
	AskPoll    AskReason = "poll"
	AskCatchUp AskReason = "catch_up"
	AskRefusal AskReason = "refusal"
	AskRefresh AskReason = "refresh"
)

// AskOutcome is how a head Ask was met, the outcome label of
// erpc_head_ask_total (spec §17).
type AskOutcome string

const (
	// AskFetched: the Ask started a fetch.
	AskFetched AskOutcome = "fetched"
	// AskSharedFresh: the shared counter met a number need.
	AskSharedFresh AskOutcome = "shared_fresh"
	// AskInflight: a local fetch sent within the cadence met it, in
	// flight or answered.
	AskInflight AskOutcome = "inflight"
	// AskBackoff: the upstream backs off; nothing was fetched.
	AskBackoff AskOutcome = "backoff"
)

// HeadAnswer is what a head Ask gives its caller: the fetch's one
// outcome, the same for every waiter.
type HeadAnswer struct {
	Outcome AskOutcome
	// Block is the upstream's latest counter after the outcome.
	Block int64
	// Header is the fetched latest header; nil with no fetch answer.
	Header json.RawMessage
	Err    error
}

// EvmHeadReaches is the one head predicate (spec §6.4, #1099): u, with head
// as its latest, reaches block bn when head plus its
// headLagToleranceBlocks is at least bn. The availability gate's
// latestBlockMinus upper bounds and the ordering of upstreams by head read
// it.
func EvmHeadReaches(u Upstream, head, bn int64) bool {
	return head+EvmHeadLagTolerance(u) >= bn
}

// EvmHeadLagTolerance is u's headLagToleranceBlocks: how many blocks above
// its observed head u can serve.
func EvmHeadLagTolerance(u Upstream) int64 {
	if u == nil {
		return 0
	}
	if cfg := u.Config(); cfg != nil && cfg.Evm != nil {
		return cfg.Evm.HeadLagToleranceBlocks
	}
	return 0
}

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

// EvmStateProvenWriter is the OPTIONAL, separately-asserted surface the
// integrity state prober records proofs through. The proven head is telemetry
// (the state-proven block / proven-lag gauges), never a routing input.
// Deliberately NOT part of EvmUpstream: that interface is implemented outside
// this repo, and widening it broke every existing implementor — the chainId
// suggest-gate silently degraded when its upstream stopped satisfying the
// assertion. Optional capabilities are asserted narrowly, never added to the
// core interface.
type EvmStateProvenWriter interface {
	// EvmSetStateProvenBlock records a successful state proof at a height.
	// Monotonic: a lower value than the current one is ignored.
	EvmSetStateProvenBlock(int64)
}

type AvailbilityConfidence int

const (
	AvailbilityConfidenceBlockHead AvailbilityConfidence = 1
	AvailbilityConfidenceFinalized AvailbilityConfidence = 2
	// NOTE: there is deliberately no "state-proven" confidence. The proven head
	// (see the integrity state prober) advances at probe cadence, so on any
	// chain whose block time is shorter than that cadence it sits structurally
	// behind every honest upstream's claimed head — a routing bound built on it
	// would refuse the entire network's tip traffic. Absence of proof is not
	// evidence; only DISPROOF is, and the prober publishes that as upstream
	// misbehavior on the health tracker for selection policies to act on
	// (misbehaviorRateAbove), not as an availability confidence.
)

func (c AvailbilityConfidence) String() string {
	switch c {
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
	// LatestReport is the latest block's report: its number, when its
	// evidence was taken (a poll's send time, else this instance's receive
	// time), and whether this instance's newest poll succeeded.
	LatestReport() (block int64, at time.Time, live bool)
	// FinalizedBlock is the upstream's finalized tag; only an upstream known
	// to have no finalized tag reports latest − the derived finality depth (#1079).
	FinalizedBlock() int64
	IsBlockFinalized(blockNumber int64) (bool, error)
	SuggestFinalizedBlock(blockNumber int64)
	SuggestLatestBlock(blockNumber int64)
	// ObserveHead runs a source header through the upstream's part of
	// intake: the chain-identity gate and the head counter, as
	// SuggestLatestBlock does for heights. accepted: the counter holds the
	// header's height now.
	ObserveHead(raw json.RawMessage) (accepted bool)
	// RefreshLatestBlock polls the latest block in the background when the
	// stored head is older than the poll debounce; non-blocking, one in flight.
	RefreshLatestBlock()
	// RefreshFinalizedBlock is RefreshLatestBlock for the finalized block,
	// against the finalized poll's debounce.
	RefreshFinalizedBlock()
	// PollHead polls latest in the background now, for a hash vote, however
	// fresh the stored head is; non-blocking, one in flight. fork: it asks
	// a voter whose vote the pins moved off, so its answer is fresh evidence.
	PollHead(fork bool)
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
