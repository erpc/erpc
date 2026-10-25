package wsclient

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func add(a *Adapter, id string, kind EventKind) error {
	_, _, err := a.AddSubscription(id, "evm:1", kind, 10, func() {})
	return err
}

func TestAdapter_AddAfterDrainIsRefused(t *testing.T) {
	a := New()
	require.NoError(t, add(a, "s1", KindLog))

	removed := a.Drain()
	require.Len(t, removed, 1)
	assert.Equal(t, "s1", removed[0].ClientSubID)

	assert.ErrorIs(t, add(a, "s2", KindLog), ErrClosed)
	assert.Equal(t, 0, a.Count())
}

// A sub removed by RemoveSubscription is not returned by a later Drain, so
// it is released exactly once.
func TestAdapter_RemoveAndDrainAreExclusive(t *testing.T) {
	a := New()
	require.NoError(t, add(a, "s1", KindLog))
	require.NoError(t, add(a, "s2", KindLog))

	_, existed := a.RemoveSubscription("s1")
	require.True(t, existed)
	_, existed = a.RemoveSubscription("s1")
	assert.False(t, existed)

	removed := a.Drain()
	require.Len(t, removed, 1)
	assert.Equal(t, "s2", removed[0].ClientSubID)
}

func TestAdapter_SubscriptionLimitIsAtomic(t *testing.T) {
	a := New()
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if add(a, fmt.Sprintf("s%d", i), KindNewHead) == nil {
				ok.Add(1)
			}
		}(i)
	}
	wg.Wait()
	assert.EqualValues(t, 10, ok.Load())
	assert.Equal(t, 10, a.Count())
	a.Drain()
}
