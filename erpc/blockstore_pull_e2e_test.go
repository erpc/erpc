package erpc

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

// withResponseCache adds an in-memory evmJsonRpcCache connector and policies
// (finalized and unfinalized) next to the block store's Redis connector.
func withResponseCache(cfg *common.Config) *common.Config {
	if cfg.Database == nil {
		cfg.Database = &common.DatabaseConfig{}
	}
	if cfg.Database.EvmJsonRpcCache == nil {
		cfg.Database.EvmJsonRpcCache = &common.CacheConfig{}
	}
	cc := cfg.Database.EvmJsonRpcCache
	cc.Connectors = append(cc.Connectors, &common.ConnectorConfig{
		Id: "resp-mem", Driver: common.DriverMemory,
		Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
	})
	for _, fin := range []common.DataFinalityState{common.DataFinalityStateFinalized, common.DataFinalityStateUnfinalized} {
		cc.Policies = append(cc.Policies, &common.CachePolicyConfig{
			Network: "*", Method: "*", Finality: fin, Connector: "resp-mem",
			TTL: common.FixedDuration(time.Minute),
		})
	}
	return cfg
}

// Block-store fetches read through (and write) eRPC's response cache: once a
// header is in the cache, the store's own fetch of it makes no upstream call.
func TestHttp_BlockStore_FetchReadsThroughResponseCache(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := withResponseCache(blockStoreTestConfig(up.URL(), &common.EvmBlockStoreConfig{
		Enabled: true, Depth: 4, PollInterval: common.Duration(100 * time.Millisecond),
	}))
	send, _, _, shutdown, instance := createServerTestFixtures(cfg, t)
	defer shutdown()
	prj, err := instance.GetProject("test_project")
	require.NoError(t, err)
	nw, err := prj.GetNetwork(t.Context(), "evm:123")
	require.NoError(t, err)
	f := &networkHeadFetcher{n: nw}

	// A client request puts eth_getBlockByNumber(10,false) in the response cache.
	client := doRpc(t, send, "eth_getBlockByNumber", `["0xa",false]`)
	require.Contains(t, string(client.Result), up.HashAt(10))
	require.Equal(t, int64(1), up.BlockCalls(10))

	// The cache write is asynchronous; let it land, then the store's fetch is free.
	time.Sleep(500 * time.Millisecond)
	raw, err := f.HeaderByNumber(t.Context(), 10)
	require.NoError(t, err)
	require.JSONEq(t, string(client.Result), string(raw))
	require.Equal(t, int64(1), up.BlockCalls(10), "the store's header fetch must be served from the response cache")
}
