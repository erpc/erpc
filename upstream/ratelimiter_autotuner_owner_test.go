package upstream

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestClaimAutoTuner_SingleOwnerPerBudget verifies that auto-tuning ownership of a
// shared budget is single-project: the first project to claim a budget owns it,
// the same project keeps ownership across its multiple upstreams (preserving
// today's single-project behavior), and other co-resident projects are denied so
// they cannot run competing tuners against the same MaxCount.
func TestClaimAutoTuner_SingleOwnerPerBudget(t *testing.T) {
	r := &RateLimitersRegistry{}

	// First project to claim a budget owns tuning for it.
	require.True(t, r.ClaimAutoTuner("budget-a", "projA"), "first claim must succeed")

	// The SAME project claiming again (e.g. a second upstream that legally shares
	// the same budget) must still own it — this preserves single-project behavior
	// where multiple auto-tuned upstreams share one budget.
	require.True(t, r.ClaimAutoTuner("budget-a", "projA"), "same project re-claim must succeed")

	// A DIFFERENT project must NOT get a competing tuner on the same budget.
	require.False(t, r.ClaimAutoTuner("budget-a", "projB"), "other project must be denied")
	require.False(t, r.ClaimAutoTuner("budget-a", "projB"), "other project stays denied")

	// A different budget is independent — its first claimer owns it.
	require.True(t, r.ClaimAutoTuner("budget-b", "projB"))
	require.False(t, r.ClaimAutoTuner("budget-b", "projA"))

	// Empty budget id never claims.
	require.False(t, r.ClaimAutoTuner("", "projA"))
}

// TestClaimAutoTuner_ConcurrentSingleWinner hammers one budget from many projects
// concurrently and asserts exactly one project wins ownership (run with -race).
func TestClaimAutoTuner_ConcurrentSingleWinner(t *testing.T) {
	r := &RateLimitersRegistry{}
	const projects = 8
	const perProject = 64

	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := map[string]bool{}

	for p := 0; p < projects; p++ {
		pid := fmt.Sprintf("proj%d", p)
		for i := 0; i < perProject; i++ {
			wg.Add(1)
			go func(pid string) {
				defer wg.Done()
				if r.ClaimAutoTuner("hot-budget", pid) {
					mu.Lock()
					winners[pid] = true
					mu.Unlock()
				}
			}(pid)
		}
	}
	wg.Wait()

	require.Len(t, winners, 1, "exactly one project may own auto-tuning for a shared budget")
}
