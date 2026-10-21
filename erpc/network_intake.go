package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
)

// networkIntake is the network's side of every source: each header is
// parsed (check 0), passed through its upstream's identity gate and head
// counter (checks 2 and 3), then voted in the trackers while its source is
// authoritative (check 4). It also owns the sources' liveness.
type networkIntake struct {
	nw      *Network
	tracker *evm.ChainTracker

	mu      sync.Mutex
	sources map[string]*sourceState // upstream id -> state
}

type sourceState struct {
	last indexer.Cursor // newest accepted header
	// behindSince is when the newest known block was first seen above last;
	// zero while it is not.
	behindSince time.Time
	live        bool
}

func newNetworkIntake(nw *Network, tracker *evm.ChainTracker) *networkIntake {
	return &networkIntake{nw: nw, tracker: tracker, sources: make(map[string]*sourceState)}
}

var _ indexer.Intake = (*networkIntake)(nil)

// Header takes one source header.
func (in *networkIntake) Header(upstreamId string, raw json.RawMessage) {
	cur, err := evm.ParseHeader(raw)
	if err != nil {
		in.count(upstreamId, "malformed")
		return
	}
	u := in.upstream(upstreamId)
	if u == nil {
		in.count(upstreamId, "gated_identity")
		return
	}
	in.mu.Lock()
	s := in.sources[upstreamId]
	dup := s != nil && s.last == cur
	in.mu.Unlock()
	if dup {
		in.count(upstreamId, "duplicate")
		return
	}
	poller := u.EvmStatePoller()
	if poller == nil || poller.IsObjectNull() || !poller.ObserveHead(raw) {
		in.count(upstreamId, "gated_identity")
		return
	}
	in.accept(upstreamId, cur)
	vote := in.authoritative(upstreamId)
	evm.ObserveSourceHeader(in.nw, u, raw, vote)
	if vote {
		in.count(upstreamId, "voted")
	} else {
		in.count(upstreamId, "gated_authority")
	}
}

func (in *networkIntake) count(upstreamId, outcome string) {
	telemetry.CounterHandle(telemetry.MetricChainTrackerSourceHeadsTotal,
		in.nw.projectId, in.nw.Label(), upstreamId, outcome).Inc()
}

func (in *networkIntake) upstream(id string) *upstream.Upstream {
	if in.nw.upstreamsRegistry == nil {
		return nil
	}
	for _, u := range in.nw.upstreamsRegistry.GetNetworkUpstreams(context.Background(), in.nw.networkId) {
		if u.Id() == id {
			return u
		}
	}
	return nil
}

// newestKnown is the higher of the tracker's head and the network's served
// tip; never a single vote.
func (in *networkIntake) newestKnown() uint64 {
	newest := uint64(0)
	if tip, ok := in.tracker.Tip(); ok {
		newest = tip.Number
	}
	if served := in.nw.EvmHighestLatestBlockNumber(context.Background()); served > 0 {
		newest = max(newest, uint64(served))
	}
	return newest
}

func (in *networkIntake) accept(upstreamId string, cur indexer.Cursor) {
	newest, bound, now := in.newestKnown(), in.tracker.LiveBound(), time.Now()
	in.mu.Lock()
	s := in.sources[upstreamId]
	if s == nil {
		s = &sourceState{}
		in.sources[upstreamId] = s
	}
	s.last, s.behindSince = cur, time.Time{}
	changed := in.evaluateLocked(upstreamId, s, newest, bound, now)
	in.mu.Unlock()
	if changed {
		evm.LivenessChanged(in.nw)
	}
}

// evaluateLocked updates s.live: a source stays live unless the newest known
// block has stood above its accepted header for longer than the live bound.
func (in *networkIntake) evaluateLocked(upstreamId string, s *sourceState, newest uint64, bound time.Duration, now time.Time) (changed bool) {
	if newest > s.last.Number {
		if s.behindSince.IsZero() {
			s.behindSince = now
		}
	} else {
		s.behindSince = time.Time{}
	}
	live := s.behindSince.IsZero() || now.Sub(s.behindSince) <= bound
	if live == s.live {
		return false
	}
	s.live = live
	v := 0.0
	if live {
		v = 1
	}
	telemetry.GaugeHandle(telemetry.MetricSourceLive, in.nw.projectId, in.nw.Label(), upstreamId).Set(v)
	return true
}

func (in *networkIntake) live(upstreamId string) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	s := in.sources[upstreamId]
	return s != nil && s.live
}

// authoritative: upstreamId is in the selection policy's eligible set, and,
// with failover on, outside the fallback tier or no eligible primary source
// is live.
func (in *networkIntake) authoritative(upstreamId string) bool {
	var self common.Upstream
	primaryLive := false
	for c := range in.nw.tipCandidateUpstreams(context.Background()) {
		if c.Id() == upstreamId {
			self = c
		}
		if !isFallbackTier(c) && in.live(c.Id()) {
			primaryLive = true
		}
	}
	if self == nil {
		return false
	}
	if in.nw.cfg == nil || !in.nw.cfg.Failover.Enabled() || !isFallbackTier(self) {
		return true
	}
	return !primaryLive
}

// livenessInterval is how often, in live bounds, liveness is re-evaluated
// without a new header.
var livenessInterval = func(bound time.Duration) time.Duration { return bound / 4 }

// run re-evaluates liveness until ctx ends, so a silent or lagging source
// stops voting within the live bound.
func (in *networkIntake) run(ctx context.Context) {
	for {
		timer := time.NewTimer(livenessInterval(in.tracker.LiveBound()))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		in.tick()
	}
}

func (in *networkIntake) tick() {
	defer func() {
		if rec := recover(); rec != nil {
			telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
				"network-intake",
				fmt.Sprintf("project:%s network:%s", in.nw.projectId, in.nw.networkId),
				common.ErrorFingerprint(rec),
			).Inc()
			in.nw.logger.Error().Interface("panic", rec).Str("stack", string(debug.Stack())).
				Msg("unexpected panic while evaluating source liveness")
		}
	}()
	newest, bound, now := in.newestKnown(), in.tracker.LiveBound(), time.Now()
	changed := false
	in.mu.Lock()
	for id, s := range in.sources {
		changed = in.evaluateLocked(id, s, newest, bound, now) || changed
	}
	in.mu.Unlock()
	if changed {
		evm.LivenessChanged(in.nw)
	}
}

// EvmSourceVotes reports whether upstreamId's vote counts now: it is a live
// source and authoritative.
func (n *Network) EvmSourceVotes(upstreamId string) bool {
	return n.intake != nil && n.intake.live(upstreamId) && n.intake.authoritative(upstreamId)
}
