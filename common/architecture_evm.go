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

type AvailbilityConfidence int

const (
	AvailbilityConfidenceBlockHead AvailbilityConfidence = 1
	AvailbilityConfidenceFinalized AvailbilityConfidence = 2
	// AvailbilityConfidenceStateReady requires that the upstream has not only
	// observed a given block at its head, but that its state DB is consistent
	// at that block. Distinguishes head-vs-trie races where a node's block
	// header pointer has advanced but state slots for that block are not yet
	// committed (returns 0x0 / "0x" / null silently).
	//
	// Requires EvmNetworkConfig.StateProbe to be configured. Without a probe
	// config this confidence falls back to BlockHead semantics.
	AvailbilityConfidenceStateReady AvailbilityConfidence = 3
)

func (c AvailbilityConfidence) String() string {
	switch c {
	case AvailbilityConfidenceBlockHead:
		return "blockHead"
	case AvailbilityConfidenceFinalized:
		return "finalizedBlock"
	case AvailbilityConfidenceStateReady:
		return "stateReady"
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
	case "stateready", "3":
		*c = AvailbilityConfidenceStateReady
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
	// PollStateReady runs the configured StateProbe at the current latest block.
	// On success advances StateReadyBlock to the probed block; on failure leaves
	// it unchanged. Returns (block at which probe was attempted, error).
	// When no StateProbe is configured, returns (0, nil) and StateReadyBlock
	// will report LatestBlock as a backward-compatible fallback.
	PollStateReady(ctx context.Context) (int64, error)
	SyncingState() EvmSyncingState
	SetSyncingState(state EvmSyncingState)
	LatestBlock() int64
	FinalizedBlock() int64
	// StateReadyBlock returns the most recent block at which the state probe
	// confirmed the upstream's state DB is consistent. When no StateProbe is
	// configured, falls back to LatestBlock.
	StateReadyBlock() int64
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

	// State-readiness probe diagnostics.
	StateReadyBlock           int64  `json:"stateReadyBlock,omitempty"`
	StateProbeConfigured      bool   `json:"stateProbeConfigured,omitempty"`
	StateProbeFailureCount    int    `json:"stateProbeFailureCount,omitempty"`
	StateProbeSuccessfulOnce  bool   `json:"stateProbeSuccessfulOnce,omitempty"`
	StateProbeDetectionIssue  string `json:"stateProbeDetectionIssue,omitempty"`

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
