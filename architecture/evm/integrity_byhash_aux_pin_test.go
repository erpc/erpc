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
				ref = "0xcanonicalhash"
			}
			body := fmt.Sprintf(`{"number":"%s","hash":"%s","parentHash":"0xpar"}`, num, ref)
			return common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`"1"`), []byte(body), nil),
			), nil
		},
		nil,
	).Maybe()
	n.On("Id").Return("evm:1").Maybe()
	n.On("ProjectId").Return("test").Maybe()
	return n
}

// A by-hash corroboration fetch must NOT move the number→hash pin.
//
// Fetching a block BY HASH answers "give me this exact block" — and that block
// may legitimately be an orphan, because unwinding a reorg by asking for
// orphaned hashes is normal indexer behaviour. Pinning from it adopts the
// orphan as the chain's answer for that height, which then fails continuity on
// every honest by-number response. observeBlockView has applied this rule to
// client responses since the by-hash scoping fix; the aux fetch path had the
// same hole.
func TestByHashAuxFetchDoesNotMoveThePin(t *testing.T) {
	const heightHex = "0x2a"
	const height = int64(42)
	orphan := "0x" + fmt.Sprintf("%064x", 0xbadf00d)

	t.Run("an orphan resolved by hash leaves the pin untouched", func(t *testing.T) {
		net := newByRefNetwork(t)
		net.byHash[orphan] = heightHex // the orphan claims height 42
		c := newChainView(net, 32, "", "", nil)
		c.observe(height, "0xcanonicalhash", nil) // the fork we are following

		h, ok := c.headerByHash(context.Background(), orphan)
		require.True(t, ok)
		require.Equal(t, orphan, h.Hash)

		pin, known := c.HashAt(height)
		require.True(t, known)
		assert.Equal(t, "0xcanonicalhash", pin,
			"a block fetched by hash must not redefine what the chain is at its height")
	})

	t.Run("the orphan header is still cached for corroboration", func(t *testing.T) {
		net := newByRefNetwork(t)
		net.byHash[orphan] = heightHex
		c := newChainView(net, 32, "", "", nil)

		_, ok := c.headerByHash(context.Background(), orphan)
		require.True(t, ok)

		c.mu.RLock()
		_, cached := c.headers[orphan]
		c.mu.RUnlock()
		assert.True(t, cached, "the header is content-addressed and immutable — caching it is always safe")
	})

	t.Run("a by-number fetch still pins, since it answers what the chain is", func(t *testing.T) {
		net := newByRefNetwork(t)
		c := newChainView(net, 32, "", "", nil)

		_, ok := c.headerByNumber(context.Background(), height, heightHex)
		require.True(t, ok)

		pin, known := c.HashAt(height)
		require.True(t, known)
		assert.Equal(t, "0xcanonicalhash", pin)
	})
}
