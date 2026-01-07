package common

import (
	"context"
	"fmt"
	"strings"
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
}

type AvailbilityConfidence int

const (
	AvailbilityConfidenceBlockHead AvailbilityConfidence = 1
	AvailbilityConfidenceFinalized AvailbilityConfidence = 2
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
	PollEarliestBlockNumber(ctx context.Context, probe EvmAvailabilityProbeType) (int64, error)
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
}
