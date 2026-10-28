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
	ups.operatorRecoveryVar()
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

	// A later cordon after an uncordon is accepted again. The originating
	// replica's reason must not leak into a peer that never issued this call.
	c.CordonAdmin(ctx, "incident #2")
	require.Eventually(t, cordoned(a), 5*time.Second, 20*time.Millisecond)
	reason, _ = a.CordonedReason("eth_call")
	require.Equal(t, "operator cordon (set on another replica)", reason)
	reason, _ = c.CordonedReason("eth_call")
	require.Equal(t, "incident #2", reason)
}

func TestCordonAdmin_DetectorsCannotLiftAnOperatorCordon(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	defer m.Close()
	ctx := context.Background()

	a := newCordonTestUpstream(t, m.Addr())
	b := newCordonTestUpstream(t, m.Addr())

	// B holds an automatic cordon (consensus sit-out) while an operator
	// cordons the same upstream from A.
	b.Cordon("*", "misbehaving in consensus", common.CordonClassPunishment)
	a.CordonAdmin(ctx, "operator")
	require.Eventually(t, func() bool {
		r, _ := b.CordonedReason("*")
		return r == "operator cordon (set on another replica)"
	}, 5*time.Second, 20*time.Millisecond)

	// The sit-out ends on B: the operator cordon holds there and on A.
	b.Uncordon("*", "end of consensus penalty", common.CordonClassPunishment)
	require.True(t, cordoned(b)(), "consensus timer must not lift an operator cordon")
	require.True(t, cordoned(a)())

	// Operator lifts from A: UncordonAdmin is the fleet-wide override and
	// clears every class bit (including a live punishment sitout) so
	// availability cordons without an automatic recovery path can be lifted.
	b.Cordon("*", "misbehaving in consensus", common.CordonClassPunishment)
	a.UncordonAdmin(ctx, "resolved")
	require.Eventually(t, notCordoned(b), 5*time.Second, 20*time.Millisecond, "UncordonAdmin clears all cordon classes fleet-wide")

	// With no operator cordon held, detectors work as before.
	b.Cordon("*", "misbehaving in consensus", common.CordonClassPunishment)
	b.Uncordon("*", "end of consensus penalty", common.CordonClassPunishment)
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

// When the shared operator counter is already zero, UncordonAdmin must still
// clear peer availability/punishment bits via a distinct recovery epoch —
// TryUpdate(0) is a no-op and a zero→epoch→zero pulse can coalesce away.
func TestUncordonAdmin_RecoveryEpochClearsPeerAvailability(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	defer m.Close()
	ctx := context.Background()

	a := newCordonTestUpstream(t, m.Addr())
	b := newCordonTestUpstream(t, m.Addr())

	require.Equal(t, int64(0), a.operatorCordonVar().GetValue())
	b.Cordon("*", "chain identity mismatch", common.CordonClassAvailability)
	require.True(t, cordoned(b)())
	require.False(t, cordoned(a)())

	a.UncordonAdmin(ctx, "ops cleared availability")
	require.True(t, notCordoned(a)())
	require.Eventually(t, notCordoned(b), 5*time.Second, 20*time.Millisecond,
		"recovery epoch must clear peer availability when operator counter is already zero")
}

// A detector cordon that lands after UncordonAdmin snapshots generations must
// survive the local clear — otherwise a concurrent sitout/availability verdict
// is erased by the admin recovery that started earlier.
func TestUncordonAdmin_PreservesPostSnapshotDetectorCordon(t *testing.T) {
	a := newCordonTestUpstream(t, "")

	a.Cordon("*", "old availability", common.CordonClassAvailability)
	require.True(t, cordoned(a)())

	gens := a.metricsTracker.CordonGens(a, "*")
	a.Cordon("*", "new punishment after snapshot", common.CordonClassPunishment)
	require.Greater(t, a.metricsTracker.CordonGens(a, "*")[0], gens[0])

	// Simulate the generation-gated clear UncordonAdmin performs after publish.
	a.metricsTracker.UncordonClassesAtMostGens(a, "*", "admin recovery", gens)
	classes := a.metricsTracker.CordonClasses(a, "*")
	require.Contains(t, classes, common.CordonClassPunishment,
		"punishment cordoned after the snapshot must survive the gated clear")
	require.NotContains(t, classes, common.CordonClassAvailability,
		"availability present at snapshot must be cleared")
}

// A CordonAdmin that arrives after UncordonAdmin has snapshotted generations
// must publish a new shared timestamp and keep the operator bit when that
// uncordon then publishes zero.
func TestCordonAdmin_ReassertSurvivesInFlightUncordon(t *testing.T) {
	ctx := context.Background()
	a := newCordonTestUpstream(t, "")
	a.CordonAdmin(ctx, "incident")
	require.True(t, cordoned(a)())

	gens := a.metricsTracker.CordonGens(a, "*")
	a.uncordonInFlight.Store(true)
	a.uncordonMaxOperatorGen.Store(gens[operatorCordonGenIndex])
	before := a.operatorCordonVar().GetValue()

	a.CordonAdmin(ctx, "incident again")
	require.Greater(t, a.operatorCordonVar().GetValue(), before, "in-flight re-assert must advance the shared counter")
	require.Greater(t, a.metricsTracker.CordonGens(a, "*")[operatorCordonGenIndex], gens[operatorCordonGenIndex])

	a.operatorCordonVar().TryUpdate(ctx, 0)
	classes := a.metricsTracker.CordonClasses(a, "*")
	require.Contains(t, classes, common.CordonClassOperator, "stale zero must not clear a re-asserted operator cordon")
}

// After UncordonAdmin completes, a subsequent CordonAdmin must stick
// (shared counter + local operator bit). Mid-clear injection is covered by
// the post-publish GetValue()!=0 guard in UncordonAdmin / recovery ordering.
func TestUncordonAdmin_AllowsSequentialReCordonAfterClear(t *testing.T) {
	ctx := context.Background()
	a := newCordonTestUpstream(t, "")

	a.Cordon("*", "availability", common.CordonClassAvailability)
	a.CordonAdmin(ctx, "incident")
	require.True(t, cordoned(a)())

	a.UncordonAdmin(ctx, "resolved")
	require.True(t, notCordoned(a)())
	a.CordonAdmin(ctx, "incident again")
	require.Eventually(t, cordoned(a), 2*time.Second, 10*time.Millisecond)
	classes := a.metricsTracker.CordonClasses(a, "*")
	require.Contains(t, classes, common.CordonClassOperator)
}
