package erpc

import (
	"cmp"
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
	// last is each upstream's accepted headers, ending at its hash vote
	// (votePath).
	last map[string]votePath
	// streams is each source's newest header number from its own channel
	// (spec §6.2): a push for a push source, a poll at its cadence for a
	// poll source. streamingSince is, per primary voter, since when it has
	// streamed without a break, as the reconciler's passes saw it.
	streams        map[string]int64
	streamingSince map[string]time.Time
	// admitted is the voters topic admission admits, as the last pass
	// read them, for per-transaction checks that take no lock.
	admitted atomic.Pointer[[]string]
	// voterIds are the network-wide voters as they were last read, and the
	// eligible set they were read from, for counting each header's outcome.
	voterIds atomic.Pointer[voterSnapshot]
	// vendors is the network's upstreams' vendors, kept until the upstream
	// set or a vendor changes.
	vendors atomic.Pointer[upstreamVendors]
	// rejected are the newest headers integrity held back, oldest first;
	// a check suspended for the network lets them in again.
	rejected []rejectedHeader
	// heldPollAt is, per upstream, when the network last polled it for a
	// held vote need (EvmPollHeads).
	heldPollAt map[string]time.Time
	// pollFailing is, per upstream whose node fails its latest polls, the
	// first and last failure since its last success (spec §6.1).
	pollFailing map[string]failedPolls
}

// failedPolls is the first and last of an upstream's failed latest polls
// with no success between.
type failedPolls struct{ first, last time.Time }

// span is how long the polls have failed, first to last.
func (f failedPolls) span() time.Duration { return f.last.Sub(f.first) }

type rejectedHeader struct {
	upstreamId string
	raw        json.RawMessage
}

// rejectedKept bounds the headers integrity held back that intake keeps.
const rejectedKept = 64

// primaryLeadAfter is how long a primary source must stream, without a
// break, before topic admission stops admitting the fallback tier (spec
// §6.2).
var primaryLeadAfter = util.NewDuration(60 * time.Second)

func newNetworkIntake(nw *Network) *networkIntake {
	return &networkIntake{nw: nw, last: make(map[string]votePath), streams: make(map[string]int64), streamingSince: make(map[string]time.Time), heldPollAt: make(map[string]time.Time), pollFailing: make(map[string]failedPolls)}
}

// pushIntake is a push source's channel into intake: its headers are the
// source's stream (spec §6.2).
type pushIntake struct{ *networkIntake }

func (p pushIntake) Header(upstreamId string, raw json.RawMessage) {
	p.header(upstreamId, raw, evm.HeaderStreamed, false)
}

var (
	_ indexer.Intake = pushIntake{}
	_ evm.PollIntake = (*networkIntake)(nil)
)

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

// Polled takes the header a poll of upstreamId's latest fetched.
func (in *networkIntake) Polled(upstreamId string, raw json.RawMessage, kind evm.HeaderKind) {
	in.mu.Lock()
	f, failed := in.pollFailing[upstreamId]
	delete(in.pollFailing, upstreamId)
	in.mu.Unlock()
	if failed && f.span() >= in.w() {
		evm.VotersChanged(in.nw)
		in.nw.sources.wake()
	}
	in.header(upstreamId, raw, kind, false)
}

// PollFailed notes a poll of upstreamId's latest that its node failed. A
// voter that is no push source is no hash voter once its failed polls span
// W with no success since (spec §6.1), so the trackers read their voters
// again when a failure makes the span reach W (EvmHashVoters).
func (in *networkIntake) PollFailed(upstreamId string) {
	now := time.Now()
	in.mu.Lock()
	f, failing := in.pollFailing[upstreamId]
	if !failing {
		f.first = now
	}
	was := f.span()
	f.last = now
	in.pollFailing[upstreamId] = f
	in.mu.Unlock()
	if w := in.w(); was < w && f.span() >= w {
		evm.VotersChanged(in.nw)
	}
}

// pollsFailedFor is how long upstreamId's failed latest polls span with no
// success since; 0 while none failed since its last success.
func (in *networkIntake) pollsFailedFor(upstreamId string) time.Duration {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.pollFailing[upstreamId].span()
}

// w is the network's freshness window, as its trackers read it.
func (in *networkIntake) w() time.Duration {
	return health.FreshnessWindow(in.nw.EvmBlockTime())
}

// header takes a source header that came as kind; replay: raw is a header
// integrity held back before a check was suspended, so it votes only when
// it is newer than its source's newest accepted header: a replay never
// moves a vote back. A fork poll's answer moves the vote whatever the path
// says: it is the node's head now.
func (in *networkIntake) header(upstreamId string, raw json.RawMessage, kind evm.HeaderKind, replay bool) {
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
	stream, fresh := kind == evm.HeaderStreamed && !replay, kind == evm.HeaderForkPolled
	in.mu.Lock()
	path := in.last[upstreamId]
	current := len(path) > 0 && path[len(path)-1].cur == cur
	behind := current || !fresh && !path.moves(cur, sh.Parent())
	in.mu.Unlock()
	if behind {
		// After a reconnect the first header is the current vote; on a
		// halted chain no newer one comes, so it renews the vote.
		if mt := in.nw.metricsTracker; current && mt != nil && mt.RenewHashVote(u) {
			evm.VotersChanged(in.nw)
			in.nw.sources.wake()
		}
		// A poll often brings a push source's header first.
		if current && stream {
			in.noteStream(upstreamId, int64(cur.Number))
		}
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
	if !in.accept(upstreamId, sh, replay, fresh) {
		in.count(upstreamId, "duplicate")
		return
	}
	// The hash vote is recorded before the trackers see the header, so the
	// tracker that handles it counts its sender as a hash voter.
	if mt := in.nw.metricsTracker; mt != nil {
		mt.RecordHashVote(u, int64(cur.Number), replay)
	}
	if stream {
		in.noteStream(upstreamId, int64(cur.Number))
	}
	evm.ObserveSourceHeader(in.nw, u, sh)
	if in.isVoter(upstreamId) {
		in.count(upstreamId, "voted")
	} else {
		in.count(upstreamId, "gated_voter")
	}
}

// noteStream notes number as upstreamId's stream head. No wake is needed
// when another header passes it: its hash age starts then, and the
// reconciler, which passes at least every W, schedules the end of its
// streaming at its next pass, within W of that start (admit).
func (in *networkIntake) noteStream(upstreamId string, number int64) {
	in.mu.Lock()
	in.streams[upstreamId] = number
	in.mu.Unlock()
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
			in.header(r.upstreamId, r.raw, evm.HeaderPolled, true)
		}
	}()
}

// disconnected expires a push source's hash vote at once: its socket
// closed, so nothing it said is fresh any more (spec §15.1), and its
// stream ended. The trackers read their voters again and the reconciler
// rechecks the topics.
func (in *networkIntake) disconnected(upstreamId string) {
	in.mu.Lock()
	delete(in.streams, upstreamId)
	in.mu.Unlock()
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

// votePath is an upstream's accepted headers up to its hash vote, newest
// last, by ascending number. Links across a gap stay: a missed header
// proves no reorg below it.
type votePath []voteLink

// moves reports whether cur, child of parent, moves the vote: it is above
// the vote, or a rewind, a lower header where the path holds another hash
// or whose parent breaks the path's ancestry. Its vote or an ancestor of it
// never moves the vote back (spec §6.5 rule 3), nor does a lower header the
// path says nothing about, such as a lagging poll's across a gap.
func (p votePath) moves(cur indexer.Cursor, parent [32]byte) bool {
	if len(p) == 0 || cur.Number > p[len(p)-1].cur.Number {
		return true
	}
	other := false
	for _, l := range p {
		switch {
		case l.cur.Number == cur.Number:
			if l.cur.Hash == cur.Hash {
				return false
			}
			other = true
		case l.cur.Number == cur.Number+1:
			if l.parent == cur.Hash {
				return false
			}
			other = true
		case l.cur.Number+1 == cur.Number:
			other = other || l.cur.Hash != parent
		}
	}
	return other
}

// with is p with cur, child of parent, as its vote: links at or above cur
// go (a rewind), and so does the link below it that names another hash
// than parent.
func (p votePath) with(cur indexer.Cursor, parent [32]byte) votePath {
	i, _ := slices.BinarySearchFunc(p, cur.Number, func(l voteLink, n uint64) int { return cmp.Compare(l.cur.Number, n) })
	p = p[:i]
	if n := len(p); n > 0 && p[n-1].cur.Number+1 == cur.Number && p[n-1].cur.Hash != parent {
		p = p[:n-1]
	}
	p = append(p, voteLink{cur, parent})
	if over := len(p) - voteLinksKept; over > 0 {
		p = slices.Delete(p, 0, over)
	}
	return p
}

// accept makes sh its upstream's hash vote when it moves it; onlyNewer:
// only when it is above the vote; fresh: whatever the path says, unless it
// is the vote. Compared and set in one section.
func (in *networkIntake) accept(upstreamId string, sh evm.SourceHeader, onlyNewer, fresh bool) bool {
	cur := sh.Cursor()
	in.mu.Lock()
	defer in.mu.Unlock()
	path := in.last[upstreamId]
	if n := len(path); n > 0 && (path[n-1].cur == cur || onlyNewer && cur.Number <= path[n-1].cur.Number) {
		return false
	}
	if !fresh && !path.moves(cur, sh.Parent()) {
		return false
	}
	in.last[upstreamId] = path.with(cur, sh.Parent())
	return true
}

// forget drops what intake holds of an upstream that left the network.
func (in *networkIntake) forget(upstreamId string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	delete(in.last, upstreamId)
	delete(in.streams, upstreamId)
	delete(in.streamingSince, upstreamId)
	delete(in.heldPollAt, upstreamId)
	delete(in.pollFailing, upstreamId)
}

// heldPollDue reports whether a held vote-need poll of upstreamId may go
// now, and notes it: one per every per upstream, however many lanes ask.
func (in *networkIntake) heldPollDue(upstreamId string, now time.Time, every time.Duration) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	if at, ok := in.heldPollAt[upstreamId]; ok && now.Sub(at) < every {
		return false
	}
	in.heldPollAt[upstreamId] = now
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

// voterSnapshot is the network-wide voters read from one eligible set
// (Network.eligibleSet).
type voterSnapshot struct {
	ids    []string
	policy []common.Upstream
	all    []*upstream.Upstream
}

func (s *voterSnapshot) of(policy []common.Upstream, all []*upstream.Upstream) bool {
	return slices.Equal(s.policy, policy) && slices.Equal(s.all, all)
}

// isVoter reports whether upstreamId is a network-wide voter: in the set
// last read, read again when the eligible set changed since, so a voter
// that joined or left the policy's set counts at once.
func (in *networkIntake) isVoter(upstreamId string) bool {
	policy, all := in.nw.eligibleSet(context.Background())
	s := in.voterIds.Load()
	if s == nil || !s.of(policy, all) {
		vs, _ := in.voters("")
		ids := make([]string, 0, len(vs))
		for _, u := range vs {
			ids = append(ids, u.Id())
		}
		s = &voterSnapshot{ids: ids, policy: policy, all: slices.Clone(all)}
		in.voterIds.Store(s)
	}
	return slices.Contains(s.ids, upstreamId)
}

// upstreamVendors is each upstream's vendor and the evidence quorum (spec
// §6.10) for one upstream set.
type upstreamVendors struct {
	ups      []*upstream.Upstream
	vendors  map[string]string
	evidence int
}

// vendorsOf is the vendors of ups, computed again only when the set or a
// vendor name changed.
func (in *networkIntake) vendorsOf(ups []*upstream.Upstream) *upstreamVendors {
	if c := in.vendors.Load(); c != nil && c.of(ups) {
		return c
	}
	c := &upstreamVendors{ups: slices.Clone(ups), vendors: make(map[string]string, len(ups)), evidence: 1}
	distinct := make(map[string]struct{}, len(ups))
	for _, u := range ups {
		c.vendors[u.Id()] = u.VendorName()
		distinct[u.VendorName()] = struct{}{}
	}
	if len(distinct) >= 2 {
		c.evidence = 2
	}
	in.vendors.Store(c)
	return c
}

func (c *upstreamVendors) of(ups []*upstream.Upstream) bool {
	if len(ups) != len(c.ups) {
		return false
	}
	for i, u := range ups {
		if c.ups[i] != u || c.vendors[u.Id()] != u.VendorName() {
			return false
		}
	}
	return true
}

// freshVote reports whether a voter with vote v is a hash voter: its hash
// age is at most w and its socket stayed up since (spec §6.1, §15.1). at
// is when it stops being one, zero while its age does not run.
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
// least every W: it notes which primary sources stream, whether one has
// streamed for primaryLeadAfter without a break, and publishes the
// admitted voters:
//
//	admitted(u) = voter(u) and (failover off, or u is not fallback tier,
//	                            or no primary source has streamed for 60s without a break)
//
// A source streams while its stream head's hash age is at most w. next is
// when a primary's streaming or lead next changes, 0 when no clock runs.
func (in *networkIntake) admit(now time.Time, w time.Duration) (next time.Duration) {
	voters, _ := in.voters("")
	soonest := func(d time.Duration) {
		if next == 0 || d < next {
			next = max(d, time.Millisecond)
		}
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	lead := primaryLeadAfter.Load()
	leads := false
	for _, u := range voters {
		if isFallbackTier(u) {
			continue
		}
		head, ok := in.streams[u.Id()]
		var age time.Duration
		if ok && in.nw.metricsTracker != nil {
			age = in.nw.metricsTracker.HashAgeAt(in.nw.networkId, head)
		}
		if !ok || age > w {
			delete(in.streamingSince, u.Id())
			continue
		}
		if age > 0 {
			soonest(w - age)
		}
		since, seen := in.streamingSince[u.Id()]
		if !seen {
			since = now
			in.streamingSince[u.Id()] = now
		}
		if left := lead - now.Sub(since); left > 0 {
			soonest(left)
		} else {
			leads = true
		}
	}
	for id := range in.streamingSince {
		if !slices.ContainsFunc(voters, func(u common.Upstream) bool { return u.Id() == id }) {
			delete(in.streamingSince, id)
		}
	}
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
	return next
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
	hv := evm.HashVoters{Evidence: 1}
	if n.intake == nil || n.upstreamsRegistry == nil {
		hv.Need = q.Of(0)
		return hv
	}
	vs := n.intake.vendorsOf(n.upstreamsRegistry.GetNetworkUpstreams(context.Background(), n.networkId))
	hv.Vendors, hv.Evidence = vs.vendors, vs.evidence
	now := time.Now()
	policy, all := n.eligibleSet(context.Background())
	voters, cands := n.intake.voters(selector)
	var stale []string
	hv.Voters = make([]string, 0, len(voters))
	hv.Hash = make([]string, 0, len(voters))
	for _, u := range voters {
		hv.Voters = append(hv.Voters, u.Id())
		v, voted := n.intake.hashVote(u)
		if voted && (v.Expired || v.Age > w) && n.sources.pushSource(u.Id()) {
			stale = append(stale, u.Id())
		}
		ok, at := freshVote(v, voted, w, now)
		if ok && n.intake.pollsFailedFor(u.Id()) >= w && !n.sources.pushSource(u.Id()) {
			ok = false
		}
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
		if old := n.intake.voterIds.Load(); old == nil || !old.of(policy, all) || !slices.Equal(old.ids, hv.Voters) {
			n.intake.voterIds.Store(&voterSnapshot{ids: hv.Voters, policy: policy, all: slices.Clone(all)})
		}
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

// setHashStale publishes the network-wide push sources whose hash age
// exceeds W or whose socket closed (spec §6.1): routing tries them after the
// rest, so a half-open socket holds no request. A push source behind by less
// than W, or any other upstream, keeps its rank: a poll schedule is no
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

// EvmSampleBlockTime takes the network-wide tracker's confirmed head, a
// source header q hash voters back, as a block time sample (spec §6.7).
func (n *Network) EvmSampleBlockTime(number, timestamp int64) {
	if n.metricsTracker != nil {
		n.metricsTracker.SampleConfirmedHeaderTime(n.networkId, n.Label(), number, timestamp)
	}
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
