package policy

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A stateful method is not a read. eth_getFilterChanges returns the changes
// since the last poll and moves the filter's cursor, so a mirrored poll to
// the node that holds the filter takes the client's events. A mirrored
// eth_newFilter installs a filter on an upstream that nobody will poll or
// uninstall. The prober must never mirror a method that the network's method
// table marks stateful.
func TestProber_NeverMirrorsAStatefulMethod(t *testing.T) {
	for _, method := range append(append([]string{}, common.DefaultStatefulMethodNames...),
		// The method table matches without regard to case, so the gate must too.
		"ETH_GETFILTERCHANGES",
	) {
		t.Run(method, func(t *testing.T) {
			deps := &stubEngine{}
			holder := &stubProbeUpstream{id: "holder"}
			deps.setExcluded("evm:1", []common.Upstream{holder})

			p, _ := newTestProber(t, deps, &ProbeConfig{SampleRate: 1.0, MaxConcurrent: 4, Timeout: 5 * time.Second})
			defer p.Stop()

			skipped := telemetry.MetricSelectionProbeSkipped.WithLabelValues("evm:1", "stateful_method")
			before := testutil.ToFloat64(skipped)

			p.Publish(makeProbeReq(t, method))
			time.Sleep(150 * time.Millisecond)

			assert.Equal(t, int64(0), holder.calls.Load(), "%s must never be mirrored", method)
			assert.Equal(t, before+1, testutil.ToFloat64(skipped), "the skip must be counted as stateful_method")
		})
	}
}

// configuredNetwork is a Network with a config and an unknown finality,
// which is all the prober reads from a request's network.
type configuredNetwork struct {
	common.Network
	cfg *common.NetworkConfig
}

func (n *configuredNetwork) Config() *common.NetworkConfig { return n.cfg }

func (n *configuredNetwork) GetFinality(context.Context, *common.NormalizedRequest, *common.NormalizedResponse) common.DataFinalityState {
	return common.DataFinalityStateUnknown
}

// The gate reads the network's own method table, so a method an operator
// marks stateful in methods.definitions is not mirrored either.
func TestProber_NeverMirrorsAMethodTheNetworkMarksStateful(t *testing.T) {
	methods := &common.MethodsConfig{Definitions: map[string]*common.CacheMethodConfig{
		"vendor_pollQueue": {Stateful: true},
	}}
	require.NoError(t, methods.SetDefaults())

	deps := &stubEngine{}
	holder := &stubProbeUpstream{id: "holder"}
	deps.setExcluded("evm:1", []common.Upstream{holder})
	p, _ := newTestProber(t, deps, &ProbeConfig{SampleRate: 1.0, MaxConcurrent: 4, Timeout: 5 * time.Second})
	defer p.Stop()

	req := makeProbeReq(t, "vendor_pollQueue")
	req.SetNetwork(&configuredNetwork{cfg: &common.NetworkConfig{Methods: methods}})
	p.Publish(req)
	time.Sleep(150 * time.Millisecond)

	assert.Equal(t, int64(0), holder.calls.Load(), "a method the network marks stateful must never be mirrored")
}

// The gate must not stop re-admission: a read is still mirrored, or an
// excluded upstream never gets the samples it needs to heal.
func TestProber_StillMirrorsReads(t *testing.T) {
	for _, method := range []string{"eth_blockNumber", "eth_getBlockByNumber", "eth_call", "eth_getLogs", "eth_getBalance"} {
		t.Run(method, func(t *testing.T) {
			deps := &stubEngine{}
			dead := &stubProbeUpstream{id: "dead"}
			deps.setExcluded("evm:1", []common.Upstream{dead})

			p, _ := newTestProber(t, deps, &ProbeConfig{SampleRate: 1.0, MaxConcurrent: 4, Timeout: 5 * time.Second})
			defer p.Stop()

			p.Publish(makeProbeReq(t, method))
			assert.Equal(t, int64(1), waitForCalls(dead, 1, time.Second), "%s is a read and must still be mirrored", method)
		})
	}
}
