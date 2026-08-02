package evm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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
const (
	multicall3Address        = "0xcA11bde05977b3631167028862bE2a173976CA11"
	multicall3GetBlockNumber = "0x42cbb15c"
)

// stateProbeMinInterval is the default floor between probes of one upstream.
const stateProbeMinInterval = 2 * time.Second

type stateProber struct {
	network  common.Network
	view     *chainView // the follower view whose verified headers anchor the probes
	interval time.Duration

	mu        sync.Mutex
	lastProbe map[string]time.Time // upstream id -> last probe start

	// proofUnsupported remembers upstreams whose provider does not expose
	// eth_getProof, so the probe is not re-attempted on every block.
	proofUnsupported sync.Map // upstream id -> true

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
	p := &stateProber{
		network:   n,
		view:      v,
		interval:  interval,
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
		return
	}
	if ctxMatch != probeMatch && proofMatch != probeMatch {
		return
	}
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
		multicall3Address, multicall3GetBlockNumber, n)
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
		p.count(u, "context", "mismatch")
		log.Warn().Str("network", p.network.Label()).Str("upstream", u.Id()).
			Int64("pinnedBlock", n).Int64("executedBlock", got).
			Msg("state probe: upstream executed a pinned call in a DIFFERENT block context (stale state)")
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
		multicall3Address, n)
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
