package policy_test

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// fakeUpstream is a minimal `common.Upstream` for engine smoke tests.
type fakeUpstream struct {
	id    string
	group string
}

func (f *fakeUpstream) Id() string           { return f.id }
func (f *fakeUpstream) VendorName() string   { return "test" }
func (f *fakeUpstream) NetworkId() string    { return "evm:1" }
func (f *fakeUpstream) NetworkLabel() string { return "evm:1" }
func (f *fakeUpstream) Config() *common.UpstreamConfig {
	return &common.UpstreamConfig{Id: f.id, Group: f.group}
}
func (f *fakeUpstream) Logger() *zerolog.Logger { l := zerolog.Nop(); return &l }
func (f *fakeUpstream) Vendor() common.Vendor   { return nil }
func (f *fakeUpstream) Tracker() common.HealthTracker {
	return nil
}
func (f *fakeUpstream) Forward(ctx context.Context, nq *common.NormalizedRequest, byPassMethodExclusion bool) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (f *fakeUpstream) Cordon(method, reason string)   {}
func (f *fakeUpstream) Uncordon(method, reason string) {}
func (f *fakeUpstream) IgnoreMethod(method string)     {}

// TestEngine_IdentityPolicy_PassesThrough runs the simplest possible eval —
// `(upstreams, ctx) => upstreams` — and verifies the engine returns the
// upstreams in registration order.
func TestEngine_IdentityPolicy_PassesThrough(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "test", time.Minute)

	cfg := &common.SelectionPolicyConfig{
		EvalInterval:    common.Duration(50 * time.Millisecond),
		EvalTimeout:     common.Duration(10 * time.Millisecond),
		DecisionHistory: common.Duration(time.Minute),
		// Distinct from `common.DefaultSelectionPolicySource` so the engine
		// does NOT auto-upgrade to the rich default policy.
		Eval: "(ups, _ctx) => ups",
	}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, cfg.Validate())

	engine := policy.NewEngine(ctx, &logger, "p1", tracker, nil)
	defer engine.Stop()

	ups := []common.Upstream{
		&fakeUpstream{id: "rpc1", group: "main"},
		&fakeUpstream{id: "rpc2", group: "main"},
		&fakeUpstream{id: "rpc3", group: "fallback"},
	}
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	ordered := engine.GetOrdered("evm:1", "*")
	require.Len(t, ordered, 3)
	require.Equal(t, "rpc1", ordered[0].Id())
	require.Equal(t, "rpc2", ordered[1].Id())
	require.Equal(t, "rpc3", ordered[2].Id())
}

// TestEngine_OverrideOrderForTest exercises the test-only helper that the
// retry/hedge/failsafe migration depends on (Phase 2.3).
func TestEngine_OverrideOrderForTest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "test", time.Minute)

	cfg := &common.SelectionPolicyConfig{
		EvalInterval:    common.Duration(0), // frozen: only manual ticks
		EvalTimeout:     common.Duration(10 * time.Millisecond),
		DecisionHistory: common.Duration(time.Minute),
	}
	require.NoError(t, cfg.SetDefaults())

	engine := policy.NewEngine(ctx, &logger, "p1", tracker, nil)
	defer engine.Stop()

	ups := []common.Upstream{
		&fakeUpstream{id: "rpc1"},
		&fakeUpstream{id: "rpc2"},
		&fakeUpstream{id: "rpc3"},
	}
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	policy.OverrideOrderForTest(engine, "evm:1", "rpc3", "rpc1")
	ordered := engine.GetOrdered("evm:1", "*")
	require.Len(t, ordered, 2)
	require.Equal(t, "rpc3", ordered[0].Id())
	require.Equal(t, "rpc1", ordered[1].Id())
}
