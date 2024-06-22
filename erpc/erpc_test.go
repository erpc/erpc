package erpc

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/flair-sdk/erpc/common"
	"github.com/rs/zerolog/log"
)

func TestBoorstrap_GracefulShutdown(t *testing.T) {
	cfg := &common.Config{
		Server: &common.ServerConfig{
			HttpHost: "localhost",
			HttpPort: fmt.Sprint(rand.Intn(1000) + 2000),
		},
	}
	db := &EvmJsonRpcCache{}
	lg := log.With().Logger()
	erpc, _ := NewERPC(&lg, db, cfg)
	erpc.Shutdown()
}

func TestBootstrap_UpstreamsRegistryFailure(t *testing.T) {
	cfg := &common.Config{
		Server: &common.ServerConfig{
			HttpHost: "localhost",
			HttpPort: fmt.Sprint(rand.Intn(1000) + 2000),
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "test",
				Upstreams: []*common.UpstreamConfig{
					{
						Id:           "test",
						Architecture: "evm",
						Endpoint:     "http://localhost:8080",
						// missing "evmChainId" will cause an error
					},
				},
			},
		},
	}

	lg := log.With().Logger()
	db := &EvmJsonRpcCache{}
	_, err := NewERPC(&lg, db, cfg)
	if err == nil {
		t.Error("expected error when bootstraping upstream orchestrator, got nil")
	}
}
