package policy_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// TestEngine_EvalTimeout_IsTickErrorAndRaceFree: an eval that exceeds
// evalTimeout must be recorded as a timeout tick error (previous cache
// retained), even though the eval goroutine is allowed to run to completion.
// Regression: the timeout branch and the eval goroutine both wrote the same
// error variable (a data race under -race), and the goroutine's later nil
// write turned the timed-out eval into a published success.
func TestEngine_EvalTimeout_IsTickErrorAndRaceFree(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "test", time.Minute)

	// Busy-waits well past evalTimeout, then drops rpc2. If the late result
	// were published, the cache would be [rpc1].
	eval := `(ups, _ctx) => { const s = Date.now(); while (Date.now() - s < 60) {} return ups.filter(u => u.id === "rpc1"); }`
	cfg := &common.SelectionPolicyConfig{
		EvalInterval: common.Duration(time.Hour), // only explicit ticks
		EvalTimeout:  common.Duration(5 * time.Millisecond),
		EvalFunc:     eval,
	}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, cfg.Validate())

	engine := policy.NewEngine(ctx, &logger, "p1", tracker, nil, nil)
	defer engine.Stop()

	ups := []common.Upstream{&fakeUpstream{id: "rpc1"}, &fakeUpstream{id: "rpc2"}}
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))
	policy.TickForTest(engine, "evm:1", "*")

	decisions := engine.RecentDecisions("evm:1", "*", "*", 0)
	require.NotEmpty(t, decisions)
	for _, d := range decisions {
		require.True(t, strings.Contains(d.Error, "timed out"),
			"every tick exceeded evalTimeout and must be a timeout error, got %q", d.Error)
	}
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.NotEqual(t, []string{"rpc1"}, got, "a timed-out eval result must not be published")
}
