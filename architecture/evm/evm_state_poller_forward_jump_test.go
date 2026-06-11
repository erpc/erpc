package evm

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Generic representative heads: an upstream with an established head abruptly
// reports a far-future head (e.g. a node serving another chain's data).
const (
	fjSaneHead    = int64(1_000_000)
	fjOutlierHead = int64(60_000_000)
	fjJumpSize    = fjOutlierHead - fjSaneHead
)

// fjFakeUpstream returns the concrete *common.FakeUpstream so tests can read
// cordon status via CordonedReason(), which is not on the common.Upstream
// interface. It still satisfies common.Upstream where the pollers need it.
func fjFakeUpstream(id string) *common.FakeUpstream {
	return common.NewFakeUpstream(id).(*common.FakeUpstream)
}

func fjTestPoller(ups common.Upstream, sitOut time.Duration) *EvmStatePoller {
	lg := zerolog.Nop()
	return &EvmStatePoller{
		projectId:         "test-project",
		appCtx:            context.Background(),
		logger:            &lg,
		upstream:          ups,
		tracker:           health.NewTracker(&lg, "test-project", 2*time.Second),
		forwardJumpSitOut: sitOut,
	}
}

func fjGaugeValue(projectId string, ups common.Upstream) float64 {
	g := telemetry.MetricUpstreamBlockHeadLargeForwardJump.WithLabelValues(
		projectId, ups.VendorName(), ups.NetworkLabel(), ups.Id(),
	)
	return promUtil.ToFloat64(g)
}

func fjTestSharedStateRegistry(t *testing.T, ctx context.Context) data.SharedStateRegistry {
	t.Helper()
	lg := zerolog.Nop()
	cfg := &common.SharedStateConfig{
		ClusterKey:      "test",
		FallbackTimeout: common.Duration(1 * time.Second),
		LockTtl:         common.Duration(30 * time.Second),
		Connector: &common.ConnectorConfig{
			Id:     "test-memory",
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     1000,
				MaxTotalSize: "10MB",
			},
		},
	}
	require.NoError(t, cfg.SetDefaults("test"))
	registry, err := data.NewSharedStateRegistry(ctx, &lg, cfg)
	require.NoError(t, err)
	return registry
}

// warmBlockTime feeds enough strictly-increasing (block, timestamp) samples
// into the tracker for the network block-time EMA to become available — the
// signal that arms the ingestion guard.
func warmBlockTime(t *testing.T, tracker *health.Tracker, ups common.Upstream, startBlock int64, blockTimeSecs int64) {
	t.Helper()
	ts := time.Now().Unix() - 100
	for i := int64(0); i < 5; i++ {
		tracker.SetLatestBlockNumber(ups, startBlock+i, ts+i*blockTimeSecs)
	}
	require.Greater(t, tracker.GetNetworkBlockTime(ups.NetworkId()), time.Duration(0),
		"block-time EMA must be warmed for the guard to arm")
}

// ─── cordon / sit-out mechanics (detection handler level) ────────────────

func TestEvmStatePoller_ForwardJump_CordonsAndRecordsMetric(t *testing.T) {
	ups := fjFakeUpstream("forward-jump-happy")
	e := fjTestPoller(ups, 50*time.Millisecond)

	e.handleLatestBlockForwardJump(fjSaneHead, fjOutlierHead)

	reason, cordoned := ups.CordonedReason()
	assert.True(t, cordoned, "upstream must be cordoned on a forward-jump detection")
	assert.Contains(t, reason, "forward block-head jump", "cordon reason should describe the forward jump")

	assert.Equal(t, float64(fjJumpSize), fjGaugeValue("test-project", ups),
		"forward-jump gauge must record the magnitude of the rejected jump")
}

func TestEvmStatePoller_ForwardJump_RecoversAfterSitOut(t *testing.T) {
	ups := fjFakeUpstream("forward-jump-recovery")
	e := fjTestPoller(ups, 60*time.Millisecond)

	e.handleLatestBlockForwardJump(fjSaneHead, fjOutlierHead)
	_, cordoned := ups.CordonedReason()
	require.True(t, cordoned, "upstream must be cordoned immediately after detection")

	require.Eventually(t, func() bool {
		_, c := ups.CordonedReason()
		return !c
	}, 2*time.Second, 5*time.Millisecond, "upstream must auto-uncordon after the sit-out window")

	// Once the sit-out timer fires it must clear itself so a future
	// detection can start a fresh penalty.
	e.forwardJumpSitOutMu.Lock()
	timerCleared := e.forwardJumpSitOutTimer == nil
	e.forwardJumpSitOutMu.Unlock()
	assert.True(t, timerCleared, "sit-out timer must be cleared after it fires")
}

func TestEvmStatePoller_ForwardJump_DedupesRepeatedDetections(t *testing.T) {
	ups := fjFakeUpstream("forward-jump-dedupe")
	// Long sit-out so the first timer never fires during the repeated calls.
	e := fjTestPoller(ups, 5*time.Second)

	e.handleLatestBlockForwardJump(fjSaneHead, fjOutlierHead)

	e.forwardJumpSitOutMu.Lock()
	firstTimer := e.forwardJumpSitOutTimer
	e.forwardJumpSitOutMu.Unlock()
	require.NotNil(t, firstTimer, "first detection must start a sit-out timer")

	for i := 0; i < 20; i++ {
		e.handleLatestBlockForwardJump(fjSaneHead, fjOutlierHead)
	}

	e.forwardJumpSitOutMu.Lock()
	sameTimer := e.forwardJumpSitOutTimer == firstTimer
	e.forwardJumpSitOutMu.Unlock()
	assert.True(t, sameTimer, "repeated detections must not stack or replace the sit-out timer")

	_, cordoned := ups.CordonedReason()
	assert.True(t, cordoned, "upstream stays cordoned across repeated detections")
}

func TestEvmStatePoller_ForwardJump_ConcurrentDetections(t *testing.T) {
	ups := fjFakeUpstream("forward-jump-concurrent")
	e := fjTestPoller(ups, 5*time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.handleLatestBlockForwardJump(fjSaneHead, fjOutlierHead)
		}()
	}
	wg.Wait()

	_, cordoned := ups.CordonedReason()
	assert.True(t, cordoned, "concurrent detections must leave the upstream cordoned")

	e.forwardJumpSitOutMu.Lock()
	timerSet := e.forwardJumpSitOutTimer != nil
	e.forwardJumpSitOutMu.Unlock()
	assert.True(t, timerSet, "exactly one sit-out timer must be in flight after concurrent detections")
}

// ─── end-to-end through the real counter + velocity bound ────────────────

// Full wiring: NewEvmStatePoller installs the velocity bound and the
// OnLargeForwardJump -> cordon hook on the latest-block counter. With the
// chain's block-time EMA known, an implausible forward jump is rejected at
// ingestion (the stored value is NOT poisoned) and the upstream is cordoned.
func TestEvmStatePoller_ForwardJump_EndToEndThroughCounter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	registry := fjTestSharedStateRegistry(t, ctx)
	lg := zerolog.Nop()
	tracker := health.NewTracker(&lg, "test-project", 2*time.Second)
	ups := fjFakeUpstream("forward-jump-e2e")

	_ = NewEvmStatePoller("test-project", ctx, &lg, ups, tracker, registry)

	warmBlockTime(t, tracker, ups, fjSaneHead-10, 2)

	// Re-fetch the SAME counter instance (LoadOrStore by key) to drive values.
	key := "latestBlock/" + common.UniqueUpstreamKey(ups)
	lbs := registry.GetCounterInt64(key, DefaultToleratedBlockHeadRollback)

	// Establish a sane head (cold-start from 0 is always accepted).
	require.Equal(t, fjSaneHead, lbs.TryUpdate(ctx, fjSaneHead))

	// A far-future jump (way past what the chain could have minted) is
	// rejected: value unchanged, upstream cordoned.
	got := lbs.TryUpdate(ctx, fjOutlierHead)
	assert.Equal(t, fjSaneHead, got, "implausible forward jump must not be stored")
	assert.Equal(t, fjSaneHead, lbs.GetValue(), "head source value must not be poisoned")

	_, cordoned := ups.CordonedReason()
	assert.True(t, cordoned, "forward jump must cordon the upstream end-to-end")
}

// Normal forward progression — including a burst up to the floor — is
// accepted and never cordons, proving the guard does not misfire on
// legitimate catch-up or bursty block production.
func TestEvmStatePoller_ForwardJump_NormalProgressionAndBurstNotCordoned(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	registry := fjTestSharedStateRegistry(t, ctx)
	lg := zerolog.Nop()
	tracker := health.NewTracker(&lg, "test-project", 2*time.Second)
	ups := fjFakeUpstream("forward-jump-normal")

	_ = NewEvmStatePoller("test-project", ctx, &lg, ups, tracker, registry)

	warmBlockTime(t, tracker, ups, fjSaneHead-10, 2)

	key := "latestBlock/" + common.UniqueUpstreamKey(ups)
	lbs := registry.GetCounterInt64(key, DefaultToleratedBlockHeadRollback)

	require.Equal(t, fjSaneHead, lbs.TryUpdate(ctx, fjSaneHead))

	// Steady progression: a few blocks per poll.
	assert.Equal(t, fjSaneHead+3, lbs.TryUpdate(ctx, fjSaneHead+3), "steady progression must be accepted")

	// Burst: a jump below the floor is never rejected, even though elapsed-time
	// velocity alone would not allow it (chains can idle then mint many blocks).
	burst := fjSaneHead + 3 + blockHeadForwardJumpFloor - 1
	assert.Equal(t, burst, lbs.TryUpdate(ctx, burst), "a burst below the floor must be accepted")

	_, cordoned := ups.CordonedReason()
	assert.False(t, cordoned, "legitimate progression must not cordon the upstream")
}

// Until the chain's block-time EMA is known the guard must stay inactive:
// rejecting with an unknown chain pace would misjudge fast chains. A huge
// jump in that window is accepted and the upstream is not cordoned.
func TestEvmStatePoller_ForwardJump_GuardInactiveUntilBlockTimeKnown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	registry := fjTestSharedStateRegistry(t, ctx)
	lg := zerolog.Nop()
	tracker := health.NewTracker(&lg, "test-project", 2*time.Second)
	ups := fjFakeUpstream("forward-jump-cold-ema")

	_ = NewEvmStatePoller("test-project", ctx, &lg, ups, tracker, registry)

	// No EMA warm-up: block time unknown.
	require.Equal(t, time.Duration(0), tracker.GetNetworkBlockTime(ups.NetworkId()))

	key := "latestBlock/" + common.UniqueUpstreamKey(ups)
	lbs := registry.GetCounterInt64(key, DefaultToleratedBlockHeadRollback)

	require.Equal(t, fjSaneHead, lbs.TryUpdate(ctx, fjSaneHead))
	assert.Equal(t, fjOutlierHead, lbs.TryUpdate(ctx, fjOutlierHead),
		"with chain pace unknown the guard must stay inactive (accept, don't guess)")

	_, cordoned := ups.CordonedReason()
	assert.False(t, cordoned, "no cordon while the guard is inactive")
}

// The elapsed-since-change semantics that make long-stall catch-up safe are
// pinned at the data layer (TestCounterInt64_ForwardBound "stall" subtest):
// the bound receives time since the value last CHANGED, so the velocity term
// scales with the true stall duration. At the poller level the floor already
// covers every catch-up smaller than blockHeadForwardJumpFloor (burst test
// above); a velocity-term-dominated catch-up needs hours of wall-clock time
// and is intentionally not simulated here.
