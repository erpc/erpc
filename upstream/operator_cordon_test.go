package upstream

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// newCordonTestUpstream builds one "replica": its own tracker and shared-state
// registry, one upstream whose shared cordon variable is initialized as
// Bootstrap would. redisAddr == "" uses the memory driver.
func newCordonTestUpstream(t *testing.T, redisAddr string) *Upstream {
	t.Helper()
	ctx := t.Context()
	logger := zerolog.New(io.Discard)

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&logger, vr, nil, nil)
	require.NoError(t, err)
	rlr, err := NewRateLimitersRegistry(ctx, &common.RateLimiterConfig{}, &logger)
	require.NoError(t, err)

	cfg := &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: common.DriverMemory}}
	if redisAddr != "" {
		cfg = &common.SharedStateConfig{
			ClusterKey: "cordon-test",
			Connector: &common.ConnectorConfig{
				Driver: common.DriverRedis,
				Redis: &common.RedisConnectorConfig{
					Addr:         redisAddr,
					ConnPoolSize: 5,
					InitTimeout:  common.Duration(2 * time.Second),
					GetTimeout:   common.Duration(2 * time.Second),
					SetTimeout:   common.Duration(2 * time.Second),
				},
			},
		}
	}
	require.NoError(t, cfg.SetDefaults("cordon-test"))
	ssr, err := data.NewSharedStateRegistry(ctx, &logger, cfg)
	require.NoError(t, err)

	mt := health.NewTracker(&logger, "main", time.Minute)
	upsCfg := &common.UpstreamConfig{
		Id:       "alchemy",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm:      &common.EvmUpstreamConfig{ChainId: 123},
	}
	reg := NewUpstreamsRegistry(ctx, &logger, "main", []*common.UpstreamConfig{upsCfg}, ssr, rlr, vr, pr, nil, mt, nil)
	ups, err := reg.NewUpstream(upsCfg)
	require.NoError(t, err)
	ups.operatorCordonVar()
	return ups
}

func cordoned(u *Upstream) func() bool {
	return func() bool { return u.metricsTracker.IsCordoned(u, "eth_call") }
}

func notCordoned(u *Upstream) func() bool {
	return func() bool { return !u.metricsTracker.IsCordoned(u, "eth_call") }
}

func TestCordonAdmin_PropagatesAcrossReplicasAndRestarts(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	defer m.Close()
	ctx := context.Background()

	a := newCordonTestUpstream(t, m.Addr())
	b := newCordonTestUpstream(t, m.Addr())

	a.CordonAdmin(ctx, "vendor incident #1")
	require.True(t, cordoned(a)(), "originating replica applies immediately")
	require.Eventually(t, cordoned(b), 5*time.Second, 20*time.Millisecond, "peer converges through shared state")
	reason, _ := b.CordonedReason("eth_call")
	require.Equal(t, "operator cordon (set on another replica)", reason)
	reason, _ = a.CordonedReason("eth_call")
	require.Equal(t, "vendor incident #1", reason)

	// Re-cordon on the peer keeps the original start.
	started := a.operatorCordonVar().GetValue()
	b.CordonAdmin(ctx, "vendor incident #1 (seen from b)")
	require.Equal(t, started, b.operatorCordonVar().GetValue())
	require.Equal(t, started, a.operatorCordonVar().GetValue())

	// A restarted pod restores the cordon from shared state alone.
	c := newCordonTestUpstream(t, m.Addr())
	require.Eventually(t, cordoned(c), 5*time.Second, 20*time.Millisecond, "restored at bootstrap, no admin call needed")

	// Uncordon on B lifts it everywhere.
	b.UncordonAdmin(ctx, "resolved")
	require.True(t, notCordoned(b)())
	require.Eventually(t, notCordoned(a), 5*time.Second, 20*time.Millisecond)
	require.Eventually(t, notCordoned(c), 5*time.Second, 20*time.Millisecond)

	// A later cordon after an uncordon is accepted again.
	c.CordonAdmin(ctx, "incident #2")
	require.Eventually(t, cordoned(a), 5*time.Second, 20*time.Millisecond)
}

func TestCordonAdmin_OperatorAndAutomaticCordonsAreIndependent(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	defer m.Close()
	ctx := context.Background()

	a := newCordonTestUpstream(t, m.Addr())
	b := newCordonTestUpstream(t, m.Addr())

	// B holds an automatic cordon (consensus sit-out) while an operator
	// cordons the same upstream from A.
	b.Cordon("*", "misbehaving in consensus")
	a.CordonAdmin(ctx, "operator")
	require.Eventually(t, func() bool {
		r, _ := b.CordonedReason("*")
		return r == "operator cordon (set on another replica)"
	}, 5*time.Second, 20*time.Millisecond, "operator reason wins while both hold")

	// The sit-out ends: the operator cordon holds.
	b.Uncordon("*", "end of consensus penalty")
	require.True(t, cordoned(b)(), "consensus timer must not lift an operator cordon")

	// Operator lifts from A: only the operator layer clears on B.
	b.Cordon("*", "misbehaving in consensus")
	a.UncordonAdmin(ctx, "resolved")
	require.Eventually(t, func() bool {
		r, ok := b.CordonedReason("*")
		return ok && r == "misbehaving in consensus"
	}, 5*time.Second, 20*time.Millisecond, "shared uncordon never touches automatic cordons")

	// A local operator uncordon is the override: it lifts the detector's
	// cordon on this replica too.
	b.UncordonAdmin(ctx, "override")
	require.True(t, notCordoned(b)())
}

func TestCordonAdmin_MemoryDriverStaysInProcess(t *testing.T) {
	ctx := context.Background()
	a := newCordonTestUpstream(t, "")
	b := newCordonTestUpstream(t, "")

	a.CordonAdmin(ctx, "local only")
	require.True(t, cordoned(a)())
	time.Sleep(200 * time.Millisecond)
	require.False(t, cordoned(b)(), "memory driver shares nothing")
	a.UncordonAdmin(ctx, "done")
	require.True(t, notCordoned(a)())
}
