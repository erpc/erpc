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
	"github.com/erpc/erpc/telemetry"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/rs/zerolog/log"
)

// The state prober answers a question nothing else in erpc can: does this
// upstream ACTUALLY hold the state it claims? A node can report head N and
// still answer eth_call/eth_getBalance from older state — silently, with a
// well-formed response. The claimed head says nothing about the state trie.
//
// Two probes, both judged against the chain follower's VERIFIED header for a
// height (the trust anchor — without the follower this verification would be
// circular, judging the node against its own claims):
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
// A success advances the upstream's state-proven head (monotonic); routing for
// state methods asserts AvailbilityConfidenceStateProven against it. Failure
// just fails to advance — a mismatch at the tip can be a fork-transient, so it
// is counted and logged, never scored as misbehavior.
// The context-probe target is per-ARCHITECTURE (integrity.ChainStateContextProbe):
// standard EVMs use Multicall3 getBlockNumber, but e.g. Nitro's block.number is
// the L1 height and needs ArbSys arbBlockNumber instead — assuming one probe
// for all chains mislabelled every honest arbitrum upstream as stale.

// stateProbeMinInterval is the default floor between probes of one upstream.
const stateProbeMinInterval = 2 * time.Second

type stateProber struct {
	network  common.Network
	view     *chainView // the follower view whose verified headers anchor the probes
	interval time.Duration
	ctxProbe *integrity.StateContextProbe // per-architecture "which block are you in"

	mu        sync.Mutex
	lastProbe map[string]time.Time // upstream id -> last probe start

	// proofUnsupported remembers upstreams whose provider does not expose
	// eth_getProof, so the probe is not re-attempted on every block.
	proofUnsupported sync.Map // upstream id -> true

	// disprovedStreak counts CONSECUTIVE probe mismatches per upstream.
	//
	// The distinction it exists to draw: an upstream that cannot be probed has
	// merely FAILED TO PROVE itself, and must keep serving (the boundary falls
	// back to its claimed head). An upstream that answers the probe and executes
	// at the wrong height has proven the OPPOSITE — measured on shadow hyperevm,
	// where one upstream returned pin_ignored on 202 of 202 probes while all six
	// of its siblings matched on all 202. Treating those two as the same thing
	// is what let a demonstrably wrong node keep serving state calls.
	disprovedStreak sync.Map // upstream id -> *atomic.Int64

	// work carries the newest followed head; size 1, newest wins — probing is
	// against the tip, so intermediate heights are worthless once superseded.
	work chan int64
}

// stateProbers is the per-network registry (the prober is network-scoped even
// though follower views are group-scoped — one probe pass covers every
// upstream, whichever view's verified header anchors it).
var stateProbers sync.Map // networkId -> *stateProber

func startStateProber(n common.Network, v *chainView, cfg *common.IntegrityStateProbeConfig) {
	interval := stateProbeMinInterval
	if cfg != nil && cfg.Interval > 0 {
		interval = cfg.Interval.Duration()
	}
	var chainId int64
	if c := n.Config(); c != nil && c.Evm != nil {
		chainId = c.Evm.ChainId
	}
	p := &stateProber{
		network:   n,
		view:      v,
		interval:  interval,
		ctxProbe:  integrity.ChainStateContextProbe(chainId),
		lastProbe: map[string]time.Time{},
		work:      make(chan int64, 1),
	}
	if _, loaded := stateProbers.LoadOrStore(n.Id(), p); loaded {
		return // another view won the race; one prober per network
	}
	go p.run()
}

// stateProberFor is the routing gate's cheap activity test: nil means probing
// is off for this network and the gate must be a no-op.
func stateProberFor(networkId string) *stateProber {
	v, ok := stateProbers.Load(networkId)
	if !ok {
		return nil
	}
	return v.(*stateProber)
}

// onNewHead hands the prober a freshly followed (verified) height. Non-blocking:
// if a probe pass is still draining the previous head, the newest replaces it.
func (p *stateProber) onNewHead(n int64) {
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

func (p *stateProber) run() {
	for n := range p.work {
		p.probeAll(n)
	}
}

func (p *stateProber) probeAll(headN int64) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, u := range enum.EvmAllUpstreams(ctx) {
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

// probeUpstream runs both probes against one upstream and advances its proven
// head when the evidence supports it.
func (p *stateProber) probeUpstream(ctx context.Context, u common.Upstream, n int64, h *integrity.Header) {
	ctxMatch := p.probeContext(ctx, u, n)
	proofMatch := p.probeProof(ctx, u, n, h)

	// Advance on any positive proof, refuse on any negative one. "unknown"
	// (unsupported/error) neither advances nor blocks — but if BOTH probes are
	// unknown there is no evidence at all, and the boundary stays put (the
	// assert falls back to the claimed head for such upstreams, visibly).
	if ctxMatch == probeMismatch || proofMatch == probeMismatch {
		p.noteDisproved(u.Id())
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
	body := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"%s","data":"%s"},"0x%x"]}`,
		p.ctxProbe.To, p.ctxProbe.Data, n)
	res, err := p.forwardTo(ctx, u, body)
	if err != nil {
		p.count(u, "context", "error")
		return probeUnknown
	}
	if res == "" || res == "0x" {
		// No returndata: Multicall3 is not deployed on this chain (or this
		// node prunes code) — no evidence, not a failure.
		p.count(u, "context", "unsupported")
		return probeUnknown
	}
	got, ok := parseHexQuantity(res)
	if !ok {
		p.count(u, "context", "error")
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

// probeProof verifies the upstream can produce an account proof rooted at the
// VERIFIED stateRoot for N — unforgeable without the state trie.
func (p *stateProber) probeProof(ctx context.Context, u common.Upstream, n int64, h *integrity.Header) probeOutcome {
	if h.StateRoot == "" {
		return probeUnknown
	}
	if _, off := p.proofUnsupported.Load(u.Id()); off {
		return probeUnknown
	}
	body := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getProof","params":["%s",[],"0x%x"]}`,
		p.ctxProbe.To, n)
	res, err := p.forwardTo(ctx, u, body)
	if err != nil {
		if isMethodUnsupportedErr(err) {
			p.proofUnsupported.Store(u.Id(), true)
			p.count(u, "proof", "unsupported")
		} else {
			p.count(u, "proof", "error")
		}
		return probeUnknown
	}
	var proof struct {
		AccountProof []string `json:"accountProof"`
	}
	if json.Unmarshal([]byte(res), &proof) != nil || len(proof.AccountProof) == 0 {
		p.count(u, "proof", "error")
		return probeUnknown
	}
	node0, err0 := hexBytes(proof.AccountProof[0])
	want, err1 := hexBytes(h.StateRoot)
	if err0 != nil || err1 != nil || len(want) != 32 {
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

// forwardTo sends one probe to EXACTLY the given upstream: cache bypassed (the
// cache would answer instead of the node — the circular-evidence trap), and
// marked internal so the state-boundary gate itself ignores it (the probe must
// be able to reach an upstream whose boundary has not advanced yet).
func (p *stateProber) forwardTo(ctx context.Context, u common.Upstream, body string) (string, error) {
	req := common.NewNormalizedRequest([]byte(body))
	req.SetDirectives(&common.RequestDirectives{
		IsInternal:    true,
		UseUpstream:   u.Id(),
		SkipCacheRead: "true",
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
// disproved rather than merely unproven. At the shadow's 2s interval that is
// ~20s of unbroken evidence; the observed real case ran 202/202, so the
// threshold exists only to rule out a transient (a probe landing across a
// reorg, a momentary lag spike), not to be a tuning knob.
const stateProbeDisprovedStreak = 10

func (p *stateProber) noteDisproved(id string) {
	v, _ := p.disprovedStreak.LoadOrStore(id, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

func (p *stateProber) clearDisproved(id string) {
	if v, ok := p.disprovedStreak.Load(id); ok {
		v.(*atomic.Int64).Store(0)
	}
}

// disproved reports whether an upstream has answered probes wrongly for long
// enough that its silence about the state trie is evidence, not a gap.
func (p *stateProber) disproved(id string) bool {
	v, ok := p.disprovedStreak.Load(id)
	if !ok {
		return false
	}
	return v.(*atomic.Int64).Load() >= stateProbeDisprovedStreak
}

// aSiblingCanServe reports whether some OTHER upstream is a credible
// alternative for `block`.
//
// Diverting away from a disproved upstream is conditioned on this because the
// alternative is the failure mode that took Base down: a selection rule that
// EXCLUDES rather than deprioritizes turns "every candidate looks bad" into an
// outage. If nothing else can serve the height, the disproved upstream still
// serves — wrong data beats no data, and the operator sees it in the probe
// metrics either way.
//
// Two tiers, because requiring PROOF at the exact height leaves the newest
// blocks unprotected: the proven head necessarily lags the followed tip (~15
// blocks on the shadow), while the defect is present at every depth — the
// upstream this was built for ignored the pin identically at the tip and 5,000
// blocks back. A sibling carrying no such evidence, whose claimed head covers
// the height, is still strictly better than one proven to answer at the wrong
// height.
func (p *stateProber) aSiblingCanServe(ctx context.Context, exceptId string, block int64) bool {
	enum, ok := p.network.(interface {
		EvmAllUpstreams(context.Context) []common.Upstream
	})
	if !ok {
		return false
	}
	fallback := false
	for _, u := range enum.EvmAllUpstreams(ctx) {
		if u.Id() == exceptId || p.disproved(u.Id()) {
			continue
		}
		// Strongest: a sibling that has PROVEN state at or beyond the height.
		if r, ok := u.(common.EvmStateProvenReader); ok && r.EvmStateProvenBlock() >= block {
			return true
		}
		if fallback {
			continue
		}
		if eu, ok := u.(common.EvmUpstream); ok {
			avail, err := eu.EvmAssertBlockAvailability(ctx, "eth_call", common.AvailbilityConfidenceBlockHead, false, block)
			if err == nil && avail {
				fallback = true
			}
		}
	}
	return fallback
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
