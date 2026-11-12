package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ethCallBodies records the raw body of every eth_call one mocked upstream
// host receives. gock runs a mock's filters before its host matcher, so the
// filter checks the host itself.
type ethCallBodies struct {
	host   string
	mu     sync.Mutex
	bodies []string
}

func (c *ethCallBodies) filter(r *http.Request) bool {
	if r.URL.Host != c.host {
		return false
	}
	body := util.SafeReadBody(r)
	if !strings.Contains(body, `"eth_call"`) {
		return false
	}
	c.mu.Lock()
	c.bodies = append(c.bodies, body)
	c.mu.Unlock()
	return true
}

func (c *ethCallBodies) get() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.bodies...)
}

// TestNetwork_RequestFormat sends one eth_call with a short storage override
// (key 0x2a, value 0x1) over a formatted upstream tried first and a plain
// vendor tried second.
func TestNetwork_RequestFormat(t *testing.T) {
	const (
		addr   = "0x00000000000000000000000000000000000000aa"
		word2a = "0x000000000000000000000000000000000000000000000000000000000000002a"
		word01 = "0x0000000000000000000000000000000000000000000000000000000000000001"
	)

	run := func(t *testing.T, format common.EvmRequestFormat, batch bool, account string) (result string, formatted, vendor []string, sharedParams []interface{}) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		fmtd, vend := &ethCallBodies{host: "formatted.localhost"}, &ethCallBodies{host: "vendor.localhost"}
		for _, id := range []string{"formatted", "vendor"} {
			util.SetupMocksForUpstream("http://"+id+".localhost", map[string]interface{}{
				"latestBlock":    "0x2000",
				"finalizedBlock": "0x1ff0",
			})
		}
		formattedReply := `{"jsonrpc":"2.0","id":1,"result":"0x11"}`
		if batch {
			formattedReply = `[{"jsonrpc":"2.0","id":1,"result":"0x11"}]`
		}
		gock.New("http://formatted.localhost").Post("").Persist().
			Filter(fmtd.filter).
			Reply(200).JSON([]byte(formattedReply))
		gock.New("http://vendor.localhost").Post("").Persist().
			Filter(vend.filter).
			Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x22"}`))

		formattedCfg := &common.UpstreamConfig{
			Id:       "formatted",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://formatted.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId:             123,
				StatePollerInterval: common.Duration(200 * time.Millisecond),
				StatePollerDebounce: common.Duration(50 * time.Millisecond),
				RequestFormat:       format,
			},
		}
		if batch {
			formattedCfg.JsonRpc = &common.JsonRpcUpstreamConfig{
				SupportsBatch: b(true),
				BatchMaxSize:  10,
				BatchMaxWait:  common.Duration(5 * time.Millisecond),
			}
		}
		upCfgs := []*common.UpstreamConfig{
			formattedCfg,
			{
				Id:       "vendor",
				Type:     common.UpstreamTypeEvm,
				Endpoint: "http://vendor.localhost",
				Evm: &common.EvmUpstreamConfig{
					ChainId:             123,
					StatePollerInterval: common.Duration(200 * time.Millisecond),
					StatePollerDebounce: common.Duration(50 * time.Millisecond),
				},
			},
		}

		rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
		vr := thirdparty.NewVendorsRegistry()
		pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
		sharedStateCfg := &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: common.DriverMemory,
				Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
			},
			LockMaxWait:     common.Duration(200 * time.Millisecond),
			UpdateMaxWait:   common.Duration(200 * time.Millisecond),
			FallbackTimeout: common.Duration(3 * time.Second),
			LockTtl:         common.Duration(4 * time.Second),
		}
		sharedStateCfg.SetDefaults("test")
		ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, sharedStateCfg)
		upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", upCfgs, ssr, rlr, vr, pr, nil, mt, nil)
		upr.Bootstrap(ctx)
		time.Sleep(100 * time.Millisecond)

		ntwCfg := &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{ChainId: 123},
		}
		network, err := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt, nil)
		require.NoError(t, err)
		require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
		require.NoError(t, network.Bootstrap(ctx))
		network.PinUpstreamOrderForTest("formatted", "vendor")

		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"%s","data":"0x"},"0x1000",{"%s":%s}]}`,
			addr, addr, account)
		req := common.NewNormalizedRequest([]byte(body))
		req.SetNetwork(network)
		jrq, err := req.JsonRpcRequest()
		require.NoError(t, err)
		evm.NormalizeHttpJsonRpc(ctx, req, jrq)

		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		jrr, err := resp.JsonRpcResponse(ctx)
		require.NoError(t, err)
		result = jrr.GetResultString()
		resp.Release()
		return result, fmtd.get(), vend.get(), jrq.Params
	}

	shortState := `{"state":{"0x2a":"0x1"}}`

	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprintf("CanonicalSendsPaddedWordsBatch=%v", batch), func(t *testing.T) {
			result, formatted, vendor, shared := run(t, common.EvmRequestFormatCanonical, batch, shortState)
			assert.Equal(t, `"0x11"`, result)
			assert.Empty(t, vendor)
			require.Len(t, formatted, 1)
			assert.Equal(t, batch, strings.HasPrefix(strings.TrimSpace(formatted[0]), "["), "batch upstreams get a JSON-RPC batch")
			assert.Contains(t, formatted[0], `"`+word2a+`":"`+word01+`"`)
			assert.NotContains(t, formatted[0], `"0x2a"`)
			// The shared request keeps the client's words for the cache and other upstreams.
			state := shared[2].(map[string]interface{})[addr].(map[string]interface{})["state"]
			assert.Equal(t, map[string]interface{}{"0x2a": "0x1"}, state)
		})
	}

	t.Run("UnsetSendsParamsUnchanged", func(t *testing.T) {
		_, formatted, _, _ := run(t, "", false, shortState)
		require.Len(t, formatted, 1)
		assert.Contains(t, formatted[0], `"0x2a":"0x1"`)
	})

	t.Run("ErigonLegacyRefusalFallsOverToVendor", func(t *testing.T) {
		result, formatted, vendor, _ := run(t, common.EvmRequestFormatErigonLegacy, false,
			`{"movePrecompileToAddress":"0x00000000000000000000000000000000000000cc"}`)
		assert.Equal(t, `"0x22"`, result)
		assert.Empty(t, formatted, "the refused request never reaches the formatted upstream")
		require.Len(t, vendor, 1)
		assert.Contains(t, vendor[0], "movePrecompileToAddress")
	})
}
