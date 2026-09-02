package evm

import (
	"context"
	"fmt"
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
// proven head; every deviation withholds it.
func TestStateProber(t *testing.T) {
	const head = int64(1000)
	trieNode := []byte("state-trie-root-node-payload")
	stateRoot := fmt.Sprintf("0x%x", gethcrypto.Keccak256(trieNode))

	t.Run("both probes match: the proven head advances", func(t *testing.T) {
		pn := newProbeNetwork(t)
		pn.execContext, pn.proofNode = head, trieNode
		p, _ := proberFor(pn, head, stateRoot)
		p.probeAll(head)
		assert.EqualValues(t, head, pn.upstream.EvmStateProvenBlock())
	})

	t.Run("a PIN-IGNORING node (executes at latest, not the pin) is also never proven", func(t *testing.T) {
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

	t.Run("STALE EXECUTION CONTEXT is never proven — the exact silent-bad-data case", func(t *testing.T) {
		pn := newProbeNetwork(t)
		pn.execContext, pn.proofNode = head-7, trieNode // claims head, executes 7 back
		p, _ := proberFor(pn, head, stateRoot)
		p.probeAll(head)
		assert.EqualValues(t, 0, pn.upstream.EvmStateProvenBlock(),
			"a node executing pinned calls in an older context must never be proven at the pin")
	})

	t.Run("a proof not rooted at the verified stateRoot does not prove, even with a matching context", func(t *testing.T) {
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
// A pre-forward boundary that refused any state request above the proven head
// therefore refused the very tip eRPC itself advertised, on nearly every
// upstream at nearly every moment — while the nodes were all behaving
// correctly.
//
// The invariant this test pins: a height the network advertises as "latest"
// must pass the upstream pre-forward hook on every honest upstream, prober
// active or not — structural probe lag is absence of proof, and absence of
// proof never blocks routing.
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
		if u.(*cadenceLaggedUpstream).EvmStateProvenBlock() < tip {
			beyondProof++
		}
	}
	require.GreaterOrEqual(t, beyondProof, len(fleet)/2+1,
		"fixture must keep its bite: the advertised tip is above most proven heads")

	// A running prober must make no difference: it publishes evidence (proven
	// heads, misbehavior), never routing refusals.
	n := &mockNetwork{}
	n.On("Id").Return("evm:42161").Maybe()
	stateProbers.Store("evm:42161", &stateProber{network: &fleetNetwork{ups: ups}})
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
