package headcache

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/require"
)

// scrape serves the head cache families from a private registry over real
// promhttp and returns the /metrics text body.
func scrape(t *testing.T) string {
	t.Helper()
	reg := prometheus.NewRegistry()
	for _, c := range []prometheus.Collector{
		telemetry.MetricHeadCacheRequestsTotal, telemetry.MetricHeadCacheFetchesTotal,
		telemetry.MetricHeadCacheReorgsTotal, telemetry.MetricHeadCachePublishedTotal,
		telemetry.MetricHeadCacheLeaderEpochsTotal, telemetry.MetricHeadCacheLeader,
		telemetry.MetricHeadCacheHead, telemetry.MetricHeadCacheBytes,
		telemetry.MetricHeadCacheSnapshotTimestamp, telemetry.MetricHeadCacheSubscribers,
		telemetry.MetricHeadCacheSubscriberClosedTotal,
	} {
		require.NoError(t, reg.Register(c))
	}
	srv := httptest.NewServer(promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

func has(body, line string) bool {
	for _, l := range strings.Split(body, "\n") {
		if l == line {
			return true
		}
	}
	return false
}

func TestMetrics_ScrapeLeaderFollowerReorgSubscribers(t *testing.T) {
	ctx := context.Background()
	ch := newFakeChain(10)
	store := NewMemoryStore()
	a := New(testOpts("a"), store, ch, ch.head, nil)
	a.EnableMetrics("mproj", "mnet-a")
	b := New(testOpts("b"), store, ch, ch.head, nil)
	b.EnableMetrics("mproj", "mnet-b")

	a.Tick(ctx)
	b.Tick(ctx)
	_, ok := a.BlockByNumber(10, false)
	require.True(t, ok)
	_, ok = a.BlockByNumber(999, false)
	require.False(t, ok)
	sub := a.Subscribe(1)
	sub2 := a.Subscribe(16)

	body := scrape(t)
	for _, l := range []string{
		`erpc_head_cache_leader{network="mnet-a",project="mproj"} 1`,
		`erpc_head_cache_leader{network="mnet-b",project="mproj"} 0`,
		`erpc_head_cache_leader_acquisitions_total{network="mnet-a",project="mproj"} 1`,
		`erpc_head_cache_head_block{network="mnet-a",project="mproj"} 10`,
		`erpc_head_cache_head_block{network="mnet-b",project="mproj"} 10`,
		`erpc_head_cache_requests_total{network="mnet-a",project="mproj",result="hit"} 1`,
		`erpc_head_cache_requests_total{network="mnet-a",project="mproj",result="miss"} 1`,
		`erpc_head_cache_fetches_total{network="mnet-a",project="mproj",result="ok"} 11`, // blocks 0..10, equals Stats.Hydrated
		`erpc_head_cache_snapshots_published_total{network="mnet-a",project="mproj"} 1`,
		`erpc_head_cache_subscribers{network="mnet-a",project="mproj"} 2`,
	} {
		require.True(t, has(body, l), "missing %q in:\n%s", l, body)
	}
	require.Contains(t, body, `erpc_head_cache_bytes{network="mnet-a",project="mproj"}`)
	require.Contains(t, body, `erpc_head_cache_snapshot_timestamp_seconds{network="mnet-b",project="mproj"}`)
	require.NotContains(t, body, `erpc_head_cache_fetches_total{network="mnet-b"`, "follower must not hydrate")

	// Reorg of the tip, then advance twice: queue-1 sub becomes slow consumer.
	ch.reorg(10, "b")
	a.Tick(ctx)
	ch.mine(1)
	a.Tick(ctx)
	sub2.Close()
	sub2.Close() // idempotent: no double count
	body = scrape(t)
	require.True(t, has(body, `erpc_head_cache_reorgs_total{network="mnet-a",project="mproj"} 1`), body)
	require.True(t, has(body, `erpc_head_cache_subscriber_closed_total{network="mnet-a",project="mproj",reason="slow_consumer"} 1`), body)
	require.True(t, has(body, `erpc_head_cache_subscriber_closed_total{network="mnet-a",project="mproj",reason="unsubscribe"} 1`), body)
	require.True(t, has(body, `erpc_head_cache_subscribers{network="mnet-a",project="mproj"} 0`), body)
	_ = sub

	// Stop: releases lease and removes every gauge series (no stale leader=1).
	a.Stop()
	b.Stop()
	body = scrape(t)
	require.NotContains(t, body, `erpc_head_cache_leader{network="mnet-a"`)
	require.NotContains(t, body, `erpc_head_cache_head_block{network="mnet-a"`)
	require.NotContains(t, body, `erpc_head_cache_subscribers{network="mnet-a"`)
	require.True(t, has(body, `erpc_head_cache_reorgs_total{network="mnet-a",project="mproj"} 1`), "counters stay monotonic")
}

func TestMetrics_LeaseLossDropsLeaderGauge(t *testing.T) {
	ctx := context.Background()
	ch := newFakeChain(5)
	store := NewMemoryStore()
	a := New(testOpts("a"), store, ch, ch.head, nil)
	a.EnableMetrics("mproj", "mnet-loss")
	b := New(testOpts("b"), store, ch, ch.head, nil)
	a.Tick(ctx)
	require.True(t, has(scrape(t), `erpc_head_cache_leader{network="mnet-loss",project="mproj"} 1`))
	store.ExpireLease(a.opt.Scope)
	b.Tick(ctx) // b takes over
	a.Tick(ctx) // a's renew fails, acquire fails
	require.True(t, has(scrape(t), `erpc_head_cache_leader{network="mnet-loss",project="mproj"} 0`))
	a.Stop()
	b.Stop()
}

func TestMetrics_DisabledIsNoop(t *testing.T) {
	ctx := context.Background()
	ch := newFakeChain(3)
	c := New(testOpts("a"), NewMemoryStore(), ch, ch.head, nil)
	c.Tick(ctx)
	c.BlockByNumber(3, false)
	s := c.Subscribe(1)
	s.Close()
	c.Stop()
	require.Nil(t, c.metrics)
	require.Equal(t, int64(1), c.Stats.Hits.Load())
}
