package erpc

import (
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

func TestBlockStore_FingerprintIncludesProviderAndDefaultTrust(t *testing.T) {
	network := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 1}}
	project := &common.ProjectConfig{
		Upstreams:        []*common.UpstreamConfig{{Id: "one", Endpoint: "https://rpc.invalid/key-a"}, {Id: "two", Endpoint: "https://rpc.invalid/key-b"}},
		Providers:        []*common.ProviderConfig{{Id: "provider", Vendor: "arbitrary-future-vendor", Settings: common.VendorSettings{"apiKey": "secret-a"}, Overrides: map[string]*common.UpstreamConfig{"evm:*": {Endpoint: "https://override.invalid/key-a"}}}},
		NetworkDefaults:  &common.NetworkDefaults{SelectionPolicy: &common.SelectionPolicyConfig{EvalFunc: "upstreams => upstreams"}},
		UpstreamDefaults: &common.UpstreamConfig{Id: "defaults", Endpoint: "https://default.invalid/key-a"},
	}
	fingerprint := func() string {
		hash, err := blockStoreFingerprint(project, network)
		require.NoError(t, err)
		require.Len(t, hash, 64)
		require.NotContains(t, hash, "secret")
		return hash
	}
	original := fingerprint()
	project.Upstreams[0], project.Upstreams[1] = project.Upstreams[1], project.Upstreams[0]
	require.Equal(t, original, fingerprint(), "set ordering is irrelevant")
	project.Providers[0].Settings["apiKey"] = "secret-b"
	require.NotEqual(t, original, fingerprint(), "provider settings must not be display-redacted before hashing")
	previous := fingerprint()
	project.Providers[0].Overrides["evm:*"].Endpoint = "https://override.invalid/key-b"
	require.NotEqual(t, previous, fingerprint())
	previous = fingerprint()
	project.Providers[0].IgnoreNetworks = []string{"evm:1"}
	require.NotEqual(t, previous, fingerprint())
	previous = fingerprint()
	project.NetworkDefaults.SelectionPolicy.EvalFunc = "upstreams => []"
	require.NotEqual(t, previous, fingerprint())
	previous = fingerprint()
	project.UpstreamDefaults.Endpoint = "https://default.invalid/key-b"
	require.NotEqual(t, previous, fingerprint())
	previous = fingerprint()
	network.SelectionPolicy = &common.SelectionPolicyConfig{EvalFunc: "upstreams => upstreams.slice(1)"}
	require.NotEqual(t, previous, fingerprint())
}

func TestHttp_BlockStore_MalformedFiltersBypassAndTopicPositionsMatchGeth(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := blockStoreTestConfig(up.URL(), &common.EvmBlockStoreConfig{Enabled: true, Depth: 16, PollInterval: common.Duration(100 * time.Millisecond)})
	send, _, _, shutdown, instance := createServerTestFixtures(cfg, t)
	defer shutdown()
	project, err := instance.GetProject("test_project")
	require.NoError(t, err)
	network, err := project.GetNetwork(t.Context(), "evm:123")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return network.BlockStore().Head() == 20 }, 10*time.Second, 20*time.Millisecond)

	// A wildcard position still requires that position to exist in the log.
	before := up.RangeLogCalls()
	result := doRpc(t, send, "eth_getLogs", fmt.Sprintf(`[{"fromBlock":"0x12","toBlock":"0x12","topics":[%q,null]}]`, scriptedTopicEven))
	require.JSONEq(t, "[]", string(result.Result))
	require.Equal(t, before, up.RangeLogCalls())

	for _, filter := range []string{
		`{"fromBlock":"0x13","toBlock":"0x13","address":"0x1234"}`,
		`{"fromBlock":"0x13","toBlock":"0x13","address":["not-an-address"]}`,
		`{"fromBlock":"0x13","toBlock":"0x13","topics":["0x12"]}`,
		`{"fromBlock":"0x13","toBlock":"0x13","topics":[["not-hex"]]}`,
		fmt.Sprintf(`{"blockHash":%q,"toBlock":"0x13"}`, up.HashAt(19)),
		`{"blockHash":null,"fromBlock":"0x13","toBlock":"0x13"}`,
		`{"blockHash":"0x12"}`,
	} {
		t.Run(filter, func(t *testing.T) {
			request := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[` + filter + `]}`))
			_, hit := network.tryServeBlockStore(t.Context(), request, "eth_getLogs")
			require.False(t, hit, "invalid filter must reach normal upstream validation")
		})
	}
}
