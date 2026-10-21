package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
)

// networkIntake is the network's side of every source: each header is
// parsed (check 0) and passed through its upstream's identity gate and head
// counter (checks 2 and 3); an accepted header becomes its source's vote.
// Intake owns the one predicate that decides whether a vote counts:
// liveness and authority (check 4).
type networkIntake struct {
	nw *Network

	mu      sync.Mutex
	sources map[string]*sourceState // upstream id -> state
	// primaryLeads: some primary source has been live and policy-eligible
	// for primaryLeadAfter; fallback-tier sources are not authoritative.
	primaryLeads bool
}

type sourceState struct {
	last indexer.Cursor // newest accepted header
	// behindSince is when the newest known block was first seen above last;
	// zero while it is not.
	behindSince time.Time
	live        bool
	// liveSince is when the source last became live; eligibleSince when a
	// primary source last entered the policy's eligible set. Zero while not.
	liveSince     time.Time
	eligibleSince time.Time
	// eligibleNow is authority's per-pass mark, under in.mu: a primary in
	// the eligible set it read.
	eligibleNow bool
}

// primaryLeadAfter is how long a primary source must stay live and eligible,
// without a break, before fallback-tier sources stop being authoritative.
var primaryLeadAfter = util.NewDuration(60 * time.Second)

func newNetworkIntake(nw *Network) *networkIntake {
	return &networkIntake{nw: nw, sources: make(map[string]*sourceState)}
}

var _ indexer.Intake = (*networkIntake)(nil)

// Subscribed takes a push source's subscribe answer; the reconciler decides
// what it changes.
func (in *networkIntake) Subscribed(_ string, t indexer.Topic, err error) {
	in.nw.sources.subscribed(t, err)
}

// Header takes one source header.
func (in *networkIntake) Header(upstreamId string, raw json.RawMessage) {
	cur, err := evm.ParseHeader(raw)
	if err != nil {
		in.count(upstreamId, "malformed")
		return
	}
	u := in.upstream(upstreamId)
	if u == nil {
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
	evm.ObserveSourceHeader(in.nw, u, raw)
	if in.votes(upstreamId) {
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
	if tip, ok := in.nw.chainTracker.Tip(); ok {
		newest = tip.Number
	}
	if served := in.nw.EvmHighestLatestBlockNumber(context.Background()); served > 0 {
		newest = max(newest, uint64(served))
	}
	return newest
}

func (in *networkIntake) accept(upstreamId string, cur indexer.Cursor) {
	newest, bound, now := in.newestKnown(), in.nw.chainTracker.LiveBound(), time.Now()
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
		in.changed()
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
	s.liveSince = time.Time{}
	if live {
		v = 1
		s.liveSince = now
	}
	telemetry.GaugeHandle(telemetry.MetricSourceLive, in.nw.projectId, in.nw.Label(), upstreamId).Set(v)
	return true
}

// changed tells the trackers to count votes again and the reconciler to
// recheck which topics run: liveness or authority moved.
func (in *networkIntake) changed() {
	evm.LivenessChanged(in.nw)
	in.nw.sources.wake()
}

func (in *networkIntake) live(upstreamId string) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	s := in.sources[upstreamId]
	return s != nil && s.live
}

// authoritative: upstreamId is in the selection policy's eligible set, and,
// with failover on, outside the fallback tier or no primary source leads.
func (in *networkIntake) authoritative(upstreamId string) bool {
	primary, eligible, leads := in.authority(upstreamId)
	return eligible && in.admits(primary, leads)
}

// admits is the tier half of authority: with failover on, a fallback-tier
// upstream is authoritative only while no primary source leads.
func (in *networkIntake) admits(primary, leads bool) bool {
	return in.nw.cfg == nil || !in.nw.cfg.Failover.Enabled() || primary || !leads
}

// authoritativeIn lists, in the policy's selection order, the authoritative
// upstreams; for a group selector, the eligible upstreams it matches, whose
// headers vote before the authority gate.
func (in *networkIntake) authoritativeIn(selector string) []string {
	_, _, leads := in.authority("")
	var out []string
	for c := range in.nw.tipCandidateUpstreams(context.Background()) {
		if selector != "" {
			if m, _ := common.UpstreamMatchesSelector(selector, c); m {
				out = append(out, c.Id())
			}
			continue
		}
		if in.admits(!isFallbackTier(c), leads) {
			out = append(out, c.Id())
		}
	}
	return out
}

// authority reads the policy's eligible set now: whether upstreamId is in
// it and outside the fallback tier, and whether some primary source has
// been live and eligible for primaryLeadAfter. A primary counts as live
// only while it is eligible. The set is read under in.mu, so a caller
// holding an older set cannot overwrite eligibleSince after a newer one.
func (in *networkIntake) authority(upstreamId string) (primary, eligible, leads bool) {
	in.mu.Lock()
	now := time.Now()
	for _, s := range in.sources {
		s.eligibleNow = false
	}
	for c := range in.nw.tipCandidateUpstreams(context.Background()) {
		isPrimary := !isFallbackTier(c)
		if c.Id() == upstreamId {
			primary, eligible = isPrimary, true
		}
		if s := in.sources[c.Id()]; s != nil && isPrimary {
			s.eligibleNow = true
		}
	}
	for _, s := range in.sources {
		if !s.eligibleNow {
			s.eligibleSince = time.Time{}
			continue
		}
		if s.eligibleSince.IsZero() {
			s.eligibleSince = now
		}
		if s.live && now.Sub(later(s.liveSince, s.eligibleSince)) >= primaryLeadAfter.Load() {
			leads = true
		}
	}
	flipped := leads != in.primaryLeads
	in.primaryLeads = leads
	in.mu.Unlock()
	if flipped {
		in.changed()
	}
	return primary, eligible, leads
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// livenessInterval is how often, in live bounds, liveness is re-evaluated
// without a new header.
var livenessInterval = func(bound time.Duration) time.Duration { return bound / 4 }

// run re-evaluates liveness until ctx ends, so a silent or lagging source
// stops voting within the live bound.
func (in *networkIntake) run(ctx context.Context) {
	for {
		timer := time.NewTimer(livenessInterval(in.nw.chainTracker.LiveBound()))
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
	newest, bound, now := in.newestKnown(), in.nw.chainTracker.LiveBound(), time.Now()
	changed := false
	in.mu.Lock()
	for id, s := range in.sources {
		changed = in.evaluateLocked(id, s, newest, bound, now) || changed
	}
	in.mu.Unlock()
	if changed {
		in.changed()
	}
	// A primary's lead begins with time alone, without a header or a
	// liveness change; authority notices it.
	in.authority("")
}

// votes is the one predicate for whether a source's vote counts: the
// source is live and authoritative now.
func (in *networkIntake) votes(upstreamId string) bool {
	return in.live(upstreamId) && in.authoritative(upstreamId)
}

func (in *networkIntake) anyLive() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, s := range in.sources {
		if s.live {
			return true
		}
	}
	return false
}

// EvmSourceVotes reports whether upstreamId's vote counts now in the tracker
// of selector ("" network-wide); the trackers count only these votes. A
// group tracker's upstreams vote while live, before the authority gate.
func (n *Network) EvmSourceVotes(upstreamId, selector string) bool {
	if n.intake == nil {
		return false
	}
	if selector != "" {
		return n.intake.live(upstreamId)
	}
	return n.intake.votes(upstreamId)
}

// EvmAnySourceLive reports whether any of the network's sources is live.
func (n *Network) EvmAnySourceLive() bool {
	return n.intake != nil && n.intake.anyLive()
}

// EvmAuthoritativeUpstreams lists the upstreams a tracker of selector asks
// for latest (settles, no-source asks), in selection order.
func (n *Network) EvmAuthoritativeUpstreams(selector string) []string {
	if n.intake == nil {
		return nil
	}
	return n.intake.authoritativeIn(selector)
}

// EvmHeadsDemanded reports whether an open tap needs newHeads.
func (n *Network) EvmHeadsDemanded() bool {
	return n.taps != nil && slices.Contains(n.taps.Demand(indexerNetworkKey(n)), indexer.TopicNewHeads)
}

// EvmTaps is the network's taps and the key its demand is counted under.
func (n *Network) EvmTaps() (*indexer.Taps, string) {
	return n.taps, indexerNetworkKey(n)
}
