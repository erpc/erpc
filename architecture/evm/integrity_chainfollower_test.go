package evm

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// fakeChain is a tiny in-memory chain the follower can walk: heights map to a
// header, and a reorg is modelled by rewriting entries at and above a height.
type fakeChain struct {
	mu       sync.Mutex
	byNumber map[int64]fakeBlock
	byHash   map[string]fakeBlock
	fetches  int
}

type fakeBlock struct {
	num    int64
	hash   string
	parent string
}

func newFakeChain() *fakeChain {
	return &fakeChain{byNumber: map[int64]fakeBlock{}, byHash: map[string]fakeBlock{}}
}

// build lays down a linear chain [from..to] whose hashes carry a fork tag, so
// two branches over the same heights have distinct hashes.
func (f *fakeChain) build(from, to int64, tag string, parentOfFirst string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prev := parentOfFirst
	for n := from; n <= to; n++ {
		h := fmt.Sprintf("0x%s%058x", tag, n)
		b := fakeBlock{num: n, hash: h, parent: prev}
		f.byNumber[n] = b
		f.byHash[h] = b
		prev = h
	}
}

func (f *fakeChain) hashAt(n int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byNumber[n].hash
}

func (f *fakeChain) network(t *testing.T) *mockNetwork {
	t.Helper()
	n := &mockNetwork{}
	n.On("Forward", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			jrq, _ := req.JsonRpcRequest()
			ref, _ := jrq.Params[0].(string)
			f.mu.Lock()
			f.fetches++
			var b fakeBlock
			var found bool
			if strings.HasPrefix(jrq.Method, "eth_getBlockByHash") {
				b, found = f.byHash[ref]
			} else {
				var num int64
				fmt.Sscanf(ref, "0x%x", &num)
				b, found = f.byNumber[num]
			}
			f.mu.Unlock()
			if !found {
				return common.NewNormalizedResponse().WithJsonRpcResponse(
					common.MustNewJsonRpcResponseFromBytes([]byte(`"1"`), []byte(`null`), nil)), nil
			}
			body := fmt.Sprintf(`{"number":"0x%x","hash":"%s","parentHash":"%s"}`, b.num, b.hash, b.parent)
			return common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`"1"`), []byte(body), nil)), nil
		},
		nil,
	).Maybe()
	n.On("Id").Return("evm:1").Maybe()
	n.On("ProjectId").Return("test").Maybe()
	n.On("Label").Return("mainnet").Maybe()
	return n
}

func followerFor(t *testing.T, chain *fakeChain, head *int64) (*chainView, *chainFollower) {
	t.Helper()
	v := newChainView(chain.network(t), 32, "", "", nil)
	f := newChainFollower(v, func(ctx context.Context) int64 { return *head }, nil)
	return v, f
}

// The follower must build a CONTIGUOUS, parent-linked chain — that is the whole
// point of following rather than passively sampling whatever traffic touches.
func TestChainFollowerBuildsALinkedChain(t *testing.T) {
	chain := newFakeChain()
	chain.build(100, 110, "a", "0xgenesis")
	head := int64(100)
	v, f := followerFor(t, chain, &head)

	f.advance(context.Background()) // bootstrap at 100
	from, to, ok := v.FollowedRange()
	require.True(t, ok)
	assert.Equal(t, int64(100), from)
	assert.Equal(t, int64(100), to)

	head = 105
	f.advance(context.Background())

	_, to, ok = v.FollowedRange()
	require.True(t, ok)
	assert.Equal(t, int64(105), to, "the follower should have walked forward to the head")

	// Every adjacent pair in the followed range must actually link.
	for n := int64(101); n <= 105; n++ {
		got, known := v.HashAt(n)
		require.True(t, known, "height %d must be held", n)
		assert.Equal(t, chain.hashAt(n), got, "height %d must hold the canonical hash", n)
	}
}

// A reorg must be reconciled the way an indexer does it: find the common
// ancestor, unwind the abandoned branch, adopt the new one.
func TestChainFollowerReconcilesAReorg(t *testing.T) {
	chain := newFakeChain()
	chain.build(100, 105, "a", "0xgenesis")
	// Bootstrap at 100 and walk up, so the follower actually holds 100..105 —
	// bootstrapping straight at the head would leave nothing beneath it to
	// reconcile against.
	head := int64(100)
	v, f := followerFor(t, chain, &head)
	f.advance(context.Background()) // bootstrap at 100
	head = 105
	f.advance(context.Background()) // walk to 105

	_, to, _ := v.FollowedRange()
	require.Equal(t, int64(105), to)
	oldHash103 := chain.hashAt(103)
	require.Equal(t, oldHash103, mustHashAt(t, v, 103))

	// Reorg: heights 103+ are replaced by a different branch rooted at 102.
	chain.build(103, 107, "b", chain.hashAt(102))
	head = 107

	f.advance(context.Background())

	_, to, ok := v.FollowedRange()
	require.True(t, ok)
	assert.Equal(t, int64(107), to, "the follower must advance onto the new branch")
	for n := int64(103); n <= 107; n++ {
		assert.Equal(t, chain.hashAt(n), mustHashAt(t, v, n),
			"height %d must hold the NEW branch's hash after reconciliation", n)
	}
	assert.Equal(t, chain.hashAt(102), mustHashAt(t, v, 102),
		"the common ancestor must be left untouched")
}

// A branch that shares no ancestor with what we follow is NOT a reorg — it is
// unrelated history. Adopting it would silently swap our verified chain for an
// unverified one, so reconciliation must refuse.
func TestChainFollowerRefusesUnrelatedHistory(t *testing.T) {
	chain := newFakeChain()
	chain.build(100, 105, "a", "0xgenesis")
	head := int64(100)
	v, f := followerFor(t, chain, &head)
	f.advance(context.Background()) // bootstrap at 100
	head = 105
	f.advance(context.Background()) // walk to 105
	require.Equal(t, int64(105), mustFollowHead(t, v))

	// An entirely different history over the same heights, rooted far below the
	// window and never joining ours.
	chain.build(60, 107, "c", "0xotherworld")
	head = 107

	f.advance(context.Background())

	// The follower must NOT have swapped onto the foreign branch: height 105
	// still holds the hash from the chain we verified block by block, not the
	// one the foreign history claims for that height.
	got := mustHashAt(t, v, 105)
	assert.True(t, strings.HasPrefix(got, "0xa"),
		"height 105 must still hold the followed branch, got %s", got)
	assert.NotEqual(t, chain.hashAt(105), got,
		"unrelated history must never be adopted as the followed chain")
}

func mustHashAt(t *testing.T, v *chainView, n int64) string {
	t.Helper()
	h, ok := v.HashAt(n)
	require.True(t, ok, "expected a pin at height %d", n)
	return h
}

func mustFollowHead(t *testing.T, v *chainView) int64 {
	t.Helper()
	_, to, ok := v.FollowedRange()
	require.True(t, ok)
	return to
}

// FollowedRange is a promise: every height in it was verified block by block.
// The consecutive-header checks decide whether a parent is trustworthy purely
// from that promise, so the range must never outlive the pins behind it.
// Eviction used to drop pins while leaving followBase pointing at the bootstrap
// height, so the range silently grew to cover heights whose pins came from
// ordinary traffic — possibly on another fork.
func TestFollowedRangeNeverClaimsEvictedHeights(t *testing.T) {
	chain := newFakeChain()
	chain.build(100, 200, "a", "0xgenesis")
	head := int64(100)
	v := newChainView(chain.network(t), 8, "", "", nil) // small window to force eviction
	f := newChainFollower(v, func(ctx context.Context) int64 { return head }, nil)

	f.advance(context.Background()) // bootstrap at 100
	for head = 105; head <= 140; head += 5 {
		f.advance(context.Background())
	}

	from, to, ok := v.FollowedRange()
	require.True(t, ok)
	assert.LessOrEqual(t, to-from, int64(v.window),
		"the claimed segment cannot be wider than the window that retains it")

	for n := from; n <= to; n++ {
		_, held := v.HashAt(n)
		assert.True(t, held, "height %d is claimed as followed but its pin was evicted", n)
	}
}
