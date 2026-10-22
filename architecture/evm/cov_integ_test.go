package evm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
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

func (n *covIntegNet) Id() string                     { return n.id }
func (n *covIntegNet) Label() string                  { return "covnet" }
func (n *covIntegNet) ProjectId() string              { return "test" }
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

func (n *covIntegHeadNet) EvmHighestLatestBlockNumber(ctx context.Context) int64    { return n.latest }
func (n *covIntegHeadNet) EvmHighestFinalizedBlockNumber(ctx context.Context) int64 { return n.finalized }

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

func covIntegHeaderJSON(num int64, hash, parent string) string {
	return fmt.Sprintf(`{"number":"0x%x","hash":"%s","parentHash":"%s"}`, num, hash, parent)
}

// covIntegChainForward serves a fakeChain's blocks by number or by hash.
func covIntegChainForward(chain *fakeChain) func(req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return func(req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		jrq, err := req.JsonRpcRequest()
		if err != nil {
			return nil, err
		}
		ref, _ := jrq.Params[0].(string)
		chain.mu.Lock()
		chain.fetches++
		var b fakeBlock
		var found bool
		if jrq.Method == "eth_getBlockByHash" {
			b, found = chain.byHash[ref]
		} else {
			var num int64
			_, _ = fmt.Sscanf(ref, "0x%x", &num)
			b, found = chain.byNumber[num]
		}
		chain.mu.Unlock()
		if !found {
			return covIntegResp("null"), nil
		}
		return covIntegResp(covIntegHeaderJSON(b.num, b.hash, b.parent)), nil
	}
}

func covIntegFetches(chain *fakeChain) int {
	chain.mu.Lock()
	defer chain.mu.Unlock()
	return chain.fetches
}

func covIntegStreak(p *stateProber, id string) int64 {
	v, ok := p.disprovedStreak.Load(id)
	if !ok {
		return 0
	}
	return v.(*atomic.Int64).Load()
}

func TestCovInteg_DoOnceJoinsInflightFetch(t *testing.T) {
	var mu sync.Mutex
	inflight := map[string]*flight[int]{}
	f := &flight[int]{val: 7, ok: true}
	f.wg.Add(1)
	inflight["k"] = f

	type res struct {
		v  int
		ok bool
	}
	done := make(chan res, 1)
	called := atomic.Bool{}
	go func() {
		v, ok := doOnce(&mu, inflight, "k", func() (int, bool) {
			called.Store(true)
			return 99, false
		})
		done <- res{v, ok}
	}()
	f.wg.Done()
	got := <-done
	assert.Equal(t, res{7, true}, got, "a joiner must receive the in-flight result")
	assert.False(t, called.Load(), "a joiner must never issue its own fetch")
}

func TestCovInteg_ChainViewStateTransitions(t *testing.T) {
	t.Run("adoptFollowed ignores headers without a hash", func(t *testing.T) {
		c := newChainView(nil, 8, "", "", nil)
		c.adoptFollowed(5, nil)
		c.adoptFollowed(5, &integrity.Header{Number: "0x5"})
		_, _, ok := c.FollowedRange()
		assert.False(t, ok)
		_, ok = c.HashAt(5)
		assert.False(t, ok)
	})

	t.Run("observeHeader and observeReceipts ignore empty keys and values", func(t *testing.T) {
		c := newChainView(nil, 8, "", "", nil)
		c.observeHeader("", &integrity.Header{Hash: "0xa"})
		c.observeHeader("0xa", nil)
		c.observeReceipts("", []integrity.Receipt{{BlockHash: "0xb"}})
		c.observeReceipts("0xb", nil)
		assert.Empty(t, c.headers)
		_, ok := c.CachedReceipts("0xb")
		assert.False(t, ok)
	})

	t.Run("CachedReceipts serves only warm blocks", func(t *testing.T) {
		c := newChainView(nil, 8, "", "", nil)
		c.observeReceipts("0xbb", []integrity.Receipt{{TransactionHash: "0x1", BlockHash: "0xbb"}})
		got, ok := c.CachedReceipts("0xbb")
		require.True(t, ok)
		assert.Equal(t, "0x1", got[0].TransactionHash)
		_, ok = c.CachedReceipts("0xcold")
		assert.False(t, ok)
	})

	t.Run("eviction drops reconfirmation stamps below the window", func(t *testing.T) {
		c := newChainView(nil, 2, "", "", nil)
		c.observe(1, "0x1", nil)
		c.mu.Lock()
		c.reconfirmedAt[1] = time.Now()
		c.mu.Unlock()
		c.observe(10, "0xa", nil)
		c.mu.RLock()
		_, kept := c.reconfirmedAt[1]
		c.mu.RUnlock()
		assert.False(t, kept)
	})

	t.Run("a followed segment fully below the window is forgotten", func(t *testing.T) {
		c := newChainView(nil, 2, "", "", nil)
		c.adoptFollowed(5, &integrity.Header{Number: "0x5", Hash: "0x5"})
		_, _, ok := c.FollowedRange()
		require.True(t, ok)
		c.observe(10, "0xa", nil) // lo=8 > followHead=5
		_, _, ok = c.FollowedRange()
		assert.False(t, ok, "the range must not claim heights that were evicted")
	})

	t.Run("finality unknown when the getter reports no finalized height", func(t *testing.T) {
		c := newChainView(nil, 8, "", "", func() int64 { return 0 })
		assert.Equal(t, "unknown", c.finalityLabel(5))
	})
}

func TestCovInteg_Reconcile(t *testing.T) {
	ctx := context.Background()

	t.Run("malformed branch heads are refused", func(t *testing.T) {
		c := newChainView(nil, 8, "", "", nil)
		for _, h := range []*integrity.Header{
			nil,
			{Number: "0x5"},
			{Hash: "0xa"},
			{Hash: "0xa", Number: "0xzz"},
			{Hash: "0xa", Number: "0x0", ParentHash: "0xp"}, // parent below genesis
		} {
			depth, ok := c.reconcile(ctx, h)
			assert.False(t, ok)
			assert.Zero(t, depth)
		}
	})

	t.Run("sparse view with no held parent anchors the branch", func(t *testing.T) {
		c := newChainView(nil, 8, "", "", nil)
		depth, ok := c.reconcile(ctx, &integrity.Header{Number: "0xa", Hash: "0xnew10", ParentHash: "0xnew9"})
		require.True(t, ok)
		assert.Equal(t, 1, depth)
		assert.Equal(t, "0xnew10", mustHashAt(t, c, 10))
		from, to, ok := c.FollowedRange()
		require.True(t, ok)
		assert.Equal(t, [2]int64{10, 10}, [2]int64{from, to})
	})

	t.Run("diverging parent that cannot be fetched fails", func(t *testing.T) {
		c := newChainView(nil, 8, "", "", nil)
		c.observe(9, "0xheld9", nil)
		_, ok := c.reconcile(ctx, &integrity.Header{Number: "0xa", Hash: "0xnew10", ParentHash: "0xnew9"})
		assert.False(t, ok)
		assert.Equal(t, "0xheld9", mustHashAt(t, c, 9))
	})

	t.Run("parent claiming the wrong height fails", func(t *testing.T) {
		net := newCovIntegNet("cov-integ-rec", nil, func(req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return covIntegResp(covIntegHeaderJSON(5, "0xnew9", "0xnew8")), nil
		})
		c := newChainView(net, 8, "", "", nil)
		c.observe(9, "0xheld9", nil)
		_, ok := c.reconcile(ctx, &integrity.Header{Number: "0xa", Hash: "0xnew10", ParentHash: "0xnew9"})
		assert.False(t, ok)
		assert.Equal(t, "0xheld9", mustHashAt(t, c, 9))
	})

	t.Run("no common ancestor within the window fails", func(t *testing.T) {
		net := newCovIntegNet("cov-integ-rec2", nil, func(req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return covIntegResp(covIntegHeaderJSON(9, "0xnew9", "0xnew8")), nil
		})
		c := newChainView(net, 1, "", "", nil)
		c.observe(9, "0xheld9", nil)
		_, ok := c.reconcile(ctx, &integrity.Header{Number: "0xa", Hash: "0xnew10", ParentHash: "0xnew9"})
		assert.False(t, ok)
		assert.Equal(t, "0xheld9", mustHashAt(t, c, 9))
		assert.Equal(t, 1, net.callCount())
		assert.Equal(t, "true", net.lastDirs().SkipCacheRead, "the ancestry walk must bypass the cache")
	})
}

func TestCovInteg_ResolveAnchors(t *testing.T) {
	ctx := context.Background()

	t.Run("a by-number answer for another height is discarded", func(t *testing.T) {
		net := newCovIntegNet("cov-integ-anchor", nil, func(req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return covIntegResp(covIntegHeaderJSON(0x11, "0xwrong", "0xp")), nil
		})
		c := newChainView(net, 8, "", "", nil)
		h, ok := c.resolveHeader(ctx, "eth_getBlockByNumber", "0x10", false)
		assert.False(t, ok)
		assert.Nil(t, h)
		_, pinned := c.HashAt(0x11)
		assert.False(t, pinned)
		_, pinned = c.HashAt(0x10)
		assert.False(t, pinned)
	})

	receiptsCases := []struct {
		name string
		body string
	}{
		{"another block's receipts", `[{"blockHash":"0xother","blockNumber":"0x10","transactionHash":"0x1"}]`},
		{"an empty receipt set", `[]`},
	}
	for _, tc := range receiptsCases {
		t.Run(tc.name+" is canonical-unavailable and not cached", func(t *testing.T) {
			net := newCovIntegNet("cov-integ-rcpt", nil, func(req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
				return covIntegResp(tc.body), nil
			})
			c := newChainView(net, 8, "", "", nil)
			got, ok := c.receiptsByHash(ctx, "0xwant")
			assert.False(t, ok)
			assert.Nil(t, got)
			_, cached := c.CachedReceipts("0xwant")
			assert.False(t, cached)
		})
	}
}

func TestCovInteg_HasVerifiedEmptyTraceBlock(t *testing.T) {
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
	store := func(group string, h *integrity.Header, n int64) {
		v := newChainView(net, 32, "", "", nil)
		v.adoptFollowed(n, h)
		key := chainViewKey{network: net, group: group}
		chainViewStore.Store(key, v)
		t.Cleanup(func() { chainViewStore.Delete(key) })
	}
	empty := func(num, hash, gas string) *integrity.Header {
		return &integrity.Header{Number: num, Hash: hash, GasUsed: gas, RawTransactions: []any{}}
	}
	store("", empty("0x10", "0xh10", "0x0"), 0x10)

	assert.False(t, HasVerifiedEmptyTraceBlock(nil))
	assert.False(t, HasVerifiedEmptyTraceBlock(mkReq(traceBody("0x10"), false, nil)), "no network")
	assert.False(t, HasVerifiedEmptyTraceBlock(mkReq(`not-json`, true, nil)), "unparseable request")
	assert.False(t, HasVerifiedEmptyTraceBlock(mkReq(traceBody("16"), true, nil)), "decimal ref is not a hex block")
	assert.False(t, HasVerifiedEmptyTraceBlock(mkReq(traceBody("0xzz"), true, nil)), "malformed hex")
	assert.False(t, HasVerifiedEmptyTraceBlock(mkReq(traceBody("0x11"), true, nil)), "outside the followed range")
	assert.True(t, HasVerifiedEmptyTraceBlock(mkReq(traceBody("0x10"), true, nil)))

	t.Run("group-scoped request reads only its group's view", func(t *testing.T) {
		dirs := &common.RequestDirectives{UseUpstream: "grp"}
		assert.False(t, HasVerifiedEmptyTraceBlock(mkReq(traceBody("0x10"), true, dirs)), "group view absent")
		store("g:grp", empty("0x20", "0xh20", "0x0"), 0x20)
		assert.True(t, HasVerifiedEmptyTraceBlock(mkReq(traceBody("0x20"), true, dirs)))
	})

	t.Run("header disagreeing with the pinned height or carrying gas is not proof", func(t *testing.T) {
		net2 := &covIntegGroupNet{newCovIntegNet("cov-integ-trace2", nil, nil)}
		for _, tc := range []struct {
			name string
			h    *integrity.Header
		}{
			{"wrong number", empty("0x31", "0xh30", "0x0")},
			{"non-zero gas", empty("0x30", "0xh30", "0x5208")},
		} {
			v := newChainView(net2, 32, "", "", nil)
			v.adoptFollowed(0x30, tc.h)
			key := chainViewKey{network: net2}
			chainViewStore.Store(key, v)
			r := common.NewNormalizedRequest([]byte(traceBody("0x30")))
			r.SetNetwork(net2)
			assert.False(t, HasVerifiedEmptyTraceBlock(r), tc.name)
			chainViewStore.Delete(key)
		}
	})
}

func TestCovInteg_GroupChainView(t *testing.T) {
	ctx := context.Background()
	assert.Nil(t, groupChainView(ctx, nil, ""))

	t.Run("selector carving a group gets its own pinned view", func(t *testing.T) {
		net := &covIntegGroupNet{newCovIntegNet("cov-integ-gcv-grp", nil, nil)}
		t.Cleanup(func() {
			chainViewStore.Delete(chainViewKey{network: net})
			chainViewStore.Delete(chainViewKey{network: net, group: "g:grp"})
		})
		g := groupChainView(ctx, net, "grp")
		wide := groupChainView(ctx, net, "")
		require.NotSame(t, g, wide)
		assert.Equal(t, "grp", g.selector)
		assert.Equal(t, "lane-grp", g.group)
		assert.Empty(t, wide.selector)
		assert.Same(t, wide, groupChainView(ctx, net, "not-a-group"), "a non-group selector falls back to the network view")
		assert.Same(t, g, groupChainView(ctx, net, "grp"), "views are created once")
		assert.Equal(t, defaultReorgWindow, g.window)
		assert.Nil(t, g.finalized)
	})

	t.Run("integrity config sizes the window and starts follower and prober", func(t *testing.T) {
		cfg := &common.NetworkConfig{
			Evm: &common.EvmNetworkConfig{ChainId: 1},
			Integrity: &common.IntegrityConfig{IntegritySettings: common.IntegritySettings{
				ReorgWindow: 7,
				Follow:      &common.IntegrityFollowConfig{Enabled: util.BoolPtr(true), Interval: common.Duration(time.Hour)},
				StateProbe:  &common.IntegrityStateProbeConfig{Enabled: util.BoolPtr(true), Interval: common.Duration(5 * time.Second)},
			}},
		}
		net := &covIntegHeadNet{covIntegNet: newCovIntegNet("cov-integ-gcv-follow", cfg, nil), finalized: 42}
		v := groupChainView(ctx, net, "")
		p := stateProberFor(net.Id())
		t.Cleanup(func() {
			if v.follower != nil {
				close(v.follower.stop)
			}
			chainViewStore.Delete(chainViewKey{network: net})
			if p != nil {
				stateProbers.Delete(net.Id())
				close(p.work)
			}
		})
		assert.Equal(t, 7, v.window)
		require.NotNil(t, v.follower)
		assert.Equal(t, time.Hour, v.follower.interval)
		require.NotNil(t, p)
		assert.Equal(t, 5*time.Second, p.interval)
		assert.Same(t, v, p.view)
		assert.Equal(t, "finalized", v.finalityLabel(42), "finality comes from the network")
		assert.Equal(t, "unfinalized", v.finalityLabel(43))
	})

	t.Run("network without a head getter yields no latest func", func(t *testing.T) {
		assert.Nil(t, networkLatest(newCovIntegNet("x", nil, nil)))
		hn := &covIntegHeadNet{covIntegNet: newCovIntegNet("y", nil, nil), latest: 9}
		fn := networkLatest(hn)
		require.NotNil(t, fn)
		assert.Equal(t, int64(9), fn(ctx))
	})
}

func TestCovInteg_ObserveViews(t *testing.T) {
	ctx := context.Background()

	t.Run("observeBlockView ignores nil inputs and non-header results", func(t *testing.T) {
		c := newChainView(nil, 8, "", "", nil)
		observeBlockView(ctx, nil, covIntegResp(covIntegHeaderJSON(1, "0x1", "0x0")), "eth_getblockbynumber")
		observeBlockView(ctx, c, nil, "eth_getblockbynumber")
		observeBlockView(ctx, c, covIntegResp(`"0x1"`), "eth_getblockbynumber")
		observeBlockView(ctx, c, covIntegResp(`{"number":"0x1"}`), "eth_getblockbynumber")
		assert.Empty(t, c.canonical)
		assert.Empty(t, c.headers)
	})

	t.Run("observeNarrowView pins only from EVM upstreams with known finality", func(t *testing.T) {
		anchor := covIntegResp(`{"blockNumber":"0x10","blockHash":"0xbb"}`)
		evmUp := common.NewFakeUpstream("u", common.WithEvmStatePoller(common.NewFakeEvmStatePoller(0x30, 0x20)))

		c := newChainView(nil, 32, "", "", nil)
		observeNarrowView(ctx, nil, evmUp, anchor)
		observeNarrowView(ctx, c, evmUp, nil)
		observeNarrowView(ctx, c, covIntegPlainUp{evmUp}, anchor)
		_, ok := c.HashAt(0x10)
		assert.False(t, ok, "a non-EVM upstream has no finality to gate the pin")

		observeNarrowView(ctx, c, evmUp, covIntegResp(`{"blockNumber":"0x11","blockHash":""}`))
		_, ok = c.HashAt(0x11)
		assert.False(t, ok, "an anchor without a hash pins nothing")

		observeNarrowView(ctx, c, evmUp, anchor)
		assert.Equal(t, "0xbb", mustHashAt(t, c, 0x10))
	})
}

func TestCovInteg_ChainFollower(t *testing.T) {
	ctx := context.Background()
	newFollower := func(t *testing.T, chain *fakeChain, head *int64, cfg *common.IntegrityFollowConfig) (*chainView, *chainFollower, *covIntegNet) {
		t.Helper()
		net := newCovIntegNet("cov-integ-follow-"+t.Name(), nil, covIntegChainForward(chain))
		v := newChainView(net, 32, "", "", nil)
		f := newChainFollower(v, func(context.Context) int64 { return *head }, cfg)
		return v, f, net
	}

	t.Run("config overrides interval and per-tick bound", func(t *testing.T) {
		chain := newFakeChain()
		chain.build(100, 110, "a", "0xgenesis")
		head := int64(100)
		v, f, _ := newFollower(t, chain, &head, &common.IntegrityFollowConfig{MaxBlocksPerTick: 2, Interval: common.Duration(time.Minute)})
		assert.Equal(t, time.Minute, f.interval)
		f.advance(ctx)
		head = 110
		f.advance(ctx)
		assert.Equal(t, int64(102), mustFollowHead(t, v), "catch-up is bounded per tick")
	})

	t.Run("no head means no work", func(t *testing.T) {
		chain := newFakeChain()
		head := int64(0)
		v, f, _ := newFollower(t, chain, &head, nil)
		f.advance(ctx)
		assert.Zero(t, covIntegFetches(chain))
		_, _, ok := v.FollowedRange()
		assert.False(t, ok)
	})

	t.Run("bootstrap that cannot fetch the head does not start following", func(t *testing.T) {
		chain := newFakeChain()
		head := int64(100)
		v, f, _ := newFollower(t, chain, &head, nil)
		f.advance(ctx)
		assert.Equal(t, 1, covIntegFetches(chain))
		_, _, ok := v.FollowedRange()
		assert.False(t, ok)
	})

	t.Run("a missing block stops the walk where it is", func(t *testing.T) {
		chain := newFakeChain()
		chain.build(100, 102, "a", "0xgenesis")
		head := int64(100)
		v, f, _ := newFollower(t, chain, &head, nil)
		f.advance(ctx)
		head = 105
		f.advance(ctx)
		assert.Equal(t, int64(102), mustFollowHead(t, v))
	})

	t.Run("a cancelled tick advances nothing", func(t *testing.T) {
		chain := newFakeChain()
		chain.build(100, 105, "a", "0xgenesis")
		head := int64(100)
		v, f, _ := newFollower(t, chain, &head, nil)
		f.advance(ctx)
		head = 105
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		f.advance(cctx)
		assert.Equal(t, int64(100), mustFollowHead(t, v))
	})

	t.Run("an evicted parent re-anchors on the next block", func(t *testing.T) {
		chain := newFakeChain()
		chain.build(100, 101, "a", "0xgenesis")
		head := int64(100)
		v, f, _ := newFollower(t, chain, &head, nil)
		f.advance(ctx)
		v.mu.Lock()
		delete(v.canonical, 100)
		v.mu.Unlock()
		head = 101
		f.advance(ctx)
		assert.Equal(t, int64(101), mustFollowHead(t, v))
		assert.Equal(t, chain.hashAt(101), mustHashAt(t, v, 101))
	})

	t.Run("a new followed head is handed to the network's prober", func(t *testing.T) {
		chain := newFakeChain()
		chain.build(100, 103, "a", "0xgenesis")
		head := int64(100)
		v, f, net := newFollower(t, chain, &head, nil)
		p := &stateProber{work: make(chan int64, 1)}
		stateProbers.Store(net.Id(), p)
		t.Cleanup(func() { stateProbers.Delete(net.Id()) })
		f.advance(ctx)
		head = 103
		f.advance(ctx)
		require.Equal(t, int64(103), mustFollowHead(t, v))
		select {
		case got := <-p.work:
			assert.Equal(t, int64(103), got)
		default:
			t.Fatal("the prober was not handed the new head")
		}
	})

	t.Run("settled heights are read from cache, the tip fresh", func(t *testing.T) {
		chain := newFakeChain()
		chain.build(100, 110, "a", "0xgenesis")
		net := newCovIntegNet("cov-integ-follow-fresh", nil, covIntegChainForward(chain))
		v := newChainView(net, 32, "", "", func() int64 { return 105 })
		_, ok := v.fetchFollowHeader(ctx, 104)
		require.True(t, ok)
		assert.Empty(t, net.lastDirs().SkipCacheRead)
		_, ok = v.fetchFollowHeader(ctx, 106)
		require.True(t, ok)
		assert.Equal(t, "true", net.lastDirs().SkipCacheRead)
	})

	t.Run("run returns once stopped without ticking", func(t *testing.T) {
		calls := atomic.Int32{}
		v := newChainView(nil, 8, "", "", nil)
		f := newChainFollower(v, func(context.Context) int64 { calls.Add(1); return 1 }, nil)
		close(f.stop)
		f.run()
		assert.Zero(t, calls.Load())
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
	mkProber := func(net common.Network, interval time.Duration) *stateProber {
		v := newChainView(net, 32, "", "", nil)
		v.adoptFollowed(head, hdr)
		return &stateProber{
			network: net, view: v, interval: interval,
			ctxProbe:  integrity.ChainStateContextProbe(1),
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
					p.proofUnsupported.Store("u1", true)
				}
				h := &integrity.Header{StateRoot: tc.root}
				assert.Equal(t, tc.want, p.probeProof(ctx, u, head, h))
				_, latched := p.proofUnsupported.Load("u1")
				assert.Equal(t, tc.latch, latched)
				assert.Equal(t, tc.forward, net.callCount() > 0)
			})
		}
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
		net := mkNet("cov-integ-sp-latest", &script{callRes: `"0x"`}, u)
		net.cfg = &common.NetworkConfig{Methods: &common.MethodsConfig{Definitions: map[string]*common.CacheMethodConfig{
			"eth_call": {TranslateLatestTag: util.BoolPtr(false)},
		}}}
		p := mkProber(net, 0)
		assert.Equal(t, probeUnknown, p.probeLatest(ctx, u))
		assert.Equal(t, 1, net.callCount())
		assert.Zero(t, fakeTrackerOf(t, u).MisbehaviorCount)
	})

	t.Run("probeAll needs an enumerable network", func(t *testing.T) {
		net := newCovIntegNet("cov-integ-sp-noenum", nil, func(req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return covIntegResp(atHead), nil
		})
		p := mkProber(net, 0)
		p.probeAll(head)
		assert.Zero(t, net.callCount())
	})

	t.Run("an upstream probed within the interval is skipped", func(t *testing.T) {
		u := common.NewFakeUpstream("u1").(*common.FakeUpstream)
		u.Config().IgnoreMethods = []string{"eth_getProof"}
		net := mkNet("cov-integ-sp-due", &script{callRes: atHead}, u)
		p := mkProber(net, time.Hour)
		p.probeAll(head)
		first := net.callCount()
		require.Positive(t, first)
		p.probeAll(head)
		assert.Equal(t, first, net.callCount())
		assert.Equal(t, head, u.EvmStateProvenBlock())
	})

	t.Run("run drains handed heads; the newest replaces a queued one", func(t *testing.T) {
		u := common.NewFakeUpstream("u1").(*common.FakeUpstream)
		u.Config().IgnoreMethods = []string{"eth_getProof"}
		net := mkNet("cov-integ-sp-run", &script{callRes: atHead}, u)
		p := mkProber(net, 0)
		p.onNewHead(5)
		p.onNewHead(head)
		close(p.work)
		p.run()
		assert.Equal(t, head, u.EvmStateProvenBlock(), "only the newest head is probed")
		assert.Equal(t, 1, net.callCount())
	})

	t.Run("nil network has empty project label", func(t *testing.T) {
		assert.Empty(t, (&stateProber{}).projectId())
	})
}

func TestCovInteg_StartStateProber(t *testing.T) {
	assert.Nil(t, stateProberFor("cov-integ-no-such-network"))

	cfg := &common.NetworkConfig{Evm: &common.EvmNetworkConfig{ChainId: 42161}}
	net := newCovIntegNet("cov-integ-start", cfg, nil)
	v1 := newChainView(net, 8, "", "", nil)
	v2 := newChainView(net, 8, "", "", nil)
	startStateProber(net, v1, &common.IntegrityStateProbeConfig{Interval: common.Duration(3 * time.Second)})
	p := stateProberFor(net.Id())
	require.NotNil(t, p)
	t.Cleanup(func() {
		stateProbers.Delete(net.Id())
		close(p.work)
	})
	assert.Equal(t, 3*time.Second, p.interval)
	assert.Equal(t, integrity.ChainStateContextProbe(42161), p.ctxProbe, "the probe target follows the chain architecture")

	startStateProber(net, v2, nil)
	assert.Same(t, p, stateProberFor(net.Id()), "one prober per network")
	assert.Same(t, v1, p.view)

	net2 := newCovIntegNet("cov-integ-start-default", nil, nil)
	startStateProber(net2, v1, nil)
	p2 := stateProberFor(net2.Id())
	require.NotNil(t, p2)
	t.Cleanup(func() {
		stateProbers.Delete(net2.Id())
		close(p2.work)
	})
	assert.Equal(t, stateProbeMinInterval, p2.interval)
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
	assert.Nil(t, newIntegrityResolver(ctx, nil, nil, ""))

	t.Run("finality needs an EVM upstream", func(t *testing.T) {
		r := &integrityResolver{upstream: covIntegPlainUp{common.NewFakeUpstream("u")}}
		fin, known := r.IsFinalized(ctx, 5)
		assert.False(t, fin)
		assert.False(t, known)
	})

	t.Run("no view means canonical unavailable", func(t *testing.T) {
		r := &integrityResolver{}
		_, ok := r.CanonicalReceipts(ctx, "0xbb")
		assert.False(t, ok)
		_, ok = r.CanonicalHeader(ctx, "0x10")
		assert.False(t, ok)
	})

	t.Run("refs route by hash, number, or tag", func(t *testing.T) {
		hash := "0x" + fmt.Sprintf("%064x", 0xabc)
		net := newCovIntegNet("cov-integ-resolver", nil, func(req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			jrq, _ := req.JsonRpcRequest()
			ref := jrq.Params[0].(string)
			switch {
			case jrq.Method == "eth_getBlockByHash" && ref == hash:
				return covIntegResp(covIntegHeaderJSON(0x20, hash, "0xp")), nil
			case jrq.Method == "eth_getBlockByNumber" && ref == "0x10":
				return covIntegResp(covIntegHeaderJSON(0x10, "0xn10", "0xp")), nil
			case jrq.Method == "eth_getBlockByNumber" && ref == "latest":
				return covIntegResp(covIntegHeaderJSON(0x30, "0xlatest", "0xp")), nil
			}
			return covIntegResp("null"), nil
		})
		r := &integrityResolver{network: net, view: newChainView(net, 64, "", "", nil)}

		h, ok := r.CanonicalHeader(ctx, hash)
		require.True(t, ok)
		assert.Equal(t, hash, h.Hash)
		_, pinned := r.view.HashAt(0x20)
		assert.False(t, pinned, "a by-hash lookup never pins")

		h, ok = r.CanonicalHeader(ctx, "0x10")
		require.True(t, ok)
		assert.Equal(t, "0xn10", h.Hash)
		assert.Equal(t, "0xn10", mustHashAt(t, r.view, 0x10))

		h, ok = r.CanonicalHeader(ctx, "latest")
		require.True(t, ok)
		assert.Equal(t, "0xlatest", h.Hash)
		assert.Equal(t, "0xlatest", mustHashAt(t, r.view, 0x30))
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
