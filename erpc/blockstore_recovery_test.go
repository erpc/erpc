package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/blockstore"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

type recoveryHeaderFetcher struct {
	blockstore.Fetcher
	up      *scriptedEvmUpstream
	calls   atomic.Int64
	failTip atomic.Bool
}

func (f *recoveryHeaderFetcher) HeaderByNumber(_ context.Context, n int64) (json.RawMessage, error) {
	f.calls.Add(1)
	if n == 21 && f.failTip.Load() {
		return nil, fmt.Errorf("tip not available")
	}
	f.up.mu.Lock()
	defer f.up.mu.Unlock()
	return json.Marshal(f.up.blockLocked(n, false))
}

// Unlike the in-memory fleet tests, this proves the Redis snapshot key survives
// a lease expiry, while stale recovery metadata still cannot authorize serving.
func TestBlockStoreRedisHandoverRetainsExpiredServingSnapshot(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := t.Context()
	cfg := &common.RedisConnectorConfig{URI: "redis://" + mr.Addr()}
	require.NoError(t, cfg.SetDefaults())
	logger := zerolog.New(io.Discard)
	rc, err := data.NewRedisConnector(ctx, &logger, "header-recovery", cfg)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return rc.Client() != nil }, 3*time.Second, 10*time.Millisecond)
	store := &blockStoreConnectorStore{connector: rc, redis: rc}
	up := newScriptedEvmUpstream(123, 21)
	defer up.Close()
	fetch := &recoveryHeaderFetcher{up: up}
	tip := int64(20)
	head := func(context.Context) int64 { return tip }
	opts := blockstore.Options{Scope: blockstore.Scope{Namespace: "recovery", ProjectId: "p", NetworkId: "evm:123"},
		Depth: 8, MaxPerTick: 8, PollInterval: time.Second, MaxStaleness: 2 * time.Second, RecordTTL: time.Hour}
	a := blockstore.New(opts, store, fetch, head, &logger)
	defer a.Stop()
	a.Tick(ctx)
	require.Equal(t, int64(20), a.Head())
	require.Equal(t, int64(8), fetch.calls.Load())
	// Expire A's lease and what used to be the snapshot TTL. Explicitly age
	// Snapshot.At too: miniredis advances Redis TTLs, not the process clock.
	_, key, err := store.fleetKeys(opts.Scope)
	require.NoError(t, err)
	raw, err := mr.Get(key)
	require.NoError(t, err)
	var snap blockstore.Snapshot
	require.NoError(t, json.Unmarshal([]byte(raw), &snap))
	snap.At = time.Now().Add(-time.Minute)
	aged, err := json.Marshal(snap)
	require.NoError(t, err)
	ttl := mr.TTL(key)
	require.NoError(t, mr.Set(key, string(aged)))
	mr.SetTTL(key, ttl)
	mr.FastForward(4 * time.Second)
	tip = 21
	fetch.failTip.Store(true)
	b := blockstore.New(opts, store, fetch, head, &logger)
	defer b.Stop()
	b.Tick(ctx)
	require.False(t, b.Fresh(), "stale recovery headers cannot authorize serving")
	require.Equal(t, int64(9), fetch.calls.Load(), "only unavailable tip is fetched")
	fetch.failTip.Store(false)
	b.Tick(ctx)
	require.Equal(t, int64(21), b.Head())
	require.Equal(t, int64(10), fetch.calls.Load(), "recovery extends the Redis window rather than backfilling it")
}
