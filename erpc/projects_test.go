package erpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
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
								Period:   common.Duration(60 * time.Second),
								WaitTime: common.Duration(0),
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

		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000, MaxTotalSize: "1GB",
				},
			},
		})
		if err != nil {
			panic(err)
		}
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
			ssr,
			nil,
			rateLimitersRegistry,
			thirdparty.NewVendorsRegistry(),
			nil, // ProxyPoolRegistry
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

		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000, MaxTotalSize: "1GB",
				},
			},
		})
		if err != nil {
			panic(err)
		}
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
							Failsafe: []*common.FailsafeConfig{{
								Timeout: &common.TimeoutPolicyConfig{
									Duration: common.Duration(10 * time.Second),
								},
							}},
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
							Failsafe: []*common.FailsafeConfig{{
								Timeout: &common.TimeoutPolicyConfig{
									Duration: common.Duration(50 * time.Millisecond),
								},
							}},
						},
					},
				},
			},
			ssr,
			nil,
			// &common.ServerConfig{
			// 	MaxTimeout: util.StringPtr("10s"), // Large server timeout
			// },
			rateLimitersRegistry,
			thirdparty.NewVendorsRegistry(),
			nil, // ProxyPoolRegistry
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
			if !strings.Contains(summary, "exceeded on upstream-level") {
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

		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000, MaxTotalSize: "1GB",
				},
			},
		})
		if err != nil {
			panic(err)
		}
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
							Failsafe: []*common.FailsafeConfig{{
								// Very short network timeout
								Timeout: &common.TimeoutPolicyConfig{
									Duration: common.Duration(50 * time.Millisecond),
								},
							}},
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
							Failsafe: []*common.FailsafeConfig{{
								Timeout: &common.TimeoutPolicyConfig{
									Duration: common.Duration(5 * time.Second),
								},
							}},
						},
					},
				},
			},
			ssr,
			nil,
			rateLimitersRegistry,
			thirdparty.NewVendorsRegistry(),
			nil, // ProxyPoolRegistry
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
				Failsafe: []*common.FailsafeConfig{{
					Timeout: &common.TimeoutPolicyConfig{
						Duration: common.Duration(7 * time.Second),
					},
				}},
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
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000, MaxTotalSize: "1GB",
				},
			},
		})
		if err != nil {
			panic(err)
		}
		reg, err := NewProjectsRegistry(
			ctx,
			&log.Logger,
			[]*common.ProjectConfig{prjConfig},
			ssr,
			nil,          // EvmJsonRpcCache
			rateLimiters, // RateLimitersRegistry
			thirdparty.NewVendorsRegistry(),
			nil, // ProxyPoolRegistry
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
				if len(nw.Failsafe) == 0 || nw.Failsafe[0].Timeout == nil || nw.Failsafe[0].Timeout.Duration.String() != "7s" {
					t.Errorf("expected lazy loaded network to have Failsafe[0].Timeout.Duration = 7s, got %+v", nw.Failsafe)
				}
				if len(nw.Failsafe) == 0 || nw.Failsafe[0].Retry != nil {
					t.Errorf("expected lazy loaded network to have Failsafe[0].Retry = nil, got %+v", nw.Failsafe)
				}
				if len(nw.Failsafe) == 0 || nw.Failsafe[0].CircuitBreaker != nil {
					t.Errorf("expected lazy loaded network to have Failsafe[0].CircuitBreaker = nil, got %+v", nw.Failsafe)
				}
				if len(nw.Failsafe) == 0 || nw.Failsafe[0].Hedge != nil {
					t.Errorf("expected lazy loaded network to have Failsafe[0].Hedge = nil, got %+v", nw.Failsafe)
				}
				break
			}
		}

		if !found {
			t.Error("Expected newly lazy-loaded EVM network with chainId=9999 to be added to project.Config.Networks, but not found")
		}
	})
}

func TestProject_NetworkAlias(t *testing.T) {
	t.Run("NetworkAliasResolution", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000, MaxTotalSize: "1GB",
				},
			},
		})
		if err != nil {
			panic(err)
		}

		rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(
			&common.RateLimiterConfig{},
			&log.Logger,
		)
		if err != nil {
			t.Fatal(err)
		}

		prjReg, err := NewProjectsRegistry(
			ctx,
			&log.Logger,
			[]*common.ProjectConfig{
				{
					Id: "prjA",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Alias: "ethereum",
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Endpoint: "http://rpc1.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
						},
					},
				},
			},
			ssr,
			nil,
			rateLimitersRegistry,
			thirdparty.NewVendorsRegistry(),
			nil, // ProxyPoolRegistry
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

		// Test getting network by alias
		arch, chainId := prj.networksRegistry.ResolveAlias("ethereum")
		if common.NetworkArchitecture(arch) != common.ArchitectureEvm || chainId != "1" {
			t.Errorf("Expected architecture=evm, chainId=1 for alias 'ethereum', got arch=%s, chainId=%s", arch, chainId)
		}

		network, err := prj.networksRegistry.GetNetwork(fmt.Sprintf("%s:%s", arch, chainId))
		if err != nil {
			t.Fatalf("Failed to get network by ID: %v", err)
		}
		if network.Id() != "evm:1" {
			t.Errorf("Expected network ID 'evm:1', got '%s'", network.Id())
		}

		// Test getting non-existent alias
		arch, chainId = prj.networksRegistry.ResolveAlias("nonexistent")
		if arch != "" || chainId != "" {
			t.Errorf("Expected empty architecture and chainId for non-existent alias, got arch=%s, chainId=%s", arch, chainId)
		}
	})

	t.Run("DuplicateNetworkAliases", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000, MaxTotalSize: "1GB",
				},
			},
		})
		if err != nil {
			panic(err)
		}

		rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(
			&common.RateLimiterConfig{},
			&log.Logger,
		)
		if err != nil {
			t.Fatal(err)
		}

		// Create a project config with duplicate network aliases
		prjReg, err := NewProjectsRegistry(
			ctx,
			&log.Logger,
			[]*common.ProjectConfig{
				{
					Id: "prjDuplicateAlias",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Alias: "same_alias", // First use of the alias
						},
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 137,
							},
							Alias: "same_alias", // Duplicate alias
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Endpoint: "http://rpc1.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
						},
						{
							Id:       "rpc2",
							Endpoint: "http://rpc2.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId: 137,
							},
						},
					},
				},
			},
			ssr,
			nil,
			rateLimitersRegistry,
			thirdparty.NewVendorsRegistry(),
			nil, // ProxyPoolRegistry
		)
		if err != nil {
			t.Fatal(err)
		}

		// Bootstrap should fail due to duplicate aliases
		err = prjReg.Bootstrap(ctx)

		// Verify that an error was returned
		if err == nil {
			t.Fatal("Expected an error due to duplicate network aliases, but got nil")
		}

		// Verify the error message contains information about the duplicate alias
		expectedErrMsg := "alias same_alias already registered for network"
		if !strings.Contains(err.Error(), expectedErrMsg) {
			t.Errorf("Expected error message to contain '%s', but got: %s", expectedErrMsg, err.Error())
		}
	})
}
