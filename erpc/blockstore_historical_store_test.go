package erpc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/blockstore"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestBlockStoreHistoricalConnectorPartitionTTLAndKinds(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	defer m.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lg := zerolog.Nop()
	config := &common.RedisConnectorConfig{Addr: m.Addr(), InitTimeout: common.Duration(3 * time.Second), GetTimeout: common.Duration(time.Second), SetTimeout: common.Duration(time.Second)}
	require.NoError(t, config.SetDefaults())
	connector, err := data.NewRedisConnector(ctx, &lg, "historical-test", config)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return connector.Client() != nil }, 3*time.Second, 10*time.Millisecond)
	store := &blockStoreHistoricalStore{connector: connector}
	scope := blockstore.Scope{Namespace: "project:historical", ProjectId: "p", NetworkId: "n"}
	other := scope
	other.NetworkId = "other"
	key := "0xabc"

	require.Error(t, store.PutHistoricalBlock(ctx, scope, key, json.RawMessage(`{"number":"0x1"}`), 0))
	require.NoError(t, store.PutHistoricalBlock(ctx, scope, key, json.RawMessage(`{"number":"0x1"}`), time.Minute))
	require.NoError(t, store.PutFinalizedHash(ctx, scope, 1, key, time.Minute))
	finalizedHash, err := store.GetFinalizedHash(ctx, scope, 1)
	require.NoError(t, err)
	require.Equal(t, key, finalizedHash)
	block, err := store.GetHistoricalBlock(ctx, scope, key)
	require.NoError(t, err)
	require.JSONEq(t, `{"number":"0x1"}`, string(block))
	_, err = store.GetHistoricalBlock(ctx, other, key)
	require.Error(t, err)

	partition, err := store.historicalPartition(scope)
	require.NoError(t, err)
	expectedPartition, err := (&blockStoreConnectorStore{}).partition(scope)
	require.NoError(t, err)
	require.Equal(t, expectedPartition, partition)
	require.NotEqual(t, partition, mustPartition(t, blockstore.Scope{Namespace: "project", ProjectId: "p", NetworkId: "n"}))
	blockTTLKey := partition + ":" + historicalKey("block", key)
	require.Equal(t, time.Minute, m.TTL(blockTTLKey))
	m.FastForward(61 * time.Second)
	_, err = store.GetHistoricalBlock(ctx, scope, key)
	require.Error(t, err)
}

func mustPartition(t *testing.T, scope blockstore.Scope) string {
	t.Helper()
	partition, err := (&blockStoreConnectorStore{}).partition(scope)
	require.NoError(t, err)
	return partition
}

func TestBlockStoreHistoricalConnectorCorruptPayloadFailsClosed(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	defer m.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lg := zerolog.Nop()
	config := &common.RedisConnectorConfig{Addr: m.Addr(), InitTimeout: common.Duration(3 * time.Second), GetTimeout: common.Duration(time.Second), SetTimeout: common.Duration(time.Second)}
	require.NoError(t, config.SetDefaults())
	connector, err := data.NewRedisConnector(ctx, &lg, "historical-corrupt-test", config)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return connector.Client() != nil }, 3*time.Second, 10*time.Millisecond)
	store := &blockStoreHistoricalStore{connector: connector}
	scope := blockstore.Scope{Namespace: "ns", ProjectId: "p", NetworkId: "n"}
	partition, err := store.historicalPartition(scope)
	require.NoError(t, err)
	bad := []byte(`{"kind":"block","key":"wrong","payload":{"number":"0x1"}}`)
	require.NoError(t, connector.Set(context.Background(), partition, historicalKey("block", "expected"), bad, nil))
	_, err = store.GetHistoricalBlock(context.Background(), scope, "expected")
	require.Error(t, err)
	require.NoError(t, connector.Set(context.Background(), partition, historicalKey("finalized", "7"), []byte(`{"kind":"finalized","height":8,"hash":"0xabc"}`), nil))
	_, err = store.GetFinalizedHash(context.Background(), scope, 7)
	require.Error(t, err)
}
