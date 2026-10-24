package evm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testHash is a 32-byte hash whose hex starts with tag (hex digits).
func testHash(tag string, n uint64) string {
	return fmt.Sprintf("0x%s%0*x", tag, 64-len(tag), n)
}

func testHeader(n uint64, hash, parent string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"number":"0x%x","hash":"%s","parentHash":"%s","timestamp":"0x%x"}`, n, hash, parent, 1_700_000_000+n))
}

// voteChain has source src vote for each block of the linear chain
// [from..to] tagged tag, and waits until the tracker applied it.
func voteChain(t *testing.T, c *ChainTracker, src, tag string, from, to uint64, parentOfFirst string) {
	t.Helper()
	parent := parentOfFirst
	for n := from; n <= to; n++ {
		h := testHash(tag, n)
		c.Observe(src, testHeader(n, h, parent), true)
		parent = h
	}
	c.sync(context.Background())
}

func TestChainTracker(t *testing.T) {
	t.Run("a vote pins its block", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		_, ok := c.HashAt(1)
		assert.False(t, ok)
		voteChain(t, c, "a", "a", 1, 1, testHash("f", 0))
		v, ok := c.HashAt(1)
		require.True(t, ok)
		assert.Equal(t, testHash("a", 1), v)
	})

	t.Run("a vote on another branch moves the pins and drops descendants", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		voteChain(t, c, "a", "a", 5, 7, testHash("f", 4))
		// The source now votes for another block at 5: its tree has the vote.
		voteChain(t, c, "a", "b", 5, 5, testHash("f", 4))
		v, ok := c.HashAt(5)
		require.True(t, ok)
		assert.Equal(t, testHash("b", 5), v)
		_, ok = c.HashAt(6)
		assert.False(t, ok, "descendant 6 is no longer pinned")
		_, ok = c.HashAt(7)
		assert.False(t, ok, "descendant 7 is no longer pinned")
	})

	t.Run("an ancestor of the current vote does not move it back", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		voteChain(t, c, "a", "a", 5, 6, testHash("f", 4))
		c.Observe("a", testHeader(5, testHash("a", 5), testHash("f", 4)), true)
		c.sync(context.Background())
		v, ok := c.HashAt(6)
		require.True(t, ok)
		assert.Equal(t, testHash("a", 6), v)
	})

	t.Run("the majority branch wins; a tie keeps the pins", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		voteChain(t, c, "a", "a", 10, 11, testHash("f", 9))
		voteChain(t, c, "b", "a", 10, 11, testHash("f", 9))
		// c and then d follow a fork at 11.
		fork := testHeader(11, testHash("b", 11), testHash("a", 10))
		c.Observe("c", fork, true)
		c.sync(context.Background())
		v, _ := c.HashAt(11)
		assert.Equal(t, testHash("a", 11), v, "2 votes against 1 keep the branch")
		c.Observe("b", fork, true)
		c.sync(context.Background())
		v, _ = c.HashAt(11)
		assert.Equal(t, testHash("b", 11), v, "2 votes against 1 move the pins")
		c.Observe("d", testHeader(11, testHash("a", 11), testHash("a", 10)), true)
		c.sync(context.Background())
		v, _ = c.HashAt(11)
		assert.Equal(t, testHash("b", 11), v, "a tie keeps the pinned child")
	})

	t.Run("a malformed header is dropped", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		c.Observe("a", json.RawMessage(`{"number":"0x3","hash":"","parentHash":"0x1"}`), true)
		c.Observe("a", json.RawMessage(`{"number":"0x3","hash":"0xabc","parentHash":"0x1"}`), true)
		c.sync(context.Background())
		_, ok := c.HashAt(3)
		assert.False(t, ok)
	})

	t.Run("evicts below the head minus the window", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 2, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		voteChain(t, c, "a", "a", 1, 5, testHash("f", 0))
		for _, gone := range []int64{1, 2} {
			_, ok := c.HashAt(gone)
			assert.False(t, ok, "block %d should be evicted", gone)
		}
		v, ok := c.HashAt(5)
		require.True(t, ok)
		assert.Equal(t, testHash("a", 5), v)
	})

	// A held header is served without a fetch: with a nil network any fetch
	// would fail, so a hit must come from what the tracker holds.
	t.Run("held headers serve without fetching", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		voteChain(t, c, "a", "b", 0x10, 0x10, testHash("f", 0xf))

		byHash, ok := c.headerByHash(context.Background(), testHash("b", 0x10))
		require.True(t, ok)
		assert.Equal(t, testHash("b", 0x10), byHash.Hash)

		byNum, ok := c.headerByNumber(context.Background(), 0x10, "0x10")
		require.True(t, ok)
		assert.Equal(t, testHash("b", 0x10), byNum.Hash)
	})

	t.Run("cache miss with no network fails closed", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		_, ok := c.headerByHash(context.Background(), testHash("e", 1))
		assert.False(t, ok)
	})
}

func putReceipts(c *ChainTracker, blockHash string, receipts []integrity.Receipt) {
	var hash [32]byte
	if !decodeHex32(blockHash, &hash) {
		panic("bad block hash " + blockHash)
	}
	for i := range receipts {
		receipts[i].BlockHash = blockHash
	}
	raw, err := json.Marshal(receipts)
	if err != nil {
		panic(err)
	}
	r, err := parseReceipts(raw, hash, nil)
	if err != nil {
		panic(err)
	}
	c.mb.put(trackerMsg{kind: msgReceipts, held: r})
	c.sync(context.Background())
}

func TestChainTracker_Receipts(t *testing.T) {
	t.Run("receipts cache hit serves without fetching", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil)) // nil network: any fetch would fail
		putReceipts(c, testHash("b", 1), []integrity.Receipt{{TransactionHash: "0xaa", BlockHash: testHash("b", 1)}})
		got, ok := c.receiptsByHash(context.Background(), testHash("B", 1), 1)
		require.True(t, ok)
		require.Len(t, got, 1)
		assert.Equal(t, "0xaa", got[0].TransactionHash)
	})

	t.Run("receipts cache miss with no network fails closed", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		_, ok := c.receiptsByHash(context.Background(), testHash("e", 1), 1)
		assert.False(t, ok)
	})

	t.Run("receipts evicted with the window", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 2, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		for i := range c.receiptsCap() + 1 {
			putReceipts(c, testHash("b", uint64(i)), []integrity.Receipt{{TransactionHash: "0xaa"}})
		}
		_, ok := c.receiptsByHash(context.Background(), testHash("b", 0), 0) // oldest → evicted, nil network
		assert.False(t, ok, "oldest receipts should be evicted")
	})
}

func TestChainTracker_NarrowAnchors(t *testing.T) {
	anchors := func(c *ChainTracker, fin int64, body string) {
		c.observeNarrowAnchors(fin, "vendor-a", []byte(body))
		c.sync(context.Background())
	}
	t.Run("finalized block → anchored", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		anchors(c, 0x20, fmt.Sprintf(`{"blockNumber":"0x10","blockHash":"%s","transactionHash":"0xaa"}`, testHash("b", 0x10)))
		v, ok := c.HashAt(0x10)
		require.True(t, ok)
		assert.Equal(t, testHash("b", 0x10), v)
	})

	t.Run("unfinalized block → not anchored (tip-thrash safety)", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		anchors(c, 0x20, fmt.Sprintf(`{"blockNumber":"0x30","blockHash":"%s"}`, testHash("c", 0x30)))
		_, ok := c.HashAt(0x30)
		assert.False(t, ok)
	})

	t.Run("finality unknown (fin<=0) → no-op", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		anchors(c, 0, fmt.Sprintf(`{"blockNumber":"0x10","blockHash":"%s"}`, testHash("b", 0x10)))
		_, ok := c.HashAt(0x10)
		assert.False(t, ok)
	})

	t.Run("array response (block receipts) → each finalized anchor", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		h := testHash("b", 0x10)
		anchors(c, 0x20, fmt.Sprintf(`[{"blockNumber":"0x10","blockHash":"%s"},{"blockNumber":"0x11","blockHash":"%s"}]`, h, h))
		v, ok := c.HashAt(0x10)
		require.True(t, ok)
		assert.Equal(t, h, v)
		_, ok = c.HashAt(0x11)
		assert.True(t, ok)
	})

	// One vendor's narrow answer must not hard-reject another vendor's
	// honest block below the window: an anchor is a fresh confirmation only
	// once 2 vendors agree on it.
	t.Run("an anchor one vendor alone answered never hard-rejects", func(t *testing.T) {
		const n = 0x10
		wrong, honest := testHash("e", n), testHash("b", n)
		c := newChainTracker(t.Context(), nil, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		// 2 sources pin 0x20..0x30, so 0x10 lies below the window.
		voteChain(t, c, "a", "c", 0x20, 0x30, testHash("c", 0x1f))
		voteChain(t, c, "b", "c", 0x20, 0x30, testHash("c", 0x1f))
		say := func(vendor, hash string) {
			c.observeNarrowAnchors(0x40, vendor, []byte(fmt.Sprintf(`{"blockNumber":"0x10","blockHash":"%s"}`, hash)))
			c.sync(context.Background())
		}
		validate := func(hash string) integrity.Result {
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x10",false]}`))
			jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), testHeader(n, hash, testHash("b", n-1)), nil)
			return integrity.Validate(context.Background(), integrity.Input{
				Method:   integrity.MethodGetBlockByNumber,
				Response: common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr),
				Checks:   integrity.CheckSet{}.Enable("hashStability", nil),
				History:  c,
				Reorg:    integrity.ReorgPolicy{Finalized: integrity.BehaviorError, Unfinalized: integrity.BehaviorError},
			})
		}

		say("vendor-a", wrong)
		got, status := c.ReconfirmPin(context.Background(), n)
		assert.Equal(t, wrong, got)
		assert.Equal(t, integrity.PinRateLimited, status)
		res := validate(honest)
		require.NoError(t, res.Err, "an honest block is served while one vendor alone backs the anchor")
		assert.Equal(t, []integrity.CheckOutcome{{CheckID: "hashStability", Outcome: "record_only"}}, res.Outcomes)

		say("vendor-b", honest)
		_, status = c.ReconfirmPin(context.Background(), n)
		assert.Equal(t, integrity.PinRateLimited, status, "1 vendor against 1 is no agreement")

		say("vendor-c", honest)
		got, status = c.ReconfirmPin(context.Background(), n)
		assert.Equal(t, honest, got, "the hash 2 vendors answered wins")
		assert.Equal(t, integrity.PinFresh, status)
		require.NoError(t, validate(honest).Err)
		assert.Error(t, validate(wrong).Err, "the wrong hash is rejected once 2 vendors agree on the anchor")
	})
}

func TestChainTracker_GroupScoping(t *testing.T) {
	t.Run("a fetch is pinned to the group selector", func(t *testing.T) {
		d := fetchSpec{selector: "group-a*", method: "eth_getBlockByHash", byHash: true}.directives()
		require.NotNil(t, d)
		assert.True(t, d.IsInternal)
		assert.Equal(t, common.UpstreamPoliciesTracker, d.UpstreamPolicies)
		assert.Equal(t, "group-a*", d.UseUpstream, "corroboration must stay in the served group")
		assert.Empty(t, d.SkipCacheRead, "a by-hash fetch may read the cache")

		fresh := fetchSpec{selector: "group-a*", method: "eth_getBlockByNumber"}.directives()
		assert.Equal(t, "group-a*", fresh.UseUpstream, "a fresh fetch still stays in the served group")
		assert.Equal(t, "true", fresh.SkipCacheRead, "every fetch not by hash skips cache reads")
	})

	t.Run("no selector → network-wide (no use-upstream pin)", func(t *testing.T) {
		d := fetchSpec{method: "eth_getBlockByNumber"}.directives()
		assert.True(t, d.IsInternal)
		assert.Equal(t, "", d.UseUpstream)
	})
}

func TestChainTracker_FinalityLabel(t *testing.T) {
	c := newChainTracker(t.Context(), nil, 8, 0, "", "", "", func() common.EvmHead { return common.EvmHead{Finalized: 100} }, newFetchScheduler(t.Context(), nil))
	assert.Equal(t, "finalized", c.finalityLabel(50))
	assert.Equal(t, "finalized", c.finalityLabel(100))
	assert.Equal(t, "unfinalized", c.finalityLabel(150))
	assert.Equal(t, "unknown", c.finalityLabel(-1))
	assert.Equal(t, "unknown", newChainTracker(t.Context(), nil, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil)).finalityLabel(50))
}

// Answers are held, never voted: a client's block response, by number or by
// hash, never moves the pins. A by-hash lookup may legitimately be an
// orphan; adopting it would roll back the real fork's descendants.
func TestObserveBlockView(t *testing.T) {
	blockResponse := func(method string, n uint64, hash, parent string) *common.NormalizedResponse {
		req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"%s","params":["x",false]}`, method)))
		jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), testHeader(n, hash, parent), nil)
		return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	}

	t.Run("answers are held but never move the pins", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		voteChain(t, c, "a", "a", 0x10, 0x11, testHash("f", 0xf))

		observeBlockView(context.Background(), c, nil, blockResponse("eth_getBlockByHash", 0x10, testHash("e", 0x10), testHash("f", 0xf)))
		observeBlockView(context.Background(), c, nil, blockResponse("eth_getBlockByNumber", 0x11, testHash("d", 0x11), testHash("a", 0x10)))
		c.sync(context.Background())

		v, ok := c.HashAt(0x10)
		require.True(t, ok)
		assert.Equal(t, testHash("a", 0x10), v, "the orphan must not become the pin")
		v, ok = c.HashAt(0x11)
		require.True(t, ok)
		assert.Equal(t, testHash("a", 0x11), v, "an answer by number must not move the pin")

		_, held := c.held.Load(testHash("e", 0x10))
		assert.True(t, held, "the answer is held by hash")
	})

	t.Run("an answer for an unseen number creates no pin", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		observeBlockView(context.Background(), c, nil, blockResponse("eth_getBlockByNumber", 0x20, testHash("c", 0x20), testHash("c", 0x1f)))
		c.sync(context.Background())
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
	var hash [32]byte
	require.True(t, decodeHex32(h, &hash))
	check := func(body string) error {
		_, err := parseReceipts([]byte(body), hash, nil)
		return err
	}
	t.Run("all receipts on the requested hash (case-insensitive) → ok", func(t *testing.T) {
		assert.NoError(t, check(`[{"transactionHash":"0x1","blockHash":"`+strings.ToLower(h)+`"},{"transactionHash":"0x2","blockHash":"`+h+`"}]`))
	})
	t.Run("another fork's receipts → refuse", func(t *testing.T) {
		assert.ErrorIs(t, check(`[{"transactionHash":"0x1","blockHash":"0xotherfork"}]`), errReceiptsAnswer)
	})
	t.Run("mixed set → refuse", func(t *testing.T) {
		assert.ErrorIs(t, check(`[{"transactionHash":"0x1","blockHash":"`+h+`"},{"transactionHash":"0x2","blockHash":"0xotherfork"}]`), errReceiptsAnswer)
	})
	t.Run("receipts without blockHash can't prove membership → refuse", func(t *testing.T) {
		assert.ErrorIs(t, check(`[{"transactionHash":"0x1"}]`), errReceiptsAnswer)
	})
	t.Run("null or empty for a block not known to be empty → refuse, never no logs", func(t *testing.T) {
		assert.ErrorIs(t, check(`null`), errReceiptsAnswer)
		assert.ErrorIs(t, check(`[]`), errReceiptsAnswer)
	})
}

func TestHeaderMatchesRef(t *testing.T) {
	parse := func(n uint64, hash string) *chainHeader {
		h, err := parseChainHeader(testHeader(n, hash, testHash("f", n-1)))
		require.NoError(t, err)
		return h
	}
	t.Run("by-hash must return that hash, compared as bytes", func(t *testing.T) {
		h := parse(0x10, testHash("ab", 0x10))
		assert.True(t, headerMatchesRef(h, "eth_getBlockByHash", "0x"+strings.ToUpper(testHash("ab", 0x10)[2:])))
		assert.False(t, headerMatchesRef(h, "eth_getBlockByHash", testHash("cd", 0x10)))
	})
	t.Run("by-number must return that number", func(t *testing.T) {
		h := parse(0x10, testHash("ab", 0x10))
		assert.True(t, headerMatchesRef(h, "eth_getBlockByNumber", "0x10"))
		assert.False(t, headerMatchesRef(h, "eth_getBlockByNumber", "0x11"))
	})
	t.Run("tag refs have no anchor to enforce", func(t *testing.T) {
		h := parse(0x10, testHash("ab", 0x10))
		assert.True(t, headerMatchesRef(h, "eth_getBlockByNumber", "latest"))
	})
}

// ReconfirmPin must report WHY it is answering, not just what the pin is.
// Inside the cooldown it returns the cached pin as PinRateLimited — carrying no
// fresh evidence — so the engine degrades the verdict instead of rejecting on
// it. Returning that as a confirmation let one stale pin hard-reject 24 honest
// responses in ~700ms on mainnet 25589196 (2026-07-22).
func TestReconfirmPin_CooldownIsRateLimitedNotConfirmed(t *testing.T) {
	pinned := func(t *testing.T) *ChainTracker {
		c := newChainTracker(t.Context(), nil, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		voteChain(t, c, "a", "a", 0x10, 0x10, testHash("f", 0xf)) // one source backs the pin
		return c
	}
	setReconfirmedAt := func(c *ChainTracker, at time.Time) {
		c.reconfirmMu.Lock()
		c.reconfirmedAt[0x10] = at
		c.reconfirmMu.Unlock()
	}

	t.Run("a cooled-down number reports PinRateLimited, not PinFresh", func(t *testing.T) {
		c := pinned(t)
		setReconfirmedAt(c, time.Now()) // a re-confirmation just happened

		hash, status := c.ReconfirmPin(context.Background(), 0x10)
		assert.Equal(t, integrity.PinRateLimited, status,
			"a rate-limited answer must not be reported as a confirmation")
		assert.Equal(t, testHash("a", 0x10), hash, "the pin is still returned, for context")
	})

	t.Run("an expired cooldown is not rate-limited (falls through to a settle)", func(t *testing.T) {
		c := pinned(t)
		before := time.Now().Add(-2 * reconfirmCooldown)
		setReconfirmedAt(c, before)

		// No network wired, so the settle cannot resolve — the point is that
		// it ran, rather than short-circuiting as rate-limited.
		_, status := c.ReconfirmPin(context.Background(), 0x10)
		assert.Equal(t, integrity.PinRateLimited, status)
		c.reconfirmMu.Lock()
		at := c.reconfirmedAt[0x10]
		c.reconfirmMu.Unlock()
		assert.True(t, at.After(before), "the re-confirmation settled again")
	})

	t.Run("a pin one source backs is never a hard reject, even unfetchable", func(t *testing.T) {
		c := pinned(t)
		_, status := c.ReconfirmPin(context.Background(), 0x10)
		assert.Equal(t, integrity.PinRateLimited, status,
			"an unanswered settle over a pin resting on one vote must degrade the verdict")
	})

	t.Run("a number with no pin is PinUnverifiable, which keeps the strict verdict", func(t *testing.T) {
		c := pinned(t)
		_, status := c.ReconfirmPin(context.Background(), 0x20)
		assert.Equal(t, integrity.PinUnverifiable, status)
	})

	t.Run("a negative number is unverifiable", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		_, status := c.ReconfirmPin(context.Background(), -1)
		assert.Equal(t, integrity.PinUnverifiable, status)
	})
}
