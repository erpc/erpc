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

// HeadNeed is what a head Ask must produce (spec §6.8); the Ask's pace
// is the oldest evidence that meets it.
type HeadNeed uint8

const (
	// HeadNeedNumber: a fresh number; a shared counter whose report is
	// younger than the pace meets it.
	HeadNeedNumber HeadNeed = iota
	// HeadNeedHash: a hash vote; only a local fetch sent within the pace
	// meets it.
	HeadNeedHash
	// HeadNeedFork: a hash need whose answer is fresh evidence of the
	// node's head, whatever its earlier votes say (spec §6.3 fork poll):
	// only a fork fetch meets it, sent after the Ask at pace 0, else
	// within the pace.
	HeadNeedFork
)

// AskReason is a head Ask's caller, the reason label of
// erpc_head_ask_total (spec §6.8, §17).
type AskReason string

const (
	AskPoll     AskReason = "poll"
	AskCatchUp  AskReason = "catch_up"
	AskRefusal  AskReason = "refusal"
	AskRefresh  AskReason = "refresh"
	AskSettle   AskReason = "settle"
	AskNoSource AskReason = "no_source"
)

// AskOutcome is how a head Ask was met, the outcome label of
// erpc_head_ask_total (spec §17).
type AskOutcome string

const (
	// AskFetched: the Ask started a fetch.
	AskFetched AskOutcome = "fetched"
	// AskSharedFresh: the shared counter met a number need.
	AskSharedFresh AskOutcome = "shared_fresh"
	// AskInflight: a local fetch sent within the pace met it, in flight
	// or answered.
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
	// Header is the fetched latest header, parsed once at its fetch; nil
	// with no fetch answer.
	Header EvmHeader `tstype:"-"`
	// At is when the evidence that met the Ask was taken: the meeting
	// fetch's send, or the shared counter's report; zero with no evidence
	// (backoff, a fork fetch queued and not yet sent).
	At time.Time
	// Next is when an Ask at the same pace may fetch again: the meeting
	// fetch's send plus the pace or its backed-off cadence, the longer;
	// zero when it may now.
	Next time.Time
	Err  error `tstype:"-"`
}

// EvidenceAt is a voter's evidence time on an axis, latest or finalized
// (spec §6.8): the later of its newest report there and the send time of
// this instance's last Ask of it there. Every oldest-first ordering of
// voters reads it: a voter whose answers sit below its own counter never
// refreshes its report, and ordered by reports alone it would take every
// slot (§19 P1).
func EvidenceAt(report, asked time.Time) time.Time {
	if asked.After(report) {
		return asked
	}
	return report
}

// EvmHeader is a latest header past intake check 0, parsed once where it
// was fetched: the evm package's SourceHeader.
type EvmHeader interface {
	// Raw is the header as its upstream sent it.
	Raw() json.RawMessage
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
	// PollLatestBlockNumber is the latest block number after an Ask with a
	// number need; the error is the Ask's.
	PollLatestBlockNumber(ctx context.Context) (int64, error)
	PollFinalizedBlockNumber(ctx context.Context) (int64, error)
	PollEarliestBlockNumber(ctx context.Context, probe EvmAvailabilityProbeType, staleness time.Duration) (int64, error)
	SyncingState() EvmSyncingState
	SetSyncingState(state EvmSyncingState)
	LatestBlock() int64
	// LatestReportAt is when the latest block report's evidence was taken:
	// a poll's send time, else this instance's receive time.
	LatestReportAt() time.Time
	// FinalizedBlock is the upstream's finalized tag; only an upstream known
	// to have no finalized tag reports latest − the derived finality depth (#1079).
	FinalizedBlock() int64
	// LatestAskAt is the send time of this instance's last Ask fetch of
	// latest, zero before any: the Ask half of EvidenceAt.
	LatestAskAt() time.Time
	IsBlockFinalized(blockNumber int64) (bool, error)
	SuggestFinalizedBlock(blockNumber int64, at time.Time)
	// SuggestLatestBlock reports blockNumber, its evidence taken at at: an
	// answer's request send time, a push's receive time (spec §6.4).
	SuggestLatestBlock(blockNumber int64, at time.Time)
	// RewindLatestBlock writes blockNumber, a height the upstream's own
	// hash vote holds on the chain tracker's new branch, to the latest
	// counter when the counter is above it (spec §14 rollback write).
	RewindLatestBlock(blockNumber int64)
	// ObserveHead runs a source header's height, parsed once by intake,
	// through the upstream's part of intake: the chain-identity gate and
	// the head counter, as SuggestLatestBlock does. accepted: the counter
	// holds that height now.
	ObserveHead(number int64, at time.Time) (accepted bool)
	// Ask is the upstream's head Ask (spec §6.8): single-flight, its
	// outcome to every waiter; it waits for the answer or ctx's end. pace
	// is the oldest evidence the caller accepts, never below the cadence
	// (0 is the cadence; a fork need's pace is as given): callers keep no
	// fetch pacer of their own. A caller that Asks again passes as after
	// the At of the answer it last took from this upstream: evidence taken
	// at or before it never meets the Ask; zero for any.
	Ask(ctx context.Context, need HeadNeed, pace time.Duration, after time.Time, reason AskReason) HeadAnswer
	// AskReaching is a number-need Ask at pace for a caller that needs
	// block: a joined fetch that answers below it is followed by one fork
	// Ask; at most W in all.
	AskReaching(ctx context.Context, block int64, pace time.Duration, reason AskReason) HeadAnswer
	// AskBackground is Ask without waiting for the answer: the answer's
	// Outcome, Block, Next and At, no header.
	AskBackground(need HeadNeed, pace time.Duration, after time.Time, reason AskReason) HeadAnswer
	// RefreshFinalized polls the finalized block when the stored one is
	// older than the finalized poll's debounce, or joins the poll in
	// flight, and waits for it until ctx ends. polls is false when the
	// upstream has no finalized poll to refresh: its finalized tag is given
	// up.
	RefreshFinalized(ctx context.Context) (polls bool)
	SetNetworkConfig(cfg *NetworkConfig)
	IsObjectNull() bool
	EarliestBlock(probe EvmAvailabilityProbeType) int64
	GetDiagnostics() *EvmStatePollerDiagnostics
	// Wait waits, once the app ctx ended, until no goroutine of the poller
	// runs; none starts after.
	Wait()
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
