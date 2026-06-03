package erpc

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

func TestFilterUpstreamsByUseUpstream(t *testing.T) {
	ups := func(ids ...string) []common.Upstream {
		out := make([]common.Upstream, len(ids))
		for i, id := range ids {
			out[i] = common.NewFakeUpstream(id)
		}
		return out
	}
	idsOf := func(us []common.Upstream) []string {
		out := make([]string, len(us))
		for i, u := range us {
			out[i] = u.Id()
		}
		return out
	}

	t.Run("empty pattern is a no-op", func(t *testing.T) {
		require.Equal(t, []string{"a", "b"}, idsOf(filterUpstreamsByUseUpstream(ups("a", "b"), "")))
	})

	t.Run("exact id narrows to one upstream", func(t *testing.T) {
		got := filterUpstreamsByUseUpstream(ups("alpha-quicknode", "alpha-nanoreth", "beta-chainstack"), "alpha-nanoreth")
		require.Equal(t, []string{"alpha-nanoreth"}, idsOf(got))
	})

	t.Run("glob pattern keeps matching upstreams in order", func(t *testing.T) {
		got := filterUpstreamsByUseUpstream(ups("a-nanoreth", "b-evm", "c-nanoreth"), "*nanoreth*")
		require.Equal(t, []string{"a-nanoreth", "c-nanoreth"}, idsOf(got))
	})

	t.Run("no match leaves the pool untouched (never empties)", func(t *testing.T) {
		require.Equal(t, []string{"a", "b"}, idsOf(filterUpstreamsByUseUpstream(ups("a", "b"), "does-not-exist")))
	})

	t.Run("empty pool is a no-op", func(t *testing.T) {
		require.Empty(t, filterUpstreamsByUseUpstream(nil, "anything"))
	})
}
