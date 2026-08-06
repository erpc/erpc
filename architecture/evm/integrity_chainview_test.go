package evm

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChainView(t *testing.T) {
	t.Run("observe and look up the pin", func(t *testing.T) {
		c := newChainView(nil, 8, "", "", nil)
		_, ok := c.HashAt(1)
		assert.False(t, ok)
		c.observe(1, "0xa", nil)
		v, ok := c.HashAt(1)
		require.True(t, ok)
		assert.Equal(t, "0xa", v)
	})

	t.Run("reorg adopts the new hash and rolls back descendants", func(t *testing.T) {
		c := newChainView(nil, 32, "", "", nil)
		c.observe(5, "0xa", nil)
		c.observe(6, "0xb", nil)
		c.observe(7, "0xc", nil)
		// A different hash for 5 is a reorg: 6 and 7 were built on the old fork, so
		// their pins are invalidated (they re-populate as the new fork extends).
		c.observe(5, "0xa2", nil)
		v, ok := c.HashAt(5)
		require.True(t, ok)
		assert.Equal(t, "0xa2", v)
		_, ok = c.HashAt(6)
		assert.False(t, ok, "descendant 6 should be rolled back")
		_, ok = c.HashAt(7)
		assert.False(t, ok, "descendant 7 should be rolled back")
	})

	t.Run("re-observing the same hash is a no-op (no rollback)", func(t *testing.T) {
		c := newChainView(nil, 32, "", "", nil)
		c.observe(5, "0xa", nil)
		c.observe(6, "0xb", nil)
		c.observe(5, "0xa", nil) // same hash — must NOT roll back 6
		v, ok := c.HashAt(6)
		require.True(t, ok)
		assert.Equal(t, "0xb", v)
	})

	t.Run("ignores empty hash and negative number", func(t *testing.T) {
		c := newChainView(nil, 8, "", "", nil)
		c.observe(3, "", nil)
		c.observe(-1, "0xa", nil)
		_, ok := c.HashAt(3)
		assert.False(t, ok)
	})

	t.Run("evicts below tip minus window", func(t *testing.T) {
		c := newChainView(nil, 2, "", "", nil)
		for i := int64(1); i <= 5; i++ {
			c.observe(i, fmt.Sprintf("0x%d", i), nil)
		}
		// tip=5, window=2 → keep numbers >= 3.
		for _, gone := range []int64{1, 2} {
			_, ok := c.HashAt(gone)
			assert.False(t, ok, "block %d should be evicted", gone)
		}
		v, ok := c.HashAt(5)
		require.True(t, ok)
		assert.Equal(t, "0x5", v)
	})

	t.Run("zero window falls back to default", func(t *testing.T) {
		c := newChainView(nil, 0, "", "", nil)
		assert.Equal(t, defaultReorgWindow, c.window)
	})

	// Fetch-once foundation: a cached header is served WITHOUT a fetch. The nil
	// network proves it — any resolve() would fail, so a hit must come from cache.
	t.Run("header cache hit serves without fetching", func(t *testing.T) {
		c := newChainView(nil, 8, "", "", nil)
		h := &integrity.Header{Hash: "0xbb", Number: "0x10"}
		c.observe(0x10, "0xbb", h)

		byHash, ok := c.headerByHash(context.Background(), "0xbb")
		require.True(t, ok)
		assert.Equal(t, "0xbb", byHash.Hash)

		byNum, ok := c.headerByNumber(context.Background(), 0x10, "0x10")
		require.True(t, ok)
		assert.Equal(t, "0xbb", byNum.Hash)
	})

	t.Run("cache miss with no network fails closed", func(t *testing.T) {
		c := newChainView(nil, 8, "", "", nil)
		_, ok := c.headerByHash(context.Background(), "0xunknown")
		assert.False(t, ok)
	})
}

func TestChainView_Receipts(t *testing.T) {
	t.Run("receipts cache hit serves without fetching", func(t *testing.T) {
		c := newChainView(nil, 8, "", "", nil) // nil network → any fetch would fail
		c.observeReceipts("0xbb", []integrity.Receipt{{TransactionHash: "0xaa", BlockHash: "0xbb"}})
		got, ok := c.receiptsByHash(context.Background(), "0xbb")
		require.True(t, ok)
		require.Len(t, got, 1)
		assert.Equal(t, "0xaa", got[0].TransactionHash)
	})

	t.Run("receipts cache miss with no network fails closed", func(t *testing.T) {
		c := newChainView(nil, 8, "", "", nil)
		_, ok := c.receiptsByHash(context.Background(), "0xunknown")
		assert.False(t, ok)
	})

	t.Run("receipts evicted with the window", func(t *testing.T) {
		c := newChainView(nil, 2, "", "", nil)
		for i := 0; i < c.window+cacheSlack+2; i++ {
			c.observeReceipts(fmt.Sprintf("0x%d", i), []integrity.Receipt{{TransactionHash: "0xaa"}})
		}
		_, ok := c.receiptsByHash(context.Background(), "0x0") // oldest → evicted, nil network
		assert.False(t, ok, "oldest receipts should be evicted")
	})
}

func TestChainView_NarrowAnchors(t *testing.T) {
	t.Run("finalized block → pinned", func(t *testing.T) {
		c := newChainView(nil, 32, "", "", nil)
		c.observeNarrowAnchors(0x20, []byte(`{"blockNumber":"0x10","blockHash":"0xbb","transactionHash":"0xaa"}`))
		v, ok := c.HashAt(0x10)
		require.True(t, ok)
		assert.Equal(t, "0xbb", v)
	})

	t.Run("unfinalized block → not pinned (tip-thrash safety)", func(t *testing.T) {
		c := newChainView(nil, 32, "", "", nil)
		c.observeNarrowAnchors(0x20, []byte(`{"blockNumber":"0x30","blockHash":"0xcc"}`)) // 0x30 > fin 0x20
		_, ok := c.HashAt(0x30)
		assert.False(t, ok)
	})

	t.Run("finality unknown (fin<=0) → no-op", func(t *testing.T) {
		c := newChainView(nil, 32, "", "", nil)
		c.observeNarrowAnchors(0, []byte(`{"blockNumber":"0x10","blockHash":"0xbb"}`))
		_, ok := c.HashAt(0x10)
		assert.False(t, ok)
	})

	t.Run("array response (block receipts) → each finalized anchor pinned", func(t *testing.T) {
		c := newChainView(nil, 32, "", "", nil)
		c.observeNarrowAnchors(0x20, []byte(`[{"blockNumber":"0x10","blockHash":"0xbb"},{"blockNumber":"0x11","blockHash":"0xbb"}]`))
		v, ok := c.HashAt(0x10)
		require.True(t, ok)
		assert.Equal(t, "0xbb", v)
		_, ok = c.HashAt(0x11)
		assert.True(t, ok)
	})
}

func TestChainView_GroupScoping(t *testing.T) {
	t.Run("force-fetch is pinned to the group selector", func(t *testing.T) {
		c := newChainView(nil, 8, "group-a*", "group-a", nil)
		d := c.fetchDirectives(false)
		require.NotNil(t, d)
		assert.True(t, d.IsInternal)
		assert.Equal(t, "group-a*", d.UseUpstream, "corroboration must stay in the served group")
		assert.Empty(t, d.SkipCacheRead, "only the pin tie-break forces a cache miss")

		fresh := c.fetchDirectives(true)
		assert.Equal(t, "group-a*", fresh.UseUpstream, "a fresh re-confirm still stays in the served group")
		assert.Equal(t, "true", fresh.SkipCacheRead)
	})

	t.Run("no selector → network-wide (no use-upstream pin)", func(t *testing.T) {
		c := newChainView(nil, 8, "", "", nil)
		d := c.fetchDirectives(false)
		assert.True(t, d.IsInternal)
		assert.Equal(t, "", d.UseUpstream)
	})
}

func TestChainView_FinalityLabel(t *testing.T) {
	c := newChainView(nil, 8, "", "", func() int64 { return 100 })
	assert.Equal(t, "finalized", c.finalityLabel(50))
	assert.Equal(t, "finalized", c.finalityLabel(100))
	assert.Equal(t, "unfinalized", c.finalityLabel(150))
	assert.Equal(t, "unknown", c.finalityLabel(-1))
	assert.Equal(t, "unknown", newChainView(nil, 8, "", "", nil).finalityLabel(50))
}

func TestObserveBlockView(t *testing.T) {
	blockResponse := func(method, number, hash string) *common.NormalizedResponse {
		req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"%s","params":["x",false]}`, method)))
		body := fmt.Sprintf(`{"number":"%s","hash":"%s","parentHash":"0xdef"}`, number, hash)
		jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(body), nil)
		return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	}

	t.Run("a by-number response pins number→hash", func(t *testing.T) {
		c := newChainView(nil, 8, "", "", nil)
		observeBlockView(context.Background(), c, blockResponse("eth_getBlockByNumber", "0x10", "0xabc"), "eth_getblockbynumber")
		v, ok := c.HashAt(0x10)
		require.True(t, ok)
		assert.Equal(t, "0xabc", v)
	})

	// Pin-poisoning guard: a by-hash lookup names the block it wants and may
	// legitimately be an orphan. Pinning from it would adopt the orphan as
	// canonical at that height and roll back the real fork's descendants,
	// turning one client's reorg-unwind into mass rejections of honest
	// by-number traffic.
	t.Run("a by-hash response caches the header but never moves the pin", func(t *testing.T) {
		c := newChainView(nil, 8, "", "", nil)
		observeBlockView(context.Background(), c, blockResponse("eth_getBlockByNumber", "0x10", "0xcanonical"), "eth_getblockbynumber")
		observeBlockView(context.Background(), c, blockResponse("eth_getBlockByNumber", "0x11", "0xchild"), "eth_getblockbynumber")

		observeBlockView(context.Background(), c, blockResponse("eth_getBlockByHash", "0x10", "0xorphan"), "eth_getblockbyhash")

		v, ok := c.HashAt(0x10)
		require.True(t, ok)
		assert.Equal(t, "0xcanonical", v, "the orphan must not become the pin")
		_, ok = c.HashAt(0x11)
		assert.True(t, ok, "descendants must not be rolled back by a by-hash lookup")

		c.mu.RLock()
		_, cached := c.headers["0xorphan"]
		c.mu.RUnlock()
		assert.True(t, cached, "the header is still cached by hash (immutable, content-addressed)")
	})

	t.Run("a by-hash response for an unseen number creates no pin", func(t *testing.T) {
		c := newChainView(nil, 8, "", "", nil)
		observeBlockView(context.Background(), c, blockResponse("eth_getBlockByHash", "0x20", "0xsome"), "eth_getblockbyhash")
		_, ok := c.HashAt(0x20)
		assert.False(t, ok)
	})
}

// The hash anchor: a by-hash fetch must never trust (or cache) receipts that
// claim a different block — a group node still on a losing fork answering a
// by-hash getBlockReceipts with its own fork's receipts was observed live
// rejecting every honest receipt for the real block for hours.
func TestReceiptsMatchBlock(t *testing.T) {
	h := "0x8C242a174154a5b2077aD649c1d8e38A01fa1e93aaaaaaaaaaaaaaaaaaaaaaaa"
	t.Run("all receipts on the requested hash (case-insensitive) → ok", func(t *testing.T) {
		assert.True(t, receiptsMatchBlock([]integrity.Receipt{
			{TransactionHash: "0x1", BlockHash: strings.ToLower(h)},
			{TransactionHash: "0x2", BlockHash: h},
		}, h))
	})
	t.Run("another fork's receipts → refuse", func(t *testing.T) {
		assert.False(t, receiptsMatchBlock([]integrity.Receipt{
			{TransactionHash: "0x1", BlockHash: "0xotherfork"},
		}, h))
	})
	t.Run("mixed set → refuse", func(t *testing.T) {
		assert.False(t, receiptsMatchBlock([]integrity.Receipt{
			{TransactionHash: "0x1", BlockHash: h},
			{TransactionHash: "0x2", BlockHash: "0xotherfork"},
		}, h))
	})
	t.Run("receipts without blockHash can't prove membership → refuse", func(t *testing.T) {
		assert.False(t, receiptsMatchBlock([]integrity.Receipt{{TransactionHash: "0x1"}}, h))
	})
	t.Run("empty set is trivially consistent (emptiness handled by the caller)", func(t *testing.T) {
		assert.True(t, receiptsMatchBlock(nil, h))
	})
}

func TestHeaderMatchesRef(t *testing.T) {
	t.Run("by-hash must return that hash", func(t *testing.T) {
		h := &integrity.Header{Hash: "0xAB", Number: "0x10"}
		assert.True(t, headerMatchesRef(h, 0x10, "eth_getBlockByHash", "0xab"))
		assert.False(t, headerMatchesRef(h, 0x10, "eth_getBlockByHash", "0xcd"))
	})
	t.Run("by-number must return that number", func(t *testing.T) {
		h := &integrity.Header{Hash: "0xab", Number: "0x10"}
		assert.True(t, headerMatchesRef(h, 0x10, "eth_getBlockByNumber", "0x10"))
		assert.False(t, headerMatchesRef(h, 0x11, "eth_getBlockByNumber", "0x10"))
	})
	t.Run("tag refs have no anchor to enforce", func(t *testing.T) {
		h := &integrity.Header{Hash: "0xab", Number: "0x10"}
		assert.True(t, headerMatchesRef(h, 0x10, "eth_getBlockByNumber", "latest"))
	})
}

// ReconfirmPin must report WHY it is answering, not just what the pin is.
// Inside the cooldown it returns the cached pin as PinRateLimited — carrying no
// fresh evidence — so the engine degrades the verdict instead of rejecting on
// it. Returning that as a confirmation let one stale pin hard-reject 24 honest
// responses in ~700ms on mainnet 25589196 (2026-07-22).
func TestReconfirmPin_CooldownIsRateLimitedNotConfirmed(t *testing.T) {
	t.Run("a cooled-down number reports PinRateLimited, not PinFresh", func(t *testing.T) {
		c := newChainView(nil, 32, "", "", nil)
		c.observe(0x10, "0xstalepin", nil)
		c.mu.Lock()
		c.reconfirmedAt[0x10] = time.Now() // a re-confirmation just happened
		c.mu.Unlock()

		hash, status := c.ReconfirmPin(context.Background(), 0x10)
		assert.Equal(t, integrity.PinRateLimited, status,
			"a rate-limited answer must not be reported as a confirmation")
		assert.Equal(t, "0xstalepin", hash, "the pin is still returned, for context")
	})

	t.Run("an expired cooldown is not rate-limited (falls through to a fetch)", func(t *testing.T) {
		c := newChainView(nil, 32, "", "", nil)
		c.observe(0x10, "0xstalepin", nil)
		c.mu.Lock()
		c.reconfirmedAt[0x10] = time.Now().Add(-2 * reconfirmCooldown)
		c.mu.Unlock()

		// No network wired, so the fetch cannot resolve — the point is that it
		// reports PinUnverifiable rather than short-circuiting as rate-limited.
		_, status := c.ReconfirmPin(context.Background(), 0x10)
		assert.Equal(t, integrity.PinUnverifiable, status)
	})

	t.Run("an unfetchable pin is PinUnverifiable, which keeps the strict verdict", func(t *testing.T) {
		c := newChainView(nil, 32, "", "", nil)
		c.observe(0x10, "0xpin", nil)
		_, status := c.ReconfirmPin(context.Background(), 0x10)
		assert.Equal(t, integrity.PinUnverifiable, status)
	})

	t.Run("a negative number is unverifiable", func(t *testing.T) {
		c := newChainView(nil, 32, "", "", nil)
		_, status := c.ReconfirmPin(context.Background(), -1)
		assert.Equal(t, integrity.PinUnverifiable, status)
	})
}
