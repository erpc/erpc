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

// removeCordoned() must see a cordon whatever slot grain the network runs
// at: a wildcard cordon (operator or automatic) shadows every method slot,
// and a method-scoped cordon shadows only its own. The lookup is by
// upstream, not by the slot's metrics bucket, so it applies before any
// traffic populated that bucket.
func TestRemoveCordoned_HonorsWildcardAndMethodCordonsAtEveryScope(t *testing.T) {
	for _, scope := range []common.EvalScope{
		common.EvalScopeNetwork,
		common.EvalScopeNetworkMethod,
		common.EvalScopeNetworkFinality,
		common.EvalScopeNetworkMethodFinality,
	} {
		t.Run(string(scope), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			logger := zerolog.Nop()
			tracker := health.NewTracker(&logger, "p1", time.Minute)
			engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install, nil)
			defer engine.Stop()
			ups := mkUpsWithTags([]struct {
				id     string
				vendor string
				tags   []string
			}{
				{id: "operator-wild", vendor: "v1"},
				{id: "operator-method", vendor: "v2"},
				{id: "auto-wild", vendor: "v3"},
				{id: "healthy", vendor: "v4"},
			})
			cfg := &common.SelectionPolicyConfig{
				EvalInterval: 0,
				EvalTimeout:  common.Duration(100 * time.Millisecond),
				EvalScope:    scope,
				EvalFunc:     `(upstreams) => upstreams.removeCordoned()`,
			}
			require.NoError(t, cfg.SetDefaults())
			require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

			byId := map[string]common.Upstream{}
			for _, u := range ups {
				byId[u.Id()] = u
			}
			tracker.SetOperatorCordon(byId["operator-wild"], "incident", 1)
			tracker.Cordon(byId["operator-method"], "eth_getLogs", "slow logs")
			tracker.Cordon(byId["auto-wild"], "*", "consensus sit-out")

			// excluded = upstreams the slot's routing output no longer contains.
			excluded := func(method, finality string) map[string]bool {
				engine.GetOrdered("evm:1", method, finality) // lazy-create the slot
				policy.TickForTestAtScope(engine, "evm:1", method, finality)
				out := map[string]bool{}
				for _, u := range ups {
					out[u.Id()] = true
				}
				for _, u := range engine.GetOrdered("evm:1", method, finality) {
					delete(out, u.Id())
				}
				return out
			}

			call := excluded("eth_call", "finalized")
			require.True(t, call["operator-wild"], "wildcard operator cordon applies to eth_call")
			require.True(t, call["auto-wild"], "wildcard automatic cordon applies to eth_call")
			require.False(t, call["operator-method"], "eth_getLogs cordon does not touch eth_call")
			require.False(t, call["healthy"])

			logs := excluded("eth_getLogs", "unfinalized")
			require.True(t, logs["operator-wild"])
			require.True(t, logs["auto-wild"])
			if scope == common.EvalScopeNetworkMethod || scope == common.EvalScopeNetworkMethodFinality {
				require.True(t, logs["operator-method"], "method-scoped cordon applies on its own method slot")
			}
			require.False(t, logs["healthy"])
		})
	}
}
