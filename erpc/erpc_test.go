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

func TestErpc_GracefulShutdown(t *testing.T) {
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

var prioMu sync.Mutex
func TestErpc_UpstreamsRegistryCorrectPriorityChange(t *testing.T) {
	prioMu.Lock()
	defer prioMu.Unlock()
	
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
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}

	for i := 0; i < 500; i++ {
		nr := upstream.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["latest",false],"id":1}`))
		_, _ = nw.Forward(context.Background(), nr)
	}

	// wait until scores are calculated and erpc is shutdown down properly
	time.Sleep(6 * time.Second)
	erpcInstance.Shutdown()
	time.Sleep(2 * time.Second)

	ups := nw.Upstreams
	var up1Score, up2Score int
	
	if ups[0].Config().Id == "rpc1" {
		up1Score = ups[0].Score
		up2Score = ups[1].Score
	} else {
		up1Score = ups[1].Score
		up2Score = ups[0].Score
	}

	if up1Score >= up2Score {
		t.Errorf("expected up1 to have lower score, got %v up1 failed/total: %f/%f up2 failed/total: %f/%f", up1Score, nw.Upstreams[0].Metrics.ErrorsTotal, nw.Upstreams[0].Metrics.RequestsTotal, nw.Upstreams[1].Metrics.ErrorsTotal, nw.Upstreams[1].Metrics.RequestsTotal)
	}
}
