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

func TestPinBlockTagInBody(t *testing.T) {
	t.Run("PinsLatestForEthCall", func(t *testing.T) {
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0xabc","data":"0x1"},"latest"]}`)
		out, changed := evm.PinBlockTagInBody(body, 0x189cbe5)
		assert.True(t, changed)
		assert.Contains(t, string(out), `"0x189cbe5"`)
		assert.NotContains(t, string(out), `"latest"`)
	})

	t.Run("PinsStorageSlotIndexTwo", func(t *testing.T) {
		body := []byte(`{"jsonrpc":"2.0","id":7,"method":"eth_getStorageAt","params":["0xabc","0x0","latest"]}`)
		out, changed := evm.PinBlockTagInBody(body, 100)
		assert.True(t, changed)
		assert.Contains(t, string(out), `"0x64"`)
	})

	t.Run("PinsEstimateGas", func(t *testing.T) {
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_estimateGas","params":[{"to":"0xabc"},"latest"]}`)
		out, changed := evm.PinBlockTagInBody(body, 100)
		assert.True(t, changed)
		assert.Contains(t, string(out), `"0x64"`)
	})

	t.Run("ConcreteHeightUntouched", func(t *testing.T) {
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0xabc"},"0x1234"]}`)
		out, changed := evm.PinBlockTagInBody(body, 100)
		assert.False(t, changed)
		assert.Equal(t, body, out)
	})

	t.Run("PendingUntouched", func(t *testing.T) {
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0xabc"},"pending"]}`)
		_, changed := evm.PinBlockTagInBody(body, 100)
		assert.False(t, changed)
	})

	t.Run("MissingParamPinned", func(t *testing.T) {
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0xabc"}]}`)
		out, changed := evm.PinBlockTagInBody(body, 100)
		assert.True(t, changed, "an absent selector means latest, so it pins as interpolation fills it")
		assert.Contains(t, string(out), `"0x64"`)
	})

	t.Run("GarbageUntouched", func(t *testing.T) {
		body := []byte(`{not json`)
		out, changed := evm.PinBlockTagInBody(body, 100)
		assert.False(t, changed)
		assert.Equal(t, []byte(`{not json`), out)
	})
}

// ethCallTags records the block selector (params[1]) of every client eth_call
// one mocked upstream host receives; the state prober's own calls to
// Multicall3 are not clients'. gock runs a mock's filters before its host
// matcher, so the filter checks the host itself.
type ethCallTags struct {
	host string
	mu   sync.Mutex
	tags []interface{}
}

func (c *ethCallTags) filter(r *http.Request) bool {
	if r.URL.Host != c.host {
		return false
	}
	body := util.SafeReadBody(r)
	if !strings.Contains(body, `"eth_call"`) || strings.Contains(strings.ToLower(body), "0xca11bde05977b3631167028862be2a173976ca11") {
		return false
	}
	var rq struct {
		Params []interface{} `json:"params"`
	}
	_ = common.SonicCfg.Unmarshal([]byte(body), &rq)
	var tag interface{}
	if len(rq.Params) > 1 {
		tag = rq.Params[1]
	}
	c.mu.Lock()
	c.tags = append(c.tags, tag)
	c.mu.Unlock()
	return true
}

func (c *ethCallTags) get() []interface{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]interface{}(nil), c.tags...)
}

// TestNetwork_PinLatestTag walks a network whose eth_call keeps "latest"
// (translateLatestTag: false) over a pinned upstream (evm.pinLatestTag,
// upper.latestBlockMinus: 0) tried first and a vendor tried second.
func TestNetwork_PinLatestTag(t *testing.T) {
	const head = int64(0x2000)
	headHex := fmt.Sprintf("0x%x", head)

	run := func(t *testing.T, pinnedHead int64, pinnedReply string) (result string, pinnedTags, vendorTags []interface{}) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		pinned, vendor := &ethCallTags{host: "pinned.localhost"}, &ethCallTags{host: "vendor.localhost"}
		heads := map[string]int64{"pinned": pinnedHead, "vendor": head}
		for id, h := range heads {
			util.SetupMocksForUpstream("http://"+id+".localhost", map[string]interface{}{
				"latestBlock":    fmt.Sprintf("0x%x", h),
				"finalizedBlock": fmt.Sprintf("0x%x", h-16),
			})
		}
		gock.New("http://pinned.localhost").Post("").Persist().
			Filter(pinned.filter).
			Reply(200).JSON([]byte(pinnedReply))
		gock.New("http://vendor.localhost").Post("").Persist().
			Filter(vendor.filter).
			Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x22"}`))

		upCfgs := []*common.UpstreamConfig{
			{
				Id:       "pinned",
				Type:     common.UpstreamTypeEvm,
				Endpoint: "http://pinned.localhost",
				Evm: &common.EvmUpstreamConfig{
					ChainId:             123,
					StatePollerInterval: common.Duration(200 * time.Millisecond),
					StatePollerDebounce: common.Duration(50 * time.Millisecond),
					BlockAvailability: &common.EvmBlockAvailabilityConfig{
						Upper: &common.EvmAvailabilityBoundConfig{LatestBlockMinus: i64(0)},
					},
					PinLatestTag: b(true),
				},
			},
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
			Evm: &common.EvmNetworkConfig{
				ChainId: 123,
				// The network head is the highest upstream head, so a lagging
				// pinned upstream is behind it.
				ServedTip: &common.EvmServedTipConfig{Quorum: 1},
			},
			Methods: &common.MethodsConfig{
				Definitions: map[string]*common.CacheMethodConfig{
					"eth_call": {ReqRefs: [][]interface{}{{1}}, TranslateLatestTag: b(false)},
				},
			},
		}
		network, err := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt, nil)
		require.NoError(t, err)
		require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
		require.NoError(t, network.Bootstrap(ctx))
		network.PinUpstreamOrderForTest("pinned", "vendor")

		for _, u := range upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123)) {
			want := heads[u.Id()]
			require.Eventually(t, func() bool { return u.EvmStatePoller().LatestBlock() == want },
				3*time.Second, 25*time.Millisecond, "poller for %s should settle on %d", u.Id(), want)
		}

		req := common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000001","data":"0x"},"latest"]}`))
		req.SetNetwork(network)
		jrq, err := req.JsonRpcRequest()
		require.NoError(t, err)
		evm.NormalizeHttpJsonRpc(ctx, req, jrq)
		require.Equal(t, "latest", jrq.Params[1], "translateLatestTag: false keeps latest on the request")

		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		jrr, err := resp.JsonRpcResponse(ctx)
		require.NoError(t, err)
		result = jrr.GetResultString()
		resp.Release()
		require.Equal(t, "latest", jrq.Params[1], "the request keeps latest after a pinned attempt")
		return result, pinned.get(), vendor.get()
	}

	t.Run("PinnedAtHeadIsSentHeadAndAnswers", func(t *testing.T) {
		result, pinnedTags, vendorTags := run(t, head, `{"jsonrpc":"2.0","id":1,"result":"0x11"}`)
		assert.Equal(t, `"0x11"`, result)
		assert.Equal(t, []interface{}{headHex}, pinnedTags)
		assert.Empty(t, vendorTags)
	})

	t.Run("PinnedBehindHeadIsSkipped", func(t *testing.T) {
		result, pinnedTags, vendorTags := run(t, head-2, `{"jsonrpc":"2.0","id":1,"result":"0x11"}`)
		assert.Equal(t, `"0x22"`, result)
		assert.Empty(t, pinnedTags)
		assert.Equal(t, []interface{}{"latest"}, vendorTags)
	})

	t.Run("PinnedMissingHeadFallsBackToVendor", func(t *testing.T) {
		result, pinnedTags, vendorTags := run(t, head,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"header not found"}}`)
		assert.Equal(t, `"0x22"`, result)
		require.NotEmpty(t, pinnedTags)
		for _, tag := range pinnedTags {
			assert.Equal(t, headHex, tag)
		}
		assert.Equal(t, []interface{}{"latest"}, vendorTags)
	})
}
