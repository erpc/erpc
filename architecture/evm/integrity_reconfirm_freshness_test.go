package evm

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// capturingNetwork records the directives of every forwarded aux fetch and
// answers with a fixed block header.
type capturingNetwork struct {
	mockNetwork
	mu    sync.Mutex
	dirs  []*common.RequestDirectives
	hash  string
	num   string
	calls int
	// sourcesVote is the intake's predicate: while false no source vote counts.
	sourcesVote atomic.Bool
}

func (n *capturingNetwork) EvmSourceVotes(string) bool { return n.sourcesVote.Load() }

func newCapturingNetwork(t *testing.T, number int64, hash string) *capturingNetwork {
	t.Helper()
	n := &capturingNetwork{hash: hash, num: fmt.Sprintf("0x%x", number)}
	n.sourcesVote.Store(true)
	n.On("Forward", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			n.mu.Lock()
			n.dirs = append(n.dirs, req.Directives())
			n.calls++
			n.mu.Unlock()
			body := fmt.Sprintf(`{"number":"%s","hash":"%s","parentHash":"%s"}`, n.num, n.hash, testHash("f", uint64(number-1)))
			return common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`"1"`), []byte(body), nil),
			), nil
		},
		nil,
	).Maybe()
	n.On("Id").Return("evm:1").Maybe()
	n.On("ProjectId").Return("test").Maybe() // aux metric labels
	return n
}

func (n *capturingNetwork) lastDirectives() *common.RequestDirectives {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.dirs) == 0 {
		return nil
	}
	return n.dirs[len(n.dirs)-1]
}

// A pin re-confirmation exists to answer ONE question: "is the pin I am holding
// still what the chain says?" Answering it from the shared cache is circular —
// the cached entry at that height is exactly what seeded the pin, so a stale pin
// re-confirms itself forever. That is not theoretical: unfinalized blocks are
// cached with no TTL, so after a reorg the orphan stays cached at its height and
// every re-confirmation replays it, upgrading a routine reorg into a hard
// rejection of honest data (mainnet, six times over 2026-07-22..29 — every one
// adjudicated a false positive against settled canonical).
func TestReconfirmPinFetchesFreshRatherThanReplayingTheCache(t *testing.T) {
	const height = int64(0x10)
	orphan, canonical := testHash("0", uint64(height)), testHash("c", uint64(height))

	// pinnedOnOrphan pins the losing fork at height and height+1 by a source
	// vote, then lets every source stop counting (none live).
	pinnedOnOrphan := func(t *testing.T, net *capturingNetwork) *ChainTracker {
		c := newChainTracker(t.Context(), net, 32, "", "", nil, newFetchScheduler(t.Context(), net))
		voteChain(t, c, "a", "0", uint64(height), uint64(height)+1, testHash("f", uint64(height-1)))
		net.sourcesVote.Store(false)
		c.lastCountedVote.Store(0)
		return c
	}

	t.Run("the re-confirmation bypasses the cache", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := pinnedOnOrphan(t, net)

		hash, status := c.ReconfirmPin(context.Background(), height)

		require.Equal(t, integrity.PinFresh, status)
		assert.Equal(t, canonical, hash, "must report what the chain says now, not the pin")
		d := net.lastDirectives()
		require.NotNil(t, d)
		assert.Equal(t, "true", d.SkipCacheRead,
			"a pin re-confirmation that reads the cache is an echo of the disputed value, not corroboration")
	})

	t.Run("with no counted source, the fresh answer is the ask vote and moves the pin", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := pinnedOnOrphan(t, net)

		_, status := c.ReconfirmPin(context.Background(), height)
		require.Equal(t, integrity.PinFresh, status)

		got, ok := c.HashAt(height)
		require.True(t, ok)
		assert.Equal(t, canonical, got, "the pin must adopt the fork the network actually serves")
		_, stillThere := c.HashAt(height + 1)
		assert.False(t, stillThere, "descendants of the abandoned fork must leave the pins")
	})

	t.Run("a counted source vote keeps its pin: never PinFresh for another hash", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := pinnedOnOrphan(t, net)
		net.sourcesVote.Store(true) // the source counts, but voted long ago

		hash, status := c.ReconfirmPin(context.Background(), height)
		assert.Equal(t, integrity.PinRateLimited, status)
		assert.Equal(t, orphan, hash)
	})

	t.Run("a source that voted within the live bound answers without a fetch", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := newChainTracker(t.Context(), net, 32, "", "", nil, newFetchScheduler(t.Context(), net))
		voteChain(t, c, "a", "0", uint64(height), uint64(height), testHash("f", uint64(height-1)))

		hash, status := c.ReconfirmPin(context.Background(), height)
		assert.Equal(t, integrity.PinFresh, status)
		assert.Equal(t, orphan, hash)
		assert.Zero(t, net.calls)
	})

	// Only by-hash fetches read the cache (spec §7): a by-number answer from
	// the cache is not a fresh view of the chain at that height.
	t.Run("by-number corroboration fetches skip the cache", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := newChainTracker(t.Context(), net, 32, "", "", nil, newFetchScheduler(t.Context(), net))

		_, ok := c.headerByNumber(context.Background(), height, fmt.Sprintf("0x%x", height))
		require.True(t, ok)
		d := net.lastDirectives()
		require.NotNil(t, d)
		assert.Equal(t, "true", d.SkipCacheRead, "a by-number tracker fetch must reach upstreams")
	})

	t.Run("by-hash header fetches still use the cache", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := newChainTracker(t.Context(), net, 32, "", "", nil, newFetchScheduler(t.Context(), net))

		_, _ = c.headerByHash(context.Background(), canonical)
		if d := net.lastDirectives(); d != nil {
			assert.Empty(t, d.SkipCacheRead,
				"a header is content-addressed by hash, so the cache can never be stale for it")
		}
	})
}
