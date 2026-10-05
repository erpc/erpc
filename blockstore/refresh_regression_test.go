package blockstore

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

// These tests exercise Tick and count actual upstream header requests. The old
// forward-only test never regressed its observed tip or promoted a cold replica.
func TestCache_TipJitterRetainsFetchedHeaders(t *testing.T) {
	ch := newFakeChain(30)
	tip := int64(20)
	c := New(testOpts(), newMapStore(), ch, func(context.Context) int64 { return tip }, nil)
	defer c.Stop()
	c.Tick(t.Context())
	sub := c.Subscribe(16)
	for _, observed := range []int64{19, 20, 21, 20, 21, 22, 21, 22, 1, 22} {
		tip = observed
		c.Tick(t.Context())
	}
	head, body, logs := ch.counts()
	require.Equal(t, int(testOpts().Depth)+2, head, "cold window plus distinct new heights only")
	require.Zero(t, body+logs)
	require.Equal(t, int64(22), c.Head())
	events, open := drain(sub)
	require.True(t, open, "lagging observations must not close subscriptions")
	require.Len(t, events, 2)
}

func TestCache_ColdLeaderHandoverReusesPublishedHeaders(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	ch := newFakeChain(20)
	a := New(testOpts(), store, ch, ch.head, nil)
	a.Tick(t.Context())
	a.Stop()
	ch.mine(2)
	before, _, _ := ch.counts()
	// B has never run a follower tick. Its only source for the old window is
	// the published snapshot and the shared header payloads.
	b := New(testOpts(), store, ch, ch.head, nil)
	defer b.Stop()
	b.Tick(t.Context())
	after, _, _ := ch.counts()
	require.Equal(t, 2, after-before)
	require.Equal(t, int64(22), b.Head())
	require.Len(t, b.snap.Hashes, int(testOpts().Depth))
}

func TestCache_UnavailableTipRetriesOnlyTipAcrossHandover(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	ch := newFakeChain(20)
	a := New(testOpts(), store, ch, ch.head, nil)
	defer a.Stop()
	a.Tick(t.Context())
	before, _, _ := ch.counts()
	ch.mine(3)
	ch.failBlock[23] = true
	active := a
	for i := 0; i < 8; i++ {
		active.Tick(t.Context())
		after, _, _ := ch.counts()
		require.Equal(t, before+i+1, after, "tick %d retries only unavailable height, including after lease turnover", i)
		require.Equal(t, int64(20), active.Head(), "last verified window survives")
		if active.lease == nil {
			active = New(testOpts(), store, ch, ch.head, nil)
			defer active.Stop()
		}
	}
	delete(ch.failBlock, 23)
	active.Tick(t.Context())
	after, _, _ := ch.counts()
	require.Equal(t, before+8+3, after)
	require.Equal(t, int64(23), active.Head())
}

func assertHeaderBudget(t *testing.T, ch *fakeChain, distinct, coldDepth, walkbacks int) {
	t.Helper()
	head, body, logs := ch.counts()
	require.LessOrEqual(t, head, distinct+coldDepth+walkbacks)
	require.Zero(t, body+logs)
}

func TestCache_SteadyStateHeaderBudgetWithJitterAndHandover(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	ch := newFakeChain(200)
	tip := int64(200)
	opts := testOpts()
	opts.Depth, opts.MaxPerTick = 128, 16
	head := func(context.Context) int64 { return tip }
	active := New(opts, store, ch, head, nil)
	defer active.Stop()
	for i := 0; i < 8; i++ {
		active.Tick(t.Context())
	}
	require.Len(t, active.snap.Hashes, 128)
	for i := 1; i <= 200; i++ {
		ch.mine(1)
		tip = 200 + int64(i)
		if i == 101 {
			active.Stop()
			active = New(opts, store, ch, head, nil)
			defer active.Stop()
		}
		active.Tick(t.Context())
		// Alternating upstream observations, including observations below the
		// retained base, must not cause a new cold fill.
		for _, lag := range []int64{1, 0, 2, 0} {
			tip = 200 + int64(i) - lag
			active.Tick(t.Context())
		}
	}
	require.Equal(t, int64(400), active.Head())
	assertHeaderBudget(t, ch, 200, 128, 0)
}

// A leader whose live tip source keeps reporting a height below its verified
// window publishes nothing. Within MaxStaleness that is free jitter; once the
// view is stale each tick counts as a failure and the leader steps down after
// maxLeaderFailures, so a healthy replica can take over. No header is fetched.
func TestCache_LaggingTipLeaderStepsDownOnceStale(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	ch := newFakeChain(20)
	tip := int64(20)
	c := New(testOpts(), store, ch, func(context.Context) int64 { return tip }, nil)
	defer c.Stop()
	c.Tick(t.Context())
	require.NotNil(t, c.lease)
	require.True(t, c.Fresh())
	before, _, _ := ch.counts()

	tip = 18
	for i := 0; i < 2*maxLeaderFailures; i++ {
		c.Tick(t.Context())
	}
	require.NotNil(t, c.lease, "lagging observations within MaxStaleness are free")
	require.Zero(t, store.releases)

	now := time.Now().Add(testOpts().MaxStaleness + time.Second)
	c.nowFn = func() time.Time { return now }
	require.False(t, c.Fresh())
	for i := 0; i < maxLeaderFailures; i++ {
		require.NotNil(t, c.lease, "tick %d", i)
		c.Tick(t.Context())
	}
	require.Nil(t, c.lease, "a stale leader that cannot progress steps down")
	require.Equal(t, 1, store.releases)
	after, _, _ := ch.counts()
	require.Equal(t, before, after, "stepping down costs no upstream call")
}

// An expired view is not re-stamped fresh by an unchanged tip: nothing proves
// the retained headers are still canonical. It stays unfresh, fetching
// nothing, until the tip advances and the new header links onto it.
func TestCache_ExpiredViewEqualTipStaysUnfresh(t *testing.T) {
	ch := newFakeChain(20)
	c := New(testOpts(), newMapStore(), ch, ch.head, nil)
	defer c.Stop()
	c.Tick(t.Context())
	require.True(t, c.Fresh())
	before, _, _ := ch.counts()

	now := time.Now().Add(testOpts().MaxStaleness + time.Second)
	c.nowFn = func() time.Time { return now }
	require.False(t, c.Fresh())
	for i := 0; i < 3; i++ {
		c.Tick(t.Context())
		require.False(t, c.Fresh(), "tick %d: an unchanged tip must not re-verify an expired view", i)
	}
	after, _, _ := ch.counts()
	require.Equal(t, before, after, "no upstream call while the tip is unchanged")

	ch.mine(1)
	c.Tick(t.Context())
	require.True(t, c.Fresh(), "the next linked tip restores freshness")
	require.Equal(t, int64(21), c.Head())
	after, _, _ = ch.counts()
	require.Equal(t, before+1, after, "one header for the new tip")
}

// A takeover leader recovers the window from a stale published snapshot (kept
// to avoid refetching it) but never serves it as fresh on an unchanged tip.
func TestCache_RecoveredStaleSnapshotEqualTipNotFresh(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	ch := newFakeChain(20)
	a := New(testOpts(), store, ch, ch.head, nil)
	a.Tick(t.Context())
	a.Stop()
	require.NotNil(t, store.snap)
	store.snap.At = time.Now().Add(-testOpts().MaxStaleness - time.Second)
	before, _, _ := ch.counts()

	b := New(testOpts(), store, ch, ch.head, nil)
	defer b.Stop()
	b.Tick(t.Context())
	require.NotNil(t, b.lease)
	require.False(t, b.Fresh(), "a stale snapshot is not verified by an unchanged tip")
	require.Equal(t, int64(-1), b.Head())
	after, _, _ := ch.counts()
	require.Equal(t, before, after)

	ch.mine(1)
	b.Tick(t.Context())
	require.True(t, b.Fresh())
	require.Equal(t, int64(21), b.Head())
	after, _, _ = ch.counts()
	require.Equal(t, before+1, after, "the recovered window is extended, not refetched")
}
