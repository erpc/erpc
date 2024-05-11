package erpc

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/util"
)

func TestBootstrap_FailToStartServer(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			HttpHost: "localhost",
			HttpPort: "10000000000000",
		},
	}

	originalOsExit := util.OsExit
	var called bool
	defer func() {
		util.OsExit = originalOsExit
	}()
	util.OsExit = func(code int) {
		if code != util.ExitCodeHttpServerFailed {
			t.Errorf("expected code %d, got %d", util.ExitCodeHttpServerFailed, code)
		} else {
			called = true
		}
	}

	_, _ = Bootstrap(cfg)

	time.Sleep(200 * time.Millisecond)

	if !called {
		t.Error("expected osExit to be called")
	}
}

func TestBoorstrap_GracefulShutdown(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			HttpHost: "localhost",
			HttpPort: fmt.Sprint(rand.Intn(1000) + 2000),
		},
	}

	shutdown, _ := Bootstrap(cfg)
	shutdown()
}

func TestBootstrap_UpstreamOrchestratorFailure(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			HttpHost: "localhost",
			HttpPort: fmt.Sprint(rand.Intn(1000) + 2000),
		},
		Projects: []config.ProjectConfig{
			{
				Id: "test",
				Upstreams: []config.UpstreamConfig{
					{
						Id:  "test",
						Architecture: "evm",
						Endpoint: "http://localhost:8080",
						// missing "evmChainId" will cause an error
						Metadata: map[string]string{},
					},
				},
			},
		},
	}

	_, err := Bootstrap(cfg)
	if err == nil {
		t.Error("expected error when bootstraping upstream orchestrator, got nil")
	}
}
