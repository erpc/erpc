package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
)

// networkIntake is the network's side of every source: each header is
// parsed (check 0), checked for integrity while it is on (check 1), and
// passed through its upstream's identity gate and head counter (checks 2
// and 3); an accepted header is its source's hash vote. Intake owns who
// votes (check 4, spec §6.2) and which topics a voter feeds (topic
// admission); the health tracker owns how fresh each vote is (§6.1).
type networkIntake struct {
	nw *Network

	mu sync.Mutex
	// last is each upstream's accepted headers: a parentHash-linked path
	// ending at its hash vote, newest last, at most voteLinksKept.
	last map[string][]voteLink
	// hashVoterSince is, per primary voter, since when it has been a hash
	// voter without a break, as the reconciler's passes saw it; leads is
	// whether one has been for primaryLeadAfter.
	hashVoterSince map[string]time.Time
	leads          bool
	// admitted is the voters topic admission admits, as the last pass
	// read them, for per-transaction checks that take no lock.
	admitted atomic.Pointer[[]string]
	// rejected are the newest headers integrity held back, oldest first;
	// a check suspended for the network lets them in again.
	rejected []rejectedHeader
}

type rejectedHeader struct {
	upstreamId string
	raw        json.RawMessage
}

// rejectedKept bounds the headers integrity held back that intake keeps.
const rejectedKept = 64

// primaryLeadAfter is how long a primary voter must stay a hash voter,
// without a break, before topic admission stops admitting the fallback
// tier (spec §6.2).
var primaryLeadAfter = util.NewDuration(60 * time.Second)

func newNetworkIntake(nw *Network) *networkIntake {
	return &networkIntake{nw: nw, last: make(map[string][]voteLink), hashVoterSince: make(map[string]time.Time)}
}

var _ indexer.Intake = (*networkIntake)(nil)

// Subscribed takes a push source's subscribe answer; the reconciler decides
// what it changes.
func (in *networkIntake) Subscribed(upstreamId string, t indexer.Topic, err error) {
	in.nw.sources.subscribed(upstreamId, t, err)
}

// PendingTx takes one pending transaction of a push source into the
// mempool feed while topic admission admits the source, as the last pass
// read it.
func (in *networkIntake) PendingTx(upstreamId, variant string, raw json.RawMessage) {
	if in.nw.mempool == nil {
		return
	}
	if ids := in.admitted.Load(); ids == nil || !slices.Contains(*ids, upstreamId) {
		return
	}
	in.nw.mempool.Add(variant, raw)
}

// Header takes one source header.
func (in *networkIntake) Header(upstreamId string, raw json.RawMessage) {
	in.header(upstreamId, raw, false)
}

// header is Header; replay: raw is a header integrity held back before a
// check was suspended, so it votes only when it is newer than its source's
// newest accepted header: a replay never moves a vote back.
func (in *networkIntake) header(upstreamId string, raw json.RawMessage, replay bool) {
	sh, err := evm.ParseSourceHeader(raw)
	if err != nil {
		in.count(upstreamId, "malformed")
		return
	}
	cur := sh.Cursor()
	u := in.upstream(upstreamId)
	if u == nil {
		return
	}
	in.mu.Lock()
	behind := onVotePath(in.last[upstreamId], cur)
	in.mu.Unlock()
	if behind {
		in.count(upstreamId, "duplicate")
		return
	}
	if !evm.CheckSourceHeader(in.nw, u, sh) {
		in.mu.Lock()
		in.rejected = append(in.rejected, rejectedHeader{upstreamId, raw})
		if over := len(in.rejected) - rejectedKept; over > 0 {
			in.rejected = slices.Delete(in.rejected, 0, over)
		}
		in.mu.Unlock()
		in.count(upstreamId, "gated_integrity")
		return
	}
	poller := u.EvmStatePoller()
	if poller == nil || poller.IsObjectNull() || !poller.ObserveHead(raw) {
		in.count(upstreamId, "gated_identity")
		return
	}
	if !in.accept(upstreamId, sh, replay) {
		in.count(upstreamId, "duplicate")
		return
	}
	// The hash vote is recorded before the trackers see the header, so the
	// tracker that handles it counts its sender as a hash voter.
	if mt := in.nw.metricsTracker; mt != nil {
		mt.RecordHashVote(u, int64(cur.Number))
		mt.SampleSourceHeaderTime(u, int64(cur.Number), sh.Timestamp())
	}
	evm.ObserveSourceHeader(in.nw, u, sh)
	if in.isVoter(upstreamId) {
		in.count(upstreamId, "voted")
	} else {
		in.count(upstreamId, "gated_voter")
	}
}

// replayRejected runs the headers integrity held back through intake
// again, oldest first, on its own goroutine: a check was suspended for the
// network, so those headers are kept (spec §12).
func (in *networkIntake) replayRejected() {
	in.mu.Lock()
	held := in.rejected
	in.rejected = nil
	in.mu.Unlock()
	if len(held) == 0 {
		return
	}
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
					"network-intake-replay",
					fmt.Sprintf("project:%s network:%s", in.nw.projectId, in.nw.networkId),
					common.ErrorFingerprint(rec),
				).Inc()
				in.nw.logger.Error().Interface("panic", rec).Str("stack", string(debug.Stack())).
					Msg("unexpected panic while replaying source headers")
			}
		}()
		for _, r := range held {
			in.header(r.upstreamId, r.raw, true)
		}
	}()
}

// disconnected expires a push source's hash vote at once: its socket
// closed, so nothing it said is fresh any more (spec §15.1). The trackers
// read their voters again and the reconciler rechecks the topics.
func (in *networkIntake) disconnected(upstreamId string) {
	u := in.upstream(upstreamId)
	if u == nil || in.nw.metricsTracker == nil {
		return
	}
	in.nw.metricsTracker.ExpireHashVote(u)
	evm.VotersChanged(in.nw)
	in.nw.sources.wake()
}

// hashVote is u's newest hash vote (spec §6.1, §6.3); false before its
// first.
func (in *networkIntake) hashVote(u common.Upstream) (health.HashVote, bool) {
	if in.nw.metricsTracker == nil {
		return health.HashVote{}, false
	}
	return in.nw.metricsTracker.HashVote(u)
}

// newestHashVoteAt is when the newest hash vote of any upstream of the
// network reached this instance; zero before one (the gRPC stall rule,
// spec §10.2).
func (n *Network) newestHashVoteAt() (at time.Time) {
	if n.intake == nil || n.upstreamsRegistry == nil {
		return at
	}
	for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(context.Background(), n.networkId) {
		if v, ok := n.intake.hashVote(u); ok && v.At.After(at) {
			at = v.At
		}
	}
	return at
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

// voteLink is one accepted header of an upstream.
type voteLink struct {
	cur    indexer.Cursor
	parent [32]byte
}

// voteLinksKept bounds the path of accepted headers kept per upstream.
const voteLinksKept = 64

// onVotePath reports whether cur is on path, the upstream's accepted headers
// up to its hash vote: its vote, or an ancestor of it. Such a header never
// moves the vote back (spec §6.5 rule 3); a lower header on another branch
// is a rewind, left to the trackers.
func onVotePath(path []voteLink, cur indexer.Cursor) bool {
	if len(path) == 0 || cur.Number > path[len(path)-1].cur.Number {
		return false
	}
	for _, l := range path {
		if l.cur == cur || l.cur.Number == cur.Number+1 && l.parent == cur.Hash {
			return true
		}
	}
	return false
}

// accept makes sh its upstream's hash vote unless it is on the vote's path;
// onlyNewer: only when it is above the vote. Compared and set in one
// section.
func (in *networkIntake) accept(upstreamId string, sh evm.SourceHeader, onlyNewer bool) bool {
	cur, parent := sh.Cursor(), sh.Parent()
	in.mu.Lock()
	defer in.mu.Unlock()
	path := in.last[upstreamId]
	if n := len(path); n > 0 {
		if cur.Number <= path[n-1].cur.Number && (onlyNewer || onVotePath(path, cur)) {
			return false
		}
		if path[n-1].cur.Hash != parent {
			path = path[:0]
		}
	}
	path = append(path, voteLink{cur, parent})
	if over := len(path) - voteLinksKept; over > 0 {
		path = slices.Delete(path, 0, over)
	}
	in.last[upstreamId] = path
	return true
}

// reported reports whether a source header of upstreamId was accepted.
func (in *networkIntake) reported(upstreamId string) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	_, seen := in.last[upstreamId]
	return seen
}

// syncing: u reports itself syncing (#202); it is no voter.
func syncing(u common.Upstream) bool {
	eu, ok := u.(common.EvmUpstream)
	return ok && eu.EvmSyncingState() == common.EvmSyncingStateSyncing
}

// voters lists the voters of the lane of selector ("" network-wide) in
// selection order (spec §6.2): upstreams in the policy's eligible set that
// the selector matches, with a state poller and not syncing, of the best
// rank present (tipVoterRank: live heads, or none yet, before benched ones
// before configured caps). No tier filter: the fallback tier votes too. cands are the
// eligible, matched upstreams that are not syncing, voters or not.
func (in *networkIntake) voters(selector string) (voters, cands []common.Upstream) {
	best := -1
	for c := range in.nw.EvmRoutableUpstreams(context.Background()) {
		if selector != "" {
			if m, _ := common.UpstreamMatchesSelector(selector, c); !m {
				continue
			}
		}
		if syncing(c) {
			continue
		}
		cands = append(cands, c)
		_, rank, ok := tipVoterRank(c, false, in.nw.metricsTracker)
		if !ok {
			// No head yet is no reason to leave the ballot of voters: a
			// poll source gets its first head only once admitted.
			eu, isEvm := c.(common.EvmUpstream)
			if !isEvm || eu.EvmStatePoller() == nil || eu.EvmStatePoller().IsObjectNull() {
				continue
			}
			rank = 0
		}
		if best >= 0 && rank > best {
			continue
		}
		if best < 0 || rank < best {
			best, voters = rank, voters[:0]
		}
		voters = append(voters, c)
	}
	return voters, cands
}

// isVoter reports whether upstreamId is a network-wide voter now.
func (in *networkIntake) isVoter(upstreamId string) bool {
	vs, _ := in.voters("")
	return slices.ContainsFunc(vs, func(u common.Upstream) bool { return u.Id() == upstreamId })
}

// hashVoter reports whether voter u is a hash voter: its hash age is at
// most w and its socket stayed up since (spec §6.1, §15.1). at is when it
// stops being one, zero while its age does not run.
func (in *networkIntake) hashVoter(u common.Upstream, w time.Duration, now time.Time) (ok bool, at time.Time) {
	v, voted := in.hashVote(u)
	return freshVote(v, voted, w, now)
}

// freshVote is hashVoter over a vote already read.
func freshVote(v health.HashVote, voted bool, w time.Duration, now time.Time) (ok bool, at time.Time) {
	if !voted || v.Expired || v.Age > w {
		return false, time.Time{}
	}
	if v.Age > 0 {
		at = now.Add(w - v.Age)
	}
	return true, at
}

// admit is topic admission (spec §6.2), run by each reconciler pass, at
// least every W: it notes which primary voters are hash voters, whether
// one has been for primaryLeadAfter without a break, and publishes the
// admitted voters:
//
//	admitted(u) = voter(u) and (failover off, or u is not fallback tier,
//	                            or no primary has been a hash voter for 60s without a break)
func (in *networkIntake) admit(now time.Time, w time.Duration) {
	voters, _ := in.voters("")
	in.mu.Lock()
	defer in.mu.Unlock()
	leads := false
	for _, u := range voters {
		if isFallbackTier(u) {
			continue
		}
		if ok, _ := in.hashVoter(u, w, now); !ok {
			delete(in.hashVoterSince, u.Id())
			continue
		}
		since, seen := in.hashVoterSince[u.Id()]
		if !seen {
			since = now
			in.hashVoterSince[u.Id()] = now
		}
		leads = leads || now.Sub(since) >= primaryLeadAfter.Load()
	}
	for id := range in.hashVoterSince {
		if !slices.ContainsFunc(voters, func(u common.Upstream) bool { return u.Id() == id }) {
			delete(in.hashVoterSince, id)
		}
	}
	in.leads = leads
	tiers := in.nw.cfg != nil && in.nw.cfg.Failover.Enabled()
	ids := make([]string, 0, len(voters))
	for _, u := range voters {
		if !tiers || !isFallbackTier(u) || !leads {
			ids = append(ids, u.Id())
		}
	}
	if old := in.admitted.Load(); old == nil || !slices.Equal(*old, ids) {
		in.admitted.Store(&ids)
	}
}

// admits reports whether topic admission admitted upstreamId at the last
// pass.
func (in *networkIntake) admits(upstreamId string) bool {
	ids := in.admitted.Load()
	return ids != nil && slices.Contains(*ids, upstreamId)
}

// EvmHashVoters is the voter set of the tracker of selector ("" network
// wide), read once per tracker message (spec §6.2, §6.5): the lane's voters
// in selection order; the hash voters, those whose hash age is at most w,
// and, while cold, the eligible sources that have not reported a header
// yet, so a cold instance's first header waits for a second; q.Of of them;
// each upstream's vendor and the evidence quorum.
func (n *Network) EvmHashVoters(selector string, w time.Duration, cold bool) evm.HashVoters {
	var q common.ServedTipQuorum
	if n.cfg != nil {
		q = n.cfg.Evm.ServedTipQuorum()
	}
	hv := evm.HashVoters{Evidence: 1, Vendors: make(map[string]string)}
	if n.intake == nil || n.upstreamsRegistry == nil {
		hv.Need = q.Of(0)
		return hv
	}
	var vendors []string
	for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(context.Background(), n.networkId) {
		hv.Vendors[u.Id()] = u.VendorName()
		if !slices.Contains(vendors, u.VendorName()) {
			vendors = append(vendors, u.VendorName())
		}
	}
	if len(vendors) >= 2 {
		hv.Evidence = 2
	}
	now := time.Now()
	voters, cands := n.intake.voters(selector)
	var stale []string
	for _, u := range voters {
		hv.Voters = append(hv.Voters, u.Id())
		v, voted := n.intake.hashVote(u)
		if voted && u.Config().IsSubscriptionSource() && (v.Expired || v.Age > w) {
			stale = append(stale, u.Id())
		}
		ok, at := freshVote(v, voted, w, now)
		if !ok {
			continue
		}
		hv.Hash = append(hv.Hash, u.Id())
		if !at.IsZero() && (hv.Next.IsZero() || at.Before(hv.Next)) {
			hv.Next = at
		}
	}
	if selector == "" {
		n.setHashStale(stale)
	}
	if cold {
		for _, u := range cands {
			if u.Config().IsSubscriptionSource() && !n.intake.reported(u.Id()) && !slices.Contains(hv.Hash, u.Id()) {
				hv.Hash = append(hv.Hash, u.Id())
			}
		}
	}
	hv.Need = q.Of(len(hv.Hash))
	return hv
}

// setHashStale publishes the network-wide sources whose hash age exceeds W
// or whose socket closed (spec §6.1): routing tries them after the rest, so
// a half-open socket holds no request. A source behind by less than W, or an
// upstream that is no source, keeps its rank: its poll schedule is no
// evidence of a fault.
func (n *Network) setHashStale(ids []string) {
	old := n.hashStale.Load()
	switch {
	case len(ids) == 0 && old != nil:
		n.hashStale.Store(nil)
	case len(ids) > 0 && (old == nil || !slices.Equal(*old, ids)):
		n.hashStale.Store(&ids)
	}
}

// EvmForkDemoted takes the network-wide tracker's minority-fork set: the
// voters whose hash vote stayed off the pins for longer than W (spec §6.3).
// They lose their number vote and go last in routing.
func (n *Network) EvmForkDemoted(ids []string) {
	if len(ids) == 0 {
		n.forkDemoted.Store(nil)
	} else {
		n.forkDemoted.Store(&ids)
	}
	if n.logger != nil {
		n.logger.Info().Strs("upstreams", ids).Msg("minority-fork demotion changed: demoted voters lose their number vote and go last in routing")
	}
}

// EvmHashVotersChanged is the network-wide tracker's note that its hash
// voters changed: the reconciler rechecks topic admission (spec §5.2).
func (n *Network) EvmHashVotersChanged() {
	n.sources.wake()
}

// isForkDemoted reports whether u is on a minority fork (EvmForkDemoted).
func (n *Network) isForkDemoted(u common.Upstream) bool {
	ids := n.forkDemoted.Load()
	return ids != nil && slices.Contains(*ids, u.Id())
}

// EvmHeadsDemanded reports whether an open tap needs newHeads.
func (n *Network) EvmHeadsDemanded() bool {
	return n.taps != nil && slices.Contains(n.taps.Demand(n.Label()), indexer.TopicNewHeads)
}

// ObserveFetchedHead moves an upstream's head counter to a latest header
// the chain tracker fetched from it, as a source header moves its source's;
// ObserveHead verifies a major jump first.
func (n *Network) ObserveFetchedHead(upstreamId string, raw json.RawMessage) {
	if n.intake == nil {
		return
	}
	if u := n.intake.upstream(upstreamId); u != nil {
		if p := u.EvmStatePoller(); p != nil && !p.IsObjectNull() {
			p.ObserveHead(raw)
		}
	}
}

// EvmTaps is the network's taps and the key its demand is counted under:
// the network label, which also labels the tap metrics (spec §15). Taps
// belong to one project's network, so the label alone is unique in them.
func (n *Network) EvmTaps() (*indexer.Taps, string) {
	return n.taps, n.Label()
}
