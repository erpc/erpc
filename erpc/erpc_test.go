package erpc

import (
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
)

func init() {
	zerolog.SetGlobalLevel(zerolog.Disabled)
}

var erpcMu sync.Mutex

func TestErpc_UpstreamsRegistryCorrectPriorityChange(t *testing.T) {
	erpcMu.Lock()
	defer erpcMu.Unlock()

	defer gock.Off()
	defer gock.Clean()
	defer gock.CleanUnmatchedRequest()

	port := rand.Intn(1000) + 2000
	cfg := &common.Config{
		Server: &common.ServerConfig{
			HttpHostV4: "0.0.0.0",
			HttpHostV6: "[::]",
			HttpPort:   port,
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
					},
					{
						Id:       "rpc2",
						Type:     "evm",
						Endpoint: "http://rpc2.localhost",
						Evm: &common.EvmUpstreamConfig{
							ChainId: 123,
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
	erpcInstance, err := NewERPC(ctx1, &lg, nil, cfg)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}

	nw, err := erpcInstance.GetNetwork("test", "evm:123")
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}

	nw.upstreamsRegistry.PrepareUpstreamsForNetwork("evm:123")
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

	sortedUpstreams, err := nw.upstreamsRegistry.GetSortedUpstreams("evm:123", "eth_getTransactionReceipt")

	expectedOrder := []string{"rpc2", "rpc1"}
	assert.NoError(t, err)
	for i, ups := range sortedUpstreams {
		assert.Equal(t, expectedOrder[i], ups.Config().Id)
	}
}
