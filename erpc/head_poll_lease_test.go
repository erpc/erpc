package erpc

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func headPollLeaseTestConfig(upstreamURL, redisAddr string) *common.Config {
	cfg := headCacheTestConfig(upstreamURL, nil)
	rc := &common.RedisConnectorConfig{
		URI: "redis://" + redisAddr, LockRetryInterval: common.Duration(20 * time.Millisecond),
	}
	if err := rc.SetDefaults(); err != nil {
		panic(err)
	}
	cfg.Database = &common.DatabaseConfig{SharedState: &common.SharedStateConfig{
		ClusterKey: "hpl-e2e",
		Connector:  &common.ConnectorConfig{Driver: common.DriverRedis, Redis: rc},
	}}
	nw := cfg.Projects[0].Networks[0]
	nw.Evm.HeadPolling = &common.EvmHeadPollingConfig{
		Mode: common.HeadPollingModeLease, LeaseTtl: common.Duration(time.Second),
		StaleAfter: common.Duration(600 * time.Millisecond),
	}
	cfg.Projects[0].Upstreams[0].Evm.StatePollerDebounce = common.Duration(50 * time.Millisecond)
	return cfg
}

// Two replicas sharing Redis on a frozen chain: exactly one holds the lease,
// the other skips latest/finalized polls (served from fresh shared counters),
// and after the holder stops the other takes over.
func TestHeadPollLease_TwoReplicasFrozenChainAndTakeover(t *testing.T) {
	mr := miniredis.RunT(t)
	up := newScriptedEvmUpstream(123, 100)
	defer up.Close()
	_, _, _, shutdownA, eA := createServerTestFixtures(headPollLeaseTestConfig(up.URL(), mr.Addr()), t)
	_, _, _, shutdownB, eB := createServerTestFixtures(headPollLeaseTestConfig(up.URL(), mr.Addr()), t)
	defer shutdownB()

	get := func(e *ERPC) *Network {
		p, err := e.GetProject("test_project")
		require.NoError(t, err)
		n, err := p.GetNetwork(t.Context(), "evm:123")
		require.NoError(t, err)
		return n
	}
	na, nb := get(eA), get(eB)
	la, lb := na.HeadPollLease(), nb.HeadPollLease()
	require.NotNil(t, la, "initHeadPollLease must be wired for mode=lease")
	require.NotNil(t, lb)

	require.Eventually(t, func() bool { return la.Held() != lb.Held() }, 5*time.Second, 20*time.Millisecond, "exactly one holder")
	holder, follower, nf := la, lb, nb
	if lb.Held() {
		holder, follower, nf = lb, la, na
	}
	skipped := func() float64 {
		return promUtil.ToFloat64(telemetry.MetricHeadPollSkippedTotal.WithLabelValues("test_project", nf.Label()))
	}
	s0 := skipped()
	require.Eventually(t, func() bool { return skipped() > s0 }, 5*time.Second, 20*time.Millisecond, "follower skips while holder keeps state fresh")
	// Frozen chain: follower keeps skipping over several stale windows because
	// the holder's same-height observations propagate freshness.
	s1 := skipped()
	time.Sleep(2 * time.Second)
	require.Greater(t, skipped(), s1)
	require.Equal(t, int64(100), nf.upstreamsRegistry.GetNetworkUpstreams(t.Context(), "evm:123")[0].EvmStatePoller().LatestBlock())

	// Holder goes away; the other replica takes the lease.
	holder.Stop()
	if holder == la {
		shutdownA()
	} else {
		defer shutdownA()
	}
	require.Eventually(t, follower.Held, 5*time.Second, 20*time.Millisecond, "takeover")
	up.Mine(2)
	require.Eventually(t, func() bool {
		return nf.upstreamsRegistry.GetNetworkUpstreams(t.Context(), "evm:123")[0].EvmStatePoller().LatestBlock() == 102
	}, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, 1.0, promUtil.ToFloat64(telemetry.MetricHeadPollLeaseHeld.WithLabelValues("test_project", nf.Label())))
}
