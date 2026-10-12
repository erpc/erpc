package evm

import (
	"context"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// queryTestNetwork is a minimal common.Network for hook tests: fixed heads
// and a pluggable Forward.
type queryTestNetwork struct {
	cfg       *common.NetworkConfig
	latest    int64
	finalized int64
	forwardFn func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)
}

func (n *queryTestNetwork) Id() string                               { return "evm:1" }
func (n *queryTestNetwork) Label() string                            { return "evm:1" }
func (n *queryTestNetwork) ProjectId() string                        { return "test-project" }
func (n *queryTestNetwork) Architecture() common.NetworkArchitecture { return common.ArchitectureEvm }
func (n *queryTestNetwork) Config() *common.NetworkConfig            { return n.cfg }
func (n *queryTestNetwork) Logger() *zerolog.Logger {
	logger := zerolog.Nop()
	return &logger
}
func (n *queryTestNetwork) GetMethodMetrics(method string) common.TrackedMetrics { return nil }
func (n *queryTestNetwork) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return n.forwardFn(ctx, req)
}
func (n *queryTestNetwork) GetFinality(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) common.DataFinalityState {
	return common.DataFinalityStateFinalized
}
func (n *queryTestNetwork) EvmHighestLatestBlockNumber(ctx context.Context) int64 { return n.latest }
func (n *queryTestNetwork) EvmHighestFinalizedBlockNumber(ctx context.Context) int64 {
	return n.finalized
}
func (n *queryTestNetwork) EvmLeaderUpstream(ctx context.Context) common.Upstream { return nil }

// queryTestConfigUpstream is a minimal common.Upstream with a given config.
type queryTestConfigUpstream struct {
	cfg *common.UpstreamConfig
}

func (u *queryTestConfigUpstream) Id() string                     { return "upstream-config" }
func (u *queryTestConfigUpstream) VendorName() string             { return "test" }
func (u *queryTestConfigUpstream) NetworkId() string              { return "evm:1" }
func (u *queryTestConfigUpstream) NetworkLabel() string           { return "evm:1" }
func (u *queryTestConfigUpstream) Config() *common.UpstreamConfig { return u.cfg }
func (u *queryTestConfigUpstream) Logger() *zerolog.Logger {
	logger := zerolog.Nop()
	return &logger
}
func (u *queryTestConfigUpstream) Vendor() common.Vendor         { return nil }
func (u *queryTestConfigUpstream) Tracker() common.HealthTracker { return nil }
func (u *queryTestConfigUpstream) Forward(ctx context.Context, nq *common.NormalizedRequest, byPassMethodExclusion bool, isHedgeAttempt bool) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (u *queryTestConfigUpstream) Cordon(method string, reason string)   {}
func (u *queryTestConfigUpstream) Uncordon(method string, reason string) {}
func (u *queryTestConfigUpstream) IgnoreMethod(method string)            {}
func (u *queryTestConfigUpstream) ShouldHandleMethod(method string) (bool, error) {
	return true, nil
}
