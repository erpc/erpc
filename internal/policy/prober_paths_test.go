package policy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyProbeErr(t *testing.T) {
	cause := errors.New("cause")
	tests := []struct {
		err  error
		want string
	}{
		{nil, "ok"},
		{common.NewErrUpstreamRequestSkipped(cause, "u"), "skipped"},
		{common.NewErrEndpointRequestTimeout(time.Second, cause), "timeout"},
		{common.NewErrEndpointUnauthorized(cause), "auth"},
		{common.NewErrEndpointCapacityExceeded(cause), "throttled"},
		{cause, "error"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyProbeErr(tc.err))
		})
	}
}

// failingProbeUpstream returns a fixed error from every probe.
type failingProbeUpstream struct {
	stubProbeUpstream
	err error
}

func (f *failingProbeUpstream) Forward(ctx context.Context, nq *common.NormalizedRequest, byPass, isHedge bool) (*common.NormalizedResponse, error) {
	f.calls.Add(1)
	return nil, f.err
}

func TestProber_FailedProbeIsRecordedAsError(t *testing.T) {
	const network = "evm:1"
	deps := &stubEngine{}
	bad := &failingProbeUpstream{stubProbeUpstream: stubProbeUpstream{id: "cov-bad"}, err: common.NewErrEndpointCapacityExceeded(errors.New("429"))}
	deps.setExcluded(network, []common.Upstream{bad})
	p, _ := newTestProber(t, deps, &ProbeConfig{SampleRate: 1, MaxConcurrent: 1})
	defer p.Stop()

	errs := func() float64 {
		return testutil.ToFloat64(telemetry.MetricSelectionProbeErrors.WithLabelValues(network, "cov-bad", "eth_chainId", "throttled"))
	}
	before := errs()
	p.Publish(makeProbeReq(t, "eth_chainId"))
	require.Eventually(t, func() bool { return errs() == before+1 }, 5*time.Second, 5*time.Millisecond)
}

func TestProber_SkipGates(t *testing.T) {
	const network = "evm:1"
	skipped := func(reason string) float64 {
		return testutil.ToFloat64(telemetry.MetricSelectionProbeSkipped.WithLabelValues(network, reason))
	}

	t.Run("request without method", func(t *testing.T) {
		deps := &stubEngine{}
		up := &stubProbeUpstream{id: "cov-nomethod"}
		deps.setExcluded(network, []common.Upstream{up})
		p, _ := newTestProber(t, deps, &ProbeConfig{SampleRate: 1})
		defer p.Stop()
		before := skipped("no_method")
		p.Publish(common.NewNormalizedRequest([]byte(`not json`)))
		require.Eventually(t, func() bool { return skipped("no_method") == before+1 }, 5*time.Second, 5*time.Millisecond)
		assert.Equal(t, int64(0), up.calls.Load())
	})

	t.Run("zero sample rate drops everything above the floor", func(t *testing.T) {
		deps := &stubEngine{}
		up := &stubProbeUpstream{id: "cov-sampled"}
		deps.setExcluded(network, []common.Upstream{up})
		p, _ := newTestProber(t, deps, &ProbeConfig{SampleRate: 0, MinSamples: 0})
		defer p.Stop()
		before := skipped("sampled_out")
		p.Publish(makeProbeReq(t, "eth_blockNumber"))
		require.Eventually(t, func() bool { return skipped("sampled_out") == before+1 }, 5*time.Second, 5*time.Millisecond)
		assert.Equal(t, int64(0), up.calls.Load())
	})

	t.Run("upstream ignoreMethods veto the probe; a glob error does not", func(t *testing.T) {
		deps := &stubEngine{}
		ignoring := &stubProbeUpstream{id: "cov-ignored", ignored: []string{"txpool_content"}}
		broken := &stubProbeUpstream{id: "cov-badglob", handleErr: errors.New("bad glob")}
		deps.setExcluded(network, []common.Upstream{ignoring, broken})
		p, _ := newTestProber(t, deps, &ProbeConfig{SampleRate: 1})
		defer p.Stop()
		before := skipped("method_ignored")
		p.Publish(makeProbeReq(t, "txpool_content"))
		require.Eventually(t, func() bool { return skipped("method_ignored") == before+1 }, 5*time.Second, 5*time.Millisecond)
		require.Eventually(t, func() bool { return broken.calls.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
		assert.Equal(t, int64(0), ignoring.calls.Load(), "a method the operator forbids is never probed")

		p.Publish(makeProbeReq(t, "eth_blockNumber"))
		require.Eventually(t, func() bool { return ignoring.calls.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	})

	t.Run("nil config and empty excluded set are no-ops", func(t *testing.T) {
		deps := &stubEngine{}
		up := &stubProbeUpstream{id: "cov-noop"}
		p, _ := newTestProber(t, deps, nil)
		defer p.Stop()
		assert.Nil(t, p.Config())
		p.onRequest(context.Background(), makeProbeReq(t, "eth_blockNumber"))
		p.UpdateConfig(&ProbeConfig{SampleRate: 1})
		p.onRequest(context.Background(), makeProbeReq(t, "eth_blockNumber"))
		assert.False(t, p.tryReserveProbe(nil, p.Config()))
		assert.Equal(t, int64(0), up.calls.Load())
	})
}

func TestProber_StoppedProberIgnoresPublishAndStopIsIdempotent(t *testing.T) {
	deps := &stubEngine{}
	up := &stubProbeUpstream{id: "cov-stopped"}
	deps.setExcluded("evm:1", []common.Upstream{up})
	p, _ := newTestProber(t, deps, &ProbeConfig{SampleRate: 1})
	p.Stop()
	p.Stop()
	p.Publish(makeProbeReq(t, "eth_blockNumber"))
	assert.Empty(t, p.feed)
	assert.Equal(t, int64(0), up.calls.Load())
}

func TestProber_WindowCounterDefaultsAndCounts(t *testing.T) {
	p, _ := newTestProber(t, &stubEngine{}, &ProbeConfig{})
	defer p.Stop()
	assert.Equal(t, int64(0), p.windowCount("u", 0))
	p.recordWindowSample("u", 0)
	p.recordWindowSample("u", time.Minute)
	assert.Equal(t, int64(2), p.windowCount("u", 0), "non-positive window defaults to one minute")
	assert.Equal(t, int64(0), p.windowCount("other", time.Minute))
}
