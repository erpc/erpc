package common

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
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
	// EvmLaneHead is the published Head of the lane keyed groupKey, ""
	// being the network-wide lane, materialized from selector on first use:
	// for readers bound to a lane, not to a request (spec §6.4, §6.5).
	EvmLaneHead(groupKey, selector string) EvmHead
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

// HeadRule is the refusal rule's row for block N on a request (spec §6.4).
type HeadRule uint8

const (
	// HeadServe: N ≤ Latest, or Latest unknown (tags and numbers reach
	// upstreams unchanged).
	HeadServe HeadRule = iota
	// HeadBand: Latest < N ≤ ceiling + ceiling age / bt, or the ceiling is
	// unknown: an empty answer is a retryable miss once.
	HeadBand
	// HeadFuture: above the band on a ceiling younger than 1 bt: an empty
	// answer is final.
	HeadFuture
	// HeadStale: above the band on an older ceiling: Ask it, then decide
	// again.
	HeadStale
)

// Rule is the refusal rule's row for block n at now, bt the block time (> 0).
func (h EvmHead) Rule(n int64, bt time.Duration, now time.Time) HeadRule {
	if h.Latest <= 0 || n <= h.Latest {
		return HeadServe
	}
	if h.Ceiling <= 0 {
		return HeadBand
	}
	age := max(now.Sub(h.CeilingAt), 0)
	switch {
	case n <= h.Ceiling+int64(age/bt):
		return HeadBand
	case age < bt:
		return HeadFuture
	}
	return HeadStale
}

// HeadRefusal is the refusal rule's verdict for one request's block, taken
// once (spec §6.4), and the empty answers it got since.
type HeadRefusal struct {
	Rule  HeadRule // HeadServe, HeadBand or HeadFuture
	Block int64
	// Asked: the ceiling was stale and Asked before Rule was decided.
	Asked  bool
	misses atomic.Int32
}

// AboveLatest reports whether the request's block is above its Latest.
func (r *HeadRefusal) AboveLatest() bool {
	return r != nil && r.Rule != HeadServe
}

// Final reports whether an empty answer now is the request's answer: a
// future block's, or a band block's after its one miss.
func (r *HeadRefusal) Final() bool {
	return r != nil && (r.Rule == HeadFuture || r.Rule == HeadBand && r.misses.Load() > 0)
}

// Spent reports whether an empty or missing answer may no longer be
// retried: a future block's after one, a band block's after two.
func (r *HeadRefusal) Spent() bool {
	if r == nil {
		return false
	}
	switch r.Rule {
	case HeadFuture:
		return r.misses.Load() > 0
	case HeadBand:
		return r.misses.Load() > 1
	}
	return false
}

// Miss records an empty or missing answer; it reports true for the band's
// one retryable miss.
func (r *HeadRefusal) Miss() bool {
	if r == nil || r.Rule == HeadServe {
		return false
	}
	return r.misses.Add(1) == 1 && r.Rule == HeadBand
}

// Retried reports whether the request got an empty or missing answer
// before its last one.
func (r *HeadRefusal) Retried() bool {
	return r != nil && r.misses.Load() > 0
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
