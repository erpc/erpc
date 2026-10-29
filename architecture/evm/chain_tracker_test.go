package evm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
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

// servedChain serves header fetches by number and by hash from the blocks
// it was given; a fetch of a held ref waits until the ref is released.
// Sources a and b, of two vendors, are its hash voters.
type servedChain struct {
	mockNetwork
	mu      sync.Mutex
	blocks  map[string]json.RawMessage // number or hash -> header
	gates   map[string]chan struct{}
	refuse  map[string]int // ref -> answers left refused for capacity
	fetches atomic.Int32
	byHash  atomic.Int32
}

func newServedChain(t *testing.T, id string) *servedChain {
	n := &servedChain{blocks: map[string]json.RawMessage{}, gates: map[string]chan struct{}{}, refuse: map[string]int{}}
	n.On("Forward", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			n.fetches.Add(1)
			jrq, _ := req.JsonRpcRequest()
			ref, _ := jrq.Params[0].(string)
			if jrq.Method == "eth_getBlockByHash" {
				n.byHash.Add(1)
			}
			n.mu.Lock()
			gate := n.gates[ref]
			n.mu.Unlock()
			if gate != nil {
				select {
				case <-gate:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			n.mu.Lock()
			body, refused := n.blocks[ref], n.refuse[ref] > 0
			if refused {
				n.refuse[ref]--
			}
			n.mu.Unlock()
			if refused {
				return nil, common.NewErrUpstreamsExhaustedWithCause(common.NewErrEndpointCapacityExceeded(errors.New("vendor-a: 429")))
			}
			if body == nil {
				return nil, errors.New("no block " + ref)
			}
			return common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`"1"`), body, nil),
			), nil
		},
		nil,
	).Maybe()
	n.On("Id").Return(id).Maybe()
	n.On("ProjectId").Return("test").Maybe()
	n.On("HeadLatest", mock.Anything).Return(int64(1) << 62).Maybe()
	t.Cleanup(func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		for ref, g := range n.gates {
			close(g)
			delete(n.gates, ref)
		}
	})
	return n
}

func (n *servedChain) EvmHashVoters(string, time.Duration, bool) HashVoters {
	return HashVoters{Voters: []string{"a", "b"}, Hash: []string{"a", "b"}, Need: 2,
		Vendors: &Vendors{Of: map[string]string{"a": "vendor-a", "b": "vendor-b"}, Distinct: 2}}
}

// serve makes the chain answer block n, hash h, by number and by hash.
func (n *servedChain) serve(number uint64, h string, header json.RawMessage) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.blocks[fmt.Sprintf("0x%x", number)], n.blocks[h] = header, header
}

func (n *servedChain) hold(ref string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.gates[ref] = make(chan struct{})
}

func (n *servedChain) release(ref string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	close(n.gates[ref])
	delete(n.gates, ref)
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

	// Both voters' votes decide the tie and no other upstream is eligible:
	// nobody can break it. On a 3-upstream network every such tie ended a
	// round at once with 0 answers and retried it each block; with no
	// target no round starts (spec §6.5 rule 5).
	t.Run("a tie no target can break starts no round", func(t *testing.T) {
		net := newServedChain(t, "evm:no-target")
		c := newChainTracker(t.Context(), net, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), net))
		t.Cleanup(c.Want(indexer.ReadOpts{}))
		rounds := func() (n float64) {
			for _, o := range []string{"decided", "undecided"} {
				n += testutil.ToFloat64(telemetry.MetricChainTrackerSettleTotal.WithLabelValues("test", "evm:no-target", "tie", o))
			}
			return n
		}
		before := rounds()
		voteChain(t, c, "a", "a", 1, 10, testHash("f", 0))
		voteChain(t, c, "b", "a", 1, 10, testHash("f", 0))
		voteChain(t, c, "a", "a", 11, 11, testHash("a", 10))
		voteChain(t, c, "b", "b", 11, 11, testHash("a", 10))
		v, _ := c.HashAt(11)
		assert.Equal(t, testHash("a", 11), v, "rule 4 keeps the pinned child")
		assert.Equal(t, before, rounds(), "a tie with no target starts no round")
		voteChain(t, c, "a", "b", 11, 11, testHash("a", 10))
		v, _ = c.HashAt(11)
		assert.Equal(t, testHash("b", 11), v, "the next vote runs rule 4 again")
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

	// A client's read of a new branch is held without a walk; a vote's walk
	// that a later answer joins from below goes on from that answer, so the
	// votes wait until the branch links to the pins and the switch is a
	// reorg at the fork point, not a re-anchor onto an unlinked tree (K5's
	// reorgWindow − 1 deep reorg ended taps fork_left_window that way).
	t.Run("a client answer inside a walked branch keeps the votes waiting for the walk", func(t *testing.T) {
		ctx := context.Background()
		net := newServedChain(t, "evm:walk-join")
		c := newChainTracker(t.Context(), net, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), net))
		t.Cleanup(c.Want(indexer.ReadOpts{}))
		voteChain(t, c, "a", "a", 1, 10, testHash("f", 0))
		voteChain(t, c, "b", "a", 1, 10, testHash("f", 0))
		b := func(n uint64) json.RawMessage {
			parent := testHash("b", n-1)
			if n == 7 {
				parent = testHash("a", 6)
			}
			return testHeader(n, testHash("b", n), parent)
		}
		for n := uint64(7); n <= 11; n++ {
			net.serve(n, testHash("b", n), b(n))
		}
		net.hold("0x8")
		net.hold("0x7")
		reanchors := func() float64 {
			return testutil.ToFloat64(telemetry.MetricChainTrackerReanchorTotal.WithLabelValues("test", "evm:walk-join"))
		}
		before := reanchors()

		c.Observe("client", b(9), false)
		c.Observe("client", b(10), false)
		c.Observe("a", b(11), true)
		c.Observe("b", b(11), true)
		c.Observe("client", b(8), false)
		c.sync(ctx)
		v, _ := c.HashAt(10)
		assert.Equal(t, testHash("a", 10), v, "the votes wait while their walk is out")

		net.release("0x7")
		require.Eventually(t, func() bool {
			c.sync(ctx)
			v, _ := c.HashAt(11)
			return v == testHash("b", 11)
		}, 5*time.Second, 5*time.Millisecond, "the walk links b7 to a6 and the votes move")
		v, _ = c.HashAt(7)
		assert.Equal(t, testHash("b", 7), v)
		v, _ = c.HashAt(6)
		assert.Equal(t, testHash("a", 6), v, "the branch hangs from the fork point")
		assert.Equal(t, before, reanchors(), "a reorg inside the window is no re-anchor")
	})

	// No tap and no integrity check read the history of an idle network,
	// polled every 30s: each poll's vote, 61 blocks above the last, walked
	// reorgWindow blocks, failed to link and re-anchored (dev: 3,840 header
	// fetches and 120 re-anchors an hour on one network). Without a reader
	// the vote is a root at once and replaces the older tree; the first tap
	// starts history from the head, walking and filling as before.
	t.Run("an idle tracker walks and fills nothing until a tap opens", func(t *testing.T) {
		ctx := context.Background()
		net := newServedChain(t, "evm:idle")
		for n := uint64(1); n <= 300; n++ {
			net.serve(n, testHash("a", n), testHeader(n, testHash("a", n), testHash("a", n-1)))
		}
		c := newChainTracker(t.Context(), net, 32, time.Hour, "", "", "", nil, newFetchScheduler(t.Context(), net))
		reanchors := func() float64 {
			return testutil.ToFloat64(telemetry.MetricChainTrackerReanchorTotal.WithLabelValues("test", "evm:idle"))
		}
		before := reanchors()
		vote := func(n uint64) {
			for _, src := range []string{"a", "b"} {
				c.Observe(src, testHeader(n, testHash("a", n), testHash("a", n-1)), true)
			}
			c.sync(ctx)
		}
		for _, n := range []uint64{100, 161, 222} {
			vote(n)
			require.Eventually(t, func() bool {
				c.sync(ctx)
				v, _ := c.HashAt(int64(n))
				return v == testHash("a", n)
			}, 5*time.Second, 5*time.Millisecond, "each poll's vote is the head")
		}
		assert.Zero(t, net.fetches.Load(), "no walk and no fill")
		assert.Equal(t, before, reanchors(), "a root without history is no re-anchor")
		for _, gone := range []uint64{100, 161, 221} {
			_, held := c.HeldNumber(testHash("a", gone))
			assert.False(t, held, "block %d: the newest root replaced the older trees", gone)
		}
		head, ok := c.Confirmed()
		require.True(t, ok)
		assert.Equal(t, uint64(222), head.Number, "the confirmed head follows the votes")

		t.Cleanup(c.Want(indexer.ReadOpts{}))
		c.sync(ctx)
		start, err := c.Head(ctx)
		require.NoError(t, err)
		assert.Equal(t, uint64(222), start.Number, "the first tap starts at the head")
		// A capacity refusal is no evidence against the number path: fill
		// retries by number under the key's backoff, never by hash.
		net.mu.Lock()
		net.refuse["0xd2"] = 2
		net.mu.Unlock()
		vote(232)
		require.Eventually(t, func() bool {
			c.sync(ctx)
			v, _ := c.HashAt(201)
			return v == testHash("a", 201)
		}, 5*time.Second, 5*time.Millisecond, "the walk links 232 to 222 and fill runs reorgWindow below the head")
		assert.Zero(t, net.byHash.Load(), "a 429 on a by-number fetch keeps fill by number")
		b, err := c.Read(ctx, start, indexer.ReadOpts{})
		require.NoError(t, err)
		require.NotEmpty(t, b.Applied)
		assert.Empty(t, b.Reverted)
		assert.Equal(t, uint64(223), b.Applied[0].Number, "the tap continues from its start with no gap")
		assert.Equal(t, uint64(232), b.Applied[len(b.Applied)-1].Number)
		assert.Equal(t, before, reanchors(), "a walk that links is no re-anchor")
	})

	// A client answer from a pruned fork is a root whose parent no vendor
	// serves any more: one full by-hash sweep that finds it nowhere makes
	// the root final (spec §6.5 rule 2), where fill once swept every
	// upstream again at each backoff until the root left the window.
	t.Run("a root whose parent no upstream returns by hash ends its fill after one sweep", func(t *testing.T) {
		ctx := context.Background()
		net := newServedChain(t, "evm:orphan-root")
		for n := uint64(0); n <= 20; n++ {
			net.serve(n, testHash("a", n), testHeader(n, testHash("a", n), testHash("a", n-1)))
		}
		net.mu.Lock()
		net.blocks[testHash("e", 14)] = json.RawMessage(`null`)
		net.mu.Unlock()
		c := newChainTracker(t.Context(), net, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), net))
		t.Cleanup(c.Want(indexer.ReadOpts{}))
		voteChain(t, c, "a", "a", 0, 20, testHash("f", 0))
		voteChain(t, c, "b", "a", 0, 20, testHash("f", 0))
		c.Observe("client", testHeader(15, testHash("e", 15), testHash("e", 14)), false)
		require.Eventually(t, func() bool {
			c.sync(ctx)
			return net.byHash.Load() > 0
		}, 5*time.Second, 5*time.Millisecond, "fill asks the orphan's parent by hash once by number answers another block")
		for end := time.Now().Add(2500 * time.Millisecond); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
			c.sync(ctx)
		}
		assert.EqualValues(t, 1, net.byHash.Load(), "one sweep that finds the parent nowhere ends the fill; the key's backoff (200ms doubling) would have retried it 3 times")
		_, held := c.HeldNumber(testHash("e", 15))
		assert.True(t, held, "the orphan stays held, a final root")
	})

	// A single upstream pinned a sibling at the finalized height, then its
	// finalized poll answered that height with another hash before its head
	// moved to the canonical branch (dev: 19 final_hash_changed on three
	// one-upstream parachains). Finality by number alone called the revert
	// a final-hash change; only finalized evidence that covers the pin's
	// hash makes it one. Either way the reverted block at or below
	// Finalized is read past its permanent cache entry (spec §6.5).
	t.Run("a revert counts as a final-hash change only when finalized evidence covered the pin", func(t *testing.T) {
		ctx := context.Background()
		const fin = 8
		head := func() common.EvmHead { return common.EvmHead{Latest: 1 << 40, Finalized: fin} }
		evidence := func(c *ChainTracker, n uint64, hash string) {
			var h [32]byte
			require.True(t, decodeHex32(hash, &h))
			c.observeFinal("a", finalEvidence{number: n, hash: h})
		}
		alarms := func() float64 {
			return testutil.ToFloat64(telemetry.MetricChainTrackerInvariantViolationsTotal.WithLabelValues("", "", string(common.InvariantFinalHashChanged)))
		}
		run := func(covered string) *ChainTracker {
			c := newChainTracker(t.Context(), nil, 32, 0, "", "", "", head, newFetchScheduler(t.Context(), nil))
			voteChain(t, c, "a", "a", 1, 10, testHash("f", 0))
			evidence(c, fin, covered)
			voteChain(t, c, "a", "c", fin, 11, testHash("a", fin-1))
			v, ok := c.HashAt(fin)
			require.True(t, ok)
			require.Equal(t, testHash("c", fin), v, "the pins follow the upstream's branch")
			c.sync(ctx)
			return c
		}
		before := alarms()
		c := run(testHash("c", fin))
		assert.Equal(t, before, alarms(), "the finalized poll named the other branch: the pin was never final")
		assert.True(t, c.RevertedFinal(fin), "a reverted pin at Finalized skips its permanent cache entry, final or not")

		c = run(testHash("a", fin))
		assert.Equal(t, before+1, alarms(), "the finalized poll named the pin: its revert is a final-hash change")
		assert.True(t, c.RevertedFinal(fin))
		assert.False(t, c.RevertedFinal(fin+1), "a pin above Finalized keeps its cache entry")
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
	r, err := parseReceipts(raw, hash, nil, false)
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

	// An indexer backfilling finalized receipts and old blocks while the
	// tracker is busy: integrity records each anchor on the request path,
	// and an answer below the window never reaches the mailbox (spec §3).
	t.Run("a burst of anchors and old answers queues nothing on a stalled tracker", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		voteChain(t, c, "a", "c", 0x20, 0x30, testHash("c", 0x1f))
		stalled, release := make(chan struct{}), make(chan struct{})
		hook := func(tr *ChainTracker, upstreamId string, _ *indexer.Cursor, _ bool) {
			if tr == c && upstreamId == "stall" {
				close(stalled)
				<-release
			}
		}
		TrackerMessageHook.Store(&hook)
		t.Cleanup(func() { TrackerMessageHook.Store(nil) })
		c.observeFinal("stall", finalEvidence{})
		<-stalled
		for i := range uint64(2 * mailboxHeaderCap) {
			n := 1 + i%0x10
			c.observeNarrowAnchors(0x40, fmt.Sprintf("vendor-%d", i), []byte(fmt.Sprintf(`{"blockNumber":"0x%x","blockHash":"%s"}`, n, testHash("b", n))))
			c.Observe("client", testHeader(n, testHash("d", i+1), testHash("d", i)), false)
		}
		c.mb.mu.Lock()
		queued := len(c.mb.queue)
		c.mb.mu.Unlock()
		close(release)
		c.sync(context.Background())
		assert.Zero(t, queued, "anchors and answers below the window wait in no mailbox")
		v, ok := c.HashAt(0x10)
		require.True(t, ok)
		assert.Equal(t, testHash("b", 0x10), v, "the anchors are recorded")
	})
}

func TestChainTracker_GroupScoping(t *testing.T) {
	t.Run("a fetch is pinned to the group selector", func(t *testing.T) {
		d := fetchSpec{selector: "group-a*", method: "eth_getBlockByHash", byHash: true}.directives()
		require.NotNil(t, d)
		assert.Equal(t, common.UpstreamPoliciesTracker, d.UpstreamPolicies)
		assert.Equal(t, "group-a*", d.UseUpstream, "corroboration must stay in the served group")
		assert.Empty(t, d.SkipCacheRead, "a by-hash fetch may read the cache")

		fresh := fetchSpec{selector: "group-a*", method: "eth_getBlockByNumber"}.directives()
		assert.Equal(t, "group-a*", fresh.UseUpstream, "a fresh fetch still stays in the served group")
		assert.Equal(t, "true", fresh.SkipCacheRead, "every fetch not by hash skips cache reads")
	})

	t.Run("no selector → network-wide (no use-upstream pin)", func(t *testing.T) {
		d := fetchSpec{method: "eth_getBlockByNumber"}.directives()
		assert.Equal(t, common.UpstreamPoliciesTracker, d.UpstreamPolicies)
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

	t.Run("an answer holds its header only, and a held hash keeps its first header", func(t *testing.T) {
		c := newChainTracker(t.Context(), nil, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
		voteChain(t, c, "a", "a", 0x10, 0x11, testHash("f", 0xf))
		answer := func(miner string) *common.NormalizedResponse {
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByHash","params":["x",true]}`))
			body := fmt.Sprintf(`{"number":"0x10","hash":"%s","parentHash":"%s","miner":"%s","size":"0x220","transactions":[{"hash":"%s","input":"0x00"}],"uncles":[],"withdrawals":[{"index":"0x1"}]}`,
				testHash("e", 0x10), testHash("f", 0xf), miner, testHash("b", 1))
			return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(common.MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(body), nil))
		}

		observeBlockView(context.Background(), c, nil, answer("0x01"))
		c.sync(context.Background())
		observeBlockView(context.Background(), c, nil, answer("0x02"))
		c.sync(context.Background())

		h, held := c.held.Load(testHash("e", 0x10))
		require.True(t, held)
		assert.JSONEq(t, fmt.Sprintf(`{"number":"0x10","hash":"%s","parentHash":"%s","miner":"0x01","size":"0x220"}`, testHash("e", 0x10), testHash("f", 0xf)),
			string(h.(*chainHeader).raw), "the header fields of the first answer, without its body")

		latest := fmt.Sprintf(`{"number":"0x11","hash":"%s","parentHash":"%s","size":"0x220","transactions":[{"hash":"%s","input":"0x00"}],"uncles":[]}`,
			testHash("e", 0x11), testHash("e", 0x10), testHash("b", 2))
		c.Observe("client", json.RawMessage(latest), false)
		c.sync(context.Background())
		h, held = c.held.Load(testHash("e", 0x11))
		require.True(t, held)
		assert.JSONEq(t, fmt.Sprintf(`{"number":"0x11","hash":"%s","parentHash":"%s","size":"0x220"}`, testHash("e", 0x11), testHash("e", 0x10)),
			string(h.(*chainHeader).raw), "a client's latest answer holds its header only")
	})
}

// BenchmarkObserveLatestAnswer is a client's eth_getBlockByNumber(latest)
// answer with 200 full transactions handed to the tracker, as
// enrichStatePoller does: once its hash is held, and while it is new.
func BenchmarkObserveLatestAnswer(b *testing.B) {
	txs := make([]string, 200)
	for i := range txs {
		txs[i] = fmt.Sprintf(`{"hash":"%s","from":"0x%040x","to":"0x%040x","input":"0x%0512x","value":"0x1","nonce":"0x%x"}`, testHash("b", uint64(i)), i, i+1, i, i)
	}
	block := func(hash string) []byte {
		return []byte(fmt.Sprintf(`{"number":"0x11","hash":"%s","parentHash":"%s","timestamp":"0x6553f111","size":"0x220","transactions":[%s],"uncles":[],"withdrawals":[]}`,
			hash, testHash("a", 0x10), strings.Join(txs, ",")))
	}
	setup := func(b *testing.B) *ChainTracker {
		c := newChainTracker(b.Context(), nil, 8, 0, "", "", "", nil, newFetchScheduler(b.Context(), nil))
		parent := testHash("f", 0xf)
		for n := uint64(0x10); n <= 0x11; n++ {
			c.Observe("a", testHeader(n, testHash("a", n), parent), true)
			parent = testHash("a", n)
		}
		c.sync(context.Background())
		return c
	}
	b.Run("held", func(b *testing.B) {
		c := setup(b)
		answer := block(testHash("a", 0x11))
		b.ReportAllocs()
		for b.Loop() {
			c.Observe("client", answer, false)
			c.sync(context.Background())
		}
	})
	b.Run("new", func(b *testing.B) {
		c := setup(b)
		answers := make([][]byte, 64)
		for i := range answers {
			answers[i] = block(testHash("c", uint64(i)))
		}
		i := 0
		b.ReportAllocs()
		for b.Loop() {
			c.Observe("client", answers[i%len(answers)], false)
			c.sync(context.Background())
			i++
		}
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
		_, err := parseReceipts([]byte(body), hash, nil, false)
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

// settleNetwork is capturingNetwork with a voter set of its own and each
// head Ask answered with its target's latest header.
type settleNetwork struct {
	*capturingNetwork
	hv     HashVoters
	latest map[string]json.RawMessage // target -> its latest header
	smu    sync.Mutex
	forks  int // head Asks with a fork need
}

func newSettleNetwork(t *testing.T, hv HashVoters, latest map[string]json.RawMessage) *settleNetwork {
	return &settleNetwork{capturingNetwork: newCapturingNetwork(t, 0, testHash("f", 0)), hv: hv, latest: latest}
}

func (n *settleNetwork) EvmHashVoters(string, time.Duration, bool) HashVoters { return n.hv }

func (n *settleNetwork) EvmAskHead(_ context.Context, id string, need common.HeadNeed, _ time.Duration, _ common.AskReason) common.HeadAnswer {
	n.smu.Lock()
	if need == common.HeadNeedFork {
		n.forks++
	}
	h := n.latest[id]
	n.smu.Unlock()
	sent := time.Now()
	return common.HeadAnswer{Outcome: common.AskFetched, Sent: sent, Next: sent.Add(time.Hour), Header: askedHeader(h)}
}

func (n *settleNetwork) forkAsks() int {
	n.smu.Lock()
	defer n.smu.Unlock()
	return n.forks
}

// A non-voter's settle answer is one more vote for its decision only
// (spec §6.5 rule 5): once the voters leave that tie for a branch that forks
// below it, the answers no longer hold the pins on the old winner. A tap
// makes the tracker walk: one that does not walk settles no tie.
func TestSettleAnswersCountForTheirDecisionOnly(t *testing.T) {
	x := testHeader(11, testHash("aa", 11), testHash("0", 10))
	net := newSettleNetwork(t, HashVoters{Voters: []string{"a", "b"}, Hash: []string{"a", "b"}, Need: 2, Others: []string{"c", "d", "e"},
		Vendors: &Vendors{Of: map[string]string{"a": "vendor-a", "b": "vendor-b", "c": "vendor-c", "d": "vendor-d", "e": "vendor-e"}, Distinct: 5}},
		map[string]json.RawMessage{"c": x, "d": x, "e": x})
	c := newChainTracker(t.Context(), net, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), net))
	t.Cleanup(c.Want(indexer.ReadOpts{}))
	voteChain(t, c, "a", "0", 0, 10, testHash("f", 0))
	voteChain(t, c, "b", "0", 0, 10, testHash("f", 0))

	// bb is pinned first and the tie keeps it while the round is out, so
	// only the round's decision pins aa.
	c.Observe("b", testHeader(11, testHash("bb", 11), testHash("0", 10)), true)
	c.Observe("a", x, true)
	require.Eventually(t, func() bool {
		c.sync(context.Background())
		v, _ := c.HashAt(11)
		return v == testHash("aa", 11)
	}, 5*time.Second, 5*time.Millisecond, "3 non-voters' answers settle the 1-vs-1 tie")

	z := testHeader(11, testHash("cc", 11), testHash("0", 10))
	c.Observe("a", z, true)
	c.Observe("b", z, true)
	c.sync(context.Background())
	v, _ := c.HashAt(11)
	assert.Equal(t, testHash("cc", 11), v, "both voters left the tie: its answers no longer count")
}

// rewindNetwork records the network-wide tracker's rollback writes; its
// head Asks back off, so settles and refreshes end on the votes alone.
type rewindNetwork struct {
	*capturingNetwork
	hv      HashVoters
	rmu     sync.Mutex
	rewound map[string]int64 // upstream id -> its last rewind
}

func (n *rewindNetwork) EvmHashVoters(string, time.Duration, bool) HashVoters { return n.hv }

func (n *rewindNetwork) EvmAskHead(context.Context, string, common.HeadNeed, time.Duration, common.AskReason) common.HeadAnswer {
	return common.HeadAnswer{Outcome: common.AskBackoff, Next: time.Now().Add(time.Hour)}
}

func (n *rewindNetwork) EvmRewindHeads(votes map[string]int64) {
	n.rmu.Lock()
	defer n.rmu.Unlock()
	for u, bn := range votes {
		n.rewound[u] = bn
	}
}

func (n *rewindNetwork) rewinds() map[string]int64 {
	n.rmu.Lock()
	defer n.rmu.Unlock()
	out := make(map[string]int64, len(n.rewound))
	for u, bn := range n.rewound {
		out[u] = bn
	}
	return out
}

// The rollback write (spec §14) follows only a move to a lower head, and
// only a voter's fresh hash vote: an instance cut off from every vendor
// re-anchors up after the gap, and its voters' votes from before it are no
// evidence (row N4: Latest fell from 4821 to a 2-minute-old 4212). A vote
// is fresh by when it arrived, not by hash age: after a rewind on every
// upstream, intake counts no voter on the new branch a hash voter until
// the counters fall (row C5), here c. A polled voter with no vote at the
// switch, p, is owed it too: its poll never lowers its counter (row C16).
func TestRollbackWriteOnlyFromFreshVotesOnALowerBranch(t *testing.T) {
	ids := []string{"a", "b", "c", "s", "p"}
	net := &rewindNetwork{capturingNetwork: newCapturingNetwork(t, 0, testHash("f", 0)), rewound: map[string]int64{},
		hv: HashVoters{Voters: ids, Hash: []string{"a", "b", "s"}, Need: 2,
			Vendors: &Vendors{Of: map[string]string{"a": "vendor-a", "b": "vendor-b", "c": "vendor-c", "s": "vendor-s", "p": "vendor-p"}, Distinct: 5}}}
	c := newChainTracker(t.Context(), net, 64, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
	beforeGap := time.Now().Add(-2 * time.Minute)
	voteAt := func(at time.Time, src, tag string, from, to uint64, parentOfFirst string) {
		parent := parentOfFirst
		for n := from; n <= to; n++ {
			h, err := parseChainHeader(testHeader(n, testHash(tag, n), parent))
			require.NoError(t, err)
			c.observeSource(src, h, at)
			parent = h.hashHex
		}
		c.sync(context.Background())
	}
	pinned := func(n int64, want string) {
		t.Helper()
		require.Eventually(t, func() bool {
			c.sync(context.Background())
			v, _ := c.HashAt(n)
			return v == want
		}, 5*time.Second, 5*time.Millisecond)
	}
	for _, u := range []string{"a", "b", "c"} {
		voteAt(beforeGap, u, "a", 1, 10, testHash("f", 0))
	}
	voteAt(beforeGap, "s", "b", 40, 45, testHash("f", 39))
	pinned(10, testHash("a", 10))

	// After the gap a and b vote on s's tree: the pins re-anchor up, 10 to
	// 50, with s's vote from before the gap on the new pins.
	voteChain(t, c, "a", "b", 46, 50, testHash("b", 45))
	c.Observe("b", testHeader(50, testHash("b", 50), testHash("b", 49)), true)
	pinned(50, testHash("b", 50))
	c.Observe("c", testHeader(50, testHash("b", 50), testHash("b", 49)), true)
	c.sync(context.Background())
	assert.Empty(t, net.rewinds(), "a re-anchor up is a catch-up: no rollback write")

	// A reorg to a lower head: a and b's fresh votes on the new branch are
	// written at the switch, c's once its fresh vote reaches the pins,
	// though c is no hash voter; s's vote on the new branch is from before
	// the gap and is never written.
	fork := testHeader(48, testHash("c", 48), testHash("b", 47))
	c.Observe("a", fork, true)
	c.Observe("b", fork, true)
	pinned(48, testHash("c", 48))
	require.Eventually(t, func() bool { return len(net.rewinds()) == 2 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, map[string]int64{"a": 48, "b": 48}, net.rewinds())
	c.Observe("c", fork, true)
	require.Eventually(t, func() bool { c.sync(context.Background()); return len(net.rewinds()) == 3 }, 5*time.Second, 5*time.Millisecond)
	c.Observe("p", fork, true)
	require.Eventually(t, func() bool { c.sync(context.Background()); return len(net.rewinds()) == 4 }, 5*time.Second, 5*time.Millisecond,
		"a voter with no vote at the switch is rewound once its fresh vote reaches the pins")
	c.sync(context.Background())
	assert.Equal(t, map[string]int64{"a": 48, "b": 48, "c": 48, "p": 48}, net.rewinds(), "a stale vote is no evidence for a rewind")
}

// A group tracker refreshes the votes behind the pins once per off voter and
// fork point (spec §6.3), however many other rounds run between.
func TestSettleRefreshRunsOncePerForkPoint(t *testing.T) {
	head := testHeader(10, testHash("0", 10), testHash("0", 9))
	off := func(tag string) json.RawMessage { return testHeader(10, testHash(tag, 10), testHash("0", 9)) }
	net := newSettleNetwork(t, HashVoters{Voters: []string{"a", "b", "c", "d"}, Hash: []string{"a", "b", "c", "d"}, Need: 3,
		Vendors: &Vendors{Of: map[string]string{"a": "vendor-a", "b": "vendor-b", "c": "vendor-c", "d": "vendor-d"}, Distinct: 4}},
		map[string]json.RawMessage{"a": head, "b": head, "c": off("dd"), "d": head})
	c := newChainTracker(t.Context(), net, 32, 0, "group-*", "group", "group", nil, newFetchScheduler(t.Context(), net))
	for _, u := range []string{"a", "b", "c", "d"} {
		voteChain(t, c, u, "0", 0, 10, testHash("f", 0))
	}

	c.Observe("c", off("dd"), true)
	require.Eventually(t, func() bool { c.sync(context.Background()); return net.forkAsks() == 3 }, 5*time.Second, 5*time.Millisecond,
		"c's vote off the pins refreshes a, b and d first")
	net.smu.Lock()
	net.latest["d"] = off("ee")
	net.smu.Unlock()
	c.Observe("d", off("ee"), true)
	require.Eventually(t, func() bool { c.sync(context.Background()); return net.forkAsks() == 5 }, 5*time.Second, 5*time.Millisecond,
		"d's vote on another branch refreshes a and b")
	// c's next head on its branch is newer evidence than every vote
	// behind the pins, yet its fork point is the one refreshed.
	c.Observe("c", testHeader(11, testHash("dd", 11), testHash("dd", 10)), true)
	for range 10 {
		c.VotersChanged()
		c.sync(context.Background())
		time.Sleep(5 * time.Millisecond)
	}
	assert.Equal(t, 5, net.forkAsks(), "c's refresh at its fork point already ran")
	v, _ := c.HashAt(10)
	assert.Equal(t, testHash("0", 10), v, "the refreshed votes keep the pins")
}

// coldNetwork's upstreams become voters only once ready, as an instance's
// upstreams do after their registration; its taps demand newHeads.
type coldNetwork struct {
	*settleNetwork
	ready atomic.Bool
}

func (n *coldNetwork) EvmHashVoters(_ string, _ time.Duration, cold bool) HashVoters {
	if !n.ready.Load() {
		return HashVoters{Need: 1}
	}
	hv := HashVoters{Voters: []string{"a", "b"}, Sources: []string{"a", "b"},
		Vendors: &Vendors{Of: map[string]string{"a": "vendor-a", "b": "vendor-b"}, Distinct: 2}}
	if cold {
		hv.Hash = []string{"a", "b"}
	}
	hv.Need = common.ServedTipQuorum(0).Of(len(hv.Hash))
	return hv
}

func (n *coldNetwork) EvmHeadsDemand() (time.Duration, bool) { return 0, true }

// A cold instance's first header waits for a second voter's (spec §6.5
// cold window), even when its upstreams become voters more than W after the
// tracker started: under load their registration takes that long, and a
// window counted from the start would have let rule 6's one answer confirm
// alone.
func TestColdWindowStartsWithTheFirstVoters(t *testing.T) {
	head := testHeader(10, testHash("0", 10), testHash("0", 9))
	net := &coldNetwork{settleNetwork: newSettleNetwork(t, HashVoters{}, map[string]json.RawMessage{"a": head, "b": head})}
	c := newChainTracker(t.Context(), net, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), net))
	c.VotersChanged()
	c.sync(context.Background())
	time.Sleep(c.W() + 100*time.Millisecond)

	net.ready.Store(true)
	c.VotersChanged()
	for range 20 {
		c.sync(context.Background())
		time.Sleep(10 * time.Millisecond)
	}
	_, ok := c.Confirmed()
	assert.False(t, ok, "no source reported yet: both count as hash voters, so nothing is confirmed")

	c.Observe("a", head, true)
	c.sync(context.Background())
	_, ok = c.Confirmed()
	assert.False(t, ok, "the first header waits for a second voter's")
	c.Observe("b", head, true)
	c.sync(context.Background())
	got, ok := c.Confirmed()
	require.True(t, ok)
	assert.Equal(t, uint64(10), got.Number)
}
