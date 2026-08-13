package evm

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
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
// traffic; refuses an upstream whose proven head does not cover the block.
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

	t.Run("no prober registered: byte-identical passthrough", func(t *testing.T) {
		n := netFor("evm:404")
		handled, resp, err := upstreamPreForward_stateBoundary(context.Background(), n, common.NewFakeUpstream("u"), mkReq("eth_call", "0x100", false), "eth_call")
		assert.False(t, handled)
		assert.Nil(t, resp)
		assert.NoError(t, err)
	})

	t.Run("with a prober active, a proven-covered block passes and an unproven one is refused", func(t *testing.T) {
		n := netFor("evm:7777")
		stateProbers.Store("evm:7777", &stateProber{})
		defer stateProbers.Delete("evm:7777")

		u := &gateUpstream{FakeUpstream: common.NewFakeUpstream("u2").(*common.FakeUpstream), proven: 0x0f0}
		// covered: 0x0e0 <= proven
		handled, _, err := upstreamPreForward_stateBoundary(context.Background(), n, u, mkReq("eth_call", "0xe0", false), "eth_call")
		assert.False(t, handled)
		assert.NoError(t, err)
		// beyond proof: refuse with a retryable block-unavailable error
		handled, _, err = upstreamPreForward_stateBoundary(context.Background(), n, u, mkReq("eth_call", "0x100", false), "eth_call")
		assert.True(t, handled)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamBlockUnavailable))
		assert.True(t, strings.Contains(err.Error(), "PROVEN"), err.Error())
	})

	t.Run("internal probe traffic is never gated — gating it would deadlock the proving", func(t *testing.T) {
		n := netFor("evm:7777")
		stateProbers.Store("evm:7777", &stateProber{})
		defer stateProbers.Delete("evm:7777")
		u := &gateUpstream{FakeUpstream: common.NewFakeUpstream("u2").(*common.FakeUpstream), proven: 1}
		handled, _, err := upstreamPreForward_stateBoundary(context.Background(), n, u, mkReq("eth_call", "0x100", true), "eth_call")
		assert.False(t, handled)
		assert.NoError(t, err)
	})

	t.Run("tag requests are not gated (the boundary is about concrete heights)", func(t *testing.T) {
		n := netFor("evm:7777")
		stateProbers.Store("evm:7777", &stateProber{})
		defer stateProbers.Delete("evm:7777")
		u := &gateUpstream{FakeUpstream: common.NewFakeUpstream("u2").(*common.FakeUpstream), proven: 1}
		handled, _, err := upstreamPreForward_stateBoundary(context.Background(), n, u, mkReq("eth_call", "latest", false), "eth_call")
		assert.False(t, handled)
		assert.NoError(t, err)
	})

	t.Run("non-canonical method casing is gated identically to canonical", func(t *testing.T) {
		// Through the real dispatch: HandleUpstreamPreForward lowercases the
		// method to route into the boundary, so the per-method config lookup
		// (block-ref extraction) must resolve case-insensitively too —
		// otherwise ETH_CALL resolves no block number and silently passes the
		// gate that eth_call would have refused.
		n := netFor("evm:7777")
		stateProbers.Store("evm:7777", &stateProber{})
		defer stateProbers.Delete("evm:7777")
		u := &gateUpstream{FakeUpstream: common.NewFakeUpstream("u2").(*common.FakeUpstream), proven: 0x0f0}

		// beyond proof: refused exactly like the lowercase request above
		handled, _, err := HandleUpstreamPreForward(context.Background(), n, u, mkReq("ETH_CALL", "0x100", false), false)
		assert.True(t, handled, "an unproven block must be refused regardless of method casing")
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamBlockUnavailable))
		assert.Contains(t, err.Error(), "ETH_CALL",
			"canonicalization is lookup-only: the wire method string stays verbatim")

		// covered by proof: passes with the same non-canonical casing
		handled, _, err = HandleUpstreamPreForward(context.Background(), n, u, mkReq("eth_Call", "0xe0", false), false)
		assert.False(t, handled)
		assert.NoError(t, err)
	})
}

// The boundary protects a CLASS of methods — everything answered from the state
// trie at a block — and the routing switch is where a member gets forgotten: an
// ungated state method reaches the upstream with no proof at all, which is the
// silent-wrong-data case the gate exists to prevent. Every case here goes
// through the public hook entry point, so dropping a method from the dispatch
// fails this test rather than silently disabling the protection for it.
//
// The params are written the way a client sends them (block parameter in its
// real position, later arguments present), so a wrong ReqRefs position shows up
// as an ungated request instead of passing by accident.
func TestStateBoundaryGate_StateMethodCoverage(t *testing.T) {
	const proven = int64(0x0f0)

	cases := []struct {
		method string
		params string // %s is where the block parameter goes
	}{
		// Already covered before this test existed — pinned so the six cannot
		// regress while the class is being widened.
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
			stateProbers.Store("evm:7777", &stateProber{})
			defer stateProbers.Delete("evm:7777")
			u := &gateUpstream{FakeUpstream: common.NewFakeUpstream("u2").(*common.FakeUpstream), proven: proven}

			// Below the proven head: the upstream demonstrably holds this state.
			handled, _, err := HandleUpstreamPreForward(context.Background(), n, u, mkReq(tc.method, tc.params, "0xe0"), false)
			assert.False(t, handled, "a block the upstream has PROVEN must pass through")
			assert.NoError(t, err)

			// Above the proven head: no proof covers it, so refuse and let
			// retry/consensus route to an upstream that can prove it.
			handled, _, err = HandleUpstreamPreForward(context.Background(), n, u, mkReq(tc.method, tc.params, "0x100"), false)
			assert.True(t, handled, "an unproven block must not be forwarded ungated")
			require.Error(t, err)
			assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamBlockUnavailable), err.Error())
			assert.Contains(t, err.Error(), tc.method, "the refusal must name the method it refused")

			// A tag names no concrete height, so there is nothing to compare it
			// against — unchanged from before this method joined the class.
			handled, _, err = HandleUpstreamPreForward(context.Background(), n, u, mkReq(tc.method, tc.params, "latest"), false)
			assert.False(t, handled, "tag requests are not gated (the boundary is about concrete heights)")
			assert.NoError(t, err)
		})
	}
}

// Widening the class must not pull in chain-data methods: those read blocks,
// receipts and logs, which a node with stale STATE still answers correctly, and
// they have their own availability enforcement.
func TestStateBoundaryGate_ChainDataMethodsStayUngated(t *testing.T) {
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x100",false]}`,
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x100"]}`,
		`{"jsonrpc":"2.0","id":1,"method":"debug_traceBlockByNumber","params":["0x100"]}`,
		`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0xdead"]}`,
	} {
		n := &mockNetwork{}
		n.On("Id").Return("evm:7777").Maybe()
		stateProbers.Store("evm:7777", &stateProber{})
		u := &gateUpstream{FakeUpstream: common.NewFakeUpstream("u2").(*common.FakeUpstream), proven: 0x0f0}
		handled, _, err := HandleUpstreamPreForward(context.Background(), n, u, common.NewNormalizedRequest([]byte(body)), false)
		stateProbers.Delete("evm:7777")
		assert.False(t, handled, body)
		assert.NoError(t, err, body)
	}
}

// gateUpstream lets a test dictate the proven head and route the assert through
// the real StateProven semantics (refuse above proven, allow at/below).
type gateUpstream struct {
	*common.FakeUpstream
	proven int64
}

func (g *gateUpstream) EvmStateProvenBlock() int64 { return g.proven }
func (g *gateUpstream) EvmAssertBlockAvailability(ctx context.Context, forMethod string, confidence common.AvailbilityConfidence, forceFreshIfStale bool, blockNumber int64) (bool, error) {
	if confidence == common.AvailbilityConfidenceStateProven && g.proven > 0 {
		return blockNumber <= g.proven, nil
	}
	return true, nil
}
