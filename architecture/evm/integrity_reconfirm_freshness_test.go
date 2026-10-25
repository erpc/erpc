package evm

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// capturingNetwork records the directives of every forwarded fetch by its
// block ref and answers with a fixed block header; its head Asks answer
// with the same header, the way a node's latest would.
type capturingNetwork struct {
	mockNetwork
	mu   sync.Mutex
	dirs map[string][]*common.RequestDirectives // block ref -> directives, in order
	asks []common.HeadNeed                      // every head Ask's need, in order
	hash string
	num  string
	// sourcesVote is the intake's predicate: while false no source is a
	// hash voter; the voters are sources a and b, of two vendors.
	sourcesVote atomic.Bool
	// backoff: every head Ask backs off until an hour from now; hold: every
	// head Ask waits for its ctx.
	backoff, hold atomic.Bool
}

func (n *capturingNetwork) EvmHashVoters(string, time.Duration, bool) HashVoters {
	hv := HashVoters{Voters: []string{"a", "b"}, Need: 1, NetworkVendors: 2,
		Vendors: map[string]string{"a": "vendor-a", "b": "vendor-b"}}
	if n.sourcesVote.Load() {
		hv.Hash, hv.Need = []string{"a", "b"}, 2
	}
	return hv
}

func (n *capturingNetwork) EvmAskHead(ctx context.Context, _ string, need common.HeadNeed, _ common.AskReason) common.HeadAnswer {
	n.mu.Lock()
	n.asks = append(n.asks, need)
	n.mu.Unlock()
	switch {
	case n.backoff.Load():
		return common.HeadAnswer{Outcome: common.AskBackoff, Next: time.Now().Add(time.Hour)}
	case n.hold.Load():
		<-ctx.Done()
		return common.HeadAnswer{Outcome: common.AskFetched, Err: ctx.Err()}
	}
	sent := time.Now()
	return common.HeadAnswer{Outcome: common.AskFetched, Sent: sent, Next: sent.Add(time.Hour),
		Header: []byte(fmt.Sprintf(`{"number":"%s","hash":"%s","parentHash":"%s"}`, n.num, n.hash, testHash("f", n.number()-1)))}
}

func (n *capturingNetwork) number() uint64 {
	v, _ := common.HexToInt64(n.num)
	return uint64(v)
}

// askedNeeds is the need of every head Ask so far.
func (n *capturingNetwork) askedNeeds() []common.HeadNeed {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.asks)
}

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
	// No served tip here: latest never caps the delivery gate.
	n.On("EvmHighestLatestBlockNumber", mock.Anything).Return(int64(1) << 62).Maybe()
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
		c := newChainTracker(t.Context(), net, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), net))
		voteChain(t, c, "a", "0", uint64(height), uint64(height)+1, testHash("f", uint64(height-1)))
		net.sourcesVote.Store(false)
		return c
	}

	t.Run("the re-confirmation Asks for a fresh fetch", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := pinnedOnOrphan(t, net)

		hash, status := c.ReconfirmPin(context.Background(), height)

		require.Equal(t, integrity.PinFresh, status)
		assert.Equal(t, canonical, hash, "must report what the chain says now, not the pin")
		asked := net.askedNeeds()
		require.NotEmpty(t, asked)
		for _, need := range asked {
			assert.Equal(t, common.HeadNeedHash, need,
				"only a local fetch, cache reads skipped, meets a hash need: a cached answer is an echo of the disputed value, not corroboration")
		}
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
		c := newChainTracker(t.Context(), net, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), net))
		// From genesis, so fill has nothing to fetch: any fetch is the
		// re-confirmation's.
		voteChain(t, c, "a", "0", 0, uint64(height), testHash("f", 0))
		voteChain(t, c, "b", "0", 0, uint64(height), testHash("f", 0))

		hash, status := c.ReconfirmPin(context.Background(), height)
		assert.Equal(t, integrity.PinFresh, status)
		assert.Equal(t, orphan, hash)
		c.sync(context.Background())
		assert.Zero(t, net.calls())
		assert.Empty(t, net.askedNeeds())
	})

	t.Run("a chain whose first block names the all-zero parentHash fills nothing below it", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := newChainTracker(t.Context(), net, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), net))
		// The chain starts at 5: its first block's parent is the zero hash,
		// a final root (spec §6.5 rule 2), never fetched by number or hash.
		zero := "0x" + strings.Repeat("0", 64)
		voteChain(t, c, "a", "0", 5, uint64(height), zero)
		voteChain(t, c, "b", "0", 5, uint64(height), zero)
		time.Sleep(300 * time.Millisecond)
		c.sync(context.Background())
		assert.Zero(t, net.calls())
	})

	// One source's vote alone never licenses a hard reject: the pin it backs
	// settles first.
	t.Run("a pin one source's vote backs settles before it is fresh", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := newChainTracker(t.Context(), net, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), net))
		voteChain(t, c, "a", "0", 0, uint64(height), testHash("f", 0))

		_, status := c.ReconfirmPin(context.Background(), height)
		assert.Equal(t, integrity.PinRateLimited, status, "a settle answer for another hash ties with the one vote")
		assert.NotEmpty(t, net.askedNeeds(), "the re-confirmation settled")
	})

	t.Run("a re-check deferred by a backoff starts no cooldown", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := pinnedOnOrphan(t, net)
		net.backoff.Store(true)

		hash, status := c.ReconfirmPin(context.Background(), height)
		assert.Equal(t, integrity.PinRateLimited, status, "no target answered")
		assert.Equal(t, orphan, hash)
		before := time.Now().Add(-2 * reconfirmCooldown)
		c.reconfirmMu.Lock()
		c.reconfirmedAt[height] = before
		c.reconfirmMu.Unlock()

		_, status = c.ReconfirmPin(context.Background(), height)
		assert.Equal(t, integrity.PinRateLimited, status, "the round waits on its targets' backoff")
		c.reconfirmMu.Lock()
		at := c.reconfirmedAt[height]
		c.reconfirmMu.Unlock()
		assert.Equal(t, before, at, "a deferred re-check starts no cooldown")
		assert.Len(t, net.askedNeeds(), 2, "the deferred re-check Asked nothing")
	})

	t.Run("a client that leaves with answers pending is rate-limited, never unverifiable", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := pinnedOnOrphan(t, net)
		net.hold.Store(true)

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		hash, status := c.ReconfirmPin(ctx, height)
		assert.Equal(t, integrity.PinRateLimited, status)
		assert.Equal(t, orphan, hash)
		c.reconfirmMu.Lock()
		_, cooled := c.reconfirmedAt[height]
		c.reconfirmMu.Unlock()
		assert.False(t, cooled, "a re-check whose client left starts no cooldown")
	})

	// A by-number answer from the cache is not a fresh view of the chain at
	// that height, finalized or not: a followed revert below Finalized
	// (final_hash_changed) must see the upstreams' current block, so a
	// by-number tracker fetch never reads the cache.
	byNumber := func(t *testing.T, finalized int64) *common.RequestDirectives {
		net := newCapturingNetwork(t, height, canonical)
		c := newChainTracker(t.Context(), net, 32, 0, "", "", "", func() common.EvmHead { return common.EvmHead{Finalized: finalized} }, newFetchScheduler(t.Context(), net))
		_, ok := c.headerByNumber(context.Background(), height, fmt.Sprintf("0x%x", height))
		require.True(t, ok)
		d := net.directivesFor(fmt.Sprintf("0x%x", height))
		require.NotNil(t, d)
		return d
	}

	t.Run("by-number corroboration fetches above the finalized height skip the cache", func(t *testing.T) {
		assert.Equal(t, "true", byNumber(t, height-1).SkipCacheRead, "a by-number tracker fetch must reach upstreams")
	})

	t.Run("by-number corroboration fetches at the finalized height skip the cache too", func(t *testing.T) {
		assert.Equal(t, "true", byNumber(t, height).SkipCacheRead, "a finalized height can still be reverted by a majority")
	})

	t.Run("a re-confirmation at a finalized height still Asks for a fresh fetch", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := newChainTracker(t.Context(), net, 32, 0, "", "", "", func() common.EvmHead { return common.EvmHead{Finalized: height} }, newFetchScheduler(t.Context(), net))
		voteChain(t, c, "a", "0", uint64(height), uint64(height)+1, testHash("f", uint64(height-1)))
		net.sourcesVote.Store(false)

		_, status := c.ReconfirmPin(context.Background(), height)
		require.Equal(t, integrity.PinFresh, status)
		asked := net.askedNeeds()
		require.NotEmpty(t, asked)
		assert.NotContains(t, asked, common.HeadNeedNumber, "a shared counter is no fresh view of the block")
	})

	t.Run("by-hash header fetches still use the cache", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := newChainTracker(t.Context(), net, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), net))

		_, _ = c.headerByHash(context.Background(), canonical)
		if d := net.directivesFor(canonical); d != nil {
			assert.Empty(t, d.SkipCacheRead,
				"a header is content-addressed by hash, so the cache can never be stale for it")
		}
	})
}
