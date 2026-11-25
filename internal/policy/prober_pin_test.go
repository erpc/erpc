package policy

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The mirror calls Forward with byPassMethodExclusion=true, and that skips
// Upstream.shouldSkip, which holds the X-ERPC-Use-Upstream check. So the
// prober itself must keep a pinned request on the upstreams its pin names.
func TestProber_MirrorsAPinnedRequestOnlyToThePinnedUpstream(t *testing.T) {
	const network = "evm:1"
	pinnedElsewhere := func() float64 {
		return testutil.ToFloat64(telemetry.MetricSelectionProbeSkipped.WithLabelValues(network, "pinned_elsewhere"))
	}

	deps := &stubEngine{}
	pinned := &stubProbeUpstream{id: "pin-target"}
	other := &stubProbeUpstream{id: "pin-other"}
	deps.setExcluded(network, []common.Upstream{other, pinned})
	p, _ := newTestProber(t, deps, &ProbeConfig{SampleRate: 1, MaxConcurrent: 4})
	defer p.Stop()

	before := pinnedElsewhere()
	req := makeProbeReq(t, "eth_blockNumber")
	req.SetDirectives(&common.RequestDirectives{UseUpstream: "pin-target"})
	p.Publish(req)

	require.Equal(t, int64(1), waitForCalls(pinned, 1, 5*time.Second), "the pinned upstream may still get its probe")
	// The dispatcher walks other before pinned, so other's mirror, if any,
	// has started by now.
	require.Never(t, func() bool { return other.calls.Load() > 0 }, 200*time.Millisecond, 5*time.Millisecond,
		"a request pinned to one upstream must not reach another")
	require.Eventually(t, func() bool { return pinnedElsewhere() == before+1 }, 5*time.Second, 5*time.Millisecond)
}

// An unpinned request still reaches every excluded upstream.
func TestProber_MirrorsAnUnpinnedRequestToEveryExcludedUpstream(t *testing.T) {
	const network = "evm:1"
	deps := &stubEngine{}
	a := &stubProbeUpstream{id: "nopin-a"}
	b := &stubProbeUpstream{id: "nopin-b"}
	deps.setExcluded(network, []common.Upstream{a, b})
	p, _ := newTestProber(t, deps, &ProbeConfig{SampleRate: 1, MaxConcurrent: 4})
	defer p.Stop()

	p.Publish(makeProbeReq(t, "eth_blockNumber"))
	assert.Equal(t, int64(1), waitForCalls(a, 1, 5*time.Second))
	assert.Equal(t, int64(1), waitForCalls(b, 1, 5*time.Second))
}
