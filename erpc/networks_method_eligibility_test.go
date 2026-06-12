package erpc

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilterMethodEligible(t *testing.T) {
	full1 := common.NewFakeUpstream("full-1")
	full2 := common.NewFakeUpstream("full-2")
	archive1 := common.NewFakeUpstream("archive-1")
	archive1.Config().IgnoreMethods = []string{"eth_send*"}
	archive2 := common.NewFakeUpstream("archive-2")
	archive2.Config().IgnoreMethods = []string{"*"}

	t.Run("drops ineligible upstreams preserving order", func(t *testing.T) {
		ups := []common.Upstream{archive1, full1, archive2, full2}
		eligible, dropped := filterMethodEligible(ups, "eth_sendRawTransaction")
		require.Equal(t, 2, dropped)
		require.Len(t, eligible, 2)
		assert.Equal(t, "full-1", eligible[0].Id())
		assert.Equal(t, "full-2", eligible[1].Id())
	})

	t.Run("returns input slice untouched when all are eligible", func(t *testing.T) {
		ups := []common.Upstream{full1, archive1, full2}
		eligible, dropped := filterMethodEligible(ups, "eth_getLogs")
		assert.Equal(t, 0, dropped)
		// Same backing array, no copy: the selection-policy cache slice is
		// passed through as-is on the hot path.
		assert.Equal(t, &ups[0], &eligible[0])
		assert.Len(t, eligible, 3)
	})

	t.Run("all ineligible reports full drop", func(t *testing.T) {
		ups := []common.Upstream{archive1, archive2}
		eligible, dropped := filterMethodEligible(ups, "eth_sendRawTransaction")
		assert.Equal(t, 2, dropped)
		assert.Empty(t, eligible)
		// The Forward call-site keeps the original list in this case so the
		// request still fails with the descriptive per-upstream
		// "method ignored" error rather than "no upstreams found".
	})

	t.Run("does not mutate the input slice", func(t *testing.T) {
		ups := []common.Upstream{full1, archive1, full2}
		eligible, dropped := filterMethodEligible(ups, "eth_sendRawTransaction")
		require.Equal(t, 1, dropped)
		require.Len(t, eligible, 2)
		assert.Equal(t, "full-1", ups[0].Id())
		assert.Equal(t, "archive-1", ups[1].Id())
		assert.Equal(t, "full-2", ups[2].Id())
	})
}
