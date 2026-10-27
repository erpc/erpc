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
	// pollAt is, per upstream and kind, when the network last polled it
	// for a held vote need or a fork (EvmPollHeads, EvmPollForks).
	pollAt map[string]map[evm.HeaderKind]time.Time
	// polls is, per polled upstream, its last successful latest poll and
	// the failures its node gave since (spec §6.1).
	polls map[string]pollRecord
	// counts are the header outcome counters, bound once each.
	counts telemetry.CounterSet[headCount]
}

// pollRecord is an upstream's last successful latest poll; since it, when
// the first poll its node failed was sent and when the last failed (zero
// while none did); and failing: those failures span W (spec §6.1). A
// failed poll spans from its send to its failure: the node did not answer
// in between, so one timeout as long as W spans W alone, and one quick 429
// spans nothing.
type pollRecord struct {
	ok, first, last time.Time
	failing         bool
}

type rejectedHeader struct {
	upstreamId string
	sh         evm.SourceHeader
	at         time.Time
}

// rejectedKept bounds the headers integrity held back that intake keeps.
const rejectedKept = 64

// primaryLeadAfter is how long a primary source must stream, without a
// break, before topic admission stops admitting the fallback tier (spec
// §6.2).
var primaryLeadAfter = util.NewDuration(60 * time.Second)

func newNetworkIntake(nw *Network) *networkIntake {
	return &networkIntake{nw: nw, last: make(map[string]votePath), streams: make(map[string]int64), streamingSince: make(map[string]time.Time), pollAt: make(map[string]map[evm.HeaderKind]time.Time), polls: make(map[string]pollRecord)}
}

// pushIntake is a push source's channel into intake: its headers are the
// source's stream (spec §6.2).
type pushIntake struct{ *networkIntake }

func (p pushIntake) Header(upstreamId string, raw json.RawMessage) {
	sh, err := evm.ParseSourceHeader(raw)
	if err != nil {
		p.count(upstreamId, "malformed")
		return
	}
	p.header(upstreamId, sh, evm.HeaderStreamed, false, time.Now())
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

// Polled takes the header a poll of upstreamId's latest, sent at sent,
// fetched; malformed: it failed intake check 0. A success ends its failing
// polls and renews the hash vote they expired (PollFailed). passed: the
// header passed intake's checks (header).
func (in *networkIntake) Polled(upstreamId string, sh evm.SourceHeader, malformed error, kind evm.HeaderKind, sent time.Time) (passed bool) {
	in.mu.Lock()
	failing := in.polls[upstreamId].failing
	in.polls[upstreamId] = pollRecord{ok: sent}
	in.mu.Unlock()
	if failing {
		in.nw.headsChanged()
		if u := in.upstream(upstreamId); !in.nw.sources.pushSource(upstreamId) && u != nil && in.nw.metricsTracker != nil && in.nw.metricsTracker.RenewHashVote(u) {
			evm.VotersChanged(in.nw)
			in.nw.sources.wake()
		}
	}
	if malformed != nil {
		in.count(upstreamId, "malformed")
		return false
	}
	return in.header(upstreamId, sh, kind, false, sent)
}

// PollFailed notes a poll of upstreamId's latest, sent at started, that its
// node failed; one sent before the last success does not count. Once its
// failed polls span W with no success since, the polls of an upstream that
// is no push source are failing (spec §6.1): the ceiling stops counting it
// (§6.4), and its hash vote expires as a push source's does when its socket
// closes. A push source's channel is its socket, so its polls never fail it.
func (in *networkIntake) PollFailed(upstreamId string, started time.Time) {
	if in.nw.sources.pushSource(upstreamId) {
		return
	}
	now, w := time.Now(), in.w()
	in.mu.Lock()
	r := in.polls[upstreamId]
	if started.Before(r.ok) {
		in.mu.Unlock()
		return
	}
	if r.first.IsZero() {
		r.first = started
	}
	r.last = now
	began := !r.failing && r.last.Sub(r.first) >= w
	r.failing = r.failing || began
	in.polls[upstreamId] = r
	in.mu.Unlock()
	if !began {
		return
	}
	in.nw.headsChanged()
	if u := in.upstream(upstreamId); u != nil && in.nw.metricsTracker != nil {
		in.nw.metricsTracker.ExpireHashVote(u)
		evm.VotersChanged(in.nw)
		in.nw.sources.wake()
	}
}

// PolledFinalized takes the header a poll of upstreamId's finalized
// fetched on this instance: its finalized evidence for the trackers (spec
// §6.5), never a vote.
func (in *networkIntake) PolledFinalized(upstreamId string, sh evm.SourceHeader) {
	if u := in.upstream(upstreamId); u != nil {
		evm.ObserveFinalizedHeader(in.nw, u, sh)
	}
}

// pollsFailing reports whether upstreamId's polls of latest are failing
// (PollFailed).
func (in *networkIntake) pollsFailing(upstreamId string) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.polls[upstreamId].failing
}

// w is the network's freshness window, as its trackers read it.
func (in *networkIntake) w() time.Duration {
	return health.FreshnessWindow(in.nw.EvmBlockTime())
}

// header takes a source header past check 0 that came as kind; replay: sh
// is a header integrity held back before a check was suspended, so it
// votes only when it is newer than its source's newest accepted header: a
// replay never moves a vote back. A fork poll's answer moves the vote
// whatever the path says: it is the node's head now. passed: sh passed
// checks 1 and 2, so a tracker may hold it; a header behind the vote,
// which never votes, is checked too when it is not the vote itself.
func (in *networkIntake) header(upstreamId string, sh evm.SourceHeader, kind evm.HeaderKind, replay bool, at time.Time) (passed bool) {
	cur := sh.Cursor()
	u := in.upstream(upstreamId)
	if u == nil {
		return false
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
		return current || evm.CheckSourceHeader(in.nw, u, sh)
	}
	if !evm.CheckSourceHeader(in.nw, u, sh) {
		in.mu.Lock()
		in.rejected = append(in.rejected, rejectedHeader{upstreamId, sh, at})
		if over := len(in.rejected) - rejectedKept; over > 0 {
			in.rejected = slices.Delete(in.rejected, 0, over)
		}
		in.mu.Unlock()
		in.count(upstreamId, "gated_integrity")
		return false
	}
	poller := u.EvmStatePoller()
	if poller == nil || poller.IsObjectNull() || !poller.ObserveHead(int64(cur.Number), at) {
		in.count(upstreamId, "gated_identity")
		return false
	}
	if !in.accept(upstreamId, sh, replay, fresh) {
		in.count(upstreamId, "duplicate")
		return true
	}
	// The hash vote is recorded before the trackers see the header, so the
	// tracker that handles it counts its sender as a hash voter.
	if mt := in.nw.metricsTracker; mt != nil {
		mt.RecordHashVote(u, int64(cur.Number), replay)
	}
	if stream {
		in.noteStream(upstreamId, int64(cur.Number))
	}
	evm.ObserveSourceHeader(in.nw, u, sh, at)
	if in.isVoter(upstreamId) {
		in.count(upstreamId, "voted")
	} else {
		in.count(upstreamId, "gated_voter")
	}
	return true
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
			in.header(r.upstreamId, r.sh, evm.HeaderPolled, true, r.at)
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

// headCount is one series of erpc_chain_tracker_source_heads_total.
type headCount struct{ project, network, upstream, outcome string }

func headCountLabels(k headCount) []string {
	return []string{k.project, k.network, k.upstream, k.outcome}
}

func (in *networkIntake) count(upstreamId, outcome string) {
	in.counts.Inc(telemetry.MetricChainTrackerSourceHeadsTotal,
		headCount{in.nw.projectId, in.nw.Label(), upstreamId, outcome}, headCountLabels)
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
	delete(in.pollAt, upstreamId)
	delete(in.polls, upstreamId)
}

// pollDue reports whether a poll of upstreamId of kind may go now, and
// notes it: one per every per upstream and kind, however many lanes ask.
func (in *networkIntake) pollDue(upstreamId string, kind evm.HeaderKind, now time.Time, every time.Duration) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	at := in.pollAt[upstreamId]
	if last, ok := at[kind]; ok && now.Sub(last) < every {
		return false
	}
	if at == nil {
		at = make(map[evm.HeaderKind]time.Time, 2)
		in.pollAt[upstreamId] = at
	}
	at[kind] = now
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

// voteKind is what a walk of the voters counts (spec §6.2).
type voteKind uint8

const (
	// numberVote: the Head ballots, Leader and the Asks that move Latest
	// (refresh, catch-up, band). A number vote needs a head: a voter on a
	// minority fork (spec §6.3) or with no head yet casts none.
	numberVote voteKind = iota
	// hashVote: the hash voters. A voter on a minority fork keeps its hash
	// vote, which is how it returns.
	hashVote
	// admissionVote: topic admission. A poller with no head yet is a voter
	// too: a poll source gets its first head only once admitted.
	admissionVote
)

// voterQuery is one use of voter(u): the vote it counts; finalized, the
// axis whose heads rank the voters; member, the subset a guarantee ballot
// runs on (nil: every voter).
type voterQuery struct {
	kind      voteKind
	finalized bool
	member    func(common.Upstream) bool
}

// voterSet is voter(u) over the lane of selector sel ("" network-wide),
// the one place its rule applies (spec §6.2): upstreams in the policy's
// eligible set, else every registered one (tipCandidates), that sel
// matches, with a state poller and not syncing, of the best rank present
// (tipVoterRank), minus what the query's kind excludes (voteKind). No
// tier filter: the fallback tier votes too. voters finds the best rank in
// one pass and each walks again, so neither allocates.
type voterSet struct {
	n     *Network
	sel   string
	q     voterQuery
	best  int
	ranks tipRanks
}

// voters is the voter set of sel's lane for q.
func (n *Network) voters(sel string, q voterQuery) voterSet {
	s := voterSet{n: n, sel: sel, q: q, best: -1}
	for u := range n.tipCandidates(sel) {
		if _, rank, ok := s.rank(u); ok {
			s.ranks[rank]++
			if s.best < 0 || rank < s.best {
				s.best = rank
			}
		}
	}
	return s
}

// rank is u's rank for the query; ok is false when u casts no such vote
// whatever the other candidates' ranks.
func (s voterSet) rank(u common.Upstream) (head int64, rank int, ok bool) {
	if s.q.member != nil && !s.q.member(u) || s.q.kind == numberVote && s.n.isForkDemoted(u) {
		return 0, 0, false
	}
	head, rank, ok = tipVoterRank(u, s.q.finalized, s.n.metricsTracker)
	if !ok && s.q.kind == admissionVote && !syncing(u) {
		if eu, isEvm := u.(common.EvmUpstream); isEvm {
			if sp := eu.EvmStatePoller(); sp != nil && !sp.IsObjectNull() {
				return 0, 0, true
			}
		}
	}
	return head, rank, ok
}

// count is how many voters the set had when it was read.
func (s voterSet) count() int {
	if s.best < 0 {
		return 0
	}
	return s.ranks[s.best]
}

// live reports whether live heads, benched or not, rank best: a cap-only
// set gives the regression guard no live reference.
func (s voterSet) live() bool {
	return s.best >= 0 && s.best <= 1
}

// each calls yield with each voter, in routing order, and its head on the
// query's axis (0 for an admitted poller with none yet), until yield
// returns false.
func (s voterSet) each(yield func(u common.Upstream, head int64) bool) {
	if s.best < 0 {
		return
	}
	for u := range s.n.tipCandidates(s.sel) {
		if head, rank, ok := s.rank(u); ok && rank == s.best && !yield(u, head) {
			return
		}
	}
}

// list is the voters in routing order.
func (s voterSet) list() []common.Upstream {
	out := make([]common.Upstream, 0, s.count())
	s.each(func(u common.Upstream, _ int64) bool {
		out = append(out, u)
		return true
	})
	return out
}

// tipVoterRank is a candidate's rank in voterSet. ok is false for an
// upstream with no head on the axis: non-EVM, no state poller, syncing, or
// no head yet. Otherwise rank orders the voters, and a voter set counts
// only the lowest rank present: 0 live, 1 benched, 2 static cap, 3 benched
// static cap.
//
// STATIC-CAP UPSTREAMS DO NOT VOTE while any live head is present. An upstream
// configured with `blockAvailability.upper.exactBlock` below the chain's head is
// declaring a SERVING RANGE ("I answer up to block X"), not observing where the
// head is — see evmTipObservation. Letting such a declaration onto the ballot
// lets two capped legacy upstreams pull the tip down: pinned at an
// `upper.exactBlock` far below the live heads, they outvote them the moment a
// third live upstream is excluded for lag, "latest" interpolates to a
// historical block, the availability gate then admits ONLY those two
// upstreams (the live ones start above the cap), and funded accounts read 0x0.
//
// BENCHED UPSTREAMS DO NOT VOTE while any non-benched one does: the health
// tracker benches a head that stayed behind peers that kept advancing
// (health.Tracker.IsHeadBenched), so one stalled node cannot hold
// "latest" at its height. A live head always outranks a cap, benched or not:
// a stalled live node is still closer to the chain than a serving range.
func tipVoterRank(u common.Upstream, finalized bool, tracker *health.Tracker) (head int64, rank int, ok bool) {
	eu, isEvm := u.(common.EvmUpstream)
	if !isEvm {
		return 0, 0, false
	}
	if sp := eu.EvmStatePoller(); sp == nil || sp.IsObjectNull() || eu.EvmSyncingState() == common.EvmSyncingStateSyncing {
		return 0, 0, false
	}
	head, capped := evmTipObservation(eu, finalized)
	if head <= 0 {
		return 0, 0, false
	}
	if capped {
		rank = 2
	}
	if tracker != nil && tracker.IsHeadBenched(u) {
		rank++
	}
	return head, rank, true
}

// tipRanks counts a voter set's candidates by tipVoterRank.
type tipRanks [4]int

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
		vs := in.nw.voters("", voterQuery{kind: hashVote}).list()
		ids := make([]string, 0, len(vs))
		for _, u := range vs {
			ids = append(ids, u.Id())
		}
		s = &voterSnapshot{ids: ids, policy: policy, all: slices.Clone(all)}
		in.voterIds.Store(s)
	}
	return slices.Contains(s.ids, upstreamId)
}

// upstreamVendors is each upstream's vendor and how many distinct vendors
// one upstream set names, which the evidence quorum reads (spec §6.10).
type upstreamVendors struct {
	ups      []*upstream.Upstream
	vendors  map[string]string
	distinct int
}

// vendorsOf is the vendors of ups, computed again only when the set or a
// vendor name changed.
func (in *networkIntake) vendorsOf(ups []*upstream.Upstream) *upstreamVendors {
	if c := in.vendors.Load(); c != nil && c.of(ups) {
		return c
	}
	c := &upstreamVendors{ups: slices.Clone(ups), vendors: make(map[string]string, len(ups))}
	distinct := make(map[string]struct{}, len(ups))
	for _, u := range ups {
		v := evm.VendorKey(u)
		c.vendors[u.Id()] = v
		distinct[v] = struct{}{}
	}
	c.distinct = len(distinct)
	in.vendors.Store(c)
	return c
}

func (c *upstreamVendors) of(ups []*upstream.Upstream) bool {
	if len(ups) != len(c.ups) {
		return false
	}
	for i, u := range ups {
		if c.ups[i] != u || c.vendors[u.Id()] != evm.VendorKey(u) {
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
	voters := in.nw.voters("", voterQuery{kind: admissionVote}).list()
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
// in selection order; the eligible non-voters with a poller, which a
// settle may Ask; the hash voters, those whose hash age is at most w,
// and, while cold, the eligible sources that have not reported a header
// yet, so a cold instance's first header waits for a second; q.Of of them;
// each upstream's vendor and how many vendors they name.
func (n *Network) EvmHashVoters(selector string, w time.Duration, cold bool) evm.HashVoters {
	var q common.ServedTipQuorum
	if n.cfg != nil {
		q = n.cfg.Evm.ServedTipQuorum()
	}
	var hv evm.HashVoters
	if n.intake == nil || n.upstreamsRegistry == nil {
		hv.Need = q.Of(0)
		return hv
	}
	vs := n.intake.vendorsOf(n.upstreamsRegistry.GetNetworkUpstreams(context.Background(), n.networkId))
	hv.Vendors, hv.NetworkVendors = vs.vendors, vs.distinct
	now := time.Now()
	policy, all := n.eligibleSet(context.Background())
	voters := n.voters(selector, voterQuery{kind: hashVote}).list()
	var stale []string
	hv.Voters = make([]string, 0, len(voters))
	hv.Hash = make([]string, 0, len(voters))
	for _, u := range voters {
		hv.Voters = append(hv.Voters, u.Id())
		if n.sources.source(u.Id()) {
			hv.Sources = append(hv.Sources, u.Id())
		}
		v, voted := n.intake.hashVote(u)
		if voted && (v.Expired || v.Age > w) && n.sources.pushSource(u.Id()) {
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
		if old := n.intake.voterIds.Load(); old == nil || !old.of(policy, all) || !slices.Equal(old.ids, hv.Voters) {
			n.intake.voterIds.Store(&voterSnapshot{ids: hv.Voters, policy: policy, all: slices.Clone(all)})
		}
	}
	for c := range n.tipCandidates(selector) {
		if syncing(c) {
			continue
		}
		if !slices.Contains(hv.Voters, c.Id()) {
			if eu, ok := c.(common.EvmUpstream); ok && eu.EvmStatePoller() != nil && !eu.EvmStatePoller().IsObjectNull() {
				hv.Others = append(hv.Others, c.Id())
			}
		}
		if cold && c.Config().IsSubscriptionSource() && !n.intake.reported(c.Id()) && !slices.Contains(hv.Hash, c.Id()) {
			hv.Hash = append(hv.Hash, c.Id())
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
// They lose their number vote at once, so every lane's Head is computed
// again, and go last in routing.
func (n *Network) EvmForkDemoted(ids []string) {
	if len(ids) == 0 {
		n.forkDemoted.Store(nil)
	} else {
		n.forkDemoted.Store(&ids)
	}
	n.headsChanged()
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

// EvmHeadsDemand reports whether an open tap needs newHeads, and the
// fastest pace one needs it at: 0 every block.
func (n *Network) EvmHeadsDemand() (pace time.Duration, ok bool) {
	if n.taps == nil {
		return 0, false
	}
	return n.taps.Pace(n.Label(), indexer.TopicNewHeads)
}

// EvmTaps is the network's taps and the key its demand is counted under:
// the network label, which also labels the tap metrics (spec §15). Taps
// belong to one project's network, so the label alone is unique in them.
func (n *Network) EvmTaps() (*indexer.Taps, string) {
	return n.taps, n.Label()
}
