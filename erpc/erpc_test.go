package erpc

import (
	"context"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
)

func init() {
	telemetry.SetHistogramBuckets("0.05,0.5,5,30")
}

func TestErpc_UpstreamsRegistryCorrectPriorityChange(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	// Set up required chainId/latest/finalized/syncing mocks BEFORE any components start
	// so upstream detectFeatures and state pollers don't hang or steal test mocks.
	util.SetupMocksForEvmStatePoller()

	port := rand.Intn(1000) + 2000
	cfg := &common.Config{
		Server: &common.ServerConfig{
			HttpHostV4: util.StringPtr("0.0.0.0"),
			HttpHostV6: util.StringPtr("[::]"),
			HttpPortV4: util.IntPtr(port),
			MaxTimeout: common.Duration(5 * time.Second).Ptr(),
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "test",
				Networks: []*common.NetworkConfig{
					{
						Architecture: "evm",
						Evm: &common.EvmNetworkConfig{
							ChainId: 123,
						},
						Failsafe: []*common.FailsafeConfig{
							{
								Retry: &common.RetryPolicyConfig{
									MaxAttempts: 3,
									Delay:       common.Duration(10 * time.Millisecond),
								},
							},
						},
						SelectionPolicy: &common.SelectionPolicyConfig{
							EvalInterval: common.Duration(100 * time.Millisecond),
							EvalTimeout:  common.Duration(50 * time.Millisecond),
							EvalFunc: `(upstreams, ctx) =>
								upstreams.sortByScore({ errorRate: 5 })`,
						},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "rpc1",
						Type:     "evm",
						Endpoint: "http://rpc1.localhost",
						Evm:      &common.EvmUpstreamConfig{ChainId: 123},
						JsonRpc:  &common.JsonRpcUpstreamConfig{SupportsBatch: &common.FALSE},
					},
					{
						Id:       "rpc2",
						Type:     "evm",
						Endpoint: "http://rpc2.localhost",
						Evm:      &common.EvmUpstreamConfig{ChainId: 123},
						JsonRpc:  &common.JsonRpcUpstreamConfig{SupportsBatch: &common.FALSE},
					},
				},
			},
		},
	}

	// rpc1: introduce some failures for eth_getTransactionReceipt only
	for i := 0; i < 30; i++ {
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(req *http.Request) bool {
				body := util.SafeReadBody(req)
				return strings.Contains(body, "eth_getTransactionReceipt")
			}).
			Times(1).
			Reply(500).
			JSON([]byte(`{"error":{"code":-32000,"message":"internal server error"}}`))
	}
	// Remaining calls succeed
	gock.New("http://rpc1.localhost").
		Persist().
		Post("").
		Filter(func(req *http.Request) bool {
			body := util.SafeReadBody(req)
			return strings.Contains(body, "eth_getTransactionReceipt")
		}).
		Reply(200).
		JSON([]byte(`{"result":{"hash":"0x123456789","fromHost":"rpc1"}}`))

	gock.New("http://rpc2.localhost").
		Persist().
		Post("").
		Filter(func(req *http.Request) bool {
			body := util.SafeReadBody(req)
			return strings.Contains(body, "eth_getTransactionReceipt")
		}).
		Reply(200).
		JSON([]byte(`{"result":{"hash":"0x123456789","fromHost":"rpc2"}}`))

	lg := log.With().Logger()
	ctx1, cancel1 := context.WithCancel(context.Background())
	ssr, err := data.NewSharedStateRegistry(ctx1, &lg, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems: 100_000, MaxTotalSize: "1GB",
			},
		},
	})
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	erpcInstance, err := NewERPC(ctx1, &lg, ssr, nil, nil, cfg)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	erpcInstance.Bootstrap(ctx1)

	nw, err := erpcInstance.GetNetwork(ctx1, "test", "evm:123")
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}

	nw.upstreamsRegistry.PrepareUpstreamsForNetwork(ctx1, "evm:123")

	ctx2, cancel2 := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			nr := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getTransactionReceipt","params":["0x123456789"],"id":1}`))
			_, _ = nw.Forward(ctx2, nr)
		}()
		time.Sleep(10 * time.Millisecond)
	}
	wg.Wait()

	// Force-tick the engine until the order flips to rpc2 (clean) ahead of
	// rpc1 (errored). The ticker is also running on its own; this just
	// removes the timing dependency.
	deadline := time.Now().Add(2 * time.Second)
	for {
		policy.TickForTest(nw.policyEngine, "evm:123", "*")
		ordered := nw.policyEngine.GetOrdered("evm:123", "*", "*")
		if len(ordered) >= 2 && ordered[0].Id() == "rpc2" {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel1()
	cancel2()

	sortedUpstreams := nw.policyEngine.GetOrdered("evm:123", "*", "*")
	expectedOrder := []string{"rpc2", "rpc1"}
	assert.Len(t, sortedUpstreams, 2)
	for i, ups := range sortedUpstreams {
		assert.Equal(t, expectedOrder[i], ups.Id())
	}
}
