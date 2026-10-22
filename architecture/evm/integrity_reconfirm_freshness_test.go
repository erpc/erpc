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

// capturingNetwork records the directives of every forwarded fetch by its
// block ref and answers with a fixed block header.
type capturingNetwork struct {
	mockNetwork
	mu   sync.Mutex
	dirs map[string][]*common.RequestDirectives // block ref -> directives, in order
	hash string
	num  string
	// sourcesVote is the intake's predicate: while false no source vote counts.
	sourcesVote atomic.Bool
}

func (n *capturingNetwork) EvmSourceVotes(string, string) bool { return n.sourcesVote.Load() }

func newCapturingNetwork(t *testing.T, number int64, hash string) *capturingNetwork {
	t.Helper()
	n := &capturingNetwork{hash: hash, num: fmt.Sprintf("0x%x", number), dirs: map[string][]*common.RequestDirectives{}}
	n.sourcesVote.Store(true)
	n.On("Forward", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			jrq, _ := req.JsonRpcRequest()
			ref, _ := jrq.Params[0].(string)
			n.mu.Lock()
			n.dirs[ref] = append(n.dirs[ref], req.Directives())
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

// directivesFor is the directives of the last fetch for ref; walks and fill
// fetch other refs in the background.
func (n *capturingNetwork) directivesFor(ref string) *common.RequestDirectives {
	n.mu.Lock()
	defer n.mu.Unlock()
	ds := n.dirs[ref]
	if len(ds) == 0 {
		return nil
	}
	return ds[len(ds)-1]
}

// calls is every fetch so far, of any ref.
func (n *capturingNetwork) calls() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	total := 0
	for _, ds := range n.dirs {
		total += len(ds)
	}
	return total
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
		c := newChainTracker(t.Context(), net, 32, 0, "", "", nil, newFetchScheduler(t.Context(), net))
		voteChain(t, c, "a", "0", uint64(height), uint64(height)+1, testHash("f", uint64(height-1)))
		net.sourcesVote.Store(false)
		return c
	}

	t.Run("the re-confirmation bypasses the cache", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := pinnedOnOrphan(t, net)

		hash, status := c.ReconfirmPin(context.Background(), height)

		require.Equal(t, integrity.PinFresh, status)
		assert.Equal(t, canonical, hash, "must report what the chain says now, not the pin")
		d := net.directivesFor("latest")
		require.NotNil(t, d)
		assert.Equal(t, "true", d.SkipCacheRead,
			"a pin re-confirmation that reads the cache is an echo of the disputed value, not corroboration")
	})

	t.Run("with no counted source, the settle answer moves the pin", func(t *testing.T) {
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

	t.Run("a pin the counted votes of 2 sources back answers without a fetch", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := newChainTracker(t.Context(), net, 32, 0, "", "", nil, newFetchScheduler(t.Context(), net))
		// From genesis, so fill has nothing to fetch: any fetch is the
		// re-confirmation's.
		voteChain(t, c, "a", "0", 0, uint64(height), testHash("f", 0))
		voteChain(t, c, "b", "0", 0, uint64(height), testHash("f", 0))

		hash, status := c.ReconfirmPin(context.Background(), height)
		assert.Equal(t, integrity.PinFresh, status)
		assert.Equal(t, orphan, hash)
		c.sync(context.Background())
		assert.Zero(t, net.calls())
	})

	// One source's vote alone never licenses a hard reject: the pin it backs
	// settles first.
	t.Run("a pin one source's vote backs settles before it is fresh", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := newChainTracker(t.Context(), net, 32, 0, "", "", nil, newFetchScheduler(t.Context(), net))
		voteChain(t, c, "a", "0", 0, uint64(height), testHash("f", 0))

		_, status := c.ReconfirmPin(context.Background(), height)
		assert.Equal(t, integrity.PinRateLimited, status, "a settle answer for another hash ties with the one vote")
		assert.NotNil(t, net.directivesFor("latest"), "the re-confirmation settled")
	})

	// A by-number answer from the cache is not a fresh view of the chain at
	// that height, unless the height is finalized: such a block cannot change,
	// and forcing those reads to upstreams once collapsed the corroboration
	// cache hit-rate.
	byNumber := func(t *testing.T, finalized int64) *common.RequestDirectives {
		net := newCapturingNetwork(t, height, canonical)
		c := newChainTracker(t.Context(), net, 32, 0, "", "", func() int64 { return finalized }, newFetchScheduler(t.Context(), net))
		_, ok := c.headerByNumber(context.Background(), height, fmt.Sprintf("0x%x", height))
		require.True(t, ok)
		d := net.directivesFor(fmt.Sprintf("0x%x", height))
		require.NotNil(t, d)
		return d
	}

	t.Run("by-number corroboration fetches above the finalized height skip the cache", func(t *testing.T) {
		assert.Equal(t, "true", byNumber(t, height-1).SkipCacheRead, "a by-number tracker fetch must reach upstreams")
	})

	t.Run("by-number corroboration fetches at the finalized height use the cache", func(t *testing.T) {
		assert.Empty(t, byNumber(t, height).SkipCacheRead, "a finalized block cannot change, so the cache serves it")
	})

	t.Run("a re-confirmation at a finalized height still bypasses the cache", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := newChainTracker(t.Context(), net, 32, 0, "", "", func() int64 { return height }, newFetchScheduler(t.Context(), net))
		voteChain(t, c, "a", "0", uint64(height), uint64(height)+1, testHash("f", uint64(height-1)))
		net.sourcesVote.Store(false)

		_, status := c.ReconfirmPin(context.Background(), height)
		require.Equal(t, integrity.PinFresh, status)
		d := net.directivesFor("latest")
		require.NotNil(t, d)
		assert.Equal(t, "true", d.SkipCacheRead)
	})

	t.Run("by-hash header fetches still use the cache", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := newChainTracker(t.Context(), net, 32, 0, "", "", nil, newFetchScheduler(t.Context(), net))

		_, _ = c.headerByHash(context.Background(), canonical)
		if d := net.directivesFor(canonical); d != nil {
			assert.Empty(t, d.SkipCacheRead,
				"a header is content-addressed by hash, so the cache can never be stale for it")
		}
	})
}
