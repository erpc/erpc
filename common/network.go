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

// EvmNetwork is the EVM-specific view of a Network. Callers read the head
// through EvmHeadOf, which type-asserts and degrades to the zero Head.
type EvmNetwork interface {
	Network
	// EvmHead is the head a request serves (spec §6.4, §7): req's, taken
	// once per request from its lane, else the request bound to ctx, else
	// the network-wide lane's. It never computes a ballot for a lane.
	EvmHead(ctx context.Context, req *NormalizedRequest) EvmHead
	// EvmRefreshLatestBlock asks the pollers of the request's latest-head
	// ballot for a fresh head in the background (see
	// EvmStatePoller.RefreshLatestBlock); it never blocks.
	EvmRefreshLatestBlock(ctx context.Context)
}

// EvmHead is one lane's head (spec §6.4), published as one value. The zero
// Head is an unknown one.
type EvmHead struct {
	// Latest is the q-th highest voter number, guarded and clamped to the
	// guarantees.
	Latest int64
	// Finalized is the same ballot over the voters' finalized numbers.
	Finalized int64
	// Leader is the highest voter within Latest + maxRetryableBlockDistance.
	Leader Upstream
	// Ceiling is the raw max head of the lane's registered upstreams, 0 while
	// any of them has none; CeilingAt is when this instance saw it.
	Ceiling   int64
	CeilingAt time.Time
}

// SvmNetwork is the SVM-specific view of a Network. Production Network
// implementations satisfy this automatically when the underlying network is
// SVM; EVM networks correctly fail the assertion.
type SvmNetwork interface {
	Network
	SvmHighestLatestSlot(ctx context.Context) int64
	SvmHighestFinalizedSlot(ctx context.Context) int64
	// SvmHighestFinalizedSlotMax is the finalized root of the MOST-ADVANCED
	// upstream, where SvmHighestFinalizedSlot is the majority tip. Rejection
	// gates (the getBlock availability guard) need this one: they must not null
	// out a slot some upstream can already serve. Advertising a head to clients
	// needs the majority one, so a lone rogue upstream cannot inflate it.
	SvmHighestFinalizedSlotMax(ctx context.Context) int64
	SvmHighestIndexedSlot(ctx context.Context) int64
	SvmEnforceBlockAvailability() bool
}

// EvmHeadOf returns n.EvmHead(ctx, req), or the zero Head when n is not an
// EVM network.
func EvmHeadOf(n Network, ctx context.Context, req *NormalizedRequest) EvmHead {
	if e, ok := n.(EvmNetwork); ok {
		return e.EvmHead(ctx, req)
	}
	return EvmHead{}
}

// EvmRefreshLatestBlock calls EvmNetwork.EvmRefreshLatestBlock when n is an
// EVM network.
func EvmRefreshLatestBlock(n Network, ctx context.Context) {
	if e, ok := n.(EvmNetwork); ok {
		e.EvmRefreshLatestBlock(ctx)
	}
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
