package erpc

import (
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
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

	port := rand.Intn(1000) + 2000
	cfg := &common.Config{
		Server: &common.ServerConfig{
			HttpHostV4: util.StringPtr("0.0.0.0"),
			HttpHostV6: util.StringPtr("[::]"),
			HttpPort:   util.IntPtr(port),
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
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "rpc1",
						Type:     "evm",
						Endpoint: "http://rpc1.localhost",
						Evm: &common.EvmUpstreamConfig{
							ChainId: 123,
						},
						JsonRpc: &common.JsonRpcUpstreamConfig{
							SupportsBatch: &common.FALSE,
						},
						Routing: &common.RoutingConfig{
							ScoreMultipliers: []*common.ScoreMultiplierConfig{
								{
									Network:         "*",
									Method:          "*",
									Overall:         util.Float64Ptr(1),
									ErrorRate:       util.Float64Ptr(5),
									RespLatency:     util.Float64Ptr(0),
									TotalRequests:   util.Float64Ptr(0),
									BlockHeadLag:    util.Float64Ptr(0),
									FinalizationLag: util.Float64Ptr(0),
									ThrottledRate:   util.Float64Ptr(0),
								},
							},
						},
					},
					{
						Id:       "rpc2",
						Type:     "evm",
						Endpoint: "http://rpc2.localhost",
						Evm: &common.EvmUpstreamConfig{
							ChainId: 123,
						},
						JsonRpc: &common.JsonRpcUpstreamConfig{
							SupportsBatch: &common.FALSE,
						},
						Routing: &common.RoutingConfig{
							ScoreMultipliers: []*common.ScoreMultiplierConfig{
								{
									Network:         "*",
									Method:          "*",
									Overall:         util.Float64Ptr(1),
									ErrorRate:       util.Float64Ptr(5),
									RespLatency:     util.Float64Ptr(0),
									TotalRequests:   util.Float64Ptr(0),
									BlockHeadLag:    util.Float64Ptr(0),
									FinalizationLag: util.Float64Ptr(0),
									ThrottledRate:   util.Float64Ptr(0),
								},
							},
						},
					},
				},
			},
		},
	}

	for i := 0; i < 1000; i++ {
		gock.New("http://rpc1.localhost").
			Post("").
			ReplyFunc(func(r *gock.Response) {
				// 30% chance of failure
				if rand.Intn(100) < 30 {
					r.Status(500)
					r.JSON([]byte(`{"error":{"code":-32000,"message":"internal server error"}}`))
				} else {
					r.Status(200)
					r.JSON([]byte(`{"result":{"hash":"0x123456789","fromHost":"rpc1"}}`))
				}
			})
	}

	gock.New("http://rpc2.localhost").
		Persist().
		Post("").
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
	erpcInstance, err := NewERPC(ctx1, &lg, ssr, nil, cfg)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	err = erpcInstance.Bootstrap(ctx1)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}

	nw, err := erpcInstance.GetNetwork(ctx1, "test", "evm:123")
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}

	nw.upstreamsRegistry.PrepareUpstreamsForNetwork(ctx1, "evm:123")
	time.Sleep(100 * time.Millisecond)
	nw.upstreamsRegistry.RefreshUpstreamNetworkMethodScores()
	time.Sleep(100 * time.Millisecond)

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

	// wait until scores are calculated and erpc is shutdown down properly
	time.Sleep(1 * time.Second)
	cancel1()
	cancel2()

	sortedUpstreams, err := nw.upstreamsRegistry.GetSortedUpstreams(context.Background(), "evm:123", "eth_getTransactionReceipt")

	expectedOrder := []string{"rpc2", "rpc1"}
	assert.NoError(t, err)
	for i, ups := range sortedUpstreams {
		assert.Equal(t, expectedOrder[i], ups.Id())
	}
}
