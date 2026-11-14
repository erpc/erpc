package evm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"weak"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	gethcommon "github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
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
	c.Sync(context.Background())
}

// logOnlyNetwork is a tracker test's network that has nothing to fetch
// from: it cannot sweep upstreams, has no config and empty labels.
type logOnlyNetwork struct{ common.Network }

func (logOnlyNetwork) Logger() *zerolog.Logger       { l := zerolog.Nop(); return &l }
func (logOnlyNetwork) ProjectId() string             { return "" }
func (logOnlyNetwork) Label() string                 { return "" }
func (logOnlyNetwork) Config() *common.NetworkConfig { return nil }

// servedChain serves header fetches by number and by hash from the blocks
// it was given; a fetch of a held ref waits until the ref is released, and
// a ref with an error answers it. Sources a and b, of two vendors, are its
// hash voters.
type servedChain struct {
	mockNetwork
	mu      sync.Mutex
	blocks  map[string]json.RawMessage // number or hash -> header
	errs    map[string]error           // number or hash -> the upstream's error
	asked   []string                   // every ref fetched, in order
	gates   map[string]chan struct{}
	refuse  map[string]int // ref -> answers left refused for capacity
	fetches atomic.Int32
	byHash  atomic.Int32
}

func newServedChain(t testing.TB, id string) *servedChain {
	n := &servedChain{blocks: map[string]json.RawMessage{}, errs: map[string]error{}, gates: map[string]chan struct{}{}, refuse: map[string]int{}}
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
			n.asked = append(n.asked, ref)
			body, refused, answer := n.blocks[ref], n.refuse[ref] > 0, n.errs[ref]
			if refused {
				n.refuse[ref]--
			}
			n.mu.Unlock()
			if answer != nil {
				return nil, answer
			}
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
	n.On("Config").Return((*common.NetworkConfig)(nil)).Maybe()
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

func (n *servedChain) EvmHashVoters(string, time.Duration) HashVoters {
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

// forkedChain is servedChain with four hash voters, a to d, of four
// vendors; a settle Ask waits until answer closes and then answers latest.
type forkedChain struct {
	*servedChain
	latest json.RawMessage
	answer chan struct{}
	asks   atomic.Int32
}

func (n *forkedChain) EvmHashVoters(string, time.Duration) HashVoters {
	ids := []string{"a", "b", "c", "d"}
	return HashVoters{Voters: ids, Hash: ids, Need: 2,
		Vendors: &Vendors{Of: map[string]string{"a": "vendor-a", "b": "vendor-b", "c": "vendor-c", "d": "vendor-d"}, Distinct: 4}}
}

func (n *forkedChain) EvmAskHead(ctx context.Context, _ string, _ common.HeadNeed, _ time.Duration, _ time.Time, _ common.AskReason) common.HeadAnswer {
	n.asks.Add(1)
	select {
	case <-n.answer:
	case <-ctx.Done():
		return common.HeadAnswer{Err: ctx.Err()}
	}
	sent := time.Now()
	return common.HeadAnswer{Outcome: common.AskFetched, At: sent, Next: sent.Add(time.Hour), Header: askedHeader(n.latest)}
}

func TestChainTracker(t *testing.T) {
	t.Run("a vote pins its block", func(t *testing.T) {
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
		_, ok := c.HashAt(1)
		assert.False(t, ok)
		voteChain(t, c, "a", "a", 1, 1, testHash("f", 0))
		v, ok := c.HashAt(1)
		require.True(t, ok)
		assert.Equal(t, testHash("a", 1), v)
	})

	t.Run("a vote on another branch moves the pins and drops descendants", func(t *testing.T) {
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
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
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
		voteChain(t, c, "a", "a", 5, 6, testHash("f", 4))
		c.Observe("a", testHeader(5, testHash("a", 5), testHash("f", 4)), true)
		c.Sync(context.Background())
		v, ok := c.HashAt(6)
		require.True(t, ok)
		assert.Equal(t, testHash("a", 6), v)
	})

	t.Run("the majority branch wins; a tie keeps the pins", func(t *testing.T) {
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
		voteChain(t, c, "a", "a", 10, 11, testHash("f", 9))
		voteChain(t, c, "b", "a", 10, 11, testHash("f", 9))
		// c and then d follow a fork at 11.
		fork := testHeader(11, testHash("b", 11), testHash("a", 10))
		c.Observe("c", fork, true)
		c.Sync(context.Background())
		v, _ := c.HashAt(11)
		assert.Equal(t, testHash("a", 11), v, "2 votes against 1 keep the branch")
		c.Observe("b", fork, true)
		c.Sync(context.Background())
		v, _ = c.HashAt(11)
		assert.Equal(t, testHash("b", 11), v, "2 votes against 1 move the pins")
		c.Observe("d", testHeader(11, testHash("a", 11), testHash("a", 10)), true)
		c.Sync(context.Background())
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
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
		c.Observe("a", json.RawMessage(`{"number":"0x3","hash":"","parentHash":"0x1"}`), true)
		c.Observe("a", json.RawMessage(`{"number":"0x3","hash":"0xabc","parentHash":"0x1"}`), true)
		c.Sync(context.Background())
		_, ok := c.HashAt(3)
		assert.False(t, ok)
	})

	// U12: a voter reports its tip's header under a number 30M ahead,
	// before the other voter pushes that tip. The real header takes its
	// place in the chain; the chain below it stays pinned and linked.
	t.Run("a header far ahead under the tip's hash keeps the chain linked", func(t *testing.T) {
		net := newServedChain(t, "evm:ahead-hash")
		c := newChainTracker(t.Context(), net, 16, 0, "", "", "", nil, newFetchScheduler(t.Context(), net))
		t.Cleanup(c.Want(indexer.ReadOpts{}))
		parent := testHash("f", 0)
		for n := uint64(1); n <= 30; n++ {
			h := testHash("a", n)
			hdr := testHeader(n, h, parent)
			net.serve(n, h, hdr)
			if n == 21 {
				c.Observe("b", testHeader(n+30_000_000, h, parent), true)
				c.Sync(context.Background())
			}
			c.Observe("a", hdr, true)
			c.Observe("b", hdr, true)
			c.Sync(context.Background())
			parent = h
		}
		for n := uint64(15); n <= 30; n++ {
			v, ok := c.HashAt(int64(n))
			require.True(t, ok, "block %d is pinned", n)
			assert.Equal(t, testHash("a", n), v, "block %d", n)
		}
	})

	t.Run("evicts below the head minus the window", func(t *testing.T) {
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 2, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
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
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
		voteChain(t, c, "a", "b", 0x10, 0x10, testHash("f", 0xf))

		byHash, ok := c.headerByHash(context.Background(), testHash("b", 0x10))
		require.True(t, ok)
		assert.Equal(t, testHash("b", 0x10), byHash.Hash)

		byNum, ok := c.headerByNumber(context.Background(), 0x10, "0x10")
		require.True(t, ok)
		assert.Equal(t, testHash("b", 0x10), byNum.Hash)
	})

	t.Run("cache miss with no network fails closed", func(t *testing.T) {
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
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
		c.Sync(ctx)
		v, _ := c.HashAt(10)
		assert.Equal(t, testHash("a", 10), v, "the votes wait while their walk is out")

		net.release("0x7")
		require.Eventually(t, func() bool {
			c.Sync(ctx)
			v, _ := c.HashAt(11)
			return v == testHash("b", 11)
		}, 5*time.Second, 5*time.Millisecond, "the walk links b7 to a6 and the votes move")
		v, _ = c.HashAt(7)
		assert.Equal(t, testHash("b", 7), v)
		v, _ = c.HashAt(6)
		assert.Equal(t, testHash("a", 6), v, "the branch hangs from the fork point")
		assert.Equal(t, before, reanchors(), "a reorg inside the window is no re-anchor")
	})

	// A revert deeper than reorgWindow re-anchors the pins on the new
	// branch, and fill links it down to the fork point in the old tree,
	// whose root is final. While a settle round holds the pins, that link
	// makes the old root the pins' root before the pins reach it: a tap
	// whose cursor is on the old branch met the pins' own root unlinked and
	// ended fork_left_window (real world C12: a 74-deep revert ended gRPC
	// streams), where it waits for the pins and reverts once they move.
	t.Run("a cursor on a branch reverted deeper than the window waits for the pins to reach the fork", func(t *testing.T) {
		ctx := context.Background()
		net := &forkedChain{servedChain: newServedChain(t, "evm:deep-revert"), answer: make(chan struct{})}
		a := func(n uint64) json.RawMessage {
			parent := "0x" + strings.Repeat("0", 64)
			if n > 0 {
				parent = testHash("a", n-1)
			}
			return testHeader(n, testHash("a", n), parent)
		}
		b := func(n uint64) json.RawMessage {
			parent := testHash("b", n-1)
			if n == 6 {
				parent = testHash("a", 5)
			}
			return testHeader(n, testHash("b", n), parent)
		}
		for n := uint64(0); n <= 20; n++ {
			net.serve(n, testHash("a", n), a(n))
		}
		net.latest = b(21)
		c := newChainTracker(t.Context(), net, 8, time.Hour, "", "", "", nil, newFetchScheduler(t.Context(), net))
		t.Cleanup(c.Want(indexer.ReadOpts{Resumable: true}))
		for _, src := range []string{"a", "b", "c", "d"} {
			voteChain(t, c, src, "a", 0, 20, "0x"+strings.Repeat("0", 64))
		}
		v, _ := c.HashAt(20)
		require.Equal(t, testHash("a", 20), v)

		for n := uint64(6); n <= 21; n++ {
			net.serve(n, testHash("b", n), b(n))
		}
		net.hold("0x6")
		hash := func(tag string, n uint64) (h [32]byte) {
			require.True(t, decodeHex32(testHash(tag, n), &h))
			return h
		}
		type result struct {
			b   indexer.Batch
			err error
		}
		read := make(chan result, 1)
		go func() {
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			b, err := c.Read(rctx, indexer.Cursor{Number: 20, Hash: hash("a", 20)}, indexer.ReadOpts{})
			read <- result{b, err}
		}()
		for _, src := range []string{"a", "b", "c"} {
			c.Observe(src, b(21), true)
		}
		require.Eventually(t, func() bool {
			c.Sync(ctx)
			v, _ := c.HashAt(21)
			_, held := c.HeldNumber(testHash("b", 7))
			return v == testHash("b", 21) && held
		}, 5*time.Second, 5*time.Millisecond, "the pins re-anchor on b and fill reaches b7")

		// d's vote forks off the pins: its refresh holds them while its
		// round Asks the others.
		c.Observe("d", testHeader(21, testHash("d", 21), testHash("b", 20)), true)
		require.Eventually(t, func() bool {
			c.Sync(ctx)
			return net.asks.Load() > 0
		}, 5*time.Second, 5*time.Millisecond, "d's refresh starts a round")

		// b6's fetch links b to a5, in a's tree.
		net.release("0x6")
		select {
		case r := <-read:
			require.Failf(t, "the read ended before the pins reached the fork point", "err %v, batch %+v", r.err, r.b)
		case <-time.After(500 * time.Millisecond):
		}
		close(net.answer)
		r := <-read
		require.NoError(t, r.err, "the cursor's branch links to the pins: a revert, not fork_left_window")
		require.Len(t, r.b.Reverted, 15)
		assert.Equal(t, hash("a", 20), r.b.Reverted[0].Hash)
		assert.Equal(t, hash("a", 6), r.b.Reverted[14].Hash)
		require.NotEmpty(t, r.b.Applied)
		assert.Equal(t, hash("b", 6), r.b.Applied[0].Hash, "the new branch applies from the fork point + 1")
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
		// Blocks above 222 carry headers whose hash recomputes, so one
		// holder serves them (spec §6.5); 225's also carries a field no
		// registered header format knows.
		hashes, raws := map[uint64]string{}, map[uint64]json.RawMessage{}
		withField := func(raw json.RawMessage, k, v string) json.RawMessage {
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &fields))
			fields[k] = json.RawMessage(v)
			out, err := json.Marshal(fields)
			require.NoError(t, err)
			return out
		}
		for n := uint64(1); n <= 300; n++ {
			if n <= 222 {
				hashes[n], raws[n] = testHash("a", n), testHeader(n, testHash("a", n), testHash("a", n-1))
			} else {
				h := &gethtypes.Header{Number: new(big.Int).SetUint64(n), ParentHash: gethcommon.HexToHash(hashes[n-1]), Time: 1_700_000_000 + n, Difficulty: big.NewInt(0)}
				raw, err := h.MarshalJSON()
				require.NoError(t, err)
				hashes[n], raws[n] = h.Hash().Hex(), raw
				if n == 225 {
					raws[n] = withField(raw, "chainContext", `"0x01"`)
				}
			}
			net.serve(n, hashes[n], raws[n])
		}
		c := newChainTracker(t.Context(), net, 32, time.Hour, "", "", "", nil, newFetchScheduler(t.Context(), net))
		reanchors := func() float64 {
			return testutil.ToFloat64(telemetry.MetricChainTrackerReanchorTotal.WithLabelValues("test", "evm:idle"))
		}
		before := reanchors()
		vote := func(n uint64, srcs ...string) {
			for _, src := range srcs {
				c.Observe(src, raws[n], true)
			}
			c.Sync(ctx)
		}
		for _, n := range []uint64{100, 161, 222} {
			vote(n, "a", "b")
			require.Eventually(t, func() bool {
				c.Sync(ctx)
				v, _ := c.HashAt(int64(n))
				return v == hashes[n]
			}, 5*time.Second, 5*time.Millisecond, "each poll's vote is the head")
		}
		assert.Zero(t, net.fetches.Load(), "no walk and no fill")
		assert.Equal(t, before, reanchors(), "a root without history is no re-anchor")
		for _, gone := range []uint64{100, 161, 221} {
			_, held := c.HeldNumber(hashes[gone])
			assert.False(t, held, "block %d: the newest root replaced the older trees", gone)
		}
		head, ok := c.View().Head()
		require.True(t, ok)
		assert.Equal(t, uint64(222), head.Number, "the confirmed head follows the votes")

		// While its view is read, one voter's vote that skipped a block the
		// tracker never fetched walks to the pins: one fetch links it, and
		// the view takes it from its one holder, where a root at once held
		// the view below it until a voter majority moved.
		c.MarkRead()
		fetched := net.fetches.Load()
		vote(224, "a")
		require.Eventually(t, func() bool {
			c.Sync(ctx)
			h, ok := c.View().Head()
			return ok && h.Number == 224
		}, 5*time.Second, 5*time.Millisecond, "the view takes the walked vote")
		assert.Equal(t, fetched+1, net.fetches.Load(), "one fetch walks the skipped block")
		assert.Equal(t, before, reanchors(), "a walk that links is no re-anchor")

		// Header proof (spec §6.5): this network verified headers. A header
		// its format cannot encode (a key it does not know) is unverified and
		// serves from one holder; one it encodes to another hash (a lossy
		// copy or a forged hash) is a mismatch and needs q holders, until a
		// held copy of that hash verifies.
		proofs := func(p, reason string) float64 {
			return testutil.ToFloat64(telemetry.MetricChainTrackerHeaderProofTotal.WithLabelValues("test", "evm:idle", p, reason))
		}
		assert.Positive(t, proofs("unverified", "unencodable"), "headers no format encodes, before any verified, are unverified")
		unencodable, differs := proofs("unverified", "unencodable"), proofs("mismatch", "hash_differs")
		vote(225, "a")
		require.Eventually(t, func() bool {
			c.Sync(ctx)
			h, ok := c.View().Head()
			return ok && h.Number == 225
		}, 5*time.Second, 5*time.Millisecond, "after headers verified, a header the format cannot encode serves from one holder")
		assert.Equal(t, unencodable+1, proofs("unverified", "unencodable"))
		c.Observe("a", withField(raws[226], "baseFeePerGas", `"0x7"`), true)
		c.Sync(ctx)
		h, _ := c.View().Head()
		assert.Equal(t, uint64(225), h.Number, "a copy its format hashes otherwise does not serve from one holder")
		assert.Equal(t, differs+1, proofs("mismatch", "hash_differs"))
		vote(226, "a")
		require.Eventually(t, func() bool {
			c.Sync(ctx)
			h, ok := c.View().Head()
			return ok && h.Number == 226 && h.Hash == gethcommon.HexToHash(hashes[226])
		}, 5*time.Second, 5*time.Millisecond, "a copy of 226 that verifies serves it from one holder")

		t.Cleanup(c.Want(indexer.ReadOpts{}))
		c.Sync(ctx)
		start, err := c.Head(ctx)
		require.NoError(t, err)
		assert.Equal(t, uint64(226), start.Number, "the first tap starts at the head")
		// A capacity refusal is no evidence against the number path: fill
		// retries by number under the key's backoff, never by hash.
		net.mu.Lock()
		net.refuse["0xd2"] = 2
		net.mu.Unlock()
		vote(232, "a", "b")
		require.Eventually(t, func() bool {
			c.Sync(ctx)
			v, _ := c.HashAt(201)
			return v == hashes[201]
		}, 5*time.Second, 5*time.Millisecond, "the walk links 232 to 226 and fill runs reorgWindow below the head")
		assert.Zero(t, net.byHash.Load(), "a 429 on a by-number fetch keeps fill by number")
		b, err := c.Read(ctx, start, indexer.ReadOpts{})
		require.NoError(t, err)
		require.NotEmpty(t, b.Applied)
		assert.Empty(t, b.Reverted)
		assert.Equal(t, uint64(227), b.Applied[0].Number, "the tap continues from its start with no gap")
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
			c.Sync(ctx)
			return net.byHash.Load() > 0
		}, 5*time.Second, 5*time.Millisecond, "fill asks the orphan's parent by hash once by number answers another block")
		for end := time.Now().Add(2500 * time.Millisecond); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
			c.Sync(ctx)
		}
		assert.EqualValues(t, 1, net.byHash.Load(), "one sweep that finds the parent nowhere ends the fill; the key's backoff (200ms doubling) would have retried it 3 times")
		_, held := c.HeldNumber(testHash("e", 15))
		assert.True(t, held, "the orphan stays held, a final root")
	})

	// Fill fetches one header per round trip, so a Read whose cursor lay
	// below the lowest pin waited for fill to reach the window base before
	// it said out of range (real world: 12s for 1,500 blocks; minutes in
	// production). One by-hash fetch of the cursor decides at once by its
	// header time (spec §10.1); a cursor inside the window still waits.
	t.Run("a cursor below the lowest pin is out of range at once when older than the window", func(t *testing.T) {
		ctx := context.Background()
		net := newServedChain(t, "evm:below-pins")
		for n := uint64(0); n <= 300; n++ {
			net.serve(n, testHash("a", n), testHeader(n, testHash("a", n), testHash("a", n-1)))
		}
		net.mu.Lock()
		net.blocks[testHash("e", 150)] = json.RawMessage(`null`)
		net.mu.Unlock()
		// 100s of chain time at one block a second: the window base is 200.
		c := newChainTracker(t.Context(), net, 32, 100*time.Second, "", "", "", nil, newFetchScheduler(t.Context(), net))
		t.Cleanup(c.Want(indexer.ReadOpts{}))
		// Fill stalls at 267, below reorgWindow under the head.
		net.hold("0x10b")
		for _, src := range []string{"a", "b"} {
			c.Observe(src, testHeader(300, testHash("a", 300), testHash("a", 299)), true)
		}
		require.Eventually(t, func() bool {
			c.Sync(ctx)
			v, _ := c.HashAt(268)
			return v == testHash("a", 268)
		}, 5*time.Second, 5*time.Millisecond, "fill runs reorgWindow below the head")
		cursor := func(tag string, n uint64) indexer.Cursor {
			var h [32]byte
			require.True(t, decodeHex32(testHash(tag, n), &h))
			return indexer.Cursor{Number: n, Hash: h}
		}
		for _, tc := range []struct {
			what   string
			c      indexer.Cursor
			reason string
		}{
			{"canonical, 150s below the head", cursor("a", 150), indexer.OutOfRangeBelowWindow},
			{"null from every upstream", cursor("e", 150), indexer.OutOfRangeCursorUnknown},
		} {
			rctx, cancel := context.WithTimeout(ctx, time.Second)
			_, err := c.Read(rctx, tc.c, indexer.ReadOpts{})
			cancel()
			var oor indexer.OutOfRangeError
			require.ErrorAs(t, err, &oor, "cursor %s: out of range within 1s while fill stalls", tc.what)
			assert.Equal(t, tc.reason, oor.Reason, tc.what)
		}

		before := net.byHash.Load()
		got := make(chan indexer.Batch, 1)
		go func() {
			b, err := c.Read(t.Context(), cursor("a", 240), indexer.ReadOpts{})
			assert.NoError(t, err)
			got <- b
		}()
		require.Eventually(t, func() bool { return net.byHash.Load()-before == 1 }, 2*time.Second, 5*time.Millisecond, "the cursor 60s below the head is fetched by hash")
		select {
		case b := <-got:
			t.Fatalf("cursor 60s below the head: Read returned %d blocks while fill stood at 268", len(b.Applied))
		case <-time.After(300 * time.Millisecond):
		}
		assert.EqualValues(t, 1, net.byHash.Load()-before, "one by-hash fetch per cursor, not one per Read pass")
		net.release("0x10b")
		select {
		case b := <-got:
			require.NotEmpty(t, b.Applied)
			assert.Equal(t, uint64(241), b.Applied[0].Number, "the Read resumes after its cursor once fill places it")
		case <-time.After(5 * time.Second):
			t.Fatal("cursor 60s below the head: no batch after fill resumed")
		}
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
			c := newChainTracker(t.Context(), &logOnlyNetwork{}, 32, 0, "", "", "", head, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
			voteChain(t, c, "a", "a", 1, 10, testHash("f", 0))
			evidence(c, fin, covered)
			voteChain(t, c, "a", "c", fin, 11, testHash("a", fin-1))
			v, ok := c.HashAt(fin)
			require.True(t, ok)
			require.Equal(t, testHash("c", fin), v, "the pins follow the upstream's branch")
			c.Sync(ctx)
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
	r, err := parseReceipts(raw, hash, nil, false, false)
	if err != nil {
		panic(err)
	}
	c.mb.put(trackerMsg{kind: msgReceipts, held: r})
	c.Sync(context.Background())
}

func TestChainTracker_Receipts(t *testing.T) {
	t.Run("receipts cache hit serves without fetching", func(t *testing.T) {
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{})) // nil network: any fetch would fail
		putReceipts(c, testHash("b", 1), []integrity.Receipt{{TransactionHash: "0xaa", BlockHash: testHash("b", 1)}})
		got, ok := c.receiptsByHash(context.Background(), testHash("B", 1), 1)
		require.True(t, ok)
		require.Len(t, got, 1)
		assert.Equal(t, "0xaa", got[0].TransactionHash)
	})

	t.Run("receipts cache miss with no network fails closed", func(t *testing.T) {
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
		_, ok := c.receiptsByHash(context.Background(), testHash("e", 1), 1)
		assert.False(t, ok)
	})

	t.Run("receipts evicted with the window", func(t *testing.T) {
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 2, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
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
		c.Sync(context.Background())
	}
	t.Run("finalized block → anchored", func(t *testing.T) {
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
		anchors(c, 0x20, fmt.Sprintf(`{"blockNumber":"0x10","blockHash":"%s","transactionHash":"0xaa"}`, testHash("b", 0x10)))
		v, ok := c.HashAt(0x10)
		require.True(t, ok)
		assert.Equal(t, testHash("b", 0x10), v)
	})

	t.Run("unfinalized block → not anchored (tip-thrash safety)", func(t *testing.T) {
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
		anchors(c, 0x20, fmt.Sprintf(`{"blockNumber":"0x30","blockHash":"%s"}`, testHash("c", 0x30)))
		_, ok := c.HashAt(0x30)
		assert.False(t, ok)
	})

	t.Run("finality unknown (fin<=0) → no-op", func(t *testing.T) {
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
		anchors(c, 0, fmt.Sprintf(`{"blockNumber":"0x10","blockHash":"%s"}`, testHash("b", 0x10)))
		_, ok := c.HashAt(0x10)
		assert.False(t, ok)
	})

	t.Run("array response (block receipts) → each finalized anchor", func(t *testing.T) {
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
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
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
		// 2 sources pin 0x20..0x30, so 0x10 lies below the window.
		voteChain(t, c, "a", "c", 0x20, 0x30, testHash("c", 0x1f))
		voteChain(t, c, "b", "c", 0x20, 0x30, testHash("c", 0x1f))
		say := func(vendor, hash string) {
			c.observeNarrowAnchors(0x40, vendor, []byte(fmt.Sprintf(`{"blockNumber":"0x10","blockHash":"%s"}`, hash)))
			c.Sync(context.Background())
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
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
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
		c.Sync(context.Background())
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
	c := newChainTracker(t.Context(), &logOnlyNetwork{}, 8, 0, "", "", "", func() common.EvmHead { return common.EvmHead{Finalized: 100} }, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
	assert.Equal(t, "finalized", c.finalityLabel(50))
	assert.Equal(t, "finalized", c.finalityLabel(100))
	assert.Equal(t, "unfinalized", c.finalityLabel(150))
	assert.Equal(t, "unknown", c.finalityLabel(-1))
	assert.Equal(t, "unknown", newChainTracker(t.Context(), &logOnlyNetwork{}, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{})).finalityLabel(50))
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
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
		voteChain(t, c, "a", "a", 0x10, 0x11, testHash("f", 0xf))

		observeBlockView(context.Background(), c, nil, blockResponse("eth_getBlockByHash", 0x10, testHash("e", 0x10), testHash("f", 0xf)))
		observeBlockView(context.Background(), c, nil, blockResponse("eth_getBlockByNumber", 0x11, testHash("d", 0x11), testHash("a", 0x10)))
		c.Sync(context.Background())

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
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
		observeBlockView(context.Background(), c, nil, blockResponse("eth_getBlockByNumber", 0x20, testHash("c", 0x20), testHash("c", 0x1f)))
		c.Sync(context.Background())
		_, ok := c.HashAt(0x20)
		assert.False(t, ok)
	})

	t.Run("an answer holds its header only, and a held hash keeps its first header", func(t *testing.T) {
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
		voteChain(t, c, "a", "a", 0x10, 0x11, testHash("f", 0xf))
		answer := func(miner string) *common.NormalizedResponse {
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByHash","params":["x",true]}`))
			body := fmt.Sprintf(`{"number":"0x10","hash":"%s","parentHash":"%s","miner":"%s","size":"0x220","transactions":[{"hash":"%s","input":"0x00"}],"uncles":[],"withdrawals":[{"index":"0x1"}]}`,
				testHash("e", 0x10), testHash("f", 0xf), miner, testHash("b", 1))
			return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(common.MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(body), nil))
		}

		observeBlockView(context.Background(), c, nil, answer("0x01"))
		c.Sync(context.Background())
		observeBlockView(context.Background(), c, nil, answer("0x02"))
		c.Sync(context.Background())

		h, held := c.held.Load(testHash("e", 0x10))
		require.True(t, held)
		assert.JSONEq(t, fmt.Sprintf(`{"number":"0x10","hash":"%s","parentHash":"%s","miner":"0x01","size":"0x220"}`, testHash("e", 0x10), testHash("f", 0xf)),
			string(h.(*chainHeader).raw), "the header fields of the first answer, without its body")

		latest := fmt.Sprintf(`{"number":"0x11","hash":"%s","parentHash":"%s","size":"0x220","transactions":[{"hash":"%s","input":"0x00"}],"uncles":[]}`,
			testHash("e", 0x11), testHash("e", 0x10), testHash("b", 2))
		c.Observe("client", json.RawMessage(latest), false)
		c.Sync(context.Background())
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
		c := newChainTracker(b.Context(), &logOnlyNetwork{}, 8, 0, "", "", "", nil, newFetchScheduler(b.Context(), &logOnlyNetwork{}))
		parent := testHash("f", 0xf)
		for n := uint64(0x10); n <= 0x11; n++ {
			c.Observe("a", testHeader(n, testHash("a", n), parent), true)
			parent = testHash("a", n)
		}
		c.Sync(context.Background())
		return c
	}
	b.Run("held", func(b *testing.B) {
		c := setup(b)
		answer := block(testHash("a", 0x11))
		b.ReportAllocs()
		for b.Loop() {
			c.Observe("client", answer, false)
			c.Sync(context.Background())
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
			c.Sync(context.Background())
			i++
		}
	})
}

// BenchmarkDeepFill is a Resumable tap with a maxBackfill depth blocks
// deep, filled from a chain that answers at once: the CPU the process
// spends per header filled. A cost per update that grows with the headers
// held shows as cpu-ns/header growing with depth.
func BenchmarkDeepFill(b *testing.B) {
	cpu := func() time.Duration {
		var ru syscall.Rusage
		_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
		return time.Duration(syscall.TimevalToNsec(ru.Utime) + syscall.TimevalToNsec(ru.Stime))
	}
	for _, depth := range []uint64{1000, 4000, 16000} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			top := depth + 20
			net := newServedChain(b, "evm:deep-fill")
			for n := uint64(0); n <= top; n++ {
				net.serve(n, testHash("a", n), testHeader(n, testHash("a", n), testHash("a", n-1)))
			}
			// testHeader's blocks are one second apart.
			start := top - depth
			var spent time.Duration
			for b.Loop() {
				ctx, cancel := context.WithCancel(context.Background())
				c := newChainTracker(ctx, net, 4, time.Duration(depth)*time.Second, "", "", "", nil, newFetchScheduler(ctx, net))
				// A tracker counts no vote before it read its voters.
				for {
					for _, src := range []string{"a", "b"} {
						c.Observe(src, testHeader(top, testHash("a", top), testHash("a", top-1)), true)
					}
					c.Sync(ctx)
					if _, _, ok := c.FollowedRange(); ok {
						break
					}
					time.Sleep(time.Millisecond)
				}
				was := cpu()
				release := c.Want(indexer.ReadOpts{Resumable: true})
				for {
					if from, _, _ := c.FollowedRange(); uint64(from) <= start {
						break
					}
					time.Sleep(time.Millisecond)
				}
				spent += cpu() - was
				release()
				cancel()
			}
			b.ReportMetric(float64(spent.Nanoseconds())/float64(b.N)/float64(depth), "cpu-ns/header")
		})
	}
}

// Eviction below a deep maxBackfill window (spec §9): a pin with no
// timestamp, or one newer than the head, drops once it leaves reorgWindow,
// even above pins maxBackfill keeps; a dropped header is no longer
// reachable from the tracker; and pins that stay unlinked count on every
// move.
func TestChainTracker_DeepWindow(t *testing.T) {
	const base = uint64(1_700_000_000)
	header := func(n, ts uint64) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"number":"0x%x","hash":"%s","parentHash":"%s","timestamp":"0x%x"}`, n, testHash("a", n), testHash("a", n-1), ts))
	}
	t.Run("odd timestamps drop and dropped headers are freed", func(t *testing.T) {
		ctx := context.Background()
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 4, 100*time.Second, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
		ts := func(n uint64) uint64 {
			switch n {
			case 250:
				return 0
			case 260:
				return base + 10_000
			}
			return base + n
		}
		vote := func(from, to uint64) {
			for n := from; n <= to; n++ {
				c.Observe("a", header(n, ts(n)), true)
				c.Sync(ctx)
			}
		}
		vote(1, 200)
		var freed []weak.Pointer[node]
		for n := uint64(110); n < 200; n++ {
			var k [32]byte
			require.True(t, decodeHex32(testHash("a", n), &k))
			require.NotNil(t, c.nodes[k], "block %d held", n)
			freed = append(freed, weak.Make(c.nodes[k]))
		}
		vote(201, 300)
		for _, tc := range []struct {
			n    uint64
			held bool
		}{{150, false}, {250, false}, {260, false}, {270, true}, {300, true}} {
			_, held := c.HeldNumber(testHash("a", tc.n))
			assert.Equal(t, tc.held, held, "block %d held", tc.n)
		}
		runtime.GC()
		for i, w := range freed {
			assert.Nil(t, w.Value(), "block %d's dropped node is freed", 110+i)
		}
	})
	t.Run("unlinked pins count on every move", func(t *testing.T) {
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 4, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
		hs := make([]*chainHeader, 6)
		for n := uint64(1); n < 6; n++ {
			h, err := parseChainHeader(header(n, base+n))
			require.NoError(t, err)
			hs[n] = h
		}
		bad, err := parseChainHeader(json.RawMessage(fmt.Sprintf(`{"number":"0x2","hash":"%s","parentHash":"%s","timestamp":"0x%x"}`, testHash("b", 2), testHash("b", 1), base+2)))
		require.NoError(t, err)
		counted := func() float64 {
			return testutil.ToFloat64(telemetry.MetricChainTrackerInvariantViolationsTotal.WithLabelValues(c.projectId(), c.networkLabel(), string(common.InvariantPinsUnlinked)))
		}
		state := func(pins ...*chainHeader) *feedState {
			return &feedState{base: pins[0].number, pins: pins, view: -1, changed: make(chan struct{})}
		}
		was := counted()
		s1 := state(hs[1], bad, hs[3])
		c.published(&feedState{view: -1}, s1, true)
		s2 := state(hs[1], bad, hs[3], hs[4])
		c.published(s1, s2, true)
		s3 := state(hs[1], hs[2], hs[3], hs[4], hs[5])
		c.published(s2, s3, true)
		assert.Equal(t, 2.0, counted()-was, "the bad link counts on both moves that hold it, not after")
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
		_, err := parseReceipts([]byte(body), hash, nil, false, false)
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
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
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
		c := newChainTracker(t.Context(), &logOnlyNetwork{}, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
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

func (n *settleNetwork) EvmHashVoters(string, time.Duration) HashVoters { return n.hv }

func (n *settleNetwork) EvmAskHead(_ context.Context, id string, need common.HeadNeed, _ time.Duration, _ time.Time, _ common.AskReason) common.HeadAnswer {
	n.smu.Lock()
	if need == common.HeadNeedFork {
		n.forks++
	}
	h := n.latest[id]
	n.smu.Unlock()
	sent := time.Now()
	return common.HeadAnswer{Outcome: common.AskFetched, At: sent, Next: sent.Add(time.Hour), Header: askedHeader(h)}
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
		c.Sync(context.Background())
		v, _ := c.HashAt(11)
		return v == testHash("aa", 11)
	}, 5*time.Second, 5*time.Millisecond, "3 non-voters' answers settle the 1-vs-1 tie")

	z := testHeader(11, testHash("cc", 11), testHash("0", 10))
	c.Observe("a", z, true)
	c.Observe("b", z, true)
	c.Sync(context.Background())
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

func (n *rewindNetwork) EvmHashVoters(string, time.Duration) HashVoters { return n.hv }

func (n *rewindNetwork) EvmAskHead(context.Context, string, common.HeadNeed, time.Duration, time.Time, common.AskReason) common.HeadAnswer {
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
	c := newChainTracker(t.Context(), net, 64, 0, "", "", "", nil, newFetchScheduler(t.Context(), &logOnlyNetwork{}))
	beforeGap := time.Now().Add(-2 * time.Minute)
	voteAt := func(at time.Time, src, tag string, from, to uint64, parentOfFirst string) {
		parent := parentOfFirst
		for n := from; n <= to; n++ {
			h, err := parseChainHeader(testHeader(n, testHash(tag, n), parent))
			require.NoError(t, err)
			c.observeSource(src, h, at, HeaderPolled)
			parent = h.hashHex
		}
		c.Sync(context.Background())
	}
	pinned := func(n int64, want string) {
		t.Helper()
		require.Eventually(t, func() bool {
			c.Sync(context.Background())
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
	c.Sync(context.Background())
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
	require.Eventually(t, func() bool { c.Sync(context.Background()); return len(net.rewinds()) == 3 }, 5*time.Second, 5*time.Millisecond)
	c.Observe("p", fork, true)
	require.Eventually(t, func() bool { c.Sync(context.Background()); return len(net.rewinds()) == 4 }, 5*time.Second, 5*time.Millisecond,
		"a voter with no vote at the switch is rewound once its fresh vote reaches the pins")
	c.Sync(context.Background())
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
	require.Eventually(t, func() bool { c.Sync(context.Background()); return net.forkAsks() == 3 }, 5*time.Second, 5*time.Millisecond,
		"c's vote off the pins refreshes a, b and d first")
	net.smu.Lock()
	net.latest["d"] = off("ee")
	net.smu.Unlock()
	c.Observe("d", off("ee"), true)
	require.Eventually(t, func() bool { c.Sync(context.Background()); return net.forkAsks() == 5 }, 5*time.Second, 5*time.Millisecond,
		"d's vote on another branch refreshes a and b")
	// c's next head on its branch is newer evidence than every vote
	// behind the pins, yet its fork point is the one refreshed.
	c.Observe("c", testHeader(11, testHash("dd", 11), testHash("dd", 10)), true)
	for range 10 {
		c.VotersChanged()
		c.Sync(context.Background())
		time.Sleep(5 * time.Millisecond)
	}
	assert.Equal(t, 5, net.forkAsks(), "c's refresh at its fork point already ran")
	v, _ := c.HashAt(10)
	assert.Equal(t, testHash("0", 10), v, "the refreshed votes keep the pins")
}

// null makes the chain answer each height in ns as Lotus answers a null
// round.
func (n *servedChain) null(t testing.TB, ns ...uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, x := range ns {
		n.errs[fmt.Sprintf("0x%x", x)] = lotusNull(t, x)
	}
}

// askedOf is every fetch of refs, in order.
func (n *servedChain) askedOf(refs ...string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, r := range n.asked {
		if slices.Contains(refs, r) {
			out = append(out, r)
		}
	}
	return out
}

// onTracker runs fn on the tracker's goroutine and waits for it.
func onTracker(c *ChainTracker, fn func()) {
	done := make(chan struct{})
	c.mb.put(trackerMsg{kind: msgCall, fn: func() { fn(); close(done) }})
	<-done
}

func TestNullRounds_Links(t *testing.T) {
	cases := []struct {
		name  string
		p, h  uint64
		nulls nullRounds
		links bool
	}{
		{"the next height", 10, 11, nil, true},
		{"across one null round", 9, 11, nullRounds{10}, true},
		{"across two null rounds", 8, 11, nullRounds{9, 10}, true},
		{"a height between not known to be null", 8, 11, nullRounds{10}, false},
		{"no null round between", 8, 11, nil, false},
		// One fork's node answered null at p's own height; the block another
		// branch holds there is still h's parent.
		{"a null round at p's own height", 10, 11, nullRounds{10}, true},
		{"a null round at p's height and between", 9, 11, nullRounds{9, 10}, true},
		{"null rounds outside the gap", 9, 11, nullRounds{5, 10, 11, 12}, true},
		{"a parent at h's height", 11, 11, nullRounds{10}, false},
		{"a parent above h", 12, 11, nil, false},
		// U12: a voter's tip header under a number 30M ahead.
		{"a header far ahead of its held parent", 21, 30_000_021, nullRounds{22, 23, 24}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.nulls.links(&chainHeader{number: tc.p}, &chainHeader{number: tc.h})
			assert.Equal(t, tc.links, got)
		})
	}
}

func TestPosition(t *testing.T) {
	run := func(ns ...uint64) []*chainHeader {
		out := make([]*chainHeader, len(ns))
		for i, n := range ns {
			out[i] = &chainHeader{number: n}
		}
		return out
	}
	dense := run(10, 11, 12, 13, 14)
	gapped := run(10, 11, 13, 16, 17) // 12, 14 and 15 are null rounds
	cases := []struct {
		name string
		run  []*chainHeader
		n    uint64
		i    int
		ok   bool
	}{
		{"dense: the first", dense, 10, 0, true},
		{"dense: inside", dense, 12, 2, true},
		{"dense: the last", dense, 14, 4, true},
		{"dense: below", dense, 9, 0, false},
		{"dense: above", dense, 15, 5, false},
		{"gapped: below the gap", gapped, 11, 1, true},
		{"gapped: a null height", gapped, 12, 2, false},
		{"gapped: above one null round", gapped, 13, 2, true},
		{"gapped: inside two null rounds", gapped, 15, 3, false},
		{"gapped: above two null rounds", gapped, 16, 3, true},
		{"gapped: the last", gapped, 17, 4, true},
		{"empty", nil, 10, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i, ok := position(tc.run, tc.n)
			assert.Equal(t, tc.i, i, "index")
			assert.Equal(t, tc.ok, ok, "found")
		})
	}
}

// A vote whose held parent sits more than one height below it asks its
// sender the heights between, one at a time from the top (spec §6.5 rule
// 1, §19 U12).
func TestChainTracker_NullRoundProbe(t *testing.T) {
	ctx := context.Background()
	newTracker := func(t *testing.T, id string) (*servedChain, *ChainTracker) {
		net := newServedChain(t, id)
		c := newChainTracker(t.Context(), net, 16, 0, "", "", "", nil, newFetchScheduler(t.Context(), net))
		voteChain(t, c, "a", "a", 1, 10, testHash("f", 0))
		voteChain(t, c, "b", "a", 1, 10, testHash("f", 0))
		return net, c
	}

	t.Run("links across null rounds, asked from the top", func(t *testing.T) {
		net, c := newTracker(t, "evm:null-probe")
		net.null(t, 11, 12)
		h13 := testHeader(13, testHash("a", 13), testHash("a", 10))
		c.Observe("a", h13, true)
		c.Observe("b", h13, true)
		require.Eventually(t, func() bool {
			c.Sync(ctx)
			v, ok := c.HashAt(13)
			return ok && v == testHash("a", 13)
		}, 5*time.Second, 10*time.Millisecond, "13 links to 10 across 11 and 12")
		assert.Equal(t, []string{"0xc", "0xb"}, net.askedOf("0xb", "0xc"), "each height asked once, from the top")
		v, ok := c.HashAt(10)
		require.True(t, ok)
		assert.Equal(t, testHash("a", 10), v)
	})

	t.Run("a block between ends the probe; the header asks nothing again", func(t *testing.T) {
		net, c := newTracker(t, "evm:null-probe-block")
		net.null(t, 12)
		net.serve(11, testHash("c", 11), testHeader(11, testHash("c", 11), testHash("a", 10)))
		bogus := testHash("d", 13)
		c.Observe("a", testHeader(13, bogus, testHash("a", 10)), true)
		require.Eventually(t, func() bool {
			var judged bool
			onTracker(c, func() { _, judged = c.notGaps[[32]byte(gethcommon.HexToHash(bogus))] })
			return judged
		}, 5*time.Second, 10*time.Millisecond, "the probe judges 13 no gap of null rounds")
		assert.Equal(t, []string{"0xc", "0xb"}, net.askedOf("0xb", "0xc"))
		_, ok := c.HashAt(13)
		assert.False(t, ok, "13 does not link to 10")

		c.Observe("a", testHeader(13, bogus, testHash("a", 10)), true)
		c.Sync(ctx)
		assert.Equal(t, []string{"0xc", "0xb"}, net.askedOf("0xb", "0xc"), "the judged header's next vote asks nothing")

		// The window moves past both: the null round and the judgment go.
		voteChain(t, c, "a", "a", 11, 60, testHash("a", 10))
		voteChain(t, c, "b", "a", 60, 60, testHash("a", 59))
		var nulls nullRounds
		var judged int
		lowest := uint64(math.MaxUint64)
		onTracker(c, func() {
			nulls, judged = c.nulls, len(c.notGaps)
			for r := range c.exts {
				lowest = min(lowest, r.h.number)
			}
		})
		require.Greater(t, lowest, uint64(12+16), "the lowest root leaves 12 a window below")
		assert.Empty(t, nulls, "null rounds below the window are pruned")
		assert.Zero(t, judged, "judged headers below the window are pruned")
	})
}
