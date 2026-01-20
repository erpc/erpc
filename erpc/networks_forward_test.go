package erpc

import (
	"context"
	"testing"
	"time"

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

func init() {
	util.ConfigureTestLogger()
}

func TestNetwork_Forward_InfiniteLoopWithAllUpstreamsSkipping(t *testing.T) {
	t.Run("DebugSkipBehavior", func(t *testing.T) {
		// This test logs the actual behavior to understand what's happening
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		logger := &log.Logger
		vr := thirdparty.NewVendorsRegistry()
		pr, _ := thirdparty.NewProvidersRegistry(logger, vr, []*common.ProviderConfig{}, nil)
		ssr, _ := data.NewSharedStateRegistry(ctx, logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems:     100_000,
					MaxTotalSize: "1GB",
				},
			},
		})
		rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, logger)
		mt := health.NewTracker(logger, "testProject", 2*time.Second)

		// Create a single upstream that skips everything
		up1 := &common.UpstreamConfig{
			Id:            "upstream1",
			Type:          common.UpstreamTypeEvm,
			Endpoint:      "http://rpc1.localhost",
			IgnoreMethods: []string{"*"}, // Ignore all methods
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		upsReg := upstream.NewUpstreamsRegistry(
			ctx, logger, "testProject",
			[]*common.UpstreamConfig{up1},
			ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil,
		)
		upsReg.Bootstrap(ctx)
		time.Sleep(100 * time.Millisecond)
		upsReg.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))

		ntw, _ := NewNetwork(
			ctx, logger, "testProject",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{ChainId: 123},
			},
			rlr, upsReg, mt,
		)
		ntw.Bootstrap(ctx)
		time.Sleep(100 * time.Millisecond)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1234",false]}`))

		// Add a timeout context to prevent actual infinite loop
		timeoutCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

		startTime := time.Now()
		resp, err := ntw.Forward(timeoutCtx, req)
		duration := time.Since(startTime)

		log.Logger.Info().
			Dur("duration", duration).
			Err(err).
			Bool("has_response", resp != nil).
			Msg("Request completed")

		// Check the errors stored for the upstream
		if exhErr, ok := err.(*common.ErrUpstreamsExhausted); ok {
			errorsByUpstream := exhErr.Errors()
			log.Logger.Info().
				Interface("errors_by_upstream", errorsByUpstream).
				Msg("Upstream errors")
		}

		// Should complete quickly, not timeout
		assert.Less(t, duration, 500*time.Millisecond, "Should not take long")
		assert.Error(t, err)
		assert.Nil(t, resp)
	})

	t.Run("UseUpstream_SingleMatch_AllSkip_ShouldExhaustNotTimeout", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		logger := &log.Logger
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(logger, vr, []*common.ProviderConfig{}, nil)
		require.NoError(t, err)

		ssr, err := data.NewSharedStateRegistry(ctx, logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
			},
		})
		require.NoError(t, err)

		rlr, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{Budgets: []*common.RateLimitBudgetConfig{}}, logger)
		require.NoError(t, err)

		mt := health.NewTracker(logger, "testProject", 2*time.Second)

		// Single upstream that will skip eth_getBlockByNumber
		up1 := &common.UpstreamConfig{
			Id:            "systx-upstream-1",
			Type:          common.UpstreamTypeEvm,
			Endpoint:      "http://rpc1.localhost",
			IgnoreMethods: []string{"eth_getBlockByNumber"},
			Evm:           &common.EvmUpstreamConfig{ChainId: 123},
		}

		upr := upstream.NewUpstreamsRegistry(ctx, logger, "testProject", []*common.UpstreamConfig{up1}, ssr, rlr, vr, pr, nil, mt, 0, nil)
		upr.Bootstrap(ctx)
		time.Sleep(100 * time.Millisecond)
		require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))

		ntw, err := NewNetwork(
			ctx, logger, "testProject",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{ChainId: 123},
				Failsafe: []*common.FailsafeConfig{
					{Timeout: &common.TimeoutPolicyConfig{Duration: common.Duration(250 * time.Millisecond)}},
				},
			},
			rlr, upr, mt,
		)
		require.NoError(t, err)
		require.NoError(t, ntw.Bootstrap(ctx))
		time.Sleep(50 * time.Millisecond)

		// Request with UseUpstream directive targeting the only upstream
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1234",false]}`))
		req.SetDirectives(&common.RequestDirectives{UseUpstream: "systx-*"})

		start := time.Now()
		resp, err := ntw.Forward(ctx, req)
		dur := time.Since(start)

		// EXPECTATION (desired behavior): should NOT spin until timeout; should exhaust immediately
		// Therefore, we assert UpstreamsExhausted.
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted), "expected upstreams exhausted, got: %v", err)
		assert.Nil(t, resp)
		// And it should finish quickly well under the timeout
		assert.Less(t, dur, 200*time.Millisecond)
	})

	t.Run("AllUpstreamsReturnSkipError_ShouldNotInfiniteLoop", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Create test logger
		logger := &log.Logger

		// Setup vendors and providers registry
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(logger, vr, []*common.ProviderConfig{}, nil)
		require.NoError(t, err)

		// Setup shared state registry
		ssr, err := data.NewSharedStateRegistry(ctx, logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems:     100_000,
					MaxTotalSize: "1GB",
				},
			},
		})
		require.NoError(t, err)

		// Setup rate limiters registry
		rlr, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, logger)
		require.NoError(t, err)

		// Setup metrics tracker
		mt := health.NewTracker(logger, "testProject", 2*time.Second)

		// Create three upstreams, all configured to ignore eth_getBlockByNumber
		up1 := &common.UpstreamConfig{
			Id:            "upstream1",
			Type:          common.UpstreamTypeEvm,
			Endpoint:      "http://rpc1.localhost",
			IgnoreMethods: []string{"eth_getBlockByNumber"}, // Will skip this method
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		up2 := &common.UpstreamConfig{
			Id:            "upstream2",
			Type:          common.UpstreamTypeEvm,
			Endpoint:      "http://rpc2.localhost",
			IgnoreMethods: []string{"eth_*"}, // Will skip all eth_ methods
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		up3 := &common.UpstreamConfig{
			Id:       "upstream3",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc3.localhost",
			// This upstream ignores everything except specific methods
			IgnoreMethods: []string{"*"},
			AllowMethods:  []string{"eth_chainId", "eth_call"}, // Doesn't include eth_getBlockByNumber
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		// Setup upstreams registry
		upsReg := upstream.NewUpstreamsRegistry(
			ctx,
			logger,
			"testProject",
			[]*common.UpstreamConfig{up1, up2, up3},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
			nil,
		)

		// Bootstrap upstreams
		upsReg.Bootstrap(ctx)
		time.Sleep(100 * time.Millisecond)

		// Prepare upstreams for the network
		err = upsReg.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		require.NoError(t, err)

		// Create network with a timeout to prevent actual infinite loop in test
		ntw, err := NewNetwork(
			ctx,
			logger,
			"testProject",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: []*common.FailsafeConfig{
					{
						Timeout: &common.TimeoutPolicyConfig{
							Duration: common.Duration(1 * time.Second), // Timeout to prevent actual infinite loop
						},
					},
				},
			},
			rlr,
			upsReg,
			mt,
		)
		require.NoError(t, err)

		// Bootstrap network
		err = ntw.Bootstrap(ctx)
		require.NoError(t, err)
		time.Sleep(100 * time.Millisecond)

		// Create a request for eth_getBlockByNumber which all upstreams will skip
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1234",false]}`))

		// Start timer to ensure the request doesn't run forever
		startTime := time.Now()

		// Forward the request - this should NOT infinite loop
		// The test should complete quickly (within a second) even though all upstreams skip
		resp, err := ntw.Forward(ctx, req)

		// Measure execution time
		duration := time.Since(startTime)

		// Assertions
		// 1. The request should complete within a reasonable time (not infinite loop)
		assert.Less(t, duration, 2*time.Second, "Request took too long, possible infinite loop")

		// 2. Should get an error (upstreams exhausted)
		assert.Error(t, err, "Expected error when all upstreams skip the request")

		// 3. The error should be ErrUpstreamsExhausted
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted),
			"Expected ErrUpstreamsExhausted error, got: %v", err)

		// 4. Response should be nil
		assert.Nil(t, resp, "Response should be nil when all upstreams skip")

		// 5. Verify the error message contains information about skipped upstreams
		if exhErr, ok := err.(*common.ErrUpstreamsExhausted); ok {
			// Check that error details show upstreams were skipped
			errorsByUpstream := exhErr.Errors()
			assert.NotEmpty(t, errorsByUpstream, "Should have errors for each upstream")

			// Verify each upstream has a skip error
			for _, upErr := range errorsByUpstream {
				assert.True(t,
					common.HasErrorCode(upErr, common.ErrCodeUpstreamRequestSkipped) ||
						common.HasErrorCode(upErr, common.ErrCodeUpstreamMethodIgnored),
					"Each upstream error should be a skip/ignore error, got: %v", upErr)
			}
		}

		log.Logger.Info().
			Dur("duration", duration).
			Err(err).
			Msg("Test completed - request properly terminated despite all upstreams skipping")
	})

	t.Run("MixedUpstreams_SomeSkipSomeWork", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		// Setup a successful response for the upstream that doesn't skip
		gock.New("http://rpc3.localhost").
			Post("").
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"number":     "0x1234",
					"hash":       "0xabcd",
					"parentHash": "0xefgh",
				},
			})

		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		logger := &log.Logger

		// Setup registries
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(logger, vr, []*common.ProviderConfig{}, nil)
		require.NoError(t, err)

		ssr, err := data.NewSharedStateRegistry(ctx, logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems:     100_000,
					MaxTotalSize: "1GB",
				},
			},
		})
		require.NoError(t, err)

		rlr, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, logger)
		require.NoError(t, err)

		mt := health.NewTracker(logger, "testProject", 2*time.Second)

		// Create upstreams where first two skip but third works
		up1 := &common.UpstreamConfig{
			Id:            "upstream1",
			Type:          common.UpstreamTypeEvm,
			Endpoint:      "http://rpc1.localhost",
			IgnoreMethods: []string{"eth_getBlockByNumber"},
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		up2 := &common.UpstreamConfig{
			Id:            "upstream2",
			Type:          common.UpstreamTypeEvm,
			Endpoint:      "http://rpc2.localhost",
			IgnoreMethods: []string{"eth_*"},
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		up3 := &common.UpstreamConfig{
			Id:       "upstream3",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc3.localhost",
			// This one doesn't ignore eth_getBlockByNumber
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		upsReg := upstream.NewUpstreamsRegistry(
			ctx,
			logger,
			"testProject",
			[]*common.UpstreamConfig{up1, up2, up3},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
			nil,
		)

		upsReg.Bootstrap(ctx)
		time.Sleep(100 * time.Millisecond)

		err = upsReg.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		require.NoError(t, err)

		ntw, err := NewNetwork(
			ctx,
			logger,
			"testProject",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
			},
			rlr,
			upsReg,
			mt,
		)
		require.NoError(t, err)

		err = ntw.Bootstrap(ctx)
		require.NoError(t, err)
		time.Sleep(100 * time.Millisecond)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1234",false]}`))

		// This should succeed as one upstream handles the request
		resp, err := ntw.Forward(ctx, req)

		// Should succeed
		assert.NoError(t, err, "Should succeed when at least one upstream handles the request")
		assert.NotNil(t, resp, "Should have a response")

		if resp != nil {
			defer resp.Release()

			// Verify the response came from upstream3
			assert.Equal(t, "upstream3", resp.Upstream().Id(), "Response should come from upstream3")
		}
	})
}
