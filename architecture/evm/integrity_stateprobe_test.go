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
	mkReq := func(block string, internal bool) *common.NormalizedRequest {
		r := common.NewNormalizedRequest([]byte(fmt.Sprintf(
			`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x1234"},"%s"]}`, block)))
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
		handled, resp, err := upstreamPreForward_stateBoundary(context.Background(), n, common.NewFakeUpstream("u"), mkReq("0x100", false), "eth_call")
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
		handled, _, err := upstreamPreForward_stateBoundary(context.Background(), n, u, mkReq("0xe0", false), "eth_call")
		assert.False(t, handled)
		assert.NoError(t, err)
		// beyond proof: refuse with a retryable block-unavailable error
		handled, _, err = upstreamPreForward_stateBoundary(context.Background(), n, u, mkReq("0x100", false), "eth_call")
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
		handled, _, err := upstreamPreForward_stateBoundary(context.Background(), n, u, mkReq("0x100", true), "eth_call")
		assert.False(t, handled)
		assert.NoError(t, err)
	})

	t.Run("tag requests are not gated (the boundary is about concrete heights)", func(t *testing.T) {
		n := netFor("evm:7777")
		stateProbers.Store("evm:7777", &stateProber{})
		defer stateProbers.Delete("evm:7777")
		u := &gateUpstream{FakeUpstream: common.NewFakeUpstream("u2").(*common.FakeUpstream), proven: 1}
		handled, _, err := upstreamPreForward_stateBoundary(context.Background(), n, u, mkReq("latest", false), "eth_call")
		assert.False(t, handled)
		assert.NoError(t, err)
	})
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
