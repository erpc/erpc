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
	ArchitectureSvm NetworkArchitecture = "svm"
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
}

// EvmNetwork is the EVM-specific view of a Network. Callers that need
// block-number accessors or leader-upstream selection should go through the
// EvmHighestLatestBlockNumber / EvmHighestFinalizedBlockNumber / EvmLeaderUpstream
// helpers below, which type-assert and degrade to zero-value on mismatch.
type EvmNetwork interface {
	Network
	EvmHighestLatestBlockNumber(ctx context.Context) int64
	EvmHighestFinalizedBlockNumber(ctx context.Context) int64
	EvmLeaderUpstream(ctx context.Context) Upstream
}

// SvmNetwork is the SVM-specific view of a Network. Production Network
// implementations satisfy this automatically when the underlying network is
// SVM; EVM networks correctly fail the assertion.
type SvmNetwork interface {
	Network
	SvmHighestLatestSlot(ctx context.Context) int64
	SvmHighestFinalizedSlot(ctx context.Context) int64
}

// EvmHighestLatestBlockNumber returns the highest observed "latest" block
// across EVM upstreams of n, or 0 if n is not an EVM network or no upstream
// has reported a block yet. Use in place of a direct method call so callers
// don't need to type-assert inline.
func EvmHighestLatestBlockNumber(n Network, ctx context.Context) int64 {
	if e, ok := n.(EvmNetwork); ok {
		return e.EvmHighestLatestBlockNumber(ctx)
	}
	return 0
}

// EvmHighestFinalizedBlockNumber mirrors EvmHighestLatestBlockNumber for the
// finalized tip.
func EvmHighestFinalizedBlockNumber(n Network, ctx context.Context) int64 {
	if e, ok := n.(EvmNetwork); ok {
		return e.EvmHighestFinalizedBlockNumber(ctx)
	}
	return 0
}

// EvmLeaderUpstream returns the currently-elected leader EVM upstream for n,
// or nil if n is not EVM-shaped or no leader has been elected.
func EvmLeaderUpstream(n Network, ctx context.Context) Upstream {
	if e, ok := n.(EvmNetwork); ok {
		return e.EvmLeaderUpstream(ctx)
	}
	return nil
}

func IsValidArchitecture(architecture string) bool {
	switch NetworkArchitecture(architecture) {
	case ArchitectureEvm, ArchitectureSvm:
		return true
	}
	return false
}

func IsValidNetwork(network string) bool {
	if strings.HasPrefix(network, "evm:") {
		chainId, err := strconv.ParseInt(strings.TrimPrefix(network, "evm:"), 10, 64)
		if err != nil {
			return false
		}
		return chainId > 0
	}
	if strings.HasPrefix(network, "svm:") {
		rest := strings.TrimPrefix(network, "svm:")
		if rest == "" {
			return false
		}
		// Two shapes: "svm:<cluster>" (implicit solana, back-compat) and
		// "svm:<chain>:<cluster>". Accept known (chain, cluster) pairs
		// outright; accept unknown ones that look like valid identifiers so
		// users can run private chains/clusters behind eRPC (bootstrap still
		// enforces the genesis hash when CheckGenesisHash is set).
		parts := strings.SplitN(rest, ":", 3)
		if len(parts) > 2 {
			return false
		}
		var chain, cluster string
		if len(parts) == 1 {
			cluster = parts[0]
		} else {
			chain, cluster = parts[0], parts[1]
		}
		if cluster == "" {
			return false
		}
		if IsValidSvmCluster(chain, cluster) {
			return true
		}
		isIdentifier := func(s string) bool {
			if s == "" {
				return true // empty chain is fine — means implicit solana
			}
			for _, r := range s {
				if !(r == '-' || r == '_' || r == '.' ||
					(r >= 'a' && r <= 'z') ||
					(r >= 'A' && r <= 'Z') ||
					(r >= '0' && r <= '9')) {
					return false
				}
			}
			return true
		}
		return isIdentifier(chain) && isIdentifier(cluster)
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
