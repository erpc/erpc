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
	// Ceiling is the raw max report of the lane's counted upstreams, 0
	// while any of them has none.
	Ceiling int64
	// OldestReportAt is when the oldest counted report's evidence was
	// taken: a poll's send time, a push's receive time; zero while one has
	// none.
	OldestReportAt time.Time
	// CeilingPeak is the highest Ceiling this instance published for the
	// lane, and CeilingFellAt when Ceiling last fell below the one before
	// it: a cursor between the two heights names a block no upstream
	// reports any more (spec §10.1, §19 C16).
	CeilingPeak   int64
	CeilingFellAt time.Time
}

// HeadRule is the refusal rule's row for block N on a request (spec §6.4).
type HeadRule uint8

const (
	// HeadServe: N ≤ Latest, or Latest unknown (tags and numbers reach
	// upstreams unchanged).
	HeadServe HeadRule = iota
	// HeadBand: Latest < N ≤ ceiling, or the ceiling is unknown.
	HeadBand
	// HeadFuture: above the ceiling, every counted report younger than
	// 1 bt: no counted upstream has N yet.
	HeadFuture
	// HeadStale: above the ceiling, a counted report 1 bt old or older:
	// those reports are Asked before N is decided again. Never a request's
	// verdict.
	HeadStale
)

// Rule is the refusal rule's row for block n at now, bt the block time (> 0).
func (h EvmHead) Rule(n int64, bt time.Duration, now time.Time) HeadRule {
	switch {
	case h.Latest <= 0 || n <= h.Latest:
		return HeadServe
	case h.Ceiling <= 0 || n <= h.Ceiling:
		return HeadBand
	case h.OldestReportAt.IsZero() || now.Sub(h.OldestReportAt) >= bt:
		return HeadStale
	}
	return HeadFuture
}

// HeadRefusal is the refusal rule's verdict for one request's block, taken
// once (spec §6.4), and the retryable misses its legs got since.
type HeadRefusal struct {
	Rule  HeadRule
	Block int64
	// Ceiling is the ceiling Block was decided against: the one the Ask
	// returned when Refreshed.
	Ceiling int64
	// Refreshed: N was decided again after its stale reports were Asked.
	Refreshed bool
	// Waited: N, near the ceiling, waited up to W for it (spec §6.4 near
	// future).
	Waited bool
	// missed: a leg of the request took its retryable miss.
	missed atomic.Bool
	// answered: a leg took its answer after the miss.
	answered atomic.Bool
}

// FutureRule is the refusal counter's rule for a future verdict:
// future_after_wait when it waited for the ceiling, future_after_refresh
// when its stale reports were Asked first.
func (r *HeadRefusal) FutureRule() string {
	switch {
	case r.Waited:
		return "future_after_wait"
	case r.Refreshed:
		return "future_after_refresh"
	}
	return "future"
}

// RefusalLeg is one execution of a request: a consensus slot, a hedge
// leg, or a retry round's. One goroutine runs it.
type RefusalLeg struct{ missed bool }

// MissVerdict is what an empty or missing answer above Latest is to the
// leg that got it.
type MissVerdict uint8

const (
	// MissRetry: the leg's one retryable miss; the leg retries.
	MissRetry MissVerdict = iota
	// MissFinal: the answer after the leg's miss, sent as the upstream
	// gave it.
	MissFinal
)

// AboveLatest reports whether the request's block is above its Latest.
func (r *HeadRefusal) AboveLatest() bool {
	return r != nil && r.Rule != HeadServe
}

// Final reports whether an empty answer now may be the request's answer:
// a leg took its miss.
func (r *HeadRefusal) Final() bool {
	return r.AboveLatest() && r.missed.Load()
}

// Spent reports whether a leg took its answer after its miss, so an empty
// or missing answer may no longer be retried.
func (r *HeadRefusal) Spent() bool {
	return r != nil && r.answered.Load()
}

// Miss records an empty or missing answer leg got for a block above
// Latest: each leg's first is its retryable miss, so a miss in one leg
// never stops another (spec §6.4: every leg gets the band's one retry);
// the leg's next is final.
func (r *HeadRefusal) Miss(leg *RefusalLeg) MissVerdict {
	r.missed.Store(true)
	if !leg.missed {
		leg.missed = true
		return MissRetry
	}
	r.answered.Store(true)
	return MissFinal
}

// Retried reports whether a leg of the request got its retryable miss.
func (r *HeadRefusal) Retried() bool {
	return r != nil && r.missed.Load()
}

// Refuse records that the request's null was decided with no dispatch (a
// future block under shortCircuitFutureBlocks): that null is its answer.
func (r *HeadRefusal) Refuse() {
	r.missed.Store(true)
	r.answered.Store(true)
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
