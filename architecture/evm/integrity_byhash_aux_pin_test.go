package evm

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var byRefCanonicalHash = testHash("c", 42)

// byRefNetwork answers each aux fetch with a header chosen by the request's
// block ref, so a by-hash fetch can return a block that is an ORPHAN at its
// height while a by-number fetch returns the canonical one.
type byRefNetwork struct {
	mockNetwork
	mu      sync.Mutex
	byHash  map[string]string // hash -> number hex
	methods []string
}

func newByRefNetwork(t *testing.T) *byRefNetwork {
	t.Helper()
	n := &byRefNetwork{byHash: map[string]string{}}
	n.On("Forward", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			jrq, _ := req.JsonRpcRequest()
			method, _ := jrq.Method, 0
			ref, _ := jrq.Params[0].(string)
			n.mu.Lock()
			n.methods = append(n.methods, method)
			num, known := n.byHash[ref]
			n.mu.Unlock()
			if !known { // a by-number request: echo that number back
				num = ref
				ref = byRefCanonicalHash
			}
			body := fmt.Sprintf(`{"number":"%s","hash":"%s","parentHash":"%s"}`, num, ref, testHash("f", 41))
			return common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`"1"`), []byte(body), nil),
			), nil
		},
		nil,
	).Maybe()
	n.On("Id").Return("evm:1").Maybe()
	n.On("ProjectId").Return("test").Maybe()
	// No served tip here: latest never caps the delivery gate.
	n.On("HeadLatest", mock.Anything).Return(int64(1) << 62).Maybe()
	return n
}

// An aux fetch is an answer: it is held, and never moves the pin. Fetching
// a block BY HASH may legitimately return an orphan, because unwinding a
// reorg by asking for orphaned hashes is normal indexer behaviour; only
// votes move pins.
func TestByHashAuxFetchDoesNotMoveThePin(t *testing.T) {
	const heightHex = "0x2a"
	const height = int64(42)
	orphan := "0x" + fmt.Sprintf("%064x", 0xbadf00d)

	t.Run("an orphan resolved by hash leaves the pin untouched", func(t *testing.T) {
		net := newByRefNetwork(t)
		net.byHash[orphan] = heightHex // the orphan claims height 42
		c := newChainTracker(t.Context(), net, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), net))
		voteChain(t, c, "a", "c", 42, 42, testHash("f", 41)) // the fork we are following

		h, ok := c.headerByHash(context.Background(), orphan)
		require.True(t, ok)
		require.Equal(t, orphan, h.Hash)
		c.sync(context.Background())

		pin, known := c.HashAt(height)
		require.True(t, known)
		assert.Equal(t, byRefCanonicalHash, pin,
			"a block fetched by hash must not redefine what the chain is at its height")
	})

	t.Run("the orphan header is still held for corroboration", func(t *testing.T) {
		net := newByRefNetwork(t)
		net.byHash[orphan] = heightHex
		c := newChainTracker(t.Context(), net, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), net))

		_, ok := c.headerByHash(context.Background(), orphan)
		require.True(t, ok)
		c.sync(context.Background())

		_, held := c.held.Load(orphan)
		assert.True(t, held, "the header is content-addressed and immutable — holding it is always safe")
	})

	t.Run("a by-number fetch is held but does not pin", func(t *testing.T) {
		net := newByRefNetwork(t)
		c := newChainTracker(t.Context(), net, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), net))

		_, ok := c.headerByNumber(context.Background(), height, heightHex)
		require.True(t, ok)
		c.sync(context.Background())

		_, known := c.HashAt(height)
		assert.False(t, known, "only votes move pins")
		_, held := c.held.Load(byRefCanonicalHash)
		assert.True(t, held)
	})
}
