package common

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/erpc/erpc/util"
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
	FinalizedBlock() int64
	IsBlockFinalized(blockNumber int64) (bool, error)
	SuggestFinalizedBlock(blockNumber int64)
	SuggestLatestBlock(blockNumber int64)
	// RefreshLatestBlock polls the latest block in the background when the
	// stored head is older than the poll debounce; non-blocking, one in flight.
	RefreshLatestBlock()
	// ResetChainIdCordon clears the chain-identity cordon ownership flag so an
	// admin uncordon fully releases this upstream.
	ResetChainIdCordon()
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

// ParseEvmBlockParam splits a JSON-RPC block parameter into a reference and a
// number: a hash or tag gives (ref, 0), a number gives (decimal ref, number).
// Block 0 gives ("", 0).
func ParseEvmBlockParam(param interface{}) (string, int64, error) {
	blockNumberStr, blockHash, err := util.ParseBlockParameter(param)
	if err != nil {
		return "", 0, err
	}
	if blockHash != nil {
		return fmt.Sprintf("0x%x", blockHash), 0, nil
	}
	if blockNumberStr == "" {
		return "", 0, nil
	}
	if !strings.HasPrefix(blockNumberStr, "0x") {
		return blockNumberStr, 0, nil
	}
	bn, err := HexToInt64(blockNumberStr)
	if err != nil {
		return "", 0, err
	}
	if bn > 0 {
		return strconv.FormatInt(bn, 10), bn, nil
	}
	return "", 0, nil
}

// EvmRequestedBlockNumber returns the highest block number the request's own
// parameters name, read through the method's reqRefs. ok is false when the
// method takes no block parameter, or when any parameter is a tag, a hash, or
// an omitted range bound: JSON-RPC reads an omitted fromBlock/toBlock as
// "latest".
func EvmRequestedBlockNumber(ctx context.Context, req *NormalizedRequest) (int64, bool) {
	method, err := req.Method()
	if err != nil {
		return 0, false
	}
	mc := ResolveMethodConfig(method, req.Network())
	if mc == nil || mc.Finalized || mc.Realtime || len(mc.ReqRefs) == 0 {
		return 0, false
	}
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		return 0, false
	}
	jrq.RLock()
	defer jrq.RUnlock()

	var highest int64
	found := false
	for _, ref := range mc.ReqRefs {
		val, err := jrq.PeekByPath(ref...)
		if err != nil {
			if omittedRangeBound(jrq, ref) {
				return 0, false
			}
			continue
		}
		blockRef, bn, err := ParseEvmBlockParam(val)
		if err != nil || (bn == 0 && blockRef != "") {
			return 0, false
		}
		if !found || bn > highest {
			highest = bn
		}
		found = true
	}
	return highest, found
}

func omittedRangeBound(jrq *JsonRpcRequest, ref []interface{}) bool {
	if len(ref) < 2 {
		return false
	}
	key, _ := ref[len(ref)-1].(string)
	if key != "fromBlock" && key != "toBlock" {
		return false
	}
	parent, err := jrq.PeekByPath(ref[:len(ref)-1]...)
	if err != nil {
		return false
	}
	_, isObject := parent.(map[string]interface{})
	return isObject
}
