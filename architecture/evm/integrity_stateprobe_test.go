package evm

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// probeNetwork simulates one upstream's node for the two probes: what block
// context its EVM executes in, and what state trie its getProof answers from.
type probeNetwork struct {
	mockNetwork
	upstream *common.FakeUpstream

	execContext int64  // the height eth_call ACTUALLY executes at
	proofNode   []byte // accountProof[0] it serves
	proofErr    string // non-empty -> getProof errors with this message
}

func (p *probeNetwork) EvmAllUpstreams(ctx context.Context) []common.Upstream {
	return []common.Upstream{p.upstream}
}

func newProbeNetwork(t *testing.T) *probeNetwork {
	t.Helper()
	pn := &probeNetwork{upstream: common.NewFakeUpstream("u1").(*common.FakeUpstream)}
	pn.On("Id").Return("evm:1").Maybe()
	pn.On("ProjectId").Return("test").Maybe()
	pn.On("Label").Return("mainnet").Maybe()
	pn.On("Forward", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			jrq, _ := req.JsonRpcRequest()
			// The probe must pin EXACTLY one upstream and must bypass the cache:
			// answering a probe from the cache would be the circular-evidence
			// trap all over again.
			dirs := req.Directives()
			if dirs == nil || dirs.UseUpstream != "u1" || dirs.SkipCacheRead != "true" || !dirs.IsInternal {
				return nil, fmt.Errorf("probe request missing required directives: %+v", dirs)
			}
			switch jrq.Method {
			case "eth_call":
				ret := fmt.Sprintf(`"0x%064x"`, pn.execContext)
				return common.NewNormalizedResponse().WithJsonRpcResponse(
					common.MustNewJsonRpcResponseFromBytes([]byte(`"1"`), []byte(ret), nil)), nil
			case "eth_getProof":
				if pn.proofErr != "" {
					return nil, fmt.Errorf("%s", pn.proofErr)
				}
				body := fmt.Sprintf(`{"accountProof":["0x%x"]}`, pn.proofNode)
				return common.NewNormalizedResponse().WithJsonRpcResponse(
					common.MustNewJsonRpcResponseFromBytes([]byte(`"1"`), []byte(body), nil)), nil
			}
			return nil, fmt.Errorf("unexpected method %s", jrq.Method)
		}, nil,
	).Maybe()
	return pn
}

func proberFor(pn *probeNetwork, headN int64, stateRoot string) (*stateProber, *chainView) {
	v := newChainView(pn, 32, "", "", nil)
	v.adoptFollowed(headN, &integrity.Header{
		Number: fmt.Sprintf("0x%x", headN), Hash: "0xhead", ParentHash: "0xpar", StateRoot: stateRoot,
	})
	p := &stateProber{
		network: pn, view: v, interval: 0,
		ctxProbe:  integrity.ChainStateContextProbe(1),
		lastProbe: map[string]time.Time{}, work: make(chan int64, 1),
	}
	return p, v
}

// The whole feature in one scenario: a node whose EVM truly executes at the
// pinned height and whose proof roots at the VERIFIED stateRoot earns the
// boundary; every deviation refuses it.
func TestStateProber(t *testing.T) {
	const head = int64(1000)
	trieNode := []byte("state-trie-root-node-payload")
	stateRoot := fmt.Sprintf("0x%x", gethcrypto.Keccak256(trieNode))

	t.Run("both probes match: the boundary advances", func(t *testing.T) {
		pn := newProbeNetwork(t)
		pn.execContext, pn.proofNode = head, trieNode
		p, _ := proberFor(pn, head, stateRoot)
		p.probeAll(head)
		assert.EqualValues(t, head, pn.upstream.EvmStateProvenBlock())
	})

	t.Run("a PIN-IGNORING node (executes at latest, not the pin) also refuses the boundary", func(t *testing.T) {
		// Measured live on a vendor endpoint: claimed head exactly current,
		// but every call pinned at N executed at N+3..N+4 — the node ignores
		// the block parameter and answers historical questions with present
		// state. Not stale; worse in a different direction.
		pn := newProbeNetwork(t)
		pn.execContext, pn.proofNode = head+4, trieNode
		p, _ := proberFor(pn, head, stateRoot)
		p.probeAll(head)
		assert.EqualValues(t, 0, pn.upstream.EvmStateProvenBlock(),
			"executing AHEAD of the pin means the pin was ignored — never proven")
	})

	t.Run("STALE EXECUTION CONTEXT refuses the boundary — the exact silent-bad-data case", func(t *testing.T) {
		pn := newProbeNetwork(t)
		pn.execContext, pn.proofNode = head-7, trieNode // claims head, executes 7 back
		p, _ := proberFor(pn, head, stateRoot)
		p.probeAll(head)
		assert.EqualValues(t, 0, pn.upstream.EvmStateProvenBlock(),
			"a node executing pinned calls in an older context must never be proven at the pin")
	})

	t.Run("a proof not rooted at the verified stateRoot refuses, even with a matching context", func(t *testing.T) {
		pn := newProbeNetwork(t)
		pn.execContext, pn.proofNode = head, []byte("some-other-trie")
		p, _ := proberFor(pn, head, stateRoot)
		p.probeAll(head)
		assert.EqualValues(t, 0, pn.upstream.EvmStateProvenBlock(),
			"the context call is spoofable in principle; the proof layer must hold on its own")
	})

	t.Run("getProof unsupported: the context probe alone advances, and support is not re-asked", func(t *testing.T) {
		pn := newProbeNetwork(t)
		pn.execContext, pn.proofErr = head, "the method eth_getProof does not exist/is not available"
		p, _ := proberFor(pn, head, stateRoot)
		p.probeAll(head)
		assert.EqualValues(t, head, pn.upstream.EvmStateProvenBlock())
		_, remembered := p.proofUnsupported.Load("u1")
		assert.True(t, remembered, "unsupported must be remembered, not rediscovered every block")
	})

	t.Run("no verified header at the height: nothing advances (no anchor, no proof)", func(t *testing.T) {
		pn := newProbeNetwork(t)
		pn.execContext, pn.proofNode = head, trieNode
		p, _ := proberFor(pn, head, stateRoot)
		p.probeAll(head + 5) // a height the follower never verified
		assert.EqualValues(t, 0, pn.upstream.EvmStateProvenBlock())
	})
}

// The routing gate: inert with the prober off; never gates internal (probe)
// traffic; diverts ONLY an upstream the probes have DISPROVED. Absence of
// proof never blocks routing — see
// TestStateBoundary_TipChosenFromClaimedHeadsMustRoute for why that
// distinction is load-bearing on a fast chain.
func TestStateBoundaryGate(t *testing.T) {
	mkReq := func(method, block string, internal bool) *common.NormalizedRequest {
		r := common.NewNormalizedRequest([]byte(fmt.Sprintf(
			`{"jsonrpc":"2.0","id":1,"method":"%s","params":[{"to":"0x1234"},"%s"]}`, method, block)))
		if internal {
			r.SetDirectives(&common.RequestDirectives{IsInternal: true})
		}
		return r
	}
	netFor := func(id string) *mockNetwork {
		n := &mockNetwork{}
		n.On("Id").Return(id).Maybe()
		return n
	}
	disprove := func(p *stateProber, id string) {
		for i := 0; i < stateProbeDisprovedStreak; i++ {
			p.noteDisproved(id)
		}
	}
	// A prober whose sibling scan finds a healthy alternative for "u2".
	proberWithSibling := func() *stateProber {
		return &stateProber{network: &siblingNetwork{ups: []common.Upstream{
			common.NewFakeUpstream("u2"), provenUp("healthy-sibling", 1<<40),
		}}}
	}

	t.Run("no prober registered: byte-identical passthrough", func(t *testing.T) {
		n := netFor("evm:404")
		handled, resp, err := upstreamPreForward_stateBoundary(context.Background(), n, common.NewFakeUpstream("u"), mkReq("eth_call", "0x100", false), "eth_call")
		assert.False(t, handled)
		assert.Nil(t, resp)
		assert.NoError(t, err)
	})

	t.Run("a height far beyond the proven head passes: absence of proof is not disproof", func(t *testing.T) {
		n := netFor("evm:7777")
		stateProbers.Store("evm:7777", proberWithSibling())
		defer stateProbers.Delete("evm:7777")
		u := common.NewFakeUpstream("u2")
		if w, ok := u.(common.EvmStateProvenWriter); ok {
			w.EvmSetStateProvenBlock(0x0f0) // proven long ago; request far above it
		}
		handled, _, err := upstreamPreForward_stateBoundary(context.Background(), n, u, mkReq("eth_call", "0x100000", false), "eth_call")
		assert.False(t, handled,
			"the proven head lags the claimed head structurally (probe cadence); it must never refuse routing")
		assert.NoError(t, err)
	})

	t.Run("a DISPROVED upstream is diverted when a sibling can serve", func(t *testing.T) {
		n := netFor("evm:7777")
		n.On("Config").Return(&common.NetworkConfig{}).Maybe()
		p := proberWithSibling()
		disprove(p, "u2")
		stateProbers.Store("evm:7777", p)
		defer stateProbers.Delete("evm:7777")
		handled, _, err := upstreamPreForward_stateBoundary(context.Background(), n, common.NewFakeUpstream("u2"), mkReq("eth_call", "0x100", false), "eth_call")
		assert.True(t, handled)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamBlockUnavailable))
		assert.True(t, strings.Contains(err.Error(), "DISPROVEN"), err.Error())
	})

	t.Run("a DISPROVED last resort keeps serving — wrong data beats no data", func(t *testing.T) {
		n := netFor("evm:7777")
		p := &stateProber{network: &siblingNetwork{ups: []common.Upstream{common.NewFakeUpstream("u2")}}}
		disprove(p, "u2")
		stateProbers.Store("evm:7777", p)
		defer stateProbers.Delete("evm:7777")
		handled, _, err := upstreamPreForward_stateBoundary(context.Background(), n, common.NewFakeUpstream("u2"), mkReq("eth_call", "0x100", false), "eth_call")
		assert.False(t, handled)
		assert.NoError(t, err)
	})

	t.Run("observeOnly suppresses the diversion — it is documented ABSOLUTE over integrity effects", func(t *testing.T) {
		n := netFor("evm:7777")
		n.On("Config").Return(&common.NetworkConfig{
			Integrity: &common.IntegrityConfig{IntegritySettings: common.IntegritySettings{ObserveOnly: util.BoolPtr(true)}},
		}).Maybe()
		p := proberWithSibling()
		disprove(p, "u2")
		stateProbers.Store("evm:7777", p)
		defer stateProbers.Delete("evm:7777")
		handled, _, err := upstreamPreForward_stateBoundary(context.Background(), n, common.NewFakeUpstream("u2"), mkReq("eth_call", "0x100", false), "eth_call")
		assert.False(t, handled, "observeOnly network must never see a routing intervention")
		assert.NoError(t, err)
	})

	t.Run("internal probe traffic is never gated — gating it would deadlock the proving", func(t *testing.T) {
		n := netFor("evm:7777")
		p := proberWithSibling()
		disprove(p, "u2")
		stateProbers.Store("evm:7777", p)
		defer stateProbers.Delete("evm:7777")
		handled, _, err := upstreamPreForward_stateBoundary(context.Background(), n, common.NewFakeUpstream("u2"), mkReq("eth_call", "0x100", true), "eth_call")
		assert.False(t, handled)
		assert.NoError(t, err)
	})

	t.Run("tag requests are not diverted (the sibling check needs a concrete height)", func(t *testing.T) {
		n := netFor("evm:7777")
		p := proberWithSibling()
		disprove(p, "u2")
		stateProbers.Store("evm:7777", p)
		defer stateProbers.Delete("evm:7777")
		handled, _, err := upstreamPreForward_stateBoundary(context.Background(), n, common.NewFakeUpstream("u2"), mkReq("eth_call", "latest", false), "eth_call")
		assert.False(t, handled)
		assert.NoError(t, err)
	})

	t.Run("non-canonical method casing is diverted identically to canonical", func(t *testing.T) {
		// Through the real dispatch: HandleUpstreamPreForward lowercases the
		// method to route into the boundary, so the per-method config lookup
		// (block-ref extraction, which the sibling check depends on) must
		// resolve case-insensitively too — otherwise ETH_CALL resolves no block
		// number and silently skips the diversion eth_call would have taken.
		n := netFor("evm:7777")
		n.On("Config").Return(&common.NetworkConfig{}).Maybe()
		p := proberWithSibling()
		disprove(p, "u2")
		stateProbers.Store("evm:7777", p)
		defer stateProbers.Delete("evm:7777")

		handled, _, err := HandleUpstreamPreForward(context.Background(), n, common.NewFakeUpstream("u2"), mkReq("ETH_CALL", "0x100", false), false)
		assert.True(t, handled, "a disproved upstream must be diverted regardless of method casing")
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamBlockUnavailable))
		assert.Contains(t, err.Error(), "ETH_CALL",
			"canonicalization is lookup-only: the wire method string stays verbatim")
	})
}

// The boundary protects a CLASS of methods — everything answered from the state
// trie at a block — and the routing switch is where a member gets forgotten: an
// ungated state method keeps flowing to a DISPROVED upstream, which is the
// silent-wrong-data case the diversion exists to prevent. Every case here goes
// through the public hook entry point, so dropping a method from the dispatch
// fails this test rather than silently disabling the protection for it.
//
// The params are written the way a client sends them (block parameter in its
// real position, later arguments present), so a wrong ReqRefs position shows up
// as an undiverted request instead of passing by accident.
func TestStateBoundaryGate_StateMethodCoverage(t *testing.T) {
	cases := []struct {
		method string
		params string // %s is where the block parameter goes
	}{
		{"eth_call", `[{"to":"0x1234"},"%s"]`},
		{"eth_getBalance", `["0x7F0d15C7FAae65896648C8273B6d7E43f58Fa842","%s"]`},
		{"eth_getCode", `["0x7F0d15C7FAae65896648C8273B6d7E43f58Fa842","%s"]`},
		{"eth_getStorageAt", `["0x7F0d15C7FAae65896648C8273B6d7E43f58Fa842","0x0","%s"]`},
		{"eth_getTransactionCount", `["0x7F0d15C7FAae65896648C8273B6d7E43f58Fa842","%s"]`},
		{"eth_estimateGas", `[{"to":"0x1234"},"%s"]`},
		// The state trie itself: block is the THIRD param, after the storage keys.
		{"eth_getProof", `["0x7F0d15C7FAae65896648C8273B6d7E43f58Fa842",["0x00"],"%s"]`},
		// EVM execution at a block, exactly like eth_call: block is the SECOND param.
		{"eth_simulateV1", `[{"blockStateCalls":[{"calls":[{"to":"0x1234"}]}]},"%s"]`},
		// Same, with a trailing tracer config the extraction must not mistake
		// for the block parameter.
		{"debug_traceCall", `[{"to":"0x1234"},"%s",{"tracer":"callTracer"}]`},
	}

	mkReq := func(method, params, block string) *common.NormalizedRequest {
		return common.NewNormalizedRequest([]byte(fmt.Sprintf(
			`{"jsonrpc":"2.0","id":1,"method":"%s","params":%s}`,
			method, fmt.Sprintf(params, block))))
	}

	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			n := &mockNetwork{}
			n.On("Id").Return("evm:7777").Maybe()
			n.On("Config").Return(&common.NetworkConfig{}).Maybe()
			p := &stateProber{network: &siblingNetwork{ups: []common.Upstream{
				common.NewFakeUpstream("u2"), provenUp("healthy-sibling", 1<<40),
			}}}
			for i := 0; i < stateProbeDisprovedStreak; i++ {
				p.noteDisproved("u2")
			}
			stateProbers.Store("evm:7777", p)
			defer stateProbers.Delete("evm:7777")
			u := common.NewFakeUpstream("u2")

			// Disproved + sibling available: diverted, whatever the depth.
			handled, _, err := HandleUpstreamPreForward(context.Background(), n, u, mkReq(tc.method, tc.params, "0x100"), false)
			assert.True(t, handled, "a state method must divert away from a disproved upstream")
			require.Error(t, err)
			assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamBlockUnavailable), err.Error())
			assert.Contains(t, err.Error(), tc.method, "the diversion must name the method it diverted")

			// A tag names no concrete height, so the sibling check cannot run —
			// unchanged from before this method joined the class.
			handled, _, err = HandleUpstreamPreForward(context.Background(), n, u, mkReq(tc.method, tc.params, "latest"), false)
			assert.False(t, handled, "tag requests are not diverted (no concrete height)")
			assert.NoError(t, err)
		})
	}
}

// Widening the class must not pull in chain-data methods: those read blocks,
// receipts and logs, which a node with a wrong-height STATE answer mode still
// answers correctly, and they have their own availability enforcement.
func TestStateBoundaryGate_ChainDataMethodsStayUngated(t *testing.T) {
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x100",false]}`,
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x100"]}`,
		`{"jsonrpc":"2.0","id":1,"method":"debug_traceBlockByNumber","params":["0x100"]}`,
		`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0xdead"]}`,
	} {
		n := &mockNetwork{}
		n.On("Id").Return("evm:7777").Maybe()
		p := &stateProber{network: &siblingNetwork{ups: []common.Upstream{
			common.NewFakeUpstream("u2"), provenUp("healthy-sibling", 1<<40),
		}}}
		for i := 0; i < stateProbeDisprovedStreak; i++ {
			p.noteDisproved("u2")
		}
		stateProbers.Store("evm:7777", p)
		handled, _, err := HandleUpstreamPreForward(context.Background(), n, common.NewFakeUpstream("u2"), common.NewNormalizedRequest([]byte(body)), false)
		stateProbers.Delete("evm:7777")
		assert.False(t, handled, body)
		assert.NoError(t, err, body)
	}
}

// A fast-chain fleet where structural probe lag exceeds nothing but the clock.
//
// eRPC advertises "latest" as the majority CLAIMED head (PickServedTip), and
// clients additionally pin state calls at concrete heights they derive
// themselves (an indexer calls eth_getBlockByNumber, then eth_call at that
// number). The state prober advances each upstream's PROVEN head only at probe
// cadence (follow interval + probe interval), so on a chain producing a block
// every ~250ms with a 2s probe floor, every HONEST upstream's proven head
// trails its claimed head by roughly 8-18 blocks at all times — purely as a
// function of cadence, not of node quality. An upstream reads 0 only for the
// instant after its own probe, and is back to ~8 by the next one.
//
// A boundary that refused any state request above the proven head therefore
// refused the very tip eRPC itself advertised, on nearly every upstream at
// nearly every moment — while the nodes were all behaving correctly.
//
// The invariant this test pins: a height the network advertises as "latest"
// must pass the state boundary on every upstream the probes have NOT disproved
// — structural probe lag is absence of proof, and absence of proof never
// blocks routing.
func TestStateBoundary_TipChosenFromClaimedHeadsMustRoute(t *testing.T) {
	const head = int64(374_000_000) // an L2-scale height; only the lags matter
	// Lags spanning one probe cycle on a ~250ms-block chain: every entry is an
	// honest node, differing only in how long ago its probe landed.
	fleet := []struct {
		id        string
		provenLag int64 // claimed head minus state-proven head
	}{
		{"upstream-a", 18},
		{"upstream-b", 13},
		{"upstream-c", 11},
		{"upstream-d", 11},
		{"upstream-e", 8},
		{"upstream-f", 0},
	}

	// The tip eRPC advertises: the majority order statistic over CLAIMED heads
	// (all live and within a block of each other).
	tips := make([]ServedTipInput, 0, len(fleet))
	ups := make([]common.Upstream, 0, len(fleet))
	for _, f := range fleet {
		tips = append(tips, ServedTipInput{UpstreamID: f.id, BlockNumber: head})
		u := &cadenceLaggedUpstream{
			FakeUpstream: common.NewFakeUpstream(f.id).(*common.FakeUpstream),
			claimed:      head,
		}
		if head-f.provenLag > 0 {
			u.EvmSetStateProvenBlock(head - f.provenLag)
		}
		ups = append(ups, u)
	}
	tip := PickServedTip(tips).Tip
	require.Equal(t, head, tip, "sanity: a healthy fleet's majority tip is the head")

	// The precondition, asserted so the fixture cannot rot into triviality: the
	// advertised tip exceeds the proven head of a strict majority of the fleet
	// (all but the just-probed one) — under a proven-head routing bound the
	// network's own "latest" is unroutable.
	beyondProof := 0
	for _, u := range ups {
		if r, ok := u.(common.EvmStateProvenReader); ok && r.EvmStateProvenBlock() < tip {
			beyondProof++
		}
	}
	require.GreaterOrEqual(t, beyondProof, len(fleet)/2+1,
		"fixture must keep its bite: the advertised tip is above most proven heads")

	n := &mockNetwork{}
	n.On("Id").Return("evm:42161").Maybe()
	stateProbers.Store("evm:42161", &stateProber{network: &siblingNetwork{ups: ups}})
	defer stateProbers.Delete("evm:42161")

	req := func() *common.NormalizedRequest {
		return common.NewNormalizedRequest([]byte(fmt.Sprintf(
			`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x1234"},"0x%x"]}`, tip)))
	}
	for _, u := range ups {
		handled, _, err := HandleUpstreamPreForward(context.Background(), n, u, req(), false)
		assert.False(t, handled,
			"%s: a tip the network advertises must be routable on an upstream nothing has disproved", u.Id())
		assert.NoError(t, err, u.Id())
	}
}

// cadenceLaggedUpstream answers availability asserts the way a real honest upstream
// does: bounded by the CLAIMED head for the known confidences — and bounded by
// the PROVEN head for any confidence it does not recognize. The second branch
// is the tripwire: if a routing bound on the proven head is ever reintroduced
// under a new AvailbilityConfidence, the fixture above goes red
// instead of silently passing through a permissive fake.
type cadenceLaggedUpstream struct {
	*common.FakeUpstream
	claimed int64
}

func (g *cadenceLaggedUpstream) EvmAssertBlockAvailability(ctx context.Context, forMethod string, confidence common.AvailbilityConfidence, forceFreshIfStale bool, blockNumber int64) (bool, error) {
	switch confidence {
	case common.AvailbilityConfidenceBlockHead, common.AvailbilityConfidenceFinalized:
		return blockNumber <= g.claimed, nil
	}
	return blockNumber <= g.EvmStateProvenBlock(), nil
}
