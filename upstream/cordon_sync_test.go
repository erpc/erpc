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

// newCordonTestReplica builds one project registry ("replica") with a
// registered-but-not-bootstrapped upstream. redisAddr == "" uses the memory
// driver. registerUpstream == false leaves the registry empty so a test can
// register later, as a restarted pod does.
func newCordonTestReplica(t *testing.T, redisAddr string, registerUpstream bool) (*UpstreamsRegistry, *Upstream) {
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
	if registerUpstream {
		reg.doRegisterBootstrappedUpstream(ups)
	}
	return reg, ups
}

func TestCordonAdmin_PropagatesAcrossReplicasAndRestarts(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	defer m.Close()
	ctx := context.Background()

	regA, upsA := newCordonTestReplica(t, m.Addr(), true)
	regB, upsB := newCordonTestReplica(t, m.Addr(), true)

	require.NoError(t, regA.CordonAdmin(ctx, "alchemy", "*", "vendor incident #1"))
	require.True(t, regA.metricsTracker.IsCordoned(upsA, "eth_call"), "originating replica applies immediately")
	require.False(t, regB.metricsTracker.IsCordoned(upsB, "eth_call"), "peer converges on its next sync tick")

	regB.syncOperatorCordons()
	require.True(t, regB.metricsTracker.IsCordoned(upsB, "eth_call"))
	reason, ok := upsB.CordonedReason("eth_call")
	require.True(t, ok)
	require.Equal(t, "vendor incident #1", reason)
	require.Equal(t, regA.OperatorCordons(), regB.OperatorCordons(), "both replicas route by the same snapshot")

	// A reason edit issued through B, which never wrote the cordon, keeps
	// the persisted start on every replica.
	started := regA.OperatorCordons().Cordons["alchemy"]["*"].CordonedAtMs
	require.NoError(t, regB.CordonAdmin(ctx, "alchemy", "*", "vendor incident #1 (extended)"))
	regA.syncOperatorCordons()
	for _, reg := range []*UpstreamsRegistry{regA, regB} {
		e := reg.OperatorCordons().Cordons["alchemy"]["*"]
		require.Equal(t, "vendor incident #1 (extended)", e.Reason)
		require.Equal(t, started, e.CordonedAtMs)
	}

	// A restarted pod restores the cordon from Redis before its upstream is
	// even registered: lookups are by id.
	regC, upsC := newCordonTestReplica(t, m.Addr(), false)
	regC.Bootstrap(ctx)
	require.True(t, regC.metricsTracker.IsCordoned(upsC, "*"), "snapshot applies without a registered upstream")
	regC.doRegisterBootstrappedUpstream(upsC)
	require.True(t, regC.metricsTracker.IsCordoned(upsC, "*"))

	// Uncordon on B lifts it on every replica after their next tick.
	require.NoError(t, regB.UncordonAdmin(ctx, "alchemy", "*", "resolved"))
	require.False(t, regB.metricsTracker.IsCordoned(upsB, "*"))
	regA.syncOperatorCordons()
	regC.syncOperatorCordons()
	require.False(t, regA.metricsTracker.IsCordoned(upsA, "*"))
	require.False(t, regC.metricsTracker.IsCordoned(upsC, "*"))
}

func TestCordonSync_StaleFetchNeverRollsBackALocalWrite(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	defer m.Close()
	ctx := context.Background()

	reg, ups := newCordonTestReplica(t, m.Addr(), true)
	require.NoError(t, reg.CordonAdmin(ctx, "alchemy", "eth_getLogs", "warming up"))
	require.NoError(t, reg.CordonAdmin(ctx, "alchemy", "*", "incident"))
	current := reg.OperatorCordons()
	require.Equal(t, int64(2), current.Version)

	// A poll that started before the write completes after it: older
	// version, must be ignored.
	stale := &common.CordonSnapshot{Version: current.Version - 1, Cordons: common.ProjectCordons{}}
	require.False(t, reg.metricsTracker.SetOperatorCordons(stale, func(string) common.Upstream { return ups }))
	require.True(t, reg.metricsTracker.IsCordoned(ups, "*"))

	// A record removed out of band reads as version 0 and is a reset.
	reset := &common.CordonSnapshot{Version: 0, Cordons: common.ProjectCordons{}}
	require.True(t, reg.metricsTracker.SetOperatorCordons(reset, func(string) common.Upstream { return ups }))
	require.False(t, reg.metricsTracker.IsCordoned(ups, "*"))
}

func TestCordonSync_OperatorAndAutomaticCordonsAreIndependent(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	defer m.Close()
	ctx := context.Background()

	regA, _ := newCordonTestReplica(t, m.Addr(), true)
	regB, upsB := newCordonTestReplica(t, m.Addr(), true)

	// B holds an automatic cordon (consensus sit-out) on the same cell an
	// operator cordons from A.
	upsB.Cordon("*", "misbehaving in consensus")
	require.NoError(t, regA.CordonAdmin(ctx, "alchemy", "*", "operator"))
	regB.syncOperatorCordons()
	reason, _ := upsB.CordonedReason("*")
	require.Equal(t, "operator", reason, "operator reason wins while both hold")

	// The sit-out ends: the operator cordon holds.
	upsB.Uncordon("*", "end of consensus penalty")
	require.True(t, regB.metricsTracker.IsCordoned(upsB, "*"), "consensus timer must not lift an operator cordon")

	// Operator lifts from A: sync clears only the operator layer on B.
	upsB.Cordon("*", "misbehaving in consensus")
	require.NoError(t, regA.UncordonAdmin(ctx, "alchemy", "*", "resolved"))
	regB.syncOperatorCordons()
	require.True(t, regB.metricsTracker.IsCordoned(upsB, "*"), "sync never touches automatic cordons")
	reason, _ = upsB.CordonedReason("*")
	require.Equal(t, "misbehaving in consensus", reason)

	// A local operator uncordon is the override: it also lifts the
	// detector's cordon on this replica.
	require.NoError(t, regB.UncordonAdmin(ctx, "alchemy", "*", "override"))
	require.False(t, regB.metricsTracker.IsCordoned(upsB, "*"))
}

func TestCordonAdmin_PersistFailureChangesNothing(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	reg, ups := newCordonTestReplica(t, m.Addr(), true)
	require.NoError(t, reg.CordonAdmin(context.Background(), "alchemy", "eth_getLogs", "slow"))
	m.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.Error(t, reg.CordonAdmin(ctx, "alchemy", "*", "incident"))
	require.False(t, reg.metricsTracker.IsCordoned(ups, "*"), "a cordon peers will never see is not applied locally")
	require.Error(t, reg.UncordonAdmin(ctx, "alchemy", "eth_getLogs", "x"))
	require.True(t, reg.metricsTracker.IsCordoned(ups, "eth_getLogs"), "an uncordon peers will never see is not applied locally")

	before := reg.OperatorCordons()
	reg.syncOperatorCordons()
	require.Equal(t, before, reg.OperatorCordons(), "unreadable store keeps the current snapshot")
}

func TestCordonAdmin_MemoryDriverStaysInProcess(t *testing.T) {
	ctx := context.Background()
	regA, upsA := newCordonTestReplica(t, "", true)
	regB, upsB := newCordonTestReplica(t, "", true)

	require.NoError(t, regA.CordonAdmin(ctx, "alchemy", "*", "local only"))
	require.True(t, regA.metricsTracker.IsCordoned(upsA, "*"))
	regB.syncOperatorCordons()
	require.False(t, regB.metricsTracker.IsCordoned(upsB, "*"), "memory driver shares nothing")
	require.NoError(t, regA.UncordonAdmin(ctx, "alchemy", "*", "done"))
	require.False(t, regA.metricsTracker.IsCordoned(upsA, "*"))
}
