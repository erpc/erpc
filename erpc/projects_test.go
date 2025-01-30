package erpc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
)

func TestProject_Forward(t *testing.T) {
	t.Run("ForwardCorrectlyRateLimitedOnProjectLevel", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(
			&common.RateLimiterConfig{
				Budgets: []*common.RateLimitBudgetConfig{
					{
						Id: "MyLimiterBudget_Test1",
						Rules: []*common.RateLimitRuleConfig{
							{
								Method:   "*",
								MaxCount: 3,
								Period:   "60s",
								WaitTime: "",
							},
						},
					},
				},
			},
			&log.Logger,
		)
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		prjReg, err := NewProjectsRegistry(
			ctx,
			&log.Logger,
			[]*common.ProjectConfig{
				{
					Id:              "prjA",
					RateLimitBudget: "MyLimiterBudget_Test1",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Endpoint: "http://rpc1.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
						},
					},
				},
			},
			nil,
			rateLimitersRegistry,
			thirdparty.NewVendorsRegistry(),
		)
		if err != nil {
			t.Fatal(err)
		}
		err = prjReg.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}

		prj, err := prjReg.GetProject("prjA")
		if err != nil {
			t.Fatal(err)
		}

		var lastErr error
		var lastResp *common.NormalizedResponse

		for i := 0; i < 5; i++ {
			fakeReq := common.NewNormalizedRequest([]byte(`{"method": "eth_chainId","params":[]}`))
			lastResp, lastErr = prj.Forward(ctx, "evm:123", fakeReq)
		}

		var e *common.ErrProjectRateLimitRuleExceeded
		if lastErr == nil || !errors.As(lastErr, &e) {
			t.Errorf("Expected %v, got %v", "ErrProjectRateLimitRuleExceeded", lastErr)
		}

		log.Logger.Info().Msgf("Last Resp: %+v", lastResp)
	})
}
func TestProject_TimeoutScenarios(t *testing.T) {
	t.Run("UpstreamTimeout", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Create a rate limiters registry (not specifically needed for this test,
		// but it's part of the usual setup.)
		rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(
			&common.RateLimiterConfig{},
			&log.Logger,
		)
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Configure a project with an extremely short upstream failsafe timeout.
		// We’ll make the server and network have bigger timeouts so that only
		// the upstream times out first.
		prjReg, err := NewProjectsRegistry(
			ctx,
			&log.Logger,
			[]*common.ProjectConfig{
				{
					Id: "test_prj_upstream_timeout",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "10s",
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
							// Very short upstream timeout
							Failsafe: &common.FailsafeConfig{
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "50ms",
								},
							},
						},
					},
				},
			},
			nil,
			// &common.ServerConfig{
			// 	MaxTimeout: util.StringPtr("10s"), // Large server timeout
			// },
			rateLimitersRegistry,
			thirdparty.NewVendorsRegistry(),
		)
		if err != nil {
			t.Fatal(err)
		}
		err = prjReg.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}

		// Mock an upstream request that will definitely exceed 50 ms.
		gock.New("http://rpc1.localhost").
			Post("/").
			Reply(200).
			Delay(500 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		prj, err := prjReg.GetProject("test_prj_upstream_timeout")
		if err != nil {
			t.Fatalf("Error retrieving project: %v", err)
		}

		fakeReq := common.NewNormalizedRequest([]byte(`{"method": "eth_blockNumber","params":[]}`))
		_, lastErr := prj.Forward(ctx, "evm:1", fakeReq)

		if lastErr == nil {
			t.Error("Expected an upstream timeout error, got nil")
		} else {
			summary := common.ErrorSummary(lastErr)
			if !strings.Contains(summary, "upstream timeout") {
				t.Errorf("Expected upstream timeout error, got: %v", lastErr)
			}
		}
	})

	t.Run("NetworkTimeout", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(
			&common.RateLimiterConfig{},
			&log.Logger,
		)
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Configure a project with an extremely short "network" timeout.
		// The upstream itself can have a longer timeout, but the network-level
		// failsafe triggers earlier.
		prjReg, err := NewProjectsRegistry(
			ctx,
			&log.Logger,
			[]*common.ProjectConfig{
				{
					Id: "test_prj_network_timeout",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								// Very short network timeout
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "50ms",
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
							// Higher upstream timeout
							Failsafe: &common.FailsafeConfig{
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "5s",
								},
							},
						},
					},
				},
			},
			nil,
			rateLimitersRegistry,
			thirdparty.NewVendorsRegistry(),
		)
		if err != nil {
			t.Fatal(err)
		}
		err = prjReg.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}

		// Mock a delay that exceeds the 50ms network timeout
		gock.New("http://rpc2.localhost").
			Post("/").
			Reply(200).
			Delay(200 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x333333",
			})

		prj, err := prjReg.GetProject("test_prj_network_timeout")
		if err != nil {
			t.Fatalf("Error retrieving project: %v", err)
		}

		fakeReq := common.NewNormalizedRequest([]byte(`{"method": "eth_getBalance","params":["0x123"]}`))
		_, lastErr := prj.Forward(ctx, "evm:1", fakeReq)

		if lastErr == nil {
			t.Error("Expected a network timeout error, got nil")
		} else {
			summary := common.ErrorSummary(lastErr)
			if !strings.Contains(summary, "timeout policy exceeded on network-level") {
				t.Errorf("Expected network timeout error, got: %v", lastErr)
			}
		}
	})
}

func TestProject_LazyLoadNetworkDefaults(t *testing.T) {
	t.Run("LazyLoadEvmNetwork_WithDefaultConfigs", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Create a project config without explicitly defining any NetworkConfig,
		// but supply some networkDefaults that should be applied to lazy-loaded networks.
		prjConfig := &common.ProjectConfig{
			Id: "test_lazy_load",
			// No networks defined
			Networks: nil,

			NetworkDefaults: &common.NetworkDefaults{
				Failsafe: &common.FailsafeConfig{
					Timeout: &common.TimeoutPolicyConfig{
						Duration: "7s",
					},
				},
			},
			Upstreams: []*common.UpstreamConfig{
				{
					Id:       "mock_upstream",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://mock.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 9999,
					},
				},
			},
		}

		// Build ProjectsRegistry with no existing EvmJsonRpcCache or RateLimiter
		rateLimiters, _ := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
		reg, err := NewProjectsRegistry(
			ctx,
			&log.Logger,
			[]*common.ProjectConfig{prjConfig},
			nil,          // EvmJsonRpcCache
			rateLimiters, // RateLimitersRegistry
			thirdparty.NewVendorsRegistry(),
		)
		if err != nil {
			t.Fatalf("failed to create ProjectsRegistry: %v", err)
		}
		err = reg.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}

		// Begin mocking an upstream response for a brand new chain "evm:9999"
		// that isn't explicitly defined in prjConfig.Networks:
		gock.New("http://mock.localhost").
			Post("/").
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xabc123",
			})

		// Get the project. Next, we'll forward a request to "evm:9999".
		prj, err := reg.GetProject("test_lazy_load")
		if err != nil {
			t.Fatalf("Error retrieving project: %v", err)
		}

		fakeReq := common.NewNormalizedRequest([]byte(`{"method": "eth_blockNumber","params":[]}`))

		// Because "evm:9999" is not in prjConfig.Networks, it should be lazy-loaded using default configs.
		resp, fwdErr := prj.Forward(ctx, "evm:9999", fakeReq)
		if fwdErr != nil {
			t.Fatalf("Forward error (lazy loading failed?): %v", fwdErr)
		}
		if resp == nil {
			t.Fatalf("Expected a valid response, got error: %v", resp)
		}

		// Assert that the new network was added and default was applied
		found := false
		for _, nw := range prj.Config.Networks {
			if nw.Architecture == common.ArchitectureEvm &&
				nw.Evm != nil && nw.Evm.ChainId == 9999 {
				found = true
				// Confirm the default failsafe timeout was set as "7s"
				if nw.Failsafe == nil || nw.Failsafe.Timeout == nil || nw.Failsafe.Timeout.Duration != "7s" {
					t.Errorf("expected lazy loaded network to have Failsafe.Timeout.Duration = 7s, got %+v", nw.Failsafe)
				}
				if nw.Failsafe == nil || nw.Failsafe.Retry != nil {
					t.Errorf("expected lazy loaded network to have Failsafe.Retry = nil, got %+v", nw.Failsafe)
				}
				if nw.Failsafe == nil || nw.Failsafe.CircuitBreaker != nil {
					t.Errorf("expected lazy loaded network to have Failsafe.CircuitBreaker = nil, got %+v", nw.Failsafe)
				}
				if nw.Failsafe == nil || nw.Failsafe.Hedge != nil {
					t.Errorf("expected lazy loaded network to have Failsafe.Hedge = nil, got %+v", nw.Failsafe)
				}
				break
			}
		}

		if !found {
			t.Error("Expected newly lazy-loaded EVM network with chainId=9999 to be added to project.Config.Networks, but not found")
		}
	})
}
