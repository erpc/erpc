package svm

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// svmDisabledUpstream is a scriptedUpstream whose svm.statePollerInterval is
// negative, i.e. polling is disabled. It records every Forward so a test can
// assert the poller issues none.
func svmDisabledUpstream() *scriptedUpstream {
	up := newScriptedUpstream()
	up.cfg = &common.UpstreamConfig{
		Id:       "test-poller",
		Type:     common.UpstreamTypeSvm,
		Endpoint: "http://x",
		Svm: &common.SvmUpstreamConfig{
			Cluster:             "mainnet-beta",
			StatePollerInterval: common.Duration(-time.Second),
		},
	}
	return up
}

// TestSvmStatePollerNegativeIntervalDisables: a negative statePollerInterval
// stops the poller from issuing ANY background request over several ticks, and
// the upstream still reports unknown slots + healthy so it stays selectable.
func TestSvmStatePollerNegativeIntervalDisables(t *testing.T) {
	up := svmDisabledUpstream()
	p := newPollerWithUpstream(t, up)
	require.NoError(t, p.Bootstrap(context.Background()))

	// Wait well over a handful of DefaultPollInterval (400ms) ticks.
	time.Sleep(6 * DefaultPollInterval)

	for _, key := range []string{"getSlot:processed", "getSlot:finalized", "getHealth", "getMaxShredInsertSlot", "oldestBlock"} {
		require.Zerof(t, up.callCount(key), "disabled poller must not call %s", key)
	}

	// Unknown slots (treated as "included" by every slot-lag / minContextSlot
	// filter) and healthy (never cordoned), so the upstream stays routable.
	require.Zero(t, p.LatestSlot())
	require.Zero(t, p.FinalizedSlot())
	require.True(t, p.IsHealthy(), "a never-polled upstream must read healthy, not unhealthy")

	// Slots pushed from elsewhere (e.g. live traffic context.slot) are still
	// accepted even though background polling is off.
	p.SuggestLatestSlot(12345)
	require.Equal(t, int64(12345), p.LatestSlot())
}

// TestSvmStatePollerEnabledByDefaultStillPolls is the control: with the
// interval unset (0) the poller runs and issues getSlot, proving a negative
// value — not merely the presence of the field — is what disables it.
func TestSvmStatePollerEnabledByDefaultStillPolls(t *testing.T) {
	up := newScriptedUpstream()
	up.script("getHealth", []byte(`"ok"`))
	up.script("getSlot:processed", []byte(`100`))
	up.script("getSlot:finalized", []byte(`90`))
	up.script("getMaxShredInsertSlot", []byte(`100`))
	up.script("oldestBlock", []byte(`[1]`))
	// cfg left as the embedded fake's default (no Svm.StatePollerInterval → 0).
	p := newPollerWithUpstream(t, up)
	require.NoError(t, p.Bootstrap(context.Background()))

	require.Eventually(t, func() bool { return up.callCount("getSlot:processed") > 0 },
		3*time.Second, 20*time.Millisecond, "an enabled poller must issue getSlot")
}
