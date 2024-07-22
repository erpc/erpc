package erpc

import (
	"context"
	"encoding/json"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
)

var erpcMu sync.Mutex

func TestErpc_GracefulShutdown(t *testing.T) {
	erpcMu.Lock()
	defer erpcMu.Unlock()

	cfg := &common.Config{
		Server: &common.ServerConfig{
			HttpHost: "localhost",
			HttpPort: rand.Intn(1000) + 2000,
		},
	}
	db := &EvmJsonRpcCache{}
	lg := log.With().Logger()
	erpc, _ := NewERPC(&lg, db, cfg)
	erpc.Shutdown()
}

func TestErpc_UpstreamsRegistryCorrectPriorityChange(t *testing.T) {
	erpcMu.Lock()
	defer erpcMu.Unlock()

	defer gock.Off()
	defer gock.Clean()
	defer gock.CleanUnmatchedRequest()

	port := rand.Intn(1000) + 2000
	cfg := &common.Config{
		Server: &common.ServerConfig{
			HttpHost: "localhost",
			HttpPort: port,
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
						Failsafe: &common.FailsafeConfig{
							Retry: &common.RetryPolicyConfig{
								MaxAttempts: 3,
								Delay:       "10ms",
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
						HealthCheckGroup: "test-hcg",
					},
					{
						Id:       "rpc2",
						Type:     "evm",
						Endpoint: "http://rpc2.localhost",
						Evm: &common.EvmUpstreamConfig{
							ChainId: 123,
						},
						HealthCheckGroup: "test-hcg",
					},
				},
			},
		},
		HealthChecks: &common.HealthCheckConfig{
			Groups: []*common.HealthCheckGroupConfig{
				{
					Id:            "test-hcg",
					CheckInterval: "1s",
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
					r.JSON(json.RawMessage(`{"error":{"code":-32000,"message":"internal server error"}}`))
				} else {
					r.Status(200)
					r.JSON(json.RawMessage(`{"result":{"hash":"0x123456789","fromHost":"rpc1"}}`))
				}
			})
	}

	gock.New("http://rpc2.localhost").
		Persist().
		Post("").
		Reply(200).
		JSON(json.RawMessage(`{"result":{"hash":"0x123456789","fromHost":"rpc2"}}`))

	lg := log.With().Logger()
	erpcInstance, err := NewERPC(&lg, nil, cfg)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}

	nw, err := erpcInstance.GetNetwork("test", "evm:123")
	upsA := nw.Upstreams[0]
	upsB := nw.Upstreams[1]

	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	for i := 0; i < 500; i++ {
		nr := upstream.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getTransactionReceipt","params":["0x123456789"],"id":1}`))
		_, _ = nw.Forward(ctx, nr)
	}
	
	// wait until scores are calculated and erpc is shutdown down properly
	time.Sleep(6 * time.Second)
	cancel()
	erpcInstance.Shutdown()
	time.Sleep(6 * time.Second)

	// TODO can we do this without exposing metrics mutex?
	var up1Score, up2Score int
	upsA.MetricsMu.RLock()
	upsB.MetricsMu.RLock()
	if upsA.Config().Id == "rpc1" {
		up1Score = upsA.Score
		up2Score = upsB.Score
	} else {
		up1Score = upsB.Score
		up2Score = upsA.Score
	}
	upsA.MetricsMu.RUnlock()
	upsB.MetricsMu.RUnlock()

	if up1Score >= up2Score {
		t.Errorf("expected up1 to have lower score, got %v up1 failed/total: %f/%f up2 failed/total: %f/%f", up1Score, nw.Upstreams[0].Metrics.ErrorsTotal, nw.Upstreams[0].Metrics.RequestsTotal, nw.Upstreams[1].Metrics.ErrorsTotal, nw.Upstreams[1].Metrics.RequestsTotal)
	}
}
