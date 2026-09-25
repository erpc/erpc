package erpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/headcache"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

func TestHeadSource_Selector(t *testing.T) {
	var served, live atomic.Int64
	now := time.Unix(1000, 0)
	s := newHeadSourceSelector(common.HeadCacheHeadSourceServed, 3*time.Second,
		func(context.Context) int64 { return served.Load() },
		func(context.Context) int64 { return live.Load() })
	s.now = func() time.Time { return now }
	ctx := context.Background()

	// Cold: served unknown -> live discovery keeps the window moving.
	live.Store(25)
	require.EqualValues(t, 25, s.Head(ctx))
	// Served known and fresh, behind live: served caps publication.
	served.Store(20)
	require.EqualValues(t, 20, s.Head(ctx))
	// Served ahead of live (live lagging): never publish above live.
	served.Store(30)
	live.Store(28)
	require.EqualValues(t, 28, s.Head(ctx))
	// Quiet chain, served == live for a long time: served stays authoritative.
	served.Store(28)
	now = now.Add(time.Hour)
	require.EqualValues(t, 28, s.Head(ctx))
	// Dormant served tip: stuck while live advances beyond maxAge -> live.
	served.Store(29)
	live.Store(40)
	require.EqualValues(t, 29, s.Head(ctx))
	now = now.Add(4 * time.Second)
	require.EqualValues(t, 40, s.Head(ctx))
	// Served advances again: back to capped.
	served.Store(35)
	require.EqualValues(t, 35, s.Head(ctx))
	// Both unknown: 0 (skip tick).
	served.Store(0)
	live.Store(0)
	require.EqualValues(t, 0, s.Head(ctx))

	// max mode ignores served.
	m := newHeadSourceSelector(common.HeadCacheHeadSourceMax, time.Second,
		func(context.Context) int64 { return 5 }, func(context.Context) int64 { return 9 })
	require.EqualValues(t, 9, m.Head(ctx))
}

// scriptedFetcher hydrates straight from a scriptedEvmUpstream.
type scriptedFetcher struct{ u *scriptedEvmUpstream }

func (f scriptedFetcher) rpc(ctx context.Context, method string, params ...interface{}) (json.RawMessage, error) {
	b, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.u.URL(), bytes.NewReader(b))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	return r.Result, nil
}
func (f scriptedFetcher) BlockByNumber(ctx context.Context, n int64) (json.RawMessage, error) {
	return f.rpc(ctx, "eth_getBlockByNumber", fmt.Sprintf("0x%x", n), true)
}
func (f scriptedFetcher) HeaderByNumber(ctx context.Context, n int64) (json.RawMessage, error) {
	return f.rpc(ctx, "eth_getBlockByNumber", fmt.Sprintf("0x%x", n), false)
}
func (f scriptedFetcher) LogsByBlockHash(ctx context.Context, h string) (json.RawMessage, error) {
	return f.rpc(ctx, "eth_getLogs", map[string]interface{}{"blockHash": h})
}

func newSelectorCache(t *testing.T, up *scriptedEvmUpstream, sel *headSourceSelector) *headcache.Cache {
	return headcache.New(headcache.Options{
		Scope: headcache.Scope{Namespace: "t", ProjectId: "p", NetworkId: "evm:123"}, Holder: "h",
		Depth: 16, MaxBytes: 64 << 20, MaxBlockSize: 1 << 20, MaxPerTick: 64, Concurrency: 4, PollInterval: time.Hour, FetchTimeout: 5 * time.Second,
		MaxStaleness: time.Minute, LeaseTTL: 10 * time.Second,
	}, headcache.NewMemoryStore(), scriptedFetcher{up}, sel.Head, nil)
}

func drain(sub *headcache.Subscription) (removed, added []int64) {
	for {
		select {
		case ev := <-sub.C:
			for _, r := range ev.Removed {
				removed = append(removed, r.Number)
			}
			for _, r := range ev.Added {
				added = append(added, r.Number)
			}
		default:
			return
		}
	}
}

// Critical no-false-reorg gate: fallback published 25, then the slower served
// tip (20) becomes known. The window must hold at 25 with zero removals; a
// real hash/parent reorg afterwards is still detected.
func TestHeadSource_Initial25ThenServed20NoFalseReorg(t *testing.T) {
	up := newScriptedEvmUpstream(123, 25)
	defer up.Close()
	var served atomic.Int64 // cold
	sel := newHeadSourceSelector(common.HeadCacheHeadSourceServed, 3*time.Second,
		func(context.Context) int64 { return served.Load() },
		func(ctx context.Context) int64 {
			raw, err := scriptedFetcher{up}.rpc(ctx, "eth_blockNumber")
			if err != nil {
				return 0
			}
			var q string
			_ = json.Unmarshal(raw, &q)
			n, _ := parseExplicitBlockNumber(q)
			return n
		})
	c := newSelectorCache(t, up, sel)
	sub := c.Subscribe(1024)
	defer sub.Close()
	ctx := context.Background()

	c.Tick(ctx) // cold: live discovery
	require.EqualValues(t, 25, c.Head())
	drain(sub)

	served.Store(20) // policy head is slower
	for i := 0; i < 3; i++ {
		c.Tick(ctx)
	}
	require.EqualValues(t, 25, c.Head(), "a lower served tip must not truncate the window")
	removed, _ := drain(sub)
	require.Empty(t, removed, "no removed events solely due to a slower served tip")
	_, ok := c.BlockByNumber(25, false)
	require.True(t, ok)

	// Served catches up and passes: the window extends only to served.
	up.Mine(3) // live 28
	served.Store(27)
	c.Tick(ctx)
	require.EqualValues(t, 27, c.Head())
	removed, added := drain(sub)
	require.Empty(t, removed)
	require.Equal(t, []int64{26, 27}, added)

	// A real reorg at 27 is still detected by hash re-verification.
	up.Reorg(27, "b")
	c.Tick(ctx)
	removed, added = drain(sub)
	require.Equal(t, []int64{27}, removed)
	require.Contains(t, added, int64(27))
}

// Cold and dormant served tips never freeze the window.
func TestHeadSource_ColdAndStaleServedTipStayLive(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	var served, live atomic.Int64
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	sel := newHeadSourceSelector(common.HeadCacheHeadSourceServed, 3*time.Second,
		func(context.Context) int64 { return served.Load() },
		func(context.Context) int64 { return live.Load() })
	sel.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	c := newSelectorCache(t, up, sel)
	ctx := context.Background()
	live.Store(20)
	c.Tick(ctx)
	require.EqualValues(t, 20, c.Head(), "cold served tip -> live")
	served.Store(20)
	up.Mine(5)
	live.Store(25)
	c.Tick(ctx)
	require.EqualValues(t, 20, c.Head(), "fresh served tip caps")
	mu.Lock()
	now = now.Add(4 * time.Second)
	mu.Unlock()
	c.Tick(ctx)
	require.EqualValues(t, 25, c.Head(), "dormant served tip -> live discovery")
}

// Wiring: initHeadCache uses the served tip (single upstream => served ==
// live) and enables metrics before Start.
func TestHeadSource_IntegrationServedAndMetrics(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := headCacheTestConfig(up.URL(), &common.EvmHeadCacheConfig{
		Enabled: true, Depth: 16, PollInterval: common.Duration(100 * time.Millisecond),
	})
	_, _, _, shutdown, e := createServerTestFixtures(cfg, t)
	defer shutdown()
	waitHead(t, e, 20)
	require.Equal(t, common.HeadCacheHeadSourceServed, cfg.Projects[0].Networks[0].Evm.HeadCache.HeadSource)
	up.Mine(2)
	waitHead(t, e, 22)
	prj, _ := e.GetProject("test_project")
	nw, _ := prj.GetNetwork(t.Context(), "evm:123")
	require.Eventually(t, func() bool {
		return promUtil.ToFloat64(telemetry.MetricHeadCacheHead.WithLabelValues("test_project", nw.Label())) == 22 &&
			promUtil.ToFloat64(telemetry.MetricHeadCacheLeader.WithLabelValues("test_project", nw.Label())) == 1
	}, 5*time.Second, 50*time.Millisecond)
}
