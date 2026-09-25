package common

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type prioFakeUpstream struct {
	Upstream
	id  string
	cfg *UpstreamConfig
}

func (f *prioFakeUpstream) Id() string              { return f.id }
func (f *prioFakeUpstream) Config() *UpstreamConfig { return f.cfg }

func pu(id string, prio *int) Upstream {
	cfg := &UpstreamConfig{Id: id}
	if prio != nil {
		cfg.Routing = &UpstreamRoutingConfig{Priority: prio}
	}
	return &prioFakeUpstream{id: id, cfg: cfg}
}

func ip(v int) *int { return &v }

func idsOf(ups []Upstream) []string {
	out := make([]string, len(ups))
	for i, u := range ups {
		out[i] = u.Id()
	}
	return out
}

func TestSortUpstreamsByPriority(t *testing.T) {
	t.Run("stable, lower first, nil=0, input untouched", func(t *testing.T) {
		// Input order models the policy's score order: paid scored best.
		in := []Upstream{pu("paidFast", ip(10)), pu("cheapB", nil), pu("mid", ip(5)), pu("cheapA", ip(0))}
		out := SortUpstreamsByPriority(in)
		require.Equal(t, []string{"cheapB", "cheapA", "mid", "paidFast"}, idsOf(out),
			"priority beats score; score order kept within a tier")
		require.Equal(t, []string{"paidFast", "cheapB", "mid", "cheapA"}, idsOf(in), "input not mutated")
	})
	t.Run("no priorities returns identical slice", func(t *testing.T) {
		in := []Upstream{pu("b", nil), pu("a", nil)}
		out := SortUpstreamsByPriority(in)
		require.Equal(t, &in[0], &out[0], "no copy, exact same ordering")
	})
	t.Run("all equal is no-op", func(t *testing.T) {
		in := []Upstream{pu("b", ip(2)), pu("a", ip(2))}
		require.Equal(t, []string{"b", "a"}, idsOf(SortUpstreamsByPriority(in)))
	})
	t.Run("explicit zero equals unset", func(t *testing.T) {
		require.False(t, HasPriorityTiers([]Upstream{pu("a", ip(0)), pu("b", nil)}))
	})
}

func TestHedgeTierFilter(t *testing.T) {
	require.Nil(t, HedgeTierFilter([]Upstream{pu("a", nil), pu("b", nil)}))
	f := HedgeTierFilter([]Upstream{pu("c1", ip(1)), pu("c2", ip(1)), pu("p", ip(9))})
	require.True(t, f(pu("c2", ip(1))))
	require.False(t, f(pu("p", ip(9))))
	require.True(t, IsHedgeLeg(WithHedgeLeg(context.Background())))
	require.False(t, IsHedgeLeg(context.Background()))
}

func TestNextUpstreamMatching_SkipsRejected(t *testing.T) {
	r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId"}`))
	ups := []Upstream{pu("c", ip(0)), pu("p", ip(5))}
	r.SetUpstreams(ups)
	f := HedgeTierFilter(ups)
	u, err := r.NextUpstreamMatching(f)
	require.NoError(t, err)
	require.Equal(t, "c", u.Id())
	_, err = r.NextUpstreamMatching(f)
	require.Error(t, err, "hedge must not reach the paid tier")
	u, err = r.NextUpstream()
	require.NoError(t, err)
	require.Equal(t, "p", u.Id(), "sequential failover still reaches paid")
}

func TestUpstreamConfigValidate_NegativePriority(t *testing.T) {
	u := &UpstreamConfig{Endpoint: "http://x", Routing: &UpstreamRoutingConfig{Priority: ip(-1)}}
	require.ErrorContains(t, u.Validate(&Config{}, false), "routing.priority")
	u.Routing.Priority = ip(0)
	require.NoError(t, u.Validate(&Config{}, false))
}
