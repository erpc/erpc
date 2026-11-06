package svm

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

// skipStatePoller stops the poller from sending any background request, and
// the upstream still reports unknown slots and healthy, so it stays routable.
func TestSvmStatePollerSkip(t *testing.T) {
	up := newScriptedUpstream()
	up.cfg = &common.UpstreamConfig{
		Id:       "test-poller",
		Type:     common.UpstreamTypeSvm,
		Endpoint: "http://x",
		Svm:      &common.SvmUpstreamConfig{Cluster: "mainnet-beta", SkipStatePoller: util.BoolPtr(true)},
	}
	p := newPollerWithUpstream(t, up)
	require.NoError(t, p.Bootstrap(context.Background()))

	time.Sleep(6 * DefaultPollInterval)

	for _, key := range []string{"getSlot:processed", "getSlot:finalized", "getHealth", "getMaxShredInsertSlot", "oldestBlock"} {
		require.Zerof(t, up.callCount(key), "a skipped poller must not call %s", key)
	}
	require.Zero(t, p.LatestSlot())
	require.Zero(t, p.FinalizedSlot())
	require.True(t, p.IsHealthy(), "a never-polled upstream must read healthy")

	p.SuggestLatestSlot(12345)
	require.Equal(t, int64(12345), p.LatestSlot())
}

// The control: without the option the same upstream is polled.
func TestSvmStatePollerPollsByDefault(t *testing.T) {
	up := newScriptedUpstream()
	up.script("getHealth", []byte(`"ok"`))
	up.script("getSlot:processed", []byte(`100`))
	up.script("getSlot:finalized", []byte(`90`))
	up.script("getMaxShredInsertSlot", []byte(`100`))
	up.script("oldestBlock", []byte(`[1]`))
	p := newPollerWithUpstream(t, up)
	require.NoError(t, p.Bootstrap(context.Background()))

	require.Eventually(t, func() bool { return up.callCount("getSlot:processed") > 0 },
		3*time.Second, 20*time.Millisecond, "an enabled poller must issue getSlot")
}

func TestSvmSkipStatePollerInheritsFromUpstreamDefaults(t *testing.T) {
	u := &common.UpstreamConfig{Svm: &common.SvmUpstreamConfig{Cluster: "mainnet-beta"}}
	require.NoError(t, u.ApplyDefaults(&common.UpstreamConfig{Svm: &common.SvmUpstreamConfig{SkipStatePoller: util.BoolPtr(true)}}))
	require.True(t, *u.Svm.SkipStatePoller)
}
