package erpc

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

// newMonoGuardTestNetwork builds the minimal Network needed by
// applyMonotonicityGuard — only the logger and networkId are read by the
// guard itself. Other Network fields are deliberately left nil so the guard
// stays the only thing under test.
func newMonoGuardTestNetwork() *Network {
	logger := zerolog.Nop()
	return &Network{
		logger:    &logger,
		networkId: "evm:1234",
	}
}

func TestApplyMonotonicityGuard_NilLastReturnedReturnsResultUnchanged(t *testing.T) {
	n := newMonoGuardTestNetwork()
	got := n.applyMonotonicityGuard(42, nil, "finalized", monoGuardInputs{})
	assert.Equal(t, int64(42), got, "with no high-water mark to compare against, the result must pass through")
}

func TestApplyMonotonicityGuard_FirstCallSeedsHighWaterMark(t *testing.T) {
	n := newMonoGuardTestNetwork()
	var hwm atomic.Int64
	got := n.applyMonotonicityGuard(100, &hwm, "finalized", monoGuardInputs{})
	assert.Equal(t, int64(100), got, "the first call returns its computed result")
	assert.Equal(t, int64(100), hwm.Load(), "and seeds the high-water mark for future calls")
}

func TestApplyMonotonicityGuard_ForwardProgressBumpsHighWaterMark(t *testing.T) {
	n := newMonoGuardTestNetwork()
	var hwm atomic.Int64
	hwm.Store(100)

	got := n.applyMonotonicityGuard(150, &hwm, "finalized", monoGuardInputs{})
	assert.Equal(t, int64(150), got, "forward progress is returned as-is")
	assert.Equal(t, int64(150), hwm.Load(), "and bumps the high-water mark")
}

func TestApplyMonotonicityGuard_EqualResultIsReturnedAndDoesNotMutateHwm(t *testing.T) {
	n := newMonoGuardTestNetwork()
	var hwm atomic.Int64
	hwm.Store(100)

	got := n.applyMonotonicityGuard(100, &hwm, "finalized", monoGuardInputs{})
	assert.Equal(t, int64(100), got)
	assert.Equal(t, int64(100), hwm.Load(), "no mutation needed when result equals the existing high-water mark")
}

// The behavior change vs the previous diagnostic-only guard: pre-clamp the
// function returned `result` even when result < prev, exposing strict
// downstream consumers to a backwards step. Post-clamp we return prev
// instead, masking transient regressions (typical cause: mid-rollout slot
// divergence).
func TestApplyMonotonicityGuard_RegressionIsClampedToHighWaterMark(t *testing.T) {
	n := newMonoGuardTestNetwork()
	var hwm atomic.Int64
	hwm.Store(1000)

	got := n.applyMonotonicityGuard(950, &hwm, "finalized", monoGuardInputs{
		localMax:     950,
		primaryMax:   950,
		fallbackMax:  0,
		anyPrimaryUp: true,
		sharedVal:    900,
		sharedNil:    false,
	})
	assert.Equal(t, int64(1000), got, "regression must be clamped to the previously-returned value, not surfaced to callers")
	assert.Equal(t, int64(1000), hwm.Load(), "high-water mark stays put on regression — it is by definition already at the floor we want")
}

// The clamp is safe to apply across many concurrent callers: we never
// invent a value, and the worst case is two callers both observe the same
// pre-bump prev and one of them ends up returning a slightly stale value
// that's still >= the high-water mark, which is exactly the contract.
func TestApplyMonotonicityGuard_ConcurrentCallersAreMonotonic(t *testing.T) {
	n := newMonoGuardTestNetwork()
	var hwm atomic.Int64

	const goroutines = 32
	const callsPer = 200

	var minSeen atomic.Int64
	minSeen.Store(int64(^uint64(0) >> 1)) // math.MaxInt64

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(seed int64) {
			defer wg.Done()
			// Mix of advancing and regressing inputs. The guard must
			// surface a non-decreasing sequence of returns to each
			// caller's view.
			var localPrev int64
			for i := 0; i < callsPer; i++ {
				// Pseudo-random walk: half the calls advance, half regress.
				var input int64
				if (seed+int64(i))%2 == 0 {
					input = seed*1000 + int64(i)
				} else {
					input = seed*1000 - int64(i)
				}
				got := n.applyMonotonicityGuard(input, &hwm, "finalized", monoGuardInputs{})
				if got < localPrev {
					// Critical invariant: the guard must never return a
					// value lower than this caller's previous observation.
					assert.GreaterOrEqualf(t, got, localPrev, "regression observed by goroutine seed=%d at i=%d: prev=%d got=%d", seed, i, localPrev, got)
				}
				if got > localPrev {
					localPrev = got
				}
			}
		}(int64(g + 1))
	}
	wg.Wait()
}
