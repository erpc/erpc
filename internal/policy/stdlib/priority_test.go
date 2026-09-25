package stdlib_test

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/internal/policy/stdlib"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func prioUp(id string, prio *int, tags ...string) *fakeUpstream {
	return &fakeUpstream{id: id, vendor: "v" + id, tags: tags, routing: &common.UpstreamRoutingConfig{Priority: prio}}
}

func intp(v int) *int { return &v }

func runPolicy(t *testing.T, eval string, ups []common.Upstream) []string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	cfg := &common.SelectionPolicyConfig{EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install, nil)
	defer engine.Stop()
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))
	policy.TickForTest(engine, "evm:1", "*")
	return ids(engine.GetOrdered("evm:1", "*", "*"))
}

// With distinct priorities, the default policy must keep every tier
// (including tier:fallback-tagged upstreams) so the request path can
// fail over within the same request.
func TestDefaultPolicy_PriorityTiersKeepAllTiers(t *testing.T) {
	ups := []common.Upstream{
		prioUp("cheap", intp(0), "tier:main"),
		prioUp("paid", intp(10), "tier:fallback"),
	}
	got := runPolicy(t, policy.DefaultPolicySource(), ups)
	require.ElementsMatch(t, []string{"cheap", "paid"}, got)
}

// Without priorities (or with all-equal priorities) the default policy's
// tier:fallback split is unchanged.
func TestDefaultPolicy_EqualPrioritiesKeepTierSplit(t *testing.T) {
	for _, p := range []*int{nil, intp(3)} {
		ups := []common.Upstream{
			prioUp("main", p, "tier:main"),
			prioUp("fb", p, "tier:fallback"),
		}
		require.Equal(t, []string{"main"}, runPolicy(t, policy.DefaultPolicySource(), ups))
	}
}

func TestEval_ExposesPriority(t *testing.T) {
	ups := []common.Upstream{prioUp("a", intp(5)), prioUp("b", nil), prioUp("c", intp(1))}
	got := runPolicy(t, `(us) => us.sortBy(u => u.priority)`, ups)
	require.Equal(t, []string{"b", "c", "a"}, got)
}
