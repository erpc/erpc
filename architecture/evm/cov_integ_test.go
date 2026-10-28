package evm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	gethcommon "github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// covIntegNet is a scripted network: it records the directives of every
// forwarded request and answers through forward.
type covIntegNet struct {
	common.Network
	id      string
	cfg     *common.NetworkConfig
	mu      sync.Mutex
	dirs    []*common.RequestDirectives
	calls   int
	forward func(req *common.NormalizedRequest) (*common.NormalizedResponse, error)
}

func newCovIntegNet(id string, cfg *common.NetworkConfig, fwd func(req *common.NormalizedRequest) (*common.NormalizedResponse, error)) *covIntegNet {
	return &covIntegNet{id: id, cfg: cfg, forward: fwd}
}

func (n *covIntegNet) Id() string                    { return n.id }
func (n *covIntegNet) Label() string                 { return "covnet" }
func (n *covIntegNet) ProjectId() string             { return "test" }
func (n *covIntegNet) Config() *common.NetworkConfig { return n.cfg }
func (n *covIntegNet) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	n.mu.Lock()
	n.dirs = append(n.dirs, req.Directives())
	n.calls++
	n.mu.Unlock()
	return n.forward(req)
}

func (n *covIntegNet) callCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls
}

func (n *covIntegNet) lastDirs() *common.RequestDirectives {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.dirs) == 0 {
		return nil
	}
	return n.dirs[len(n.dirs)-1]
}

// covIntegFleetNet adds upstream enumeration (what the state prober needs).
type covIntegFleetNet struct {
	*covIntegNet
	ups []common.Upstream
}

func (n *covIntegFleetNet) EvmAllUpstreams(ctx context.Context) []common.Upstream { return n.ups }

// covIntegGroupNet carves the selector "grp" out as its own node group.
type covIntegGroupNet struct{ *covIntegNet }

func (n *covIntegGroupNet) EvmUpstreamGroupForSelector(ctx context.Context, sel string) (string, string) {
	if sel == "grp" {
		return "g:grp", "lane-grp"
	}
	return "", ""
}

// covIntegHeadNet reports network-wide latest/finalized heights.
type covIntegHeadNet struct {
	*covIntegNet
	latest, finalized int64
}

func (n *covIntegHeadNet) EvmHead(ctx context.Context, req *common.NormalizedRequest) common.EvmHead {
	return common.EvmHead{Latest: n.latest, Finalized: n.finalized}
}

func (n *covIntegHeadNet) EvmLaneHead(groupKey, selector string) common.EvmHead {
	return common.EvmHead{Latest: n.latest, Finalized: n.finalized}
}

func (n *covIntegHeadNet) EvmRefreshHead(context.Context, bool) {}

// covIntegFleetHeadNet is a fleet with a network-wide Latest.
type covIntegFleetHeadNet struct {
	*covIntegFleetNet
	latest int64
}

func (n *covIntegFleetHeadNet) EvmHead(context.Context, *common.NormalizedRequest) common.EvmHead {
	return common.EvmHead{Latest: n.latest}
}
func (n *covIntegFleetHeadNet) EvmLaneHead(string, string) common.EvmHead {
	return common.EvmHead{Latest: n.latest}
}
func (n *covIntegFleetHeadNet) EvmRefreshHead(context.Context, bool) {}

// covIntegRefusingNet is a fleet whose Forward applies the refusal rule
// first, as the network does before dispatch: a request's lane is the
// upstream it names, whose head counter is its Latest and ceiling, and
// each stale-ceiling or band Ask is counted by that upstream.
type covIntegRefusingNet struct {
	*covIntegFleetNet
	askMu sync.Mutex
	asks  map[string]int
}

func (n *covIntegRefusingNet) upstreamHead(id string) int64 {
	for _, u := range n.ups {
		if u.Id() == id {
			return u.(common.EvmUpstream).EvmEffectiveLatestBlock()
		}
	}
	return 0
}

func (n *covIntegRefusingNet) laneHead(sel string) common.EvmHead {
	h := n.upstreamHead(sel)
	return common.EvmHead{Latest: h, Ceiling: h}
}

func (n *covIntegRefusingNet) EvmHead(_ context.Context, req *common.NormalizedRequest) common.EvmHead {
	return n.laneHead(req.Directives().UseUpstream)
}
func (n *covIntegRefusingNet) EvmLaneHead(_, sel string) common.EvmHead { return n.laneHead(sel) }
func (n *covIntegRefusingNet) EvmRefreshHead(context.Context, bool)     {}
func (n *covIntegRefusingNet) EvmBlockTime() time.Duration              { return time.Second }
func (n *covIntegRefusingNet) EvmAskStale(_ context.Context, sel string) (int64, time.Time) {
	n.ask(sel)
	return n.upstreamHead(sel), time.Time{}
}
func (n *covIntegRefusingNet) EvmAskBand(sel string, _ int64) { n.ask(sel) }

func (n *covIntegRefusingNet) ask(sel string) {
	n.askMu.Lock()
	defer n.askMu.Unlock()
	n.asks[sel]++
}

func (n *covIntegRefusingNet) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	HeadRefusalOf(ctx, req)
	return n.covIntegNet.Forward(ctx, req)
}

// probesTo counts the requests forwarded to upstream id.
func (n *covIntegRefusingNet) probesTo(id string) (c int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, d := range n.dirs {
		if d != nil && d.UseUpstream == id {
			c++
		}
	}
	return c
}

// covIntegPlainUp hides every EVM capability of the wrapped upstream.
type covIntegPlainUp struct{ common.Upstream }

// covIntegWriterUp can record a proven head but is not an EvmUpstream.
type covIntegWriterUp struct {
	common.Upstream
	proven atomic.Int64
}

func (u *covIntegWriterUp) EvmSetStateProvenBlock(n int64) { u.proven.Store(n) }

func covIntegResp(result string) *common.NormalizedResponse {
	return common.NewNormalizedResponse().WithJsonRpcResponse(
		common.MustNewJsonRpcResponseFromBytes([]byte(`"1"`), []byte(result), nil))
}

// covIntegSweepNet also serves tracker fetches, through the same scripted
// forward.
type covIntegSweepNet struct{ *covIntegNet }

func (n *covIntegSweepNet) SweepUpstreams(ctx context.Context, req *common.NormalizedRequest, _ string, check func(*common.NormalizedResponse) error) (*common.NormalizedResponse, error) {
	resp, err := n.Forward(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp, check(resp)
}

// covIntegEmptyHeader is a header whose hash recomputes, with the empty-trie
// transactionsRoot and the given gasUsed.
func covIntegEmptyHeader(t *testing.T, n, gasUsed uint64) json.RawMessage {
	t.Helper()
	h := &gethtypes.Header{
		ParentHash: gethcommon.Hash{byte(n), 0xff}, UncleHash: gethtypes.EmptyUncleHash, TxHash: gethtypes.EmptyTxsHash,
		ReceiptHash: gethtypes.EmptyReceiptsHash, Difficulty: big.NewInt(0), Number: new(big.Int).SetUint64(n),
		GasLimit: 30_000_000, GasUsed: gasUsed, Time: 1_700_000_000 + n, BaseFee: big.NewInt(7),
	}
	raw, err := h.MarshalJSON()
	require.NoError(t, err)
	return raw
}

func covIntegHeldCount(c *ChainTracker) int {
	n := 0
	c.held.Range(func(_, _ any) bool { n++; return true })
	return n
}

func covIntegStreak(p *stateProber, id string) int64 {
	v, ok := p.disprovedStreak.Load(id)
	if !ok {
		return 0
	}
	return v.(*atomic.Int64).Load()
}

func TestCovInteg_ChainTrackerHeldData(t *testing.T) {
	t.Run("CachedReceipts serves only warm blocks", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		putReceipts(c, testHash("b", 1), []integrity.Receipt{{TransactionHash: "0x1"}})
		got, ok := c.CachedReceipts(testHash("b", 1))
		require.True(t, ok)
		assert.Equal(t, "0x1", got[0].TransactionHash)
		_, ok = c.CachedReceipts(testHash("c", 1))
		assert.False(t, ok)
	})

	t.Run("finality unknown when the getter reports no finalized height", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 8, 0, "", "", "", func() common.EvmHead { return common.EvmHead{Finalized: 0} }, newFetchScheduler(t.Context(), nil))
		assert.Equal(t, "unknown", c.finalityLabel(5))
	})
}

func TestCovInteg_ResolveAnchors(t *testing.T) {
	ctx := context.Background()

	t.Run("a by-number answer for another height is discarded", func(t *testing.T) {
		net := &covIntegSweepNet{newCovIntegNet("cov-integ-anchor", nil, func(req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return covIntegResp(string(testHeader(0x11, testHash("e", 0x11), testHash("e", 0x10)))), nil
		})}
		c := newChainTracker(t.Context(), net, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), net))
		h, ok := c.headerByNumber(ctx, 0x10, "0x10")
		assert.False(t, ok)
		assert.Nil(t, h)
		_, pinned := c.HashAt(0x11)
		assert.False(t, pinned)
		_, pinned = c.HashAt(0x10)
		assert.False(t, pinned)
	})

	want := testHash("b", 0x10)
	receiptsCases := []struct {
		name string
		body string
	}{
		{"another block's receipts", fmt.Sprintf(`[{"blockHash":"%s","blockNumber":"0x10","transactionHash":"0x1"}]`, testHash("c", 0x10))},
		{"an empty receipt set", `[]`},
	}
	for _, tc := range receiptsCases {
		t.Run(tc.name+" is canonical-unavailable and not cached", func(t *testing.T) {
			net := &covIntegSweepNet{newCovIntegNet("cov-integ-rcpt", nil, func(req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
				return covIntegResp(tc.body), nil
			})}
			c := newChainTracker(t.Context(), net, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), net))
			got, ok := c.receiptsByHash(ctx, want, -1)
			assert.False(t, ok)
			assert.Nil(t, got)
			_, cached := c.CachedReceipts(want)
			assert.False(t, cached)
		})
	}
}

func TestCovInteg_HasVerifiedEmptyTraceBlock(t *testing.T) {
	ctx := t.Context()
	net := &covIntegGroupNet{newCovIntegNet("cov-integ-trace", nil, nil)}
	mkReq := func(body string, withNet bool, dirs *common.RequestDirectives) *common.NormalizedRequest {
		r := common.NewNormalizedRequest([]byte(body))
		if withNet {
			r.SetNetwork(net)
		}
		if dirs != nil {
			r.SetDirectives(dirs)
		}
		return r
	}
	traceBody := func(ref string) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"trace_block","params":["%s"]}`, ref)
	}
	pin := func(c *ChainTracker, n, gasUsed uint64) {
		c.Observe("a", covIntegEmptyHeader(t, n, gasUsed), true)
		c.sync(context.Background())
	}
	pin(StartChainTrackers(ctx, net), 0x10, 0)

	assert.False(t, HasVerifiedEmptyTraceBlock(nil))
	assert.False(t, HasVerifiedEmptyTraceBlock(mkReq(traceBody("0x10"), false, nil)), "no network")
	assert.False(t, HasVerifiedEmptyTraceBlock(mkReq(`not-json`, true, nil)), "unparseable request")
	assert.False(t, HasVerifiedEmptyTraceBlock(mkReq(traceBody("16"), true, nil)), "decimal ref is not a hex block")
	assert.False(t, HasVerifiedEmptyTraceBlock(mkReq(traceBody("0xzz"), true, nil)), "malformed hex")
	assert.False(t, HasVerifiedEmptyTraceBlock(mkReq(traceBody("0x11"), true, nil)), "no pin at that height")
	assert.True(t, HasVerifiedEmptyTraceBlock(mkReq(traceBody("0x10"), true, nil)))

	t.Run("group-scoped request reads only its group's tracker", func(t *testing.T) {
		dirs := &common.RequestDirectives{UseUpstream: "grp"}
		assert.False(t, HasVerifiedEmptyTraceBlock(mkReq(traceBody("0x10"), true, dirs)), "group tracker absent")
		pin(ChainTrackerOf(ctx, net, "grp"), 0x20, 0)
		assert.True(t, HasVerifiedEmptyTraceBlock(mkReq(traceBody("0x20"), true, dirs)))
	})

	t.Run("a pinned header carrying gas is not proof", func(t *testing.T) {
		net2 := &covIntegGroupNet{newCovIntegNet("cov-integ-trace2", nil, nil)}
		pin(StartChainTrackers(ctx, net2), 0x30, 0x5208)
		r := common.NewNormalizedRequest([]byte(traceBody("0x30")))
		r.SetNetwork(net2)
		assert.False(t, HasVerifiedEmptyTraceBlock(r))
	})
}

func TestCovInteg_ChainTrackerOf(t *testing.T) {
	ctx := t.Context()
	assert.Nil(t, ChainTrackerOf(ctx, nil, ""))

	t.Run("selector carving a group gets its own tracker", func(t *testing.T) {
		net := &covIntegGroupNet{newCovIntegNet("cov-integ-gcv-grp", nil, nil)}
		assert.Nil(t, ChainTrackerOf(ctx, net, ""), "no tracker before the network starts its trackers")
		wide := StartChainTrackers(ctx, net)
		g := ChainTrackerOf(ctx, net, "grp")
		require.NotSame(t, g, wide)
		assert.Equal(t, "grp", g.selector)
		assert.Equal(t, "lane-grp", g.group)
		assert.Empty(t, wide.selector)
		assert.Same(t, wide, ChainTrackerOf(ctx, net, "not-a-group"), "a non-group selector falls back to the network-wide tracker")
		assert.Same(t, g, ChainTrackerOf(ctx, net, "grp"), "trackers are created once")
		assert.Equal(t, 32, g.ReorgWindow())
		assert.Nil(t, g.laneHead)
	})

	t.Run("chainTracker config sizes the window; integrity follow adds the pinned probes", func(t *testing.T) {
		cfg := &common.NetworkConfig{
			Evm: &common.EvmNetworkConfig{ChainId: 1, ChainTracker: &common.EvmChainTrackerConfig{ReorgWindow: 7}},
			Integrity: &common.IntegrityConfig{IntegritySettings: common.IntegritySettings{
				Follow:     &common.IntegrityFollowConfig{Enabled: util.BoolPtr(true)},
				StateProbe: &common.IntegrityStateProbeConfig{Enabled: util.BoolPtr(true), Interval: common.Duration(5 * time.Second)},
			}},
		}
		net := &covIntegHeadNet{covIntegNet: newCovIntegNet("cov-integ-gcv-follow", cfg, nil), finalized: 42}
		v := StartChainTrackers(ctx, net)
		assert.Nil(t, stateProberFor(net), "an idle network probes nothing")
		ClientRequested(net)
		p := stateProberFor(net)
		assert.Equal(t, 7, v.ReorgWindow())
		require.NotNil(t, p, "the first client request starts the probes")
		assert.Equal(t, 5*time.Second, p.interval)
		assert.True(t, p.pinnedProbes)
		assert.Same(t, v, p.view)
		assert.Equal(t, "finalized", v.finalityLabel(42), "finality comes from the network")
		assert.Equal(t, "unfinalized", v.finalityLabel(43))
	})

	t.Run("without follow the prober starts for the latest probe only", func(t *testing.T) {
		cfg := &common.NetworkConfig{Evm: &common.EvmNetworkConfig{ChainId: 1}, Integrity: &common.IntegrityConfig{IntegritySettings: common.IntegritySettings{
			StateProbe: &common.IntegrityStateProbeConfig{Enabled: util.BoolPtr(true)},
		}}}
		net := &covIntegHeadNet{covIntegNet: newCovIntegNet("cov-integ-gcv-nofollow", cfg, nil)}
		v := StartChainTrackers(ctx, net)
		assert.Nil(t, stateProberFor(net))
		ClientRequested(net)
		p := stateProberFor(net)
		require.NotNil(t, p, "every network probes tag-state heads from its first client request")
		assert.Same(t, v, p.view)
		assert.False(t, p.pinnedProbes, "context and proof probes still need follow")
		p.onNewHead(5)
		assert.Empty(t, p.work, "a followed head starts no pinned pass")
	})

	t.Run("network without a lane Head reads no head", func(t *testing.T) {
		assert.Nil(t, StartChainTrackers(ctx, newCovIntegNet("x", nil, nil)).laneHead)
		hn := &covIntegHeadNet{covIntegNet: newCovIntegNet("y", nil, nil), latest: 9}
		tr := StartChainTrackers(ctx, hn)
		require.NotNil(t, tr.laneHead)
		assert.Equal(t, uint64(9), tr.sendCeiling())
	})
}

func TestCovInteg_ObserveViews(t *testing.T) {
	ctx := context.Background()

	t.Run("observeBlockView ignores nil inputs and non-header results", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		observeBlockView(ctx, nil, nil, covIntegResp(string(testHeader(1, testHash("a", 1), testHash("a", 0)))))
		observeBlockView(ctx, c, nil, nil)
		observeBlockView(ctx, c, nil, covIntegResp(`"0x1"`))
		observeBlockView(ctx, c, nil, covIntegResp(`{"number":"0x1"}`))
		c.sync(ctx)
		_, ok := c.HashAt(1)
		assert.False(t, ok)
		assert.Zero(t, covIntegHeldCount(c))
	})

	t.Run("observeNarrowView anchors only from EVM upstreams at or below the lane's Head.Finalized", func(t *testing.T) {
		hash := testHash("b", 0x10)
		anchor := covIntegResp(fmt.Sprintf(`{"blockNumber":"0x10","blockHash":"%s"}`, hash))
		evmUp := common.NewFakeUpstream("u", common.WithEvmStatePoller(common.NewFakeEvmStatePoller(0x30, 0x20)))

		laneHead := func() common.EvmHead { return common.EvmHead{Latest: 0x30, Finalized: 0x20} }
		c := newChainTracker(t.Context(), nil, 32, 0, "", "", "", laneHead, newFetchScheduler(t.Context(), nil))
		observeNarrowView(ctx, nil, evmUp, anchor)
		observeNarrowView(ctx, c, evmUp, nil)
		observeNarrowView(ctx, c, covIntegPlainUp{evmUp}, anchor)
		c.sync(ctx)
		_, ok := c.HashAt(0x10)
		assert.False(t, ok, "a non-EVM upstream's answer anchors nothing")

		observeNarrowView(ctx, c, evmUp, covIntegResp(`{"blockNumber":"0x11","blockHash":""}`))
		c.sync(ctx)
		_, ok = c.HashAt(0x11)
		assert.False(t, ok, "an anchor without a hash anchors nothing")

		observeNarrowView(ctx, c, evmUp, anchor)
		c.sync(ctx)
		got, ok := c.HashAt(0x10)
		require.True(t, ok)
		assert.Equal(t, hash, got)
	})
}

func TestCovInteg_StateProber(t *testing.T) {
	ctx := context.Background()
	const head = int64(1000)
	hdr := &integrity.Header{Number: "0x3e8", Hash: "0xhead", ParentHash: "0xpar"}

	// callAnswer scripts eth_call; proofAnswer scripts eth_getProof.
	type script struct {
		callRes  string
		callErr  error
		callNil  bool
		proofRes string
		proofErr error
	}
	mkNet := func(id string, sc *script, ups ...common.Upstream) *covIntegFleetNet {
		return &covIntegFleetNet{
			covIntegNet: newCovIntegNet(id, nil, func(req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
				jrq, _ := req.JsonRpcRequest()
				switch jrq.Method {
				case "eth_call":
					if sc.callNil {
						return nil, nil
					}
					if sc.callErr != nil {
						return nil, sc.callErr
					}
					return covIntegResp(sc.callRes), nil
				case "eth_getProof":
					if sc.proofErr != nil {
						return nil, sc.proofErr
					}
					return covIntegResp(sc.proofRes), nil
				}
				return nil, fmt.Errorf("unexpected %s", jrq.Method)
			}),
			ups: ups,
		}
	}
	// The prober's view pins the head only: probes judge the upstreams, never
	// fetch through the tracker.
	mkProber := func(net common.Network, interval time.Duration) *stateProber {
		v := newChainTracker(t.Context(), nil, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		voteChain(t, v, "a", "e", uint64(head), uint64(head), testHash("f", uint64(head-1)))
		return &stateProber{
			network: net, view: v, interval: interval, pinnedProbes: true,
			lastProbe: map[string]time.Time{}, work: make(chan int64, 1),
		}
	}
	atHead := fmt.Sprintf(`"0x%064x"`, head)

	t.Run("context probe outcomes", func(t *testing.T) {
		cases := []struct {
			name    string
			sc      script
			ignore  bool
			want    probeOutcome
			forward bool
		}{
			{name: "match", sc: script{callRes: atHead}, want: probeMatch, forward: true},
			{name: "eth_call excluded by config", ignore: true, want: probeUnknown},
			{name: "transport error", sc: script{callErr: errors.New("dial tcp: i/o timeout")}, want: probeUnknown, forward: true},
			{name: "nil response", sc: script{callNil: true}, want: probeUnknown, forward: true},
			{name: "no returndata", sc: script{callRes: `"0x"`}, want: probeUnknown, forward: true},
			{name: "unparseable returndata", sc: script{callRes: `"0xzz"`}, want: probeUnknown, forward: true},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				u := common.NewFakeUpstream("u1")
				if tc.ignore {
					u.Config().IgnoreMethods = []string{"eth_call"}
				}
				net := mkNet("cov-integ-sp-ctx", &tc.sc, u)
				p := mkProber(net, 0)
				assert.Equal(t, tc.want, p.probeContext(ctx, u, head))
				assert.Equal(t, tc.forward, net.callCount() > 0)
			})
		}
	})

	t.Run("proof probe outcomes", func(t *testing.T) {
		root := "0x" + "1111111111111111111111111111111111111111111111111111111111111111"[:62] // 31 bytes
		cases := []struct {
			name    string
			sc      script
			root    string
			latched bool
			want    probeOutcome
			latch   bool
			forward bool
		}{
			{name: "no verified stateRoot", root: "", want: probeUnknown},
			{name: "already latched unsupported", root: root, latched: true, want: probeUnknown, latch: true},
			{name: "transient error is not latched", root: root, sc: script{proofErr: errors.New("connection reset by peer")}, want: probeUnknown, forward: true},
			{name: "method-ignored error latches", root: root, sc: script{proofErr: common.NewErrUpstreamMethodIgnored("eth_getProof", "u1")}, want: probeUnknown, latch: true, forward: true},
			{name: "endpoint-unsupported error latches", root: root, sc: script{proofErr: common.NewErrEndpointUnsupported(errors.New("nope"))}, want: probeUnknown, latch: true, forward: true},
			{name: "empty account proof", root: root, sc: script{proofRes: `{"accountProof":[]}`}, want: probeUnknown, forward: true},
			{name: "stateRoot not 32 bytes", root: root, sc: script{proofRes: `{"accountProof":["0xabcd"]}`}, want: probeUnknown, forward: true},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				u := common.NewFakeUpstream("u1")
				net := mkNet("cov-integ-sp-proof", &tc.sc, u)
				p := mkProber(net, 0)
				if tc.latched {
					p.stopProofs("u1")
				}
				h := &integrity.Header{StateRoot: tc.root}
				assert.Equal(t, tc.want, p.probeProof(ctx, u, head, h))
				assert.Equal(t, tc.latch, p.proofsOff("u1"))
				assert.Equal(t, tc.forward, net.callCount() > 0)
			})
		}
	})

	t.Run("10 proof errors in a row stop proofs for a while; an answer resets the count", func(t *testing.T) {
		root := "0x" + strings.Repeat("11", 32)
		u := common.NewFakeUpstream("u1")
		sc := &script{proofErr: errors.New("connection reset by peer")}
		net := mkNet("cov-integ-sp-proof-errs", sc, u)
		p := mkProber(net, 0)
		h := &integrity.Header{StateRoot: root}
		for range stateProbeProofErrors - 1 {
			p.probeProof(ctx, u, head, h)
		}
		sc.proofErr, sc.proofRes = nil, `{"accountProof":["0xabcd"]}`
		assert.Equal(t, probeMismatch, p.probeProof(ctx, u, head, h), "an answer, even a wrong one, ends the run of errors")
		sc.proofErr = errors.New("connection reset by peer")
		for range stateProbeProofErrors - 1 {
			p.probeProof(ctx, u, head, h)
		}
		assert.False(t, p.proofsOff("u1"), "9 errors since the last answer stop nothing")
		calls := net.callCount()
		p.probeProof(ctx, u, head, h)
		assert.True(t, p.proofsOff("u1"), "the 10th error in a row stops proof probes")
		p.probeProof(ctx, u, head, h)
		assert.Equal(t, calls+1, net.callCount(), "a stopped upstream gets no proof probe")

		p.proofOff.Store("u1", time.Now().Add(-time.Second))
		p.probeProof(ctx, u, head, h)
		assert.Equal(t, calls+2, net.callCount(), "the switch expires: proofs are probed again")
	})

	t.Run("no evidence at all leaves the streak untouched", func(t *testing.T) {
		u := common.NewFakeUpstream("u1").(*common.FakeUpstream)
		u.Config().IgnoreMethods = []string{"eth_call", "eth_getProof"}
		net := mkNet("cov-integ-sp-none", &script{}, u)
		p := mkProber(net, 0)
		p.noteDisproved(u)
		p.probeUpstream(ctx, u, head, &integrity.Header{StateRoot: "0x" + fmt.Sprintf("%064x", 1)})
		assert.Equal(t, int64(1), covIntegStreak(p, "u1"))
		assert.Zero(t, u.EvmStateProvenBlock())
		assert.Zero(t, net.callCount())
	})

	t.Run("a match from a non-writer clears the streak but records no head", func(t *testing.T) {
		u := covIntegPlainUp{common.NewFakeUpstream("u1")}
		net := mkNet("cov-integ-sp-plain", &script{callRes: atHead}, u)
		p := mkProber(net, 0)
		p.noteDisproved(u)
		p.probeUpstream(ctx, u, head, hdr)
		assert.Zero(t, covIntegStreak(p, "u1"))
	})

	t.Run("a writer that is not an EVM upstream still gets its proven head", func(t *testing.T) {
		u := &covIntegWriterUp{Upstream: common.NewFakeUpstream("u1")}
		net := mkNet("cov-integ-sp-writer", &script{callRes: atHead}, u)
		p := mkProber(net, 0)
		p.probeUpstream(ctx, u, head, hdr)
		assert.Equal(t, head, u.proven.Load())
	})

	t.Run("latest probe without returndata is no evidence", func(t *testing.T) {
		u := common.NewFakeUpstream("u1", common.WithEvmStatePoller(common.NewFakeEvmStatePoller(head, head-10)))
		fleet := mkNet("cov-integ-sp-latest", &script{callRes: `"0x"`}, u)
		fleet.cfg = &common.NetworkConfig{Methods: &common.MethodsConfig{Definitions: map[string]*common.CacheMethodConfig{
			"eth_call": {TranslateLatestTag: util.BoolPtr(false)},
		}}}
		net := &covIntegFleetHeadNet{covIntegFleetNet: fleet, latest: head}
		p := mkProber(net, 0)
		p.probeLatest(ctx, u)
		assert.Equal(t, 2, fleet.callCount(), "ArbSys answers no code, so Multicall3 is decided and asked")
		assert.True(t, fleet.lastDirs().SkipInterpolation, "the probe sends latest as the tag")
		assert.Equal(t, tagNoEvidence, p.tagHead("u1", head, time.Now()).state)
		assert.Zero(t, fakeTrackerOf(t, u).MisbehaviorCount)
	})

	t.Run("latest probe with the Head unknown sends nothing", func(t *testing.T) {
		u := common.NewFakeUpstream("u1")
		fleet := mkNet("cov-integ-sp-latest-cold", &script{callRes: atHead}, u)
		p := mkProber(&covIntegFleetHeadNet{covIntegFleetNet: fleet}, 0)
		p.probeLatest(ctx, u)
		assert.Zero(t, fleet.callCount())
	})

	t.Run("probeAll needs an enumerable network", func(t *testing.T) {
		net := newCovIntegNet("cov-integ-sp-noenum", nil, func(req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return covIntegResp(atHead), nil
		})
		p := mkProber(net, 0)
		p.probeAll(ctx, head)
		assert.Zero(t, net.callCount())
	})

	t.Run("an upstream probed within the interval is skipped", func(t *testing.T) {
		u := common.NewFakeUpstream("u1", common.WithEvmStatePoller(common.NewFakeEvmStatePoller(head, head-10))).(*common.FakeUpstream)
		u.Config().IgnoreMethods = []string{"eth_getProof"}
		net := mkNet("cov-integ-sp-due", &script{callRes: atHead}, u)
		p := mkProber(net, time.Hour)
		p.probeAll(ctx, head)
		first := net.callCount()
		require.Positive(t, first)
		p.probeAll(ctx, head)
		assert.Equal(t, first, net.callCount())
		assert.Equal(t, head, u.EvmStateProvenBlock())
	})

	t.Run("run probes the newest handed head; the newest replaces a queued one", func(t *testing.T) {
		u := common.NewFakeUpstream("u1", common.WithEvmStatePoller(common.NewFakeEvmStatePoller(head, head-10))).(*common.FakeUpstream)
		u.Config().IgnoreMethods = []string{"eth_getProof"}
		net := mkNet("cov-integ-sp-run", &script{callRes: atHead}, u)
		p := mkProber(net, 0)
		p.onNewHead(5)
		p.onNewHead(head)
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { p.run(runCtx); close(done) }()
		require.Eventually(t, func() bool { return u.EvmStateProvenBlock() == head }, 5*time.Second, 5*time.Millisecond, "only the newest head is probed")
		cancel()
		<-done
		assert.Equal(t, 1, net.callCount())
	})

	t.Run("a lagging upstream gets no probe and no head Ask until its head counter reaches the pin", func(t *testing.T) {
		node := []byte("state-trie-root-node")
		root := fmt.Sprintf("0x%x", gethcrypto.Keccak256(node))
		lead := common.NewFakeUpstream("lead", common.WithEvmStatePoller(common.NewFakeEvmStatePoller(head, head-10))).(*common.FakeUpstream)
		lagPoller := common.NewFakeEvmStatePoller(head-16, head-26)
		lag := common.NewFakeUpstream("lag", common.WithEvmStatePoller(lagPoller)).(*common.FakeUpstream)
		net := &covIntegRefusingNet{asks: map[string]int{}}
		net.covIntegFleetNet = &covIntegFleetNet{ups: []common.Upstream{lead, lag}, covIntegNet: newCovIntegNet("cov-integ-sp-lag", nil, func(req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			jrq, _ := req.JsonRpcRequest()
			at := jrq.Params[1]
			if jrq.Method == "eth_getProof" {
				at = jrq.Params[2]
			}
			n, _ := parseHexQuantity(at.(string))
			if n > net.upstreamHead(req.Directives().UseUpstream) {
				return nil, errors.New("header not found") // a node holds no state above its head
			}
			if jrq.Method == "eth_getProof" {
				return covIntegResp(fmt.Sprintf(`{"accountProof":["0x%x"]}`, node)), nil
			}
			return covIntegResp(fmt.Sprintf(`"0x%064x"`, n)), nil
		})}
		v := newChainTracker(t.Context(), nil, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		v.Observe("a", json.RawMessage(fmt.Sprintf(`{"number":"0x%x","hash":"%s","parentHash":"%s","stateRoot":"%s"}`,
			head, testHash("e", uint64(head)), testHash("f", uint64(head-1)), root)), true)
		v.sync(t.Context())
		p := &stateProber{
			network: net, view: v, pinnedProbes: true,
			lastProbe: map[string]time.Time{}, work: make(chan int64, 1),
		}

		proofErrs := func() int64 {
			v, ok := p.proofErrors.Load("lag")
			if !ok {
				return 0
			}
			return v.(*atomic.Int64).Load()
		}
		for range stateProbeProofErrors {
			p.probeAll(ctx, head)
		}
		assert.Equal(t, 2*stateProbeProofErrors, net.probesTo("lead"), "an upstream at the pin gets the context and proof probes")
		assert.Equal(t, head, lead.EvmStateProvenBlock())
		assert.Zero(t, net.probesTo("lag"), "an upstream below the pin gets no probe")
		assert.Empty(t, net.asks, "no probe Asks a head")
		assert.Zero(t, proofErrs(), "a lagging upstream's missing state is no proof error")
		assert.False(t, p.proofsOff("lag"), "the proof switch never counts a lagging upstream")
		assert.Zero(t, covIntegStreak(p, "lag"))

		lagPoller.SuggestLatestBlock(head, time.Now())
		p.probeAll(ctx, head)
		assert.Equal(t, 2, net.probesTo("lag"), "once its head counter reached the pin it is probed")
		assert.Equal(t, head, lag.EvmStateProvenBlock())
		assert.Empty(t, net.asks, "no probe Asks a head")
		assert.Zero(t, proofErrs())
	})

	t.Run("nil network has empty project label", func(t *testing.T) {
		assert.Empty(t, (&stateProber{}).projectId())
	})
}

func TestCovInteg_StartStateProber(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cfg := &common.NetworkConfig{Evm: &common.EvmNetworkConfig{ChainId: 42161}}
	net := newCovIntegNet("cov-integ-start", cfg, nil)
	assert.Nil(t, stateProberFor(net))
	v1 := newChainTracker(ctx, nil, 8, 0, "", "", "", nil, newFetchScheduler(ctx, nil))
	v2 := newChainTracker(ctx, nil, 8, 0, "", "", "", nil, newFetchScheduler(ctx, nil))
	startStateProber(ctx, net, v1, &common.IntegrityStateProbeConfig{Interval: common.Duration(3 * time.Second)})
	p := stateProberFor(net)
	require.NotNil(t, p)
	assert.Equal(t, 3*time.Second, p.interval)
	assert.True(t, p.pinnedProbes)
	assert.Nil(t, p.stateCtx.probe(), "the chain id decides no probe target: the first probe's ArbSys answer does")

	startStateProber(ctx, net, v2, nil)
	assert.Same(t, p, stateProberFor(net), "one prober per network")
	assert.Same(t, v1, p.view)

	net2 := newCovIntegNet("cov-integ-start-default", nil, nil)
	startStateProber(ctx, net2, v1, nil)
	p2 := stateProberFor(net2)
	require.NotNil(t, p2)
	assert.Equal(t, stateProbeMinInterval, p2.interval)
	assert.False(t, p2.pinnedProbes, "without follow and stateProbe only the latest probe runs")

	twin := newCovIntegNet("cov-integ-start", cfg, nil)
	startStateProber(ctx, twin, v2, nil)
	assert.NotSame(t, p, stateProberFor(twin), "another project's network of the same id has its own prober")
}

func TestCovInteg_IsMethodUnsupportedErr(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{common.NewErrEndpointUnsupported(errors.New("x")), true},
		{common.NewErrUpstreamMethodIgnored("eth_getProof", "u1"), true},
		{errors.New("the method eth_getProof does not exist/is not available"), true},
		{errors.New("Method not found"), true},
		{errors.New("eth_getProof not supported"), true},
		{errors.New("execution reverted"), false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, isMethodUnsupportedErr(tc.err), "%v", tc.err)
	}
}

func TestCovInteg_Resolver(t *testing.T) {
	ctx := context.Background()
	assert.Nil(t, newIntegrityResolver(nil, nil, nil, false))

	t.Run("finality needs an EVM upstream", func(t *testing.T) {
		r := &integrityResolver{upstream: covIntegPlainUp{common.NewFakeUpstream("u")}}
		fin, known := r.IsFinalized(ctx, 5)
		assert.False(t, fin)
		assert.False(t, known)
	})

	t.Run("no view means canonical unavailable", func(t *testing.T) {
		r := &integrityResolver{}
		_, ok := r.CanonicalReceipts(ctx, "0xbb", -1)
		assert.False(t, ok)
		_, ok = r.CanonicalHeader(ctx, "0x10")
		assert.False(t, ok)
	})

	// Only votes move pins (spec §16): a fetched header is held, never pinned.
	t.Run("refs route by hash or number", func(t *testing.T) {
		hash := testHash("abc", 0x20)
		n10 := testHash("a", 0x10)
		net := &covIntegSweepNet{newCovIntegNet("cov-integ-resolver", nil, func(req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			jrq, _ := req.JsonRpcRequest()
			ref := jrq.Params[0].(string)
			switch {
			case jrq.Method == "eth_getBlockByHash" && ref == hash:
				return covIntegResp(string(testHeader(0x20, hash, testHash("abc", 0x1f)))), nil
			case jrq.Method == "eth_getBlockByNumber" && ref == "0x10":
				return covIntegResp(string(testHeader(0x10, n10, testHash("a", 0xf)))), nil
			}
			return covIntegResp("null"), nil
		})}
		view := newChainTracker(t.Context(), net, 64, 0, "", "", "", nil, newFetchScheduler(t.Context(), net))
		r := &integrityResolver{network: net, view: view}

		h, ok := r.CanonicalHeader(ctx, hash)
		require.True(t, ok)
		assert.Equal(t, hash, h.Hash)
		_, pinned := view.HashAt(0x20)
		assert.False(t, pinned, "a by-hash lookup never pins")

		h, ok = r.CanonicalHeader(ctx, "0x10")
		require.True(t, ok)
		assert.Equal(t, n10, h.Hash)
		_, pinned = view.HashAt(0x10)
		assert.False(t, pinned, "a by-number lookup never pins")
	})
}

func TestCovInteg_ConfigOverridesAndOverlay(t *testing.T) {
	t.Run("override without an enable flag only touches checks the level enabled", func(t *testing.T) {
		cs, _ := compileIntegritySettings(&common.IntegritySettings{
			Level: "intrinsic",
			Checks: map[string]*common.IntegrityCheckConfig{
				"receiptVsBlock": {Params: map[string]string{"k": "v"}},
				"bloomMatch":     {Params: map[string]string{"mode": "superset"}},
				"indexMagnitude": nil,
			},
		}, 0)
		assert.False(t, cs.For("receiptVsBlock").Enabled, "params alone must not enable a check above the level")
		require.True(t, cs.For("bloomMatch").Enabled)
		assert.Equal(t, "superset", cs.For("bloomMatch").Params["mode"])
		assert.True(t, cs.For("indexMagnitude").Enabled, "a nil override is ignored")
	})

	t.Run("a selected profile overlays every set field as an independent copy", func(t *testing.T) {
		fls := false
		profile := &common.IntegritySettings{
			Budget:                  &common.IntegrityBudgetConfig{MaxPerSecond: 5},
			InvalidBehavior:         &common.IntegrityInvalidBehaviorConfig{Finalized: "hardReject"},
			AutoCorrectWhenPossible: &fls,
			Checks:                  map[string]*common.IntegrityCheckConfig{"bloomMatch": {OnFailure: "recordOnly"}},
		}
		cfg := &common.IntegrityConfig{
			IntegritySettings: common.IntegritySettings{Level: "intrinsic"},
			HeaderMode:        "profiles",
			Profiles:          map[string]*common.IntegritySettings{"p": profile},
		}
		got := resolveRequestSettings(cfg, "p")
		require.NotNil(t, got)
		assert.Equal(t, "intrinsic", got.Level, "an unset level keeps the base")
		require.NotNil(t, got.Budget)
		assert.Equal(t, 5, got.Budget.MaxPerSecond)
		require.NotNil(t, got.InvalidBehavior)
		assert.Equal(t, "hardReject", got.InvalidBehavior.Finalized)
		require.NotNil(t, got.AutoCorrectWhenPossible)
		assert.False(t, *got.AutoCorrectWhenPossible)
		require.Contains(t, got.Checks, "bloomMatch")
		assert.Equal(t, "recordOnly", got.Checks["bloomMatch"].OnFailure)

		got.Budget.MaxPerSecond = 99
		got.Checks["bloomMatch"].OnFailure = "hardReject"
		assert.Equal(t, 5, profile.Budget.MaxPerSecond, "the profile must not be mutated through the result")
		assert.Equal(t, "recordOnly", profile.Checks["bloomMatch"].OnFailure)

		_, policy, _, autoCorrect := func() (integrity.CheckSet, integrity.ReorgPolicy, bool, bool) {
			cs, p := compileIntegritySettings(resolveRequestSettings(cfg, "p"), 0)
			return cs, p, false, *resolveRequestSettings(cfg, "p").AutoCorrectWhenPossible
		}()
		assert.Equal(t, integrity.BehaviorError, policy.Finalized)
		assert.False(t, autoCorrect)
	})
}
