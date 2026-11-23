package erpc

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fallbackHeadsServer runs one network with failover on: a primary wss
// source that pushes from init, and a tier:fallback wss source at its
// default (onDemand). onDemand sets failover.onDemandFallbackHeads. It
// returns once the primary leads, before any client subscribes.
func fallbackHeadsServer(t *testing.T, onDemand bool) (primary, fallback *mockWsVendor, nw *Network, addr string) {
	t.Helper()
	setupGock()
	t.Cleanup(util.ResetGock)
	prevLead := primaryLeadAfter.Load()
	primaryLeadAfter.Store(300 * time.Millisecond)
	t.Cleanup(func() { primaryLeadAfter.Store(prevLead) })

	chain := newMockChain(0x100, false)
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go chain.run(ctx, 150*time.Millisecond)
	primary, primaryURL := newMockWsVendor(t, chain, nil)
	fallback, fallbackURL := newMockWsVendor(t, chain, nil)

	fb := sourceUpstream("ws-fallback", fallbackURL, "")
	fb.Tags = []string{common.TagTierFallback}
	cfg := sourcesConfig(func(n *common.NetworkConfig) {
		n.Failover = &common.FailoverConfig{OnDefaultsExhausted: util.BoolPtr(true)}
		if onDemand {
			n.Failover.OnDemandFallbackHeads = util.BoolPtr(true)
		}
	}, sourceUpstream("ws-primary", primaryURL, common.SubscriptionSourceAlways), fb)
	erpcSrv, addr, cancel := startTestERPCServer(t, cfg)
	t.Cleanup(func() { _ = erpcSrv.Shutdown(&log.Logger); cancel() })
	prj, err := erpcSrv.erpc.GetProject("test_ws")
	require.NoError(t, err)
	nw, err = prj.GetNetwork(ctx, "evm:123")
	require.NoError(t, err)

	require.Eventually(t, nw.intake.holdsFallback, 10*time.Second, 20*time.Millisecond,
		"the primary leads once it streamed for primaryLeadAfter")
	return primary, fallback, nw, addr
}

// fallbackHeadsLive: the fallback holds a newHeads subscription now.
func fallbackHeadsLive(nw *Network, v *mockWsVendor) bool {
	return nw.sources.pushSource("ws-fallback") && v.subscribes.Load() > v.headsUnsubscribes.Load()
}

// With failover.onDemandFallbackHeads, a fallback-tier wss source holds its
// newHeads while a primary leads, even with a client that needs every
// block; it subscribes once no primary streams (the primary's socket
// closed, or its pushed head is older than W), and lets go again once a
// primary has streamed for primaryLeadAfter.
func TestSourceReconciler_FallbackHeadsOnDemand(t *testing.T) {
	primary, fallback, nw, addr := fallbackHeadsServer(t, true)

	client := dialWs(t, addr)
	defer client.Close()
	subscribeNewHeads(t, client)
	requireContiguous(t, client, 5, 0)
	assert.Zero(t, fallback.subscribes.Load(), "no newHeads subscribe on the fallback while the primary leads")
	assert.False(t, nw.sources.pushSource("ws-fallback"), "the fallback is no push source while held")

	// The primary's node goes away: its socket closes and the reconnected
	// one pushes nothing, so no primary streams.
	primary.pushing.Store(false)
	primary.dropConns()
	require.Eventually(t, func() bool { return fallbackHeadsLive(nw, fallback) }, 10*time.Second, 20*time.Millisecond,
		"the fallback subscribes newHeads once the primary stopped streaming")

	primary.pushing.Store(true)
	require.Eventually(t, func() bool {
		return fallback.headsUnsubscribes.Load() > 0 && !fallbackHeadsLive(nw, fallback)
	}, 10*time.Second, 20*time.Millisecond, "the fallback unsubscribes once the primary leads again")
	subscribes := fallback.subscribes.Load()

	// The client is fed throughout, and the fallback stays released.
	n, _ := nextNewHead(t, client, 10*time.Second)
	requireContiguous(t, client, 3, n)
	assert.Equal(t, subscribes, fallback.subscribes.Load(), "the fallback stays released while the primary leads")
}

// Without the option, a fallback-tier wss source pushes newHeads whenever a
// client needs every block, whether or not a primary leads (spec §5.2).
func TestSourceReconciler_FallbackHeadsWithoutOption(t *testing.T) {
	_, fallback, nw, addr := fallbackHeadsServer(t, false)
	assert.Zero(t, fallback.subscribes.Load(), "no demand yet: an onDemand fallback has no subscription")

	client := dialWs(t, addr)
	defer client.Close()
	subscribeNewHeads(t, client)
	require.Eventually(t, func() bool { return fallbackHeadsLive(nw, fallback) }, 10*time.Second, 20*time.Millisecond,
		"the fallback subscribes newHeads on demand while the primary leads")
	requireContiguous(t, client, 3, 0)
}
