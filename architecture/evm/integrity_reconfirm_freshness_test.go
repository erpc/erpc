package evm

import (
	"context"
	"fmt"
	"sync"
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
}

func newCapturingNetwork(t *testing.T, number int64, hash string) *capturingNetwork {
	t.Helper()
	n := &capturingNetwork{hash: hash, num: fmt.Sprintf("0x%x", number)}
	n.On("Forward", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			n.mu.Lock()
			n.dirs = append(n.dirs, req.Directives())
			n.calls++
			n.mu.Unlock()
			body := fmt.Sprintf(`{"number":"%s","hash":"%s","parentHash":"0xpar"}`, n.num, n.hash)
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
	orphan, canonical := "0xorphanfork", "0xcanonicalfork"

	t.Run("the re-confirmation bypasses the cache", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := newChainView(net, 32, "", "", nil)
		c.observe(height, orphan, nil) // pin holds the losing fork

		hash, status := c.ReconfirmPin(context.Background(), height)

		require.Equal(t, integrity.PinFresh, status)
		assert.Equal(t, canonical, hash, "must report what the chain says now, not the pin")
		d := net.lastDirectives()
		require.NotNil(t, d)
		assert.Equal(t, "true", d.SkipCacheRead,
			"a pin re-confirmation that reads the cache is an echo of the disputed value, not corroboration")
	})

	t.Run("and the refreshed pin adopts the current fork", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := newChainView(net, 32, "", "", nil)
		c.observe(height, orphan, nil)
		c.observe(height+1, "0xdescendant", nil) // built on the orphan

		_, status := c.ReconfirmPin(context.Background(), height)
		require.Equal(t, integrity.PinFresh, status)

		got, ok := c.HashAt(height)
		require.True(t, ok)
		assert.Equal(t, canonical, got, "the pin must adopt the fork the network actually serves")
		_, stillThere := c.HashAt(height + 1)
		assert.False(t, stillThere, "descendants of the abandoned fork must be rolled back")
	})

	// The bypass is scoped to the tie-break. Ordinary corroboration fetches stay
	// cache-backed: they are pure cost, and starving that cache once collapsed
	// polygon's corroboration hit-rate.
	t.Run("ordinary corroboration fetches still use the cache", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := newChainView(net, 32, "", "", nil)

		_, ok := c.headerByNumber(context.Background(), height, fmt.Sprintf("0x%x", height))
		require.True(t, ok)
		d := net.lastDirectives()
		require.NotNil(t, d)
		assert.Empty(t, d.SkipCacheRead, "corroboration reads should not force a cache miss")
	})

	t.Run("by-hash header fetches still use the cache", func(t *testing.T) {
		net := newCapturingNetwork(t, height, canonical)
		c := newChainView(net, 32, "", "", nil)

		_, _ = c.headerByHash(context.Background(), canonical)
		if d := net.lastDirectives(); d != nil {
			assert.Empty(t, d.SkipCacheRead,
				"a header is content-addressed by hash, so the cache can never be stale for it")
		}
	})
}
