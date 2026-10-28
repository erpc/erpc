package erpc

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The methods eRPC answers itself, over HTTP, against one upstream that
// answers each of them with its own (different) value: on by default with no
// upstream call, routed to the upstream once a network turns them off, and
// routed when a request skips the cache read.
func TestHttpServer_LocalAnswers(t *testing.T) {
	upstreamAnswers := map[string]string{
		"eth_chainId":        `"0x7b"`,
		"net_version":        `"7777"`,
		"net_listening":      `false`,
		"net_peerCount":      `"0x40"`,
		"web3_clientVersion": `"Geth/v1.14.0"`,
		"eth_syncing":        `{"startingBlock":"0x0","currentBlock":"0x1","highestBlock":"0x2"}`,
	}
	localAnswers := map[string]any{
		"eth_chainId":   "0x7b",
		"net_version":   "123",
		"net_listening": true,
		"net_peerCount": "0x1",
		"eth_syncing":   false,
	}
	require.ElementsMatch(t, common.LocalAnswerMethods(), keys(upstreamAnswers), "every local answer is covered")

	var mu sync.Mutex
	calls := map[string]int{}
	callsOf := func(method string) int {
		mu.Lock()
		defer mu.Unlock()
		return calls[method]
	}
	mockUpstream := func() {
		util.ResetGock()
		for method, result := range upstreamAnswers {
			gock.New("http://rpc1.localhost").Post("").Persist().
				Filter(func(r *http.Request) bool {
					// gock filters see every outgoing request, the test client's too.
					if r.URL.Host != "rpc1.localhost" || !strings.Contains(util.SafeReadBody(r), `"`+method+`"`) {
						return false
					}
					mu.Lock()
					calls[method]++
					mu.Unlock()
					return true
				}).
				Reply(200).
				JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":` + result + `}`))
		}
		gock.New("http://rpc1.localhost").Post("").Persist().
			Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber") }).
			Reply(200).
			JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x100","timestamp":"0x6702a8f0"}}`))
	}
	config := func(off map[string]bool) *common.Config {
		return &common.Config{
			Server: &common.ServerConfig{MaxTimeout: common.Duration(5 * time.Second).Ptr()},
			Projects: []*common.ProjectConfig{{
				Id: "test_project",
				Networks: []*common.NetworkConfig{{
					Architecture: common.ArchitectureEvm,
					Evm:          &common.EvmNetworkConfig{ChainId: 123, LocalAnswers: off},
				}},
				Upstreams: []*common.UpstreamConfig{{
					Id:       "rpc1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId:             123,
						StatePollerInterval: common.Duration(time.Hour),
						SkipSyncingCheck:    util.BoolPtr(true),
					},
				}},
			}},
		}
	}
	call := func(t *testing.T, send func(string, map[string]string, map[string]string) (int, map[string]string, string), method string, headers map[string]string) (result any, upstreamCalls int) {
		t.Helper()
		before := callsOf(method)
		status, _, body := send(`{"jsonrpc":"2.0","id":7,"method":"`+method+`","params":[]}`, headers, nil)
		require.Equal(t, http.StatusOK, status, "%s: %s", method, body)
		var parsed map[string]any
		require.NoError(t, sonic.UnmarshalString(body, &parsed), "%s: %s", method, body)
		require.Nil(t, parsed["error"], "%s: %s", method, body)
		return parsed["result"], callsOf(method) - before
	}
	upstreamValue := func(t *testing.T, method string) any {
		var v any
		require.NoError(t, sonic.UnmarshalString(upstreamAnswers[method], &v))
		return v
	}

	t.Run("on by default: eRPC answers, no upstream call", func(t *testing.T) {
		mockUpstream()
		defer util.ResetGock()
		send, _, _, shutdown, _ := createServerTestFixtures(config(nil), t)
		defer shutdown()

		for method := range upstreamAnswers {
			got, n := call(t, send, method, nil)
			assert.Zero(t, n, "%s must not reach the upstream", method)
			if method == "web3_clientVersion" {
				assert.True(t, strings.HasPrefix(got.(string), "erpc/"), "%s = %v", method, got)
				continue
			}
			assert.Equal(t, localAnswers[method], got, method)
		}

		got, n := call(t, send, "net_peerCount", map[string]string{"X-ERPC-Skip-Cache-Read": "true"})
		assert.Equal(t, 1, n, "skip-cache-read asks the upstream")
		assert.Equal(t, upstreamValue(t, "net_peerCount"), got)
	})

	t.Run("off per method: routed to the upstream", func(t *testing.T) {
		mockUpstream()
		defer util.ResetGock()
		off := map[string]bool{}
		for method := range upstreamAnswers {
			off[method] = false
		}
		send, _, _, shutdown, _ := createServerTestFixtures(config(off), t)
		defer shutdown()

		for method := range upstreamAnswers {
			got, n := call(t, send, method, nil)
			assert.Equal(t, 1, n, "%s must reach the upstream", method)
			assert.Equal(t, upstreamValue(t, method), got, method)
		}
	})

	t.Run("a network staticResponse wins over the local answer", func(t *testing.T) {
		mockUpstream()
		defer util.ResetGock()
		cfg := config(nil)
		cfg.Projects[0].Networks[0].StaticResponses = []*common.StaticResponseConfig{
			{Method: "web3_clientVersion", Response: &common.StaticResponseBodyConfig{Result: "my-chain/v1"}},
		}
		send, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		got, n := call(t, send, "web3_clientVersion", nil)
		assert.Zero(t, n)
		assert.Equal(t, "my-chain/v1", got)

		got, n = call(t, send, "net_version", nil)
		assert.Zero(t, n)
		assert.Equal(t, localAnswers["net_version"], got, "methods without a static response keep the local answer")
	})

	t.Run("a method without a local answer is refused in config", func(t *testing.T) {
		cfg := config(map[string]bool{"eth_getBalance": false})
		require.NoError(t, cfg.SetDefaults(nil))
		err := cfg.Validate()
		require.ErrorContains(t, err, "localAnswers.eth_getBalance")
	})
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
