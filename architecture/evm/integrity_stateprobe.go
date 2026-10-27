package evm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// The state prober answers a question nothing else in erpc can: does this
// upstream ACTUALLY hold the state it claims? A node can report head N and
// still answer eth_call/eth_getBalance from older state — silently, with a
// well-formed response. The claimed head says nothing about the state trie.
//
// Three probes. The first two are judged against the chain follower's
// VERIFIED header for a height (the trust anchor — without the follower this
// verification would be circular, judging the node against its own claims):
//
//   - context probe: eth_call to the Multicall3 getBlockNumber() pinned at
//     block N. The EVM answers from the execution context it actually used, so
//     a node executing against older state returns the OLDER height. Universal
//     (Multicall3 is deployed at one address on 200+ chains) and catches the
//     operational failure: fleets/nodes that lag their own claimed head.
//     Validated live: at a pinned block the returned number matches the
//     verified header exactly; pointed at N-5000 it returns N-5000's context.
//
//   - proof probe: eth_getProof for the same account at N; the first account-
//     proof node must keccak-hash to the verified header's stateRoot. That is
//     unforgeable without the trie, so it is the cryptographic layer the
//     context call (which a hostile node could special-case) cannot provide.
//     Not all providers expose eth_getProof — support is DISCOVERED per
//     upstream, never assumed.
//
//   - latest probe: the context call at the "latest" tag, sent as the tag. A
//     node can hold state at its head and answer every pinned call right,
//     yet resolve "latest" for state calls to an older block: measured on
//     HyperEVM vendors, one froze 31,733 blocks back, another ran ~110 back,
//     both with a current head. Its answers are the tag-state heads the
//     tag-state gate reads (spec §6.11, tagStateHead); it needs no follower,
//     runs from network start on each change of Latest, and probes only
//     upstreams that get tag requests (probeTagStates).
//
// The prober PUBLISHES evidence; it holds no routing authority of its own.
// A success advances the upstream's state-proven head (monotonic) — pure
// telemetry, never a routing bound: the proven head advances at probe cadence,
// so on any chain whose block time is shorter than that cadence it trails
// every honest upstream's claimed head, and a bound built on it would refuse
// the network's own advertised tip. A single failure just fails to advance —
// a mismatch at the tip can be a fork-transient. Only a SUSTAINED streak of
// wrong-height pinned answers is evidence against a node, and it goes where
// every other proven-bad-data signal already goes: upstream misbehavior on
// the health tracker (the ledger consensus disputes, integrity deterministic
// rejects and wrong-empty responses feed — see recordMisbehavior).
// Whether and when to exclude is the operator's selection policy's call, via
// the existing misbehaviorRateAbove predicate. The tag-state heads are the
// one exception: the gate in checkEvmUpstreamBlockAvailability skips an
// upstream whose "latest" state is stale for tag requests.
// The context-probe target is per-ARCHITECTURE (integrity.ChainStateContextProbe):
// standard EVMs use Multicall3 getBlockNumber, but e.g. Nitro's block.number is
// the L1 height and needs ArbSys arbBlockNumber instead — assuming one probe
// for all chains mislabelled every honest arbitrum upstream as stale.

// stateProbeMinInterval is the default floor between probes of one upstream.
const stateProbeMinInterval = 2 * time.Second

// stateProbeProofErrors is how many eth_getProof errors in a row, with no
// answer between them, stop an upstream's proof probes (spec §15.1).
const stateProbeProofErrors = 10

// stateProbeProofOffFor is how long an upstream's proof probes stay off
// once stopped: like an IgnoreMethod entry, the switch expires.
var stateProbeProofOffFor = 10 * time.Minute

type stateProber struct {
	network  common.Network
	view     *ChainTracker // the tracker whose pinned headers anchor the probes
	interval time.Duration
	ctxProbe *integrity.StateContextProbe // per-architecture "which block are you in"
	// pinnedProbes: integrity follow and stateProbe are on, so followed heads
	// run the context and proof probes (#1109). The latest probe runs anyway.
	pinnedProbes bool

	mu        sync.Mutex
	lastProbe map[string]time.Time // upstream id -> last probe start

	// proofOff stops an upstream's proof probes until a time: its provider
	// does not expose eth_getProof, or erred stateProbeProofErrors times in
	// a row. Context probes go on. The switch expires, as IgnoreMethod
	// entries do, so a vendor that fixed its proofs is probed again.
	proofOff sync.Map // upstream id -> time.Time
	// proofErrors counts an upstream's eth_getProof errors since its last
	// proof answer.
	proofErrors sync.Map // upstream id -> *atomic.Int64

	// disprovedStreak counts CONSECUTIVE probe mismatches per upstream — the
	// INTERNAL debounce between one unlucky probe and real evidence. Never
	// config, never API: it only separates a probe landing across a reorg from
	// an upstream that answers pinned calls at the wrong height sustainedly
	// (measured on a live shadow: one upstream returned pin_ignored on 202 of
	// 202 probes while all six of its siblings matched on all 202). An upstream
	// that merely CANNOT be probed accrues no streak — absence of proof is not
	// evidence and has no effect anywhere.
	disprovedStreak sync.Map // upstream id -> *atomic.Int64

	// work carries the newest followed head; size 1, newest wins — probing is
	// against the tip, so intermediate heights are worthless once superseded.
	work chan int64

	// The tag-state store (spec §6.11), by upstream id; maps made on first use.
	tagMu       sync.Mutex
	tagHeads    map[string]*tagStateHead
	tagSent     map[string]time.Time // newest tag-state sample's send time: the probe floor
	tagReached  map[string]time.Time // last skip-interpolation family-3 request admitted
	tagInflight map[string]bool
	tagAsked    map[string]bool // upstreams a skip asked to re-probe
	tagMoved    bool            // Latest changed since the last pass
	tagWake     chan struct{}   // size 1
}

// stateProbers is the per-network registry (the prober is network-scoped even
// though follower views are group-scoped — one probe pass covers every
// upstream, whichever view's verified header anchors it). Keyed by the
// network, not its id: two projects may serve one chain id, each with its
// own upstreams. A prober leaves it when its network's ctx ends.
var stateProbers sync.Map // common.Network -> *stateProber

// startStateProber starts the network's prober, once, at its first client
// request (ClientRequested). Every EVM network has one then, for the latest
// probe; cfg is nil unless integrity follow and stateProbe are on, which
// add the context and proof probes.
func startStateProber(ctx context.Context, n common.Network, v *ChainTracker, cfg *common.IntegrityStateProbeConfig) {
	interval := stateProbeMinInterval
	if cfg != nil && cfg.Interval > 0 {
		interval = cfg.Interval.Duration()
	}
	var chainId int64
	if c := n.Config(); c != nil && c.Evm != nil {
		chainId = c.Evm.ChainId
	}
	p := &stateProber{
		network:      n,
		view:         v,
		interval:     interval,
		ctxProbe:     integrity.ChainStateContextProbe(chainId),
		pinnedProbes: cfg != nil,
		lastProbe:    map[string]time.Time{},
		work:         make(chan int64, 1),
		tagWake:      make(chan struct{}, 1),
	}
	if _, loaded := stateProbers.LoadOrStore(n, p); loaded {
		return // another view won the race; one prober per network
	}
	if !v.fetch.bg.Go(func() { p.run(ctx) }) {
		stateProbers.CompareAndDelete(n, p)
		return
	}
	// Its own loop, so a pinned pass (up to 20s) never delays a tag-state
	// probe.
	v.fetch.bg.Go(func() { p.runTagStates(ctx) })
}

// stateProberFor returns n's running prober, nil before its first client
// request.
func stateProberFor(n common.Network) *stateProber {
	v, ok := stateProbers.Load(n)
	if !ok {
		return nil
	}
	return v.(*stateProber)
}

// onNewHead hands the prober a freshly followed (verified) height. Non-blocking:
// if a probe pass is still draining the previous head, the newest replaces it.
func (p *stateProber) onNewHead(n int64) {
	if !p.pinnedProbes {
		return
	}
	for {
		select {
		case p.work <- n:
			return
		default:
			select { // drop the stale queued head, keep the newest
			case <-p.work:
			default:
			}
		}
	}
}

func (p *stateProber) run(ctx context.Context) {
	defer stateProbers.CompareAndDelete(p.network, p)
	for {
		select {
		case <-ctx.Done():
			return
		case n := <-p.work:
			p.probeAll(ctx, n)
		}
	}
}

func (p *stateProber) probeAll(ctx context.Context, headN int64) {
	header, ok := p.view.HeaderAt(headN)
	if !ok || header == nil {
		return // verified header gone (reorg unwound it) — nothing to anchor on
	}
	enum, ok := p.network.(interface {
		EvmAllUpstreams(context.Context) []common.Upstream
	})
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	for _, u := range enum.EvmAllUpstreams(ctx) {
		// Probed at a pin above its head counter, u would meet the refusal
		// rule (spec §6.4) and have its head Asked first, and its missing
		// state would read as a failed probe. It is probed at a later pin it
		// reached (spec §13), so it stays due.
		if eu, isEvm := u.(common.EvmUpstream); !isEvm || eu.EvmEffectiveLatestBlock() < headN {
			continue
		}
		id := u.Id()
		p.mu.Lock()
		last := p.lastProbe[id]
		due := time.Since(last) >= p.interval
		if due {
			p.lastProbe[id] = time.Now()
		}
		p.mu.Unlock()
		if !due {
			continue
		}
		p.probeUpstream(ctx, u, headN, header)
	}
}

// probeUpstream runs the probes against one upstream and advances its proven
// head when the evidence supports it.
func (p *stateProber) probeUpstream(ctx context.Context, u common.Upstream, n int64, h *integrity.Header) {
	ctxMatch := p.probeContext(ctx, u, n)
	proofMatch := p.probeProof(ctx, u, n, h)

	// Advance on any positive proof, count any negative one. "unknown"
	// (unsupported/error) neither advances nor counts — if BOTH probes are
	// unknown there is no evidence at all, and nothing changes anywhere.
	if ctxMatch == probeMismatch || proofMatch == probeMismatch {
		p.noteDisproved(u)
		return
	}
	if ctxMatch != probeMatch && proofMatch != probeMatch {
		return // no evidence either way — the streak is untouched
	}
	p.clearDisproved(u.Id())
	w, ok := u.(common.EvmStateProvenWriter)
	if !ok {
		return
	}
	w.EvmSetStateProvenBlock(n)
	telemetry.MetricUpstreamStateProvenBlock.WithLabelValues(
		p.projectId(), u.VendorName(), p.network.Label(), u.Id(),
	).Set(float64(n))
	eu, isEvm := u.(common.EvmUpstream)
	if !isEvm {
		return
	}
	if claimed := eu.EvmEffectiveLatestBlock(); claimed >= n {
		telemetry.MetricUpstreamStateProvenLag.WithLabelValues(
			p.projectId(), u.VendorName(), p.network.Label(), u.Id(),
		).Set(float64(claimed - n))
	}
}

type probeOutcome int

const (
	probeMatch probeOutcome = iota
	probeMismatch
	probeUnknown // unsupported or transport error — no evidence either way
)

// probeContext asks the upstream's EVM which block it executes in when pinned
// at N. Stale state answers with the stale height.
func (p *stateProber) probeContext(ctx context.Context, u common.Upstream, n int64) probeOutcome {
	got, ok := p.executedBlock(ctx, u, "context", fmt.Sprintf("0x%x", n))
	if !ok {
		return probeUnknown
	}
	if got != n {
		// The direction diagnoses the failure. executed < pinned = STALE state
		// (the node lags what it claims). executed > pinned = PIN-IGNORING:
		// the node silently executes at its latest head regardless of the
		// requested block — historical state questions answered with present
		// state. Both are silent wrong-data modes; they need different
		// operator responses, so the metric separates them.
		outcome := "stale"
		if got > n {
			outcome = "pin_ignored"
		}
		p.count(u, "context", outcome)
		log.Warn().Str("network", p.network.Label()).Str("upstream", u.Id()).
			Int64("pinnedBlock", n).Int64("executedBlock", got).Str("mode", outcome).
			Msg("state probe: upstream executed a pinned call in a DIFFERENT block context")
		return probeMismatch
	}
	p.count(u, "context", "match")
	return probeMatch
}

// Tag-state store timing (spec §6.11). Variables so tests run fast.
var (
	// tagStateProbeFloor is the least time between two tag-state samples of
	// one upstream; a packed sample counts as one.
	tagStateProbeFloor = 2 * time.Second
	// tagStateReachedFor keeps an upstream probed this long after a
	// skip-interpolation family-3 request reached it.
	tagStateReachedFor = 10 * time.Minute
	// tagStateRefuseBehind: an all-stale request is refused when the highest
	// credited tag-state head is more than this much chain below Latest.
	tagStateRefuseBehind = 60 * time.Second
	// tagStateSweepEvery: how often the probe loop readmits heads with no
	// traffic and no change of Latest.
	tagStateSweepEvery = time.Second
)

// tagStateFreshStreak fresh samples in a row readmit a stale upstream.
const tagStateFreshStreak = 3

type tagState int

const (
	tagNoEvidence tagState = iota // admitted (#934)
	tagFresh
	tagStale
)

// tagSample is one tag-state sample: the block an upstream executed
// "latest" at, and when the request that asked was sent.
type tagSample struct {
	block  int64
	sentAt time.Time
}

// credited is the sample's block with its age credited at block time bt,
// as #1296 credits lag: block + (now − sentAt) / bt.
func (s tagSample) credited(now time.Time, bt time.Duration) int64 {
	return s.block + int64(now.Sub(s.sentAt)/bt)
}

// staleAgainst is the one rule for a stale sample (spec §6.11): ref, the
// reader's Latest, is more than W of chain above the sample's credited
// block.
func (s tagSample) staleAgainst(ref int64, now time.Time, bt, w time.Duration) bool {
	return time.Duration(ref-s.credited(now, bt))*bt > w
}

// tagStateHead is an upstream's newest tag-state samples, oldest first:
// enough to tell whether the fresh ones since a stale one readmit it.
// published is its verdict against the network-wide Latest as the gauge
// and the log last showed it.
type tagStateHead struct {
	samples   [tagStateFreshStreak]tagSample
	n         int
	published bool
}

// add keeps s as the newest sample; one older than the newest is dropped.
func (h *tagStateHead) add(s tagSample) {
	if h.n > 0 && s.sentAt.Before(h.samples[h.n-1].sentAt) {
		return
	}
	if h.n == len(h.samples) {
		copy(h.samples[:], h.samples[1:])
		h.n--
	}
	h.samples[h.n] = s
	h.n++
}

// state judges h against ref, the reader's Latest, at now: stale from a
// stale sample until tagStateFreshStreak fresh ones follow it, or until
// tagStateProbeFloor + W pass after it with no stale one (W alone can fall
// between two probes); then admitted, fresh once a sample came after that.
// No evidence before the first sample.
func (h *tagStateHead) state(ref int64, now time.Time, bt, w time.Duration) tagState {
	if h.n == 0 {
		return tagNoEvidence
	}
	for i := h.n - 1; i >= 0; i-- {
		s := h.samples[i]
		if !s.staleAgainst(ref, now, bt, w) {
			continue
		}
		readmit := s.sentAt.Add(tagStateProbeFloor + w)
		switch {
		case !now.After(readmit):
			return tagStale
		case h.samples[h.n-1].sentAt.After(readmit):
			return tagFresh
		}
		return tagNoEvidence
	}
	return tagFresh
}

// tagView is an upstream's tag-state head judged against one reference:
// its newest sample and its state.
type tagView struct {
	tagSample
	state tagState
}

// tagTiming is the network's block time and W, the tag-state tolerance.
func (p *stateProber) tagTiming() (bt, w time.Duration) {
	bt = health.ColdBlockTime
	if p.view != nil {
		bt = p.view.BlockTime()
	}
	return bt, health.FreshnessWindow(bt)
}

// networkLatest is the reference where no request decides (spec §6.11):
// the gauges and the probe logs.
func (p *stateProber) networkLatest() int64 {
	return common.EvmHeadOf(p.network, context.Background(), nil).Latest
}

// publishTagStates sets each upstream's tag-state gauge and logs each
// change of its verdict, judged against the network-wide Latest at now.
// Nothing is judged while Latest is unknown.
func (p *stateProber) publishTagStates(now time.Time) {
	ref := p.networkLatest()
	if ref <= 0 {
		return
	}
	bt, w := p.tagTiming()
	p.tagMu.Lock()
	defer p.tagMu.Unlock()
	for id, h := range p.tagHeads {
		stale := h.state(ref, now, bt, w) == tagStale
		p.setTagGauge(id, stale)
		if stale != h.published {
			h.published = stale
			p.logTagState(id, stale, h.samples[h.n-1].block, ref)
		}
	}
}

func (p *stateProber) setTagGauge(id string, stale bool) {
	v := 0.0
	if stale {
		v = 1
	}
	telemetry.MetricUpstreamTagStateStale.WithLabelValues(p.projectId(), p.network.Label(), id).Set(v)
}

// logTagState logs u entering or leaving the tag-state skip (spec §17):
// its newest sample executed "latest" at block, judged against Latest.
func (p *stateProber) logTagState(id string, stale bool, block, latest int64) {
	msg := "tag-state gate readmits upstream for tag requests"
	if stale {
		msg = "tag-state gate skips upstream for tag requests: its \"latest\" state is stale"
	}
	common.TransitionLog(&log.Logger, zerolog.InfoLevel, p.network.Label(), "tag_state").Str("upstream", id).
		Bool("stale", stale).Int64("block", block).Int64("latest", latest).Msg(msg)
}

// recordTagSample keeps one sample of u's tag-state head: u executed
// "latest" at block when sent at sentAt. Its readers judge it (tagHead).
func (p *stateProber) recordTagSample(id string, block int64, sentAt time.Time) {
	p.tagMu.Lock()
	if p.tagSent == nil {
		p.tagSent = map[string]time.Time{}
	}
	if sentAt.After(p.tagSent[id]) {
		p.tagSent[id] = sentAt
	}
	if p.tagHeads == nil {
		p.tagHeads = map[string]*tagStateHead{}
	}
	h := p.tagHeads[id]
	if h == nil {
		h = &tagStateHead{}
		p.tagHeads[id] = h
	}
	h.add(tagSample{block: block, sentAt: sentAt})
	p.tagMu.Unlock()
	p.publishTagStates(time.Now())
}

// tagHead is u's tag-state head judged against ref, the reader's Latest,
// at now.
func (p *stateProber) tagHead(id string, ref int64, now time.Time) tagView {
	bt, w := p.tagTiming()
	p.tagMu.Lock()
	defer p.tagMu.Unlock()
	h := p.tagHeads[id]
	if h == nil || h.n == 0 {
		return tagView{}
	}
	return tagView{tagSample: h.samples[h.n-1], state: h.state(ref, now, bt, w)}
}

// latestChanged asks for a tag-state pass: the network's Latest moved.
func (p *stateProber) latestChanged() {
	p.tagMu.Lock()
	p.tagMoved = true
	p.tagMu.Unlock()
	p.wakeTagStates()
}

// tagSkipped asks to re-probe u: the gate skipped it for a request.
func (p *stateProber) tagSkipped(id string) {
	p.tagMu.Lock()
	if p.tagAsked == nil {
		p.tagAsked = map[string]bool{}
	}
	p.tagAsked[id] = true
	p.tagMu.Unlock()
	p.wakeTagStates()
}

// tagReachedBy notes that a skip-interpolation family-3 request reached u
// as the tag, so u is probed for tagStateReachedFor.
func (p *stateProber) tagReachedBy(id string) {
	p.tagMu.Lock()
	if p.tagReached == nil {
		p.tagReached = map[string]time.Time{}
	}
	p.tagReached[id] = time.Now()
	p.tagMu.Unlock()
}

func (p *stateProber) wakeTagStates() {
	select {
	case p.tagWake <- struct{}{}:
	default:
	}
}

// runTagStates runs a pass on each wake, and every tagStateSweepEvery so a
// head with no traffic and no change of Latest is still readmitted.
func (p *stateProber) runTagStates(ctx context.Context) {
	sweep := time.NewTicker(tagStateSweepEvery)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.tagWake:
		case <-sweep.C:
		}
		p.probeTagStates(ctx)
	}
}

// familyKeepsTag reports whether a family-3 method's config keeps "latest"
// on its way to upstreams (translateLatestTag: false).
func (p *stateProber) familyKeepsTag() bool {
	if keepsLatestTag(getMethodConfig("eth_callMany", p.network)) {
		return true
	}
	for method := range stateBlockParam {
		if keepsLatestTag(getMethodConfig(method, p.network)) {
			return true
		}
	}
	return false
}

// probeTagStates starts the latest probe of every upstream due one: on a
// change of Latest each upstream that gets tag requests, and each one a skip
// asked for, at most every tagStateProbeFloor; an ask the floor or a running
// probe holds back stays, and the pass wakes again when it can run. Each
// probe runs on its own, so a slow upstream delays no other. Upstreams that
// do not serve eth_call are never probed. The pass also readmits every head
// whose W without stale evidence ran out, so the gauge drops without traffic.
func (p *stateProber) probeTagStates(ctx context.Context) {
	enum, ok := p.network.(interface {
		EvmAllUpstreams(context.Context) []common.Upstream
	})
	if !ok {
		return
	}
	keeps := p.familyKeepsTag()
	ups := enum.EvmAllUpstreams(ctx)
	serves := make(map[string]bool, len(ups))
	for _, u := range ups {
		serves[u.Id()], _ = u.ShouldHandleMethod("eth_call")
	}
	now := time.Now()
	p.publishTagStates(now)
	var due []common.Upstream
	var wakeIn time.Duration
	p.tagMu.Lock()
	moved := p.tagMoved
	p.tagMoved = false
	if p.tagInflight == nil {
		p.tagInflight = map[string]bool{}
	}
	if p.tagSent == nil {
		p.tagSent = map[string]time.Time{}
	}
	for _, u := range ups {
		id := u.Id()
		if !serves[id] {
			delete(p.tagAsked, id)
			continue
		}
		gets := !pinsLatestTag(u) && (keeps || now.Sub(p.tagReached[id]) < tagStateReachedFor)
		asked := p.tagAsked[id]
		if !(moved && gets || asked) || p.tagInflight[id] {
			continue // a running probe's end wakes the pass for an ask
		}
		if wait := tagStateProbeFloor - now.Sub(p.tagSent[id]); wait > 0 {
			if asked && (wakeIn == 0 || wait < wakeIn) {
				wakeIn = wait
			}
			continue
		}
		delete(p.tagAsked, id)
		p.tagInflight[id] = true
		p.tagSent[id] = now
		due = append(due, u)
	}
	p.tagMu.Unlock()
	if wakeIn > 0 {
		time.AfterFunc(wakeIn, p.wakeTagStates)
	}
	for _, u := range due {
		done := func() {
			p.tagMu.Lock()
			delete(p.tagInflight, u.Id())
			asked := p.tagAsked[u.Id()]
			p.tagMu.Unlock()
			if asked {
				p.wakeTagStates()
			}
		}
		if !p.view.fetch.bg.Go(func() {
			defer done()
			pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			p.probeLatest(pctx, u)
		}) {
			done()
		}
	}
}

// probeLatest asks which block u's EVM executes in at "latest", sent as the
// tag, records the answer as a tag-state sample, and counts and logs it as
// judged against the network-wide Latest.
func (p *stateProber) probeLatest(ctx context.Context, u common.Upstream) {
	if p.networkLatest() <= 0 {
		return // Head unknown: tag requests are not gated, nothing to judge
	}
	sentAt := time.Now()
	got, ok := p.executedBlock(ctx, u, "latest", "latest")
	if !ok {
		return
	}
	p.recordTagSample(u.Id(), got, sentAt)
	latest := p.networkLatest()
	bt, w := p.tagTiming()
	if (tagSample{block: got, sentAt: sentAt}).staleAgainst(latest, time.Now(), bt, w) {
		p.count(u, "latest", "stale")
		log.Warn().Str("network", p.network.Label()).Str("upstream", u.Id()).
			Int64("latest", latest).Int64("executedBlock", got).
			Msg("state probe: upstream executes \"latest\" calls in state older than the network's Latest")
		return
	}
	p.count(u, "latest", "match")
}

// DecideTagState makes r's tag-state decision (spec §6.11), once, before its
// first attempt, from its routed upstreams: each one that r's "latest"
// reaches as the tag and whose tag-state head is stale against r's Latest
// is skipped. With every routed upstream stale, the one with the highest
// credited head is kept and the request counted routed; when that head is
// more than tagStateRefuseBehind of chain below Latest, r is refused with
// -32603, counted refused. r keeps its tag either way. Requests outside
// family 3, numbered ones, those while Head is unknown and eRPC's own
// (probes, tracker fetches) are not gated.
func DecideTagState(ctx context.Context, n common.Network, r *common.NormalizedRequest) error {
	if r.UpstreamPolicies() != common.UpstreamPoliciesConfigured {
		return nil
	}
	p := stateProberFor(n)
	if p == nil {
		return nil
	}
	jrq, err := r.JsonRpcRequest(ctx)
	if err != nil || jrq == nil {
		return nil
	}
	jrq.RLock()
	method := jrq.Method
	_, tag := latestStateSelector(method, jrq.Params)
	jrq.RUnlock()
	if !tag {
		return nil
	}
	latest := common.EvmHeadOf(n, ctx, r).Latest
	if latest <= 0 {
		return nil
	}
	var sel string
	if d := r.Directives(); d != nil {
		sel = d.UseUpstream
	}
	now := time.Now()
	bt, _ := p.tagTiming()
	skips := map[string]bool{}
	// able: routed upstreams that can take the attempt. One that does not
	// serve the method, or pins latest while lagging below the number it
	// would be sent, is no fresh alternative to a stale one.
	able := 0
	var best string
	var bestHead int64
	for _, u := range r.Upstreams() {
		if sel != "" {
			if m, _ := common.UpstreamMatchesSelector(sel, u); !m {
				continue
			}
		}
		if ok, _ := u.ShouldHandleMethod(method); !ok {
			continue
		}
		if pinsLatestTag(u) {
			if eu, ok := u.(common.EvmUpstream); ok && eu.EvmEffectiveLatestBlock() >= latest {
				able++ // sent the number, never the tag
			}
			continue
		}
		able++
		h := p.tagHead(u.Id(), latest, now)
		if h.state != tagStale {
			continue
		}
		skips[u.Id()] = true
		if c := h.credited(now, bt); best == "" || c > bestHead {
			best, bestHead = u.Id(), c
		}
	}
	if able > 0 && len(skips) == able {
		delete(skips, best)
		if time.Duration(latest-bestHead)*bt > tagStateRefuseBehind {
			telemetry.MetricUpstreamTagStateAllStaleTotal.WithLabelValues(n.ProjectId(), n.Label(), "refused").Inc()
			return common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorServerSideException,
				fmt.Sprintf("every upstream executes \"latest\" in state more than %s of chain behind the served tip", tagStateRefuseBehind), nil,
				map[string]interface{}{"data": map[string]interface{}{
					"executedBlock": fmt.Sprintf("0x%x", bestHead),
					"servedTip":     fmt.Sprintf("0x%x", latest),
				}})
		}
		telemetry.MetricUpstreamTagStateAllStaleTotal.WithLabelValues(n.ProjectId(), n.Label(), "routed").Inc()
	}
	r.TakeTagStateSkips(skips)
	return nil
}

// TagStateSkip reports whether r's tag-state decision skips u, and asks a
// re-probe of u when it does. A skip-interpolation request that reaches u
// as the tag keeps u probed for tagStateReachedFor (probeTagStates).
func TagStateSkip(n common.Network, u common.Upstream, r *common.NormalizedRequest) bool {
	skips := r.TagStateSkips()
	if skips == nil {
		return false
	}
	p := stateProberFor(n)
	if p == nil {
		return false
	}
	if skips[u.Id()] {
		p.tagSkipped(u.Id())
		return true
	}
	if d := r.Directives(); d != nil && d.SkipInterpolation && !pinsLatestTag(u) {
		p.tagReachedBy(u.Id())
	}
	return false
}

// executedBlock sends the context probe call at blockParam and returns the
// block the upstream's EVM executed it in. ok is false when the answer is no
// evidence; that outcome is already counted under probe.
func (p *stateProber) executedBlock(ctx context.Context, u common.Upstream, probe, blockParam string) (int64, bool) {
	if ok, _ := u.ShouldHandleMethod("eth_call"); !ok {
		p.count(u, probe, "unsupported")
		return 0, false
	}
	body := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"%s","data":"%s"},"%s"]}`,
		p.ctxProbe.To, p.ctxProbe.Data, blockParam)
	res, err := p.forwardTo(ctx, u, body)
	if err != nil {
		p.count(u, probe, "error")
		return 0, false
	}
	if res == "" || res == "0x" {
		// No returndata: Multicall3 is not deployed on this chain (or this
		// node prunes code) — no evidence, not a failure.
		p.count(u, probe, "unsupported")
		return 0, false
	}
	got, ok := parseHexQuantity(res)
	if !ok {
		p.count(u, probe, "error")
		return 0, false
	}
	return got, true
}

// probeProof verifies the upstream can produce an account proof rooted at the
// VERIFIED stateRoot for N — unforgeable without the state trie.
func (p *stateProber) probeProof(ctx context.Context, u common.Upstream, n int64, h *integrity.Header) probeOutcome {
	if h.StateRoot == "" {
		return probeUnknown
	}
	if p.proofsOff(u.Id()) {
		return probeUnknown
	}
	// An upstream whose own configuration excludes the method (ignoreMethods /
	// allowMethods) never receives the probe: the router refuses the forward
	// first, and that refusal does not read as "unsupported" to the check
	// below — so the probe erred on every block instead of latching once.
	// Ask the upstream before forwarding and remember the answer.
	if ok, _ := u.ShouldHandleMethod("eth_getProof"); !ok {
		p.stopProofs(u.Id())
		p.count(u, "proof", "unsupported")
		return probeUnknown
	}
	body := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getProof","params":["%s",[],"0x%x"]}`,
		p.ctxProbe.To, n)
	res, err := p.forwardTo(ctx, u, body)
	if err != nil {
		if isMethodUnsupportedErr(err) {
			p.stopProofs(u.Id())
			p.count(u, "proof", "unsupported")
		} else {
			p.proofErred(u)
		}
		return probeUnknown
	}
	var proof struct {
		AccountProof []string `json:"accountProof"`
	}
	if json.Unmarshal([]byte(res), &proof) != nil || len(proof.AccountProof) == 0 {
		p.proofErred(u)
		return probeUnknown
	}
	node0, err := hexBytes(proof.AccountProof[0])
	if err != nil {
		p.proofErred(u)
		return probeUnknown
	}
	if v, ok := p.proofErrors.Load(u.Id()); ok {
		v.(*atomic.Int64).Store(0)
	}
	// The verified root itself unusable is no fault of u's.
	want, err := hexBytes(h.StateRoot)
	if err != nil || len(want) != 32 {
		p.count(u, "proof", "error")
		return probeUnknown
	}
	if string(gethcrypto.Keccak256(node0)) != string(want) {
		p.count(u, "proof", "mismatch")
		log.Warn().Str("network", p.network.Label()).Str("upstream", u.Id()).
			Int64("block", n).Str("verifiedStateRoot", h.StateRoot).
			Msg("state probe: account proof does not root at the verified stateRoot (absent or stale trie)")
		return probeMismatch
	}
	p.count(u, "proof", "match")
	return probeMatch
}

// proofsOff reports whether upstream id's proof probes are stopped now.
func (p *stateProber) proofsOff(id string) bool {
	until, ok := p.proofOff.Load(id)
	return ok && time.Now().Before(until.(time.Time))
}

// stopProofs stops upstream id's proof probes for stateProbeProofOffFor.
func (p *stateProber) stopProofs(id string) {
	p.proofOff.Store(id, time.Now().Add(stateProbeProofOffFor))
}

// proofErred counts an eth_getProof error of u; the stateProbeProofErrors-th
// in a row stops its proof probes.
func (p *stateProber) proofErred(u common.Upstream) {
	p.count(u, "proof", "error")
	v, _ := p.proofErrors.LoadOrStore(u.Id(), new(atomic.Int64))
	if v.(*atomic.Int64).Add(1) < stateProbeProofErrors {
		return
	}
	v.(*atomic.Int64).Store(0)
	p.stopProofs(u.Id())
	log.Warn().Str("network", p.network.Label()).Str("upstream", u.Id()).Int("errors", stateProbeProofErrors).
		Dur("for", stateProbeProofOffFor).Msg("state probe: eth_getProof keeps failing, stopping proof probes for a while")
}

// forwardTo sends one probe to EXACTLY the given upstream: cache bypassed (the
// cache would answer instead of the node — the circular-evidence trap), as
// eRPC's own so the integrity engine does not recurse into validating the
// probe's own response, timeout-only: no network or upstream retry or
// hedge, no breaker (spec §6.11),
// and with interpolation skipped, so a "latest" reaches u as the tag. A
// Use-Upstream request shares a multiplexer only with requests pinned to u.
func (p *stateProber) forwardTo(ctx context.Context, u common.Upstream, body string) (string, error) {
	req := common.NewNormalizedRequest([]byte(body))
	req.SetDirectives(&common.RequestDirectives{
		UpstreamPolicies:  common.UpstreamPoliciesTimeoutOnly,
		UseUpstream:       u.Id(),
		SkipCacheRead:     "true",
		SkipInterpolation: true,
	})
	req.SetNetwork(p.network)
	resp, err := p.network.Forward(ctx, req)
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", fmt.Errorf("nil response")
	}
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return "", fmt.Errorf("no json-rpc response: %w", err)
	}
	return string(jrr.GetResultBytes()), nil
}

// stateProbeDisprovedStreak is how many CONSECUTIVE mismatches make an upstream
// disproved rather than merely unproven. At the default 2s interval that is
// ~20s of unbroken evidence; the observed real case ran 202/202, so the
// threshold exists only to rule out a transient (a probe landing across a
// reorg, a momentary lag spike), not to be a tuning knob — which is why it is
// a constant here and not config.
const stateProbeDisprovedStreak = 10

// noteDisproved counts a probe mismatch and, once the streak crosses the
// debounce, records each further mismatch as upstream misbehavior — the same
// ledger consensus disputes, integrity deterministic rejects and wrong-empty
// responses already feed. Disproof is a fourth witness to that ledger, not a
// new concept: the prober publishes the evidence, and the operator's selection
// policy decides what to do with it (misbehaviorRateAbove in the policy stdlib
// reads exactly this counter). Recording per mismatch — not once per crossing —
// keeps the rolling rate elevated for exactly as long as the node keeps
// failing probes; one good probe resets the streak and the evidence stops.
func (p *stateProber) noteDisproved(u common.Upstream) {
	v, _ := p.disprovedStreak.LoadOrStore(u.Id(), new(atomic.Int64))
	if v.(*atomic.Int64).Add(1) < stateProbeDisprovedStreak {
		return
	}
	p.recordMisbehavior(u)
}

func (p *stateProber) recordMisbehavior(u common.Upstream) {
	// Under integrity observeOnly no check may influence routing: rejections
	// are suppressed before they reach misbehavior scoring, so probe disproof
	// stays consistent and records nothing either.
	if integrityObserveOnly(p.network) {
		return
	}
	if ht := u.Tracker(); ht != nil {
		// Recorded under the context probes' own method: they ARE eth_calls,
		// and eth_call traffic is what a wrong-height execution context
		// silently corrupts. They run at the followed (not yet finalized) head,
		// hence the unfinalized stratum.
		ht.RecordUpstreamMisbehavior(u, "eth_call", common.DataFinalityStateUnfinalized)
	}
}

func (p *stateProber) clearDisproved(id string) {
	if v, ok := p.disprovedStreak.Load(id); ok {
		v.(*atomic.Int64).Store(0)
	}
}

// integrityObserveOnly reports the network's base integrity observeOnly
// setting — the operator's blanket "measure, never intervene". Read directly
// (not via resolveIntegrity) because the prober is network-scoped: per-request
// selector profiles adjust CHECKS, not evidence recording.
func integrityObserveOnly(n common.Network) bool {
	cfg := n.Config()
	if cfg == nil || cfg.Integrity == nil || cfg.Integrity.ObserveOnly == nil {
		return false
	}
	return *cfg.Integrity.ObserveOnly
}

func (p *stateProber) count(u common.Upstream, probe, outcome string) {
	telemetry.MetricUpstreamStateProbe.WithLabelValues(
		p.projectId(), u.VendorName(), p.network.Label(), u.Id(), probe, outcome,
	).Inc()
}

func (p *stateProber) projectId() string {
	if p.network == nil {
		return ""
	}
	return p.network.ProjectId()
}

// parseHexQuantity decodes a JSON-encoded 32-byte hex return value into an int64.
func parseHexQuantity(raw string) (int64, bool) {
	s := strings.Trim(raw, `"`)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if s == "" {
		return 0, false
	}
	// returndata is left-padded to 32 bytes; the height fits the low bytes
	if len(s) > 16 {
		s = s[len(s)-16:]
	}
	n, err := common.HexToInt64("0x" + s)
	if err != nil {
		return 0, false
	}
	return n, true
}

func hexBytes(s string) ([]byte, error) {
	s = strings.Trim(s, `"`)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if len(s)%2 == 1 {
		s = "0" + s
	}
	out := make([]byte, len(s)/2)
	_, err := fmt.Sscanf(s, "%x", &out)
	return out, err
}

// isMethodUnsupportedErr recognises "this provider does not serve eth_getProof"
// so the probe is disabled for that upstream instead of erroring every block.
func isMethodUnsupportedErr(err error) bool {
	if err == nil {
		return false
	}
	if common.HasErrorCode(err, common.ErrCodeEndpointUnsupported) ||
		common.HasErrorCode(err, common.ErrCodeUpstreamMethodIgnored) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "method not found") ||
		strings.Contains(msg, "not supported") ||
		strings.Contains(msg, "does not exist/is not available")
}
