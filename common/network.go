package common

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

type NetworkArchitecture string

const (
	ArchitectureEvm NetworkArchitecture = "evm"
)

type Network interface {
	Id() string
	Label() string
	ProjectId() string
	Architecture() NetworkArchitecture
	Config() *NetworkConfig
	Logger() *zerolog.Logger
	GetMethodMetrics(method string) TrackedMetrics
	Forward(ctx context.Context, nq *NormalizedRequest) (*NormalizedResponse, error)
	GetFinality(ctx context.Context, req *NormalizedRequest, resp *NormalizedResponse) DataFinalityState

	// TODO Move to EvmNetwork interface?
	// EvmHighestLatestBlockNumber returns the highest "latest" block number across
	// upstreams. Optional extraMethods are unioned with the network's configured
	// LatestBlockGuaranteedMethods; the resulting set constrains which upstream
	// blocks count (see EvmNetworkConfig.LatestBlockGuaranteedMethods).
	EvmHighestLatestBlockNumber(ctx context.Context, extraMethods ...string) int64
	// EvmHighestFinalizedBlockNumber mirrors EvmHighestLatestBlockNumber for the
	// "finalized" tag.
	EvmHighestFinalizedBlockNumber(ctx context.Context, extraMethods ...string) int64
	EvmLeaderUpstream(ctx context.Context) Upstream
}

func IsValidArchitecture(architecture string) bool {
	return architecture == string(ArchitectureEvm) // TODO add more architectures when they are supported
}

func IsValidNetwork(network string) bool {
	if strings.HasPrefix(network, "evm:") {
		chainId, err := strconv.ParseInt(strings.TrimPrefix(network, "evm:"), 10, 64)
		if err != nil {
			return false
		}
		return chainId > 0
	}

	return false
}

type QuantileTracker interface {
	Add(value float64)
	GetQuantile(qtile float64) time.Duration
	Reset()
}

type TrackedMetrics interface {
	ErrorRate() float64
	GetResponseQuantiles() QuantileTracker
}
