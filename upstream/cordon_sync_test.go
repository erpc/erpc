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
// registered-but-not-bootstrapped upstream. All replicas share the Redis.
func newCordonTestReplica(t *testing.T, redisAddr string) (*UpstreamsRegistry, *Upstream) {
	t.Helper()
	ctx := t.Context()
	logger := zerolog.New(io.Discard)

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&logger, vr, nil, nil)
	require.NoError(t, err)
	rlr, err := NewRateLimitersRegistry(ctx, &common.RateLimiterConfig{}, &logger)
	require.NoError(t, err)

	var ssr data.SharedStateRegistry
	if redisAddr != "" {
		cfg := &common.SharedStateConfig{
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
		require.NoError(t, cfg.SetDefaults("cordon-test"))
		ssr, err = data.NewSharedStateRegistry(ctx, &logger, cfg)
		require.NoError(t, err)
	} else {
		cfg := &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: common.DriverMemory}}
		require.NoError(t, cfg.SetDefaults("cordon-test"))
		ssr, err = data.NewSharedStateRegistry(ctx, &logger, cfg)
		require.NoError(t, err)
	}

	mt := health.NewTracker(&logger, "main", time.Minute)
	cfg := &common.UpstreamConfig{
		Id:       "alchemy",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm:      &common.EvmUpstreamConfig{ChainId: 123},
	}
	reg := NewUpstreamsRegistry(ctx, &logger, "main", []*common.UpstreamConfig{cfg}, ssr, rlr, vr, pr, nil, mt, nil)
	ups, err := reg.NewUpstream(cfg)
	require.NoError(t, err)
	reg.upstreamsMu.Lock()
	reg.allUpstreams = append(reg.allUpstreams, ups)
	reg.upstreamsMu.Unlock()
	return reg, ups
}

func TestCordonAdmin_PropagatesAcrossReplicasAndRestarts(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	defer m.Close()
	ctx := context.Background()

	regA, upsA := newCordonTestReplica(t, m.Addr())
	regB, upsB := newCordonTestReplica(t, m.Addr())

	require.NoError(t, regA.CordonAdmin(ctx, upsA, "*", "vendor incident #1"))
	require.True(t, regA.metricsTracker.IsCordoned(upsA, "eth_call"), "originating replica applies immediately")
	require.False(t, regB.metricsTracker.IsCordoned(upsB, "eth_call"), "peer only converges on its next sync tick")

	regB.syncCordons()
	require.True(t, regB.metricsTracker.IsCordoned(upsB, "eth_call"))
	reason, ok := upsB.CordonedReason("*")
	require.True(t, ok)
	require.Equal(t, "vendor incident #1", reason)
	entryA, _ := regA.metricsTracker.CordonsOwnedBy(upsA, health.CordonOwnerAdmin)["*"]
	entryB, _ := regB.metricsTracker.CordonsOwnedBy(upsB, health.CordonOwnerAdmin)["*"]
	require.Equal(t, entryA, entryB, "peer restores the original cordon edge, not its own clock")

	// Reason edit on A keeps the start and reaches B.
	require.NoError(t, regA.CordonAdmin(ctx, upsA, "*", "vendor incident #1 (extended)"))
	regB.syncCordons()
	reason, _ = upsB.CordonedReason("*")
	require.Equal(t, "vendor incident #1 (extended)", reason)
	entryB, _ = regB.metricsTracker.CordonsOwnedBy(upsB, health.CordonOwnerAdmin)["*"]
	require.Equal(t, entryA.CordonedAtMs, entryB.CordonedAtMs)

	// A "restarted pod" (fresh registry) restores state from Redis alone.
	regC, upsC := newCordonTestReplica(t, m.Addr())
	regC.syncCordons()
	require.True(t, regC.metricsTracker.IsCordoned(upsC, "*"))

	// Uncordon on B lifts it on every replica after their next tick.
	require.NoError(t, regB.UncordonAdmin(ctx, upsB, "*"))
	require.False(t, regB.metricsTracker.IsCordoned(upsB, "*"))
	regA.syncCordons()
	regC.syncCordons()
	require.False(t, regA.metricsTracker.IsCordoned(upsA, "*"))
	require.False(t, regC.metricsTracker.IsCordoned(upsC, "*"))
}

func TestCordonSync_NeverTouchesAutomaticCordons(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	defer m.Close()
	ctx := context.Background()

	regA, upsA := newCordonTestReplica(t, m.Addr())
	regB, upsB := newCordonTestReplica(t, m.Addr())

	// B has an in-process automatic cordon (e.g. consensus sit-out) on the
	// same cell an operator cordons from A.
	upsB.Cordon("*", "misbehaving in consensus")
	require.NoError(t, regA.CordonAdmin(ctx, upsA, "*", "operator"))
	regB.syncCordons()
	require.True(t, regB.metricsTracker.IsCordoned(upsB, "*"))

	// The sit-out ends: only the auto owner leaves; the operator cordon holds.
	upsB.Uncordon("*", "end of consensus penalty")
	require.True(t, regB.metricsTracker.IsCordoned(upsB, "*"), "consensus timer must not lift an operator cordon")

	// Operator lifts remotely: sync clears only the admin owner...
	upsB.Cordon("*", "misbehaving in consensus")
	require.NoError(t, regA.UncordonAdmin(ctx, upsA, "*"))
	regB.syncCordons()
	require.True(t, regB.metricsTracker.IsCordoned(upsB, "*"), "sync must not lift an automatic cordon")
	_, hasAdmin := regB.metricsTracker.CordonsOwnedBy(upsB, health.CordonOwnerAdmin)["*"]
	require.False(t, hasAdmin)

	// ...whereas a local operator uncordon is an override and clears everything,
	// which is how an identity-mismatch cordon with no self-heal path gets lifted.
	require.NoError(t, regB.UncordonAdmin(ctx, upsB, "*"))
	require.False(t, regB.metricsTracker.IsCordoned(upsB, "*"))
}

func TestCordonAdmin_PersistFailureChangesNothing(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	reg, ups := newCordonTestReplica(t, m.Addr())
	require.NoError(t, reg.CordonAdmin(context.Background(), ups, "eth_getLogs", "slow"))
	m.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.Error(t, reg.CordonAdmin(ctx, ups, "*", "incident"))
	require.False(t, reg.metricsTracker.IsCordoned(ups, "*"), "a cordon peers will never see is not applied locally")
	require.Error(t, reg.UncordonAdmin(ctx, ups, "eth_getLogs"))
	require.True(t, reg.metricsTracker.IsCordoned(ups, "eth_getLogs"), "an uncordon peers will never see is not applied locally")

	before := reg.metricsTracker.CordonsOwnedBy(ups, health.CordonOwnerAdmin)
	reg.syncCordons()
	require.Equal(t, before, reg.metricsTracker.CordonsOwnedBy(ups, health.CordonOwnerAdmin), "unreadable store keeps local state")
}

func TestCordonAdmin_MemoryDriverStaysInProcess(t *testing.T) {
	reg, ups := newCordonTestReplica(t, "")
	require.Nil(t, reg.cordonStore())
	require.NoError(t, reg.CordonAdmin(context.Background(), ups, "*", "local only"))
	require.True(t, reg.metricsTracker.IsCordoned(ups, "*"))
	reg.syncCordons()
	require.True(t, reg.metricsTracker.IsCordoned(ups, "*"), "no store means no sync can clear it")
	require.NoError(t, reg.UncordonAdmin(context.Background(), ups, "*"))
	require.False(t, reg.metricsTracker.IsCordoned(ups, "*"))
}
