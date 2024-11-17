package erpc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/erpc/erpc/vendors"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
)

func init() {
	util.ConfigureTestLogger()
}

func TestNetwork_Forward(t *testing.T) {

	t.Run("ForwardCorrectlyRateLimitedOnNetworkLevel", func(t *testing.T) {
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

		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		upsReg := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			rateLimitersRegistry,
			vendors.NewVendorsRegistry(),
			mt,
			1*time.Second,
		)
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				RateLimitBudget: "MyLimiterBudget_Test1",
			},
			rateLimitersRegistry,
			upsReg,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		err = upsReg.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upsReg.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		var lastErr error
		var lastResp *common.NormalizedResponse

		for i := 0; i < 5; i++ {
			fakeReq := common.NewNormalizedRequest([]byte(`{"method": "eth_chainId","params":[]}`))
			lastResp, lastErr = ntw.Forward(ctx, fakeReq)
		}

		var e *common.ErrNetworkRateLimitRuleExceeded
		if lastErr == nil || !errors.As(lastErr, &e) {
			t.Errorf("Expected %v, got %v", "ErrNetworkRateLimitRuleExceeded", lastErr)
		}

		log.Logger.Info().Msgf("Last Resp: %+v", lastResp)
	})

	t.Run("ForwardNotRateLimitedOnNetworkLevel", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(
			&common.RateLimiterConfig{
				Budgets: []*common.RateLimitBudgetConfig{
					{
						Id: "MyLimiterBudget_Test2",
						Rules: []*common.RateLimitRuleConfig{
							{
								Method:   "*",
								MaxCount: 1000,
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

		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		upsReg := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			rateLimitersRegistry,
			vendors.NewVendorsRegistry(),
			mt,
			1*time.Second,
		)
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				RateLimitBudget: "MyLimiterBudget_Test2",
			},
			rateLimitersRegistry,
			upsReg,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		err = upsReg.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upsReg.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}

		var lastErr error

		for i := 0; i < 10; i++ {
			fakeReq := common.NewNormalizedRequest([]byte(`{"method": "eth_chainId","params":[]}`))
			_, lastErr = ntw.Forward(ctx, fakeReq)
		}

		var e *common.ErrNetworkRateLimitRuleExceeded
		if lastErr != nil && errors.As(lastErr, &e) {
			t.Errorf("Did not expect ErrNetworkRateLimitRuleExceeded")
		}
	})

	t.Run("ForwardUpstreamRetryIntermittentFailuresWithoutSuccessAndNoErrCode", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Times(3).
			Post("").
			Reply(503).
			JSON([]byte(`{"error":{"message":"some random provider issue"}}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)

		fsCfg := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 3,
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			Failsafe: fsCfg,
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup)
		if err != nil {
			t.Fatal(err)
		}
		pup.Client = cl
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
			},
			rlr,
			upr,
			health.NewTracker("prjA", 2*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		fakeReq := common.NewNormalizedRequest(requestBytes)
		_, err = ntw.Forward(ctx, fakeReq)

		if err == nil {
			t.Errorf("Expected an error, got nil")
		} else if !strings.Contains(common.ErrorSummary(err), "ErrUpstreamsExhausted") {
			t.Errorf("Expected %v, got %v", "ErrUpstreamsExhausted", err)
		}
	})

	t.Run("ForwardRetryFailuresWithoutSuccessErrorWithCode", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":9199,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Times(3).
			Post("").
			Reply(503).
			JSON([]byte(`{"jsonrpc":"2.0","id":9199,"error":{"code":-32603,"message":"some random provider issue"}}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)

		fsCfg := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 3,
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			Failsafe: fsCfg,
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup)
		if err != nil {
			t.Fatal(err)
		}
		pup.Client = cl
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
			},
			rlr,
			upr,
			health.NewTracker("prjA", 2*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		fakeReq := common.NewNormalizedRequest(requestBytes)
		_, err = ntw.Forward(ctx, fakeReq)

		if err == nil {
			t.Errorf("Expected an error, got nil")
		}

		if !strings.Contains(common.ErrorSummary(err), "ErrUpstreamsExhausted") {
			t.Errorf("Expected %v, got %v", "ErrUpstreamsExhausted", err)
		}
	})

	t.Run("ForwardSkipsNonRetryableFailuresFromUpstreams", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Times(1).
			Post("").
			Reply(401).
			JSON([]byte(`{"error":{"code":-32016,"message":"unauthorized rpc1"}}`))

		gock.New("http://rpc2.localhost").
			Times(2).
			Post("").
			Reply(503).
			JSON([]byte(`{"error":"random rpc2 unavailable"}`))

		gock.New("http://rpc2.localhost").
			Times(1).
			Post("").
			Reply(200).
			JSON([]byte(`{"result":"0x1234567"}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)

		upsFsCfg := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 2,
			},
		}
		ntwFsCfg := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 2,
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			Failsafe: upsFsCfg,
		}
		up2 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			Failsafe: upsFsCfg,
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
				up2,
			},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl

		pup2, err := upr.NewUpstream(
			"prjA",
			up2,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: ntwFsCfg,
			},
			rlr,
			upr,
			health.NewTracker("prjA", 2*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		fakeReq := common.NewNormalizedRequest(requestBytes)
		_, err = ntw.Forward(ctx, fakeReq)

		if err != nil {
			t.Errorf("Expected an nil, got error %v", err)
		}
	})

	t.Run("ForwardNotSkipsRetryableFailuresFromUpstreams", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Times(3).
			Post("").
			Reply(503).
			JSON([]byte(`{"error":"random rpc1 unavailable"}`))

		gock.New("http://rpc2.localhost").
			Times(3).
			Post("").
			Reply(503).
			JSON([]byte(`{"error":"random rpc2 unavailable"}`))

		gock.New("http://rpc2.localhost").
			Times(1).
			Post("").
			Reply(200).
			JSON([]byte(`{"result":"0x1234567"}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)

		upsFsCfg := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 3,
			},
		}
		ntwFsCfg := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 2,
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			Failsafe: upsFsCfg,
		}
		up2 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			Failsafe: upsFsCfg,
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
				up2,
			},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl

		pup2, err := upr.NewUpstream(
			"prjA",
			up2,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: ntwFsCfg,
			},
			rlr,
			upr,
			health.NewTracker("prjA", 2*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		fakeReq := common.NewNormalizedRequest(requestBytes)
		_, err = ntw.Forward(ctx, fakeReq)

		if len(gock.Pending()) > 0 {
			t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %s => status %d, body %s", pending.Request().URLStruct, pending.Response().StatusCode, string(pending.Response().BodyBuffer))
			}
		}

		if err != nil {
			t.Errorf("Expected an nil, got error %v", err)
		}
	})

	t.Run("NotRetryWhenBlockIsFinalizedNodeIsSynced", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1)

		// Prepare a JSON-RPC request payload as a byte array
		var requestBytes = []byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getLogs",
			"params": [{
				"address": "0x1234567890abcdef1234567890abcdef12345678",
				"fromBlock": "0x4",
				"toBlock": "0x7"
			}],
			"id": 1
		}`)

		// Mock an empty logs response from the first upstream
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			JSON([]byte(`{"result":[]}`))

		// Mock a non-empty logs response from the second upstream
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			JSON([]byte(`{"result":[{"logIndex":444}]}`))

		// Set up a context and a cancellation function
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Initialize various components for the test environment
		clr := upstream.NewClientRegistry(&log.Logger)
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)

		// Set up upstream configurations
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		up2 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		// Initialize the upstreams registry
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}

		// Create and register clients for both upstreams
		pup1, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(
			"prjA",
			up2,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		// Set up the network configuration
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: &common.FailsafeConfig{
					Retry: &common.RetryPolicyConfig{
						MaxAttempts: 1,
					},
				},
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		// Bootstrap the network and make the simulated request
		ntw.Bootstrap(ctx)
		time.Sleep(100 * time.Millisecond)

		poller := ntw.evmStatePollers["rpc1"]
		poller.SuggestLatestBlock(9)
		poller.SuggestFinalizedBlock(8)

		pup1.SetEvmSyncingState(common.EvmSyncingStateNotSyncing)
		pup2.SetEvmSyncingState(common.EvmSyncingStateNotSyncing)

		// Create a fake request and forward it through the network
		fakeReq := common.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)

		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		// Convert the raw response to a map to access custom fields like fromHost
		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Failed to get JsonRpcResponse: %v", err)
		}

		// Check that the result field is an empty array as expected
		if len(jrr.Result) != 2 || jrr.Result[0] != '[' || jrr.Result[1] != ']' {
			t.Fatalf("Expected result to be an empty array, got %s", string(jrr.Result))
		}
	})

	t.Run("RetryWhenNodeIsNotSynced", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Prepare a JSON-RPC request payload as a byte array
		var requestBytes = []byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getLogs",
			"params": [{
				"address": "0x1234567890abcdef1234567890abcdef12345678",
				"fromBlock": "0x4",
				"toBlock": "0x7"
			}],
			"id": 1
		}`)

		// Mock an empty logs response from the first upstream
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			JSON([]byte(`{"result":[]}`))

		// Mock a non-empty logs response from the second upstream
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			JSON([]byte(`{"result":[{"logIndex":444}]}`))

		// Set up a context and a cancellation function
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Initialize various components for the test environment
		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfg := &common.FailsafeConfig{
			Hedge:   nil,
			Timeout: nil,
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 2, // Allow up to 2 retry attempts
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)

		// Set up upstream configurations
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		up2 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		// Initialize the upstreams registry
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}

		// Create and register clients for both upstreams
		pup1, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(
			"prjA",
			up2,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		// Set up the network configuration
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		// Bootstrap the network and make the simulated request
		ntw.Bootstrap(ctx)
		time.Sleep(100 * time.Millisecond)

		poller1 := ntw.evmStatePollers["rpc1"]
		poller1.SuggestLatestBlock(9)
		poller1.SuggestFinalizedBlock(8)

		poller2 := ntw.evmStatePollers["rpc2"]
		poller2.SuggestLatestBlock(9)
		poller2.SuggestFinalizedBlock(8)

		pup1.SetEvmSyncingState(common.EvmSyncingStateSyncing)

		time.Sleep(100 * time.Millisecond)

		// Create a fake request and forward it through the network
		fakeReq := common.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)

		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		// Convert the raw response to a map to access custom fields like fromHost
		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Failed to get JsonRpcResponse: %v", err)
		}
		var result []interface{}
		err = sonic.Unmarshal(jrr.Result, &result)
		if err != nil {
			t.Fatalf("Failed to unmarshal response body: %v", err)
		}

		if len(result) == 0 {
			t.Fatalf("Expected non-empty result array")
		}
	})

	t.Run("ForwardWithMinimumMemoryAllocation", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1)

		// Prepare a JSON-RPC request payload as a byte array
		var requestBytes = []byte(`{
			"jsonrpc": "2.0",
			"method": "debug_traceTransaction",
			"params": [
				"0x1234567890abcdef1234567890abcdef12345678"
			],
			"id": 1
		}`)

		sampleSize := 100 * 1024 * 1024
		allowedOverhead := 30 * 1024 * 1024
		largeResult := strings.Repeat("x", sampleSize)

		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "debug_traceTransaction")
			}).
			Reply(200).
			JSON([]byte(fmt.Sprintf(`{"result":"%s"}`, largeResult)))

		// Set up a context and a cancellation function
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Initialize various components for the test environment
		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfg := &common.FailsafeConfig{}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)

		// Set up upstream configurations
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		// Initialize the upstreams registry
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}

		// Create and register clients for both upstreams
		pup1, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		// Set up the network configuration
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		// Bootstrap the network and make the simulated request
		ntw.Bootstrap(ctx)
		time.Sleep(100 * time.Millisecond)

		poller1 := ntw.evmStatePollers["rpc1"]
		poller1.SuggestLatestBlock(9)
		poller1.SuggestFinalizedBlock(8)

		time.Sleep(100 * time.Millisecond)

		// Create a fake request and forward it through the network
		fakeReq := common.NewNormalizedRequest(requestBytes)

		// Measure memory usage before request
		var mBefore runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&mBefore)

		resp, err := ntw.Forward(ctx, fakeReq)

		// Measure memory usage after parsing
		var mAfter runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&mAfter)

		// Calculate the difference in memory usage
		memUsed := mAfter.Alloc - mBefore.Alloc
		memUsedMB := float64(memUsed) / (1024 * 1024)

		// Log the memory usage
		t.Logf("Memory used for request: %.2f MB", memUsedMB)

		// Check that memory used does not exceed sample size + overhead
		expectedMemUsage := uint64(sampleSize) + uint64(allowedOverhead)
		expectedMemUsageMB := float64(expectedMemUsage) / (1024 * 1024)
		if memUsed > expectedMemUsage {
			t.Fatalf("Memory usage exceeded expected limit of %.2f MB used %.2f MB", expectedMemUsageMB, memUsedMB)
		}

		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		// Convert the raw response to a map to access custom fields like fromHost
		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Failed to get JsonRpcResponse: %v", err)
		}

		// add 2 for quote marks
		if len(jrr.Result) != sampleSize+2 {
			t.Fatalf("Expected result to be %d, got %d", sampleSize+2, len(jrr.Result))
		}
	})

	t.Run("RetryWhenWeDoNotKnowNodeSyncState", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Prepare a JSON-RPC request payload as a byte array
		var requestBytes = []byte(`{
				"jsonrpc": "2.0",
				"method": "eth_getLogs",
				"params": [{
					"address": "0x1234567890abcdef1234567890abcdef12345678",
					"fromBlock": "0x4",
					"toBlock": "0x7"
				}],
				"id": 1
			}`)

		// Mock an empty logs response from the first upstream
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			JSON([]byte(`{"result":[]}`))

		// Mock a non-empty logs response from the second upstream
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			JSON([]byte(`{"result":[{"logIndex":444,"fromHost":"rpc2"}]}`))

		// Set up a context and a cancellation function
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Initialize various components for the test environment
		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfg := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 2, // Allow up to 2 retry attempts
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)

		// Set up upstream configurations
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		up2 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		// Initialize the upstreams registry
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}

		// Create and register clients for both upstreams
		pup1, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(
			"prjA",
			up2,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		// Set up the network configuration
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		// Bootstrap the network and make the simulated request
		ntw.Bootstrap(ctx)
		time.Sleep(100 * time.Millisecond)

		poller1 := ntw.evmStatePollers["rpc1"]
		poller1.SuggestLatestBlock(9)
		poller1.SuggestFinalizedBlock(8)

		poller2 := ntw.evmStatePollers["rpc2"]
		poller2.SuggestLatestBlock(9)
		poller2.SuggestFinalizedBlock(8)

		pup1.SetEvmSyncingState(common.EvmSyncingStateUnknown)
		pup2.SetEvmSyncingState(common.EvmSyncingStateUnknown)

		time.Sleep(100 * time.Millisecond)

		// Create a fake request and forward it through the network
		fakeReq := common.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)

		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		// Convert the raw response to a map to access custom fields like fromHost
		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Failed to get JSON-RPC response: %v", err)
		}

		if jrr.Result == nil {
			t.Fatalf("Expected non-nil result")
		}

		fromHost, err := jrr.PeekStringByPath(0, "fromHost")
		if err != nil {
			t.Fatalf("Failed to get fromHost from result: %v", err)
		}
		if fromHost != "rpc2" {
			t.Errorf("Expected fromHost to be %q, got %q", "rpc2", fromHost)
		}
	})

	t.Run("RetryWhenBlockIsNotFinalized", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Prepare a JSON-RPC request payload as a byte array
		var requestBytes = []byte(`{
				"jsonrpc": "2.0",
				"method": "eth_getLogs",
				"params": [{
					"address": "0x1234567890abcdef1234567890abcdef12345678",
					"fromBlock": "0x0",
					"toBlock": "0x1273c18"
				}],
				"id": 1
			}`)

		// Mock an empty logs response from the first upstream
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			JSON([]byte(`{"result":[]}`))

		// Mock a non-empty logs response from the second upstream
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			JSON([]byte(`{"result":[{"logIndex":444, "fromHost":"rpc2"}]}`))

		// Set up a context and a cancellation function
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Initialize various components for the test environment
		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfg := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 2, // Allow up to 2 retry attempts
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)

		// Set up upstream configurations
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		up2 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		// Initialize the upstreams registry
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}

		// Create and register clients for both upstreams
		pup1, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(
			"prjA",
			up2,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		// Set up the network configuration
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		// Bootstrap the network and make the simulated request
		ntw.Bootstrap(ctx)
		time.Sleep(100 * time.Millisecond)

		// Create a fake request and forward it through the network
		fakeReq := common.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)

		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		// Parse and validate the JSON-RPC response
		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Failed to get JSON-RPC response: %v", err)
		}

		if jrr.Result == nil {
			t.Fatalf("Expected non-nil result")
		}

		fromHost, err := jrr.PeekStringByPath(0, "fromHost")
		if err != nil {
			t.Fatalf("Failed to get fromHost from result: %v", err)
		}
		if fromHost != "rpc2" {
			t.Errorf("Expected fromHost to be %q, got %q", "rpc2", fromHost)
		}
	})

	t.Run("RetryWhenBlockFinalizationIsNotAvailable", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Prepare a JSON-RPC request payload as a byte array
		var requestBytes = []byte(`{
				"jsonrpc": "2.0",
				"method": "eth_getLogs",
				"params": [{
					"address": "0x1234567890abcdef1234567890abcdef12345678",
					"fromBlock": "0x0",
					"toBlock": "0x1273c18"
				}],
				"id": 1
			}`)

		// Mock an empty logs response from the first upstream
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			JSON([]byte(`{"result":[]}`))

		// Mock a non-empty logs response from the second upstream
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			JSON([]byte(`{"result":[{"logIndex":444, "fromHost":"rpc2"}]}`))

		// Set up a context and a cancellation function
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Initialize various components for the test environment
		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfg := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 2, // Allow up to 2 retry attempts
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)

		// Set up upstream configurations
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		up2 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		// Initialize the upstreams registry
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}

		// Create and register clients for both upstreams
		pup1, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(
			"prjA",
			up2,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		// Set up the network configuration
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		// Bootstrap the network and make the simulated request
		ntw.Bootstrap(ctx)
		time.Sleep(100 * time.Millisecond)

		// Create a fake request and forward it through the network
		fakeReq := common.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)

		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		// Parse and validate the JSON-RPC response
		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Failed to get JSON-RPC response: %v", err)
		}

		if jrr.Result == nil {
			t.Fatalf("Expected non-nil result")
		}

		fromHost, err := jrr.PeekStringByPath(0, "fromHost")
		if err != nil {
			t.Fatalf("Failed to get fromHost from result: %v", err)
		}
		if fromHost != "rpc2" {
			t.Errorf("Expected fromHost to be %q, got %q", "rpc2", fromHost)
		}
	})

	t.Run("RetryPendingTXsWhenDirectiveIsSet", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Mock a pending transaction response from the first upstream
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				b := util.SafeReadBody(request)
				return strings.Contains(b, "eth_getTransactionByHash")
			}).
			Reply(200).
			JSON([]byte(`{"result":{"blockNumber":null,"hash":"0xabcdef","fromHost":"rpc1"}}`))

		// Mock a non-pending transaction response from the second upstream
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				b := util.SafeReadBody(request)
				return strings.Contains(b, "eth_getTransactionByHash")
			}).
			Reply(200).
			JSON([]byte(`{"result":{"blockNumber":"0x54C563","hash":"0xabcdef","fromHost":"rpc2"}}`))

		// Set up a context and a cancellation function
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Initialize various components for the test environment
		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfg := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 2, // Allow up to 2 retry attempts
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)

		// Set up upstream configurations
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		up2 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		// Initialize the upstreams registry
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			rlr,
			vndr,
			mt,
			0,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}

		// Create and register clients for both upstreams
		pup1, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(
			"prjA",
			up2,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		// Set up the network configuration
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		// Bootstrap the network and make the simulated request
		ntw.Bootstrap(ctx)
		time.Sleep(100 * time.Millisecond)

		// Create a fake request and forward it through the network
		fakeReq := common.NewNormalizedRequest([]byte(`{
				"jsonrpc": "2.0",
				"method": "eth_getTransactionByHash",
				"params": ["0xabcdef"],
				"id": 1
			}`))
		fakeReq.ApplyDirectivesFromHttp(http.Header{}, url.Values{})
		resp, err := ntw.Forward(ctx, fakeReq)

		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		// Parse and validate the JSON-RPC response
		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Failed to get JSON-RPC response: %v", err)
		}

		if jrr.Result == nil {
			t.Fatalf("Expected non-nil result")
		}

		blockNumber, err := jrr.PeekStringByPath("blockNumber")
		if err != nil {
			t.Fatalf("Failed to get blockNumber from result: %v", err)
		}
		if blockNumber != "0x54C563" {
			t.Errorf("Expected blockNumber to be %q, got %q", "0x54C563", blockNumber)
		}

		fromHost, err := jrr.PeekStringByPath("fromHost")
		if err != nil {
			t.Fatalf("Failed to get fromHost from result: %v", err)
		}
		if fromHost != "rpc2" {
			t.Errorf("Expected fromHost to be %q, got %q", "rpc2", fromHost)
		}
	})

	t.Run("ReturnPendingDataEvenAfterRetryingExhaustedWhenDirectiveIsSet", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Mock a pending transaction response from the first upstream
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				b := util.SafeReadBody(request)
				return strings.Contains(b, "eth_getTransactionByHash")
			}).
			Reply(200).
			JSON([]byte(`{"result":{"blockNumber":null,"hash":"0xabcdef","fromHost":"rpc1"}}`))

		// Mock a non-pending transaction response from the second upstream
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				b := util.SafeReadBody(request)
				return strings.Contains(b, "eth_getTransactionByHash")
			}).
			Reply(200).
			JSON([]byte(`{"result":{"blockNumber":null,"hash":"0xabcdef","fromHost":"rpc2"}}`))

		// Set up a context and a cancellation function
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Initialize various components for the test environment
		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfg := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 3, // Allow up to 3 retry attempts
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)

		// Set up upstream configurations
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		up2 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		// Initialize the upstreams registry
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			rlr,
			vndr,
			mt,
			0,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}

		// Create and register clients for both upstreams
		pup1, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(
			"prjA",
			up2,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		// Set up the network configuration
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		// Bootstrap the network and make the simulated request
		ntw.Bootstrap(ctx)
		time.Sleep(100 * time.Millisecond)

		// Create a fake request and forward it through the network
		fakeReq := common.NewNormalizedRequest([]byte(`{
				"jsonrpc": "2.0",
				"method": "eth_getTransactionByHash",
				"params": ["0xabcdef"],
				"id": 1
			}`))

		headers := http.Header{}
		queryArgs := url.Values{}
		fakeReq.ApplyDirectivesFromHttp(headers, queryArgs)
		resp, err := ntw.Forward(ctx, fakeReq)

		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		// Parse and validate the JSON-RPC response
		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Failed to get JSON-RPC response: %v", err)
		}

		if jrr.Result == nil {
			t.Fatalf("Expected non-nil result")
		}

		hash, err := jrr.PeekStringByPath("hash")
		if err != nil {
			t.Fatalf("Failed to get hash from result: %v", err)
		}
		if hash != "0xabcdef" {
			t.Errorf("Expected hash to be %q, got %q", "0xabcdef", hash)
		}

		fromHost, err := jrr.PeekStringByPath("fromHost")
		if err != nil {
			t.Fatalf("Failed to get fromHost from result: %v", err)
		}
		if fromHost != "rpc2" {
			t.Errorf("Expected fromHost to be %q, got %q", "rpc2", fromHost)
		}
	})

	t.Run("NotRetryPendingTXsWhenDirectiveIsNotSet", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Mock a pending transaction response from the first upstream
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				b := util.SafeReadBody(request)
				return strings.Contains(b, "eth_getTransactionByHash")
			}).
			Reply(200).
			JSON([]byte(`{"result":{"blockNumber":null,"hash":"0xabcdef","fromHost":"rpc1"}}`))

		// Set up a context and a cancellation function
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Initialize various components for the test environment
		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfg := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 2, // Allow up to 2 retry attempts
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)

		// Set up upstream configurations
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		up2 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		// Initialize the upstreams registry
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			rlr,
			vndr,
			mt,
			0,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}

		// Create and register clients for both upstreams
		pup1, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(
			"prjA",
			up2,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		// Set up the network configuration
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		// Bootstrap the network and make the simulated request
		ntw.Bootstrap(ctx)
		time.Sleep(100 * time.Millisecond)

		// Create a fake request and forward it through the network
		fakeReq := common.NewNormalizedRequest([]byte(`{
				"jsonrpc": "2.0",
				"method": "eth_getTransactionByHash",
				"params": ["0xabcdef"],
				"id": 1
			}`))

		headers := http.Header{}
		headers.Set("x-erpc-retry-pending", "false")
		queryArgs := url.Values{}
		fakeReq.ApplyDirectivesFromHttp(headers, queryArgs)
		resp, err := ntw.Forward(ctx, fakeReq)

		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		// Parse and validate the JSON-RPC response
		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Failed to get JSON-RPC response: %v", err)
		}

		if jrr.Result == nil {
			t.Fatalf("Expected non-nil result")
		}

		blockNumber, err := jrr.PeekStringByPath("blockNumber")
		if err != nil {
			t.Fatalf("Failed to get blockNumber from result: %v", err)
		}
		if blockNumber != "" {
			t.Errorf("Expected blockNumber to be empty, got %q", blockNumber)
		}

		fromHost, err := jrr.PeekStringByPath("fromHost")
		if err != nil {
			t.Fatalf("Failed to get fromHost from result: %v", err)
		}
		if fromHost != "rpc1" {
			t.Fatalf("Expected fromHost to be string, got %T", fromHost)
		}
	})

	t.Run("ForwardMustNotReadFromCacheIfDirectiveIsSet", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":9199,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		// Mock the upstream response
		gock.New("http://rpc1.localhost").
			Post("").
			Times(2). // Expect two calls
			Reply(200).
			JSON([]byte(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9","fromHost":"rpc1"}}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfg := &common.FailsafeConfig{}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		// First request (should be cached)
		fakeReq1 := common.NewNormalizedRequest(requestBytes)
		resp1, err := ntw.Forward(ctx, fakeReq1)
		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		// Second request with no-cache directive
		fakeReq2 := common.NewNormalizedRequest(requestBytes)
		headers := http.Header{}
		headers.Set("x-erpc-skip-cache-read", "true")
		fakeReq2.ApplyDirectivesFromHttp(headers, url.Values{})
		resp2, err := ntw.Forward(ctx, fakeReq2)
		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		// Check that both responses are not nil and different
		if resp1 == nil || resp2 == nil {
			t.Fatalf("Expected non-nil responses")
		}
	})

	t.Run("ForwardDynamicallyAddsIgnoredMethods", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":9199,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Times(1).
			Post("").
			Reply(404).
			JSON([]byte(`{"jsonrpc":"2.0","id":9199,"error":{"code":-32601,"message":"Method not supported"}}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)

		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			AutoIgnoreUnsupportedMethods: &common.TRUE,
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup)
		if err != nil {
			t.Fatal(err)
		}
		pup.Client = cl
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
			},
			rlr,
			upr,
			health.NewTracker("prjA", 2*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		fakeReq := common.NewNormalizedRequest(requestBytes)

		// First request marks the method as ignored
		_, _ = ntw.Forward(ctx, fakeReq)
		// Second attempt will not have any more upstreams to try
		_, err = ntw.Forward(ctx, fakeReq)

		if err == nil {
			t.Errorf("Expected an error, got nil")
		}
		if !strings.Contains(err.Error(), "ErrUpstreamsExhausted") {
			t.Errorf("Expected %v, got %v", "ErrUpstreamsExhausted", err)
		}
	})

	t.Run("ForwardMustNotRetryRevertedEthCalls", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":9199,"method":"eth_call","params":[{"to":"0x362fa9d0bca5d19f743db50738345ce2b40ec99f","data":"0xa4baa10c"}]}`)

		gock.New("http://rpc1.localhost").
			Times(1).
			Post("").
			Reply(404).
			JSON([]byte(`{"jsonrpc":"2.0","id":9199,"error":{"code":-32000,"message":"historical backend error: execution reverted: Dai/insufficient-balance"}}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)

		fsCfg := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 3,
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			Failsafe: fsCfg,
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup)
		if err != nil {
			t.Fatal(err)
		}
		pup.Client = cl
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			health.NewTracker("prjA", 2*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		fakeReq := common.NewNormalizedRequest(requestBytes)

		_, err = ntw.Forward(ctx, fakeReq)

		if err == nil {
			t.Errorf("Expected an error, got nil")
		}
		if !strings.Contains(err.Error(), "ErrEndpointClientSideException") {
			t.Errorf("Expected %v, got %v", "ErrEndpointClientSideException", err)
		}
	})

	t.Run("ForwardMustNotRetryBillingIssues", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":9199,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.alchemy.com.localhost").
			Times(1).
			Post("").
			Reply(503).
			JSON([]byte(`{"jsonrpc":"2.0","id":9179,"error":{"code":-32600,"message":"Monthly capacity limit exceeded."}}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)

		fsCfg := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 3,
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		FALSE := false
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.alchemy.com.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			JsonRpc: &common.JsonRpcUpstreamConfig{
				SupportsBatch: &FALSE,
			},
			Failsafe: fsCfg,
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup)
		if err != nil {
			t.Fatal(err)
		}
		pup.Client = cl
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
			},
			rlr,
			upr,
			health.NewTracker("prjA", 10*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		fakeReq := common.NewNormalizedRequest(requestBytes)
		_, err = ntw.Forward(ctx, fakeReq)

		if err == nil {
			t.Errorf("Expected an error, got nil")
		}
		if strings.Contains(err.Error(), "ErrFailsafeRetryExceeded") {
			t.Errorf("Did not expect ErrFailsafeRetryExceeded, got %v", err)
		}
		if !strings.Contains(err.Error(), "ErrEndpointBillingIssue") {
			t.Errorf("Expected ErrEndpointBillingIssue, got %v", err)
		}
	})

	t.Run("ForwardNotRateLimitedOnNetworkLevel", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(
			&common.RateLimiterConfig{
				Budgets: []*common.RateLimitBudgetConfig{
					{
						Id: "MyLimiterBudget_Test2",
						Rules: []*common.RateLimitRuleConfig{
							{
								Method:   "*",
								MaxCount: 1000,
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

		mt := health.NewTracker("prjA", 2*time.Second)
		upsReg := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				{
					Type:     common.UpstreamTypeEvm,
					Id:       "test",
					Endpoint: "http://rpc1.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			rateLimitersRegistry,
			vendors.NewVendorsRegistry(),
			mt,
			1*time.Second,
		)

		err = upsReg.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}

		err = upsReg.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}

		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				RateLimitBudget: "MyLimiterBudget_Test2",
			},
			rateLimitersRegistry,
			upsReg,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		var lastErr error

		for i := 0; i < 10; i++ {
			fakeReq := common.NewNormalizedRequest([]byte(`{"method": "eth_chainId","params":[]}`))
			_, lastErr = ntw.Forward(ctx, fakeReq)
		}

		var e *common.ErrNetworkRateLimitRuleExceeded
		if lastErr != nil && errors.As(lastErr, &e) {
			t.Errorf("Did not expect ErrNetworkRateLimitRuleExceeded")
		}
	})

	t.Run("ForwardRetryFailuresWithoutSuccess", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Times(3).
			Post("").
			Reply(503).
			JSON([]byte(`{"error":{"message":"some random provider issue"}}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)

		fsCfg := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 3,
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup)
		if err != nil {
			t.Fatal(err)
		}
		pup.Client = cl
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		fakeReq := common.NewNormalizedRequest(requestBytes)
		_, err = ntw.Forward(ctx, fakeReq)

		if err == nil {
			t.Errorf("Expected an error, got nil")
		}
		if !strings.Contains(common.ErrorSummary(err), "ErrUpstreamsExhausted") {
			t.Errorf("Expected %v, got %v", "ErrUpstreamsExhausted", err)
		}
	})

	t.Run("ForwardRetryFailuresWithSuccess", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Times(3).
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_traceTransaction")
			}).
			Reply(503).
			JSON([]byte(`{"error":{"message":"some random provider issue"}}`))

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_traceTransaction")
			}).
			Reply(200).
			JSON([]byte(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfg := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 4,
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup)
		if err != nil {
			t.Fatal(err)
		}
		pup.Client = cl
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		fakeReq := common.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)

		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}
		if jrr.Result == nil {
			t.Fatalf("Expected result, got %v", jrr)
		}

		hash, err := jrr.PeekStringByPath("hash")
		if err != nil || hash == "" {
			t.Fatalf("Expected hash to exist and be non-empty, got %v", hash)
		}
	})

	t.Run("ForwardTimeoutPolicyFail", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_traceTransaction")
			}).
			Reply(200).
			Delay(100 * time.Millisecond).
			JSON([]byte(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup)
		if err != nil {
			t.Fatal(err)
		}
		pup.Client = cl
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: &common.FailsafeConfig{
					Timeout: &common.TimeoutPolicyConfig{
						Duration: "30ms",
					},
				},
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		fakeReq := common.NewNormalizedRequest(requestBytes)
		_, err = ntw.Forward(ctx, fakeReq)

		if err == nil {
			t.Errorf("Expected error, got nil")
		}

		// TODO in here we should ONLY see failsafe timeout error but currently sometimes we see upstream timeout, we must fix this
		if !common.HasErrorCode(err, common.ErrCodeFailsafeTimeoutExceeded) && !common.HasErrorCode(err, common.ErrCodeEndpointRequestTimeout) {
			t.Errorf("Expected %v or %v, got %v", common.ErrCodeFailsafeTimeoutExceeded, common.ErrCodeEndpointRequestTimeout, err)
		}
	})

	t.Run("ForwardTimeoutPolicyPass", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Reply(200).
			Delay(100 * time.Millisecond).
			JSON([]byte(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfg := &common.FailsafeConfig{
			Timeout: &common.TimeoutPolicyConfig{
				Duration: "1s",
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup)
		if err != nil {
			t.Fatal(err)
		}
		pup.Client = cl
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		fakeReq := common.NewNormalizedRequest(requestBytes)
		_, err = ntw.Forward(ctx, fakeReq)

		if err != nil {
			t.Errorf("Expected nil error, got %v", err)
		}

		var e *common.ErrFailsafeTimeoutExceeded
		if errors.As(err, &e) {
			t.Errorf("Did not expect %v", "ErrFailsafeTimeoutExceeded")
		}
	})

	t.Run("ForwardHedgePolicyTriggered", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Reply(200).
			JSON([]byte(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9","fromHost":"rpc1"}}`)).
			Delay(500 * time.Millisecond)

		gock.New("http://rpc2.localhost").
			Post("").
			Reply(200).
			JSON([]byte(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9","fromHost":"rpc2"}}`)).
			Delay(200 * time.Millisecond)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfg := &common.FailsafeConfig{
			Hedge: &common.HedgePolicyConfig{
				Delay:    "200ms",
				MaxCount: 1,
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		up2 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(
			"prjA",
			up2,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		fakeReq := common.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)

		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		if jrr.Result == nil {
			t.Fatalf("Expected result, got nil")
		}

		fromHost, err := jrr.PeekStringByPath("fromHost")
		if err != nil || fromHost != "rpc2" {
			t.Errorf("Expected fromHost to be %v, got %v", "rpc2", fromHost)
		}
	})

	t.Run("ForwardHedgePolicyNotTriggered", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Reply(200).
			JSON([]byte(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9","fromHost":"rpc1"}}`)).
			Delay(20 * time.Millisecond)

		log.Logger.Info().Msgf("Mocks registered: %d", len(gock.Pending()))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfg := &common.FailsafeConfig{
			Hedge: &common.HedgePolicyConfig{
				Delay:    "100ms",
				MaxCount: 5,
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		up2 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(
			"prjA",
			up2,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		fakeReq := common.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)

		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}
		if jrr.Result == nil {
			t.Fatalf("Expected result, got nil")
		}

		fromHost, err := jrr.PeekStringByPath("fromHost")
		if err != nil || fromHost != "rpc1" {
			t.Errorf("Expected fromHost to be %v, got %v", "rpc1", fromHost)
		}
	})

	t.Run("ForwardHedgePolicySkipsWriteMethods", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["0x1273c18"]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Reply(200).
			JSON([]byte(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9","fromHost":"rpc1"}}`)).
			Delay(2000 * time.Millisecond)

		log.Logger.Info().Msgf("Mocks registered: %d", len(gock.Pending()))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfg := &common.FailsafeConfig{
			Hedge: &common.HedgePolicyConfig{
				Delay:    "100ms",
				MaxCount: 5,
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		up2 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://alchemy.com",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(
			"prjA",
			up2,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		fakeReq := common.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)

		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}
		if jrr.Result == nil {
			t.Fatalf("Expected result, got nil")
		}

		fromHost, err := jrr.PeekStringByPath("fromHost")
		if err != nil || fromHost != "rpc1" {
			t.Errorf("Expected fromHost to be %v, got %v", "rpc1", fromHost)
		}
	})

	t.Run("ForwardCBOpensAfterConstantFailure", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Times(2).
			Reply(200).
			JSON([]byte(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

		gock.New("http://rpc1.localhost").
			Post("").
			Times(2).
			Reply(503).
			JSON([]byte(`{"error":{"message":"some random provider issue"}}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfg := &common.FailsafeConfig{
			CircuitBreaker: &common.CircuitBreakerPolicyConfig{
				FailureThresholdCount:    2,
				FailureThresholdCapacity: 4,
				HalfOpenAfter:            "2s",
			},
		}

		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("test_cb", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "upstream1",
			Endpoint: "http://rpc1.localhost",
			Failsafe: fsCfg,
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"test_cb",
			[]*common.UpstreamConfig{up1},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(
			"test_cb",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl
		ntw, err := NewNetwork(
			&log.Logger,
			"test_cb",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		var lastErr error
		for i := 0; i < 10; i++ {
			fakeReq := common.NewNormalizedRequest(requestBytes)
			_, lastErr = ntw.Forward(ctx, fakeReq)
		}

		if lastErr == nil {
			t.Fatalf("Expected an error, got nil")
		}

		var e *common.ErrFailsafeCircuitBreakerOpen
		if !errors.As(lastErr, &e) {
			t.Errorf("Expected %v, got %v", "ErrFailsafeCircuitBreakerOpen", lastErr)
		}
	})

	t.Run("ForwardSkipsOpenedCB", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Times(1).
			Reply(503).
			JSON([]byte(`{"error":{"message":"some random provider issue"}}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfgNetwork := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 1,
			},
		}
		fsCfgUp1 := &common.FailsafeConfig{
			CircuitBreaker: &common.CircuitBreakerPolicyConfig{
				FailureThresholdCount:    1,
				FailureThresholdCapacity: 1,
				HalfOpenAfter:            "20s",
			},
		}

		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("test_cb", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "upstream1",
			Endpoint: "http://rpc1.localhost",
			Failsafe: fsCfgUp1,
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"test_cb",
			[]*common.UpstreamConfig{up1},
			rlr,
			vndr, mt, 1*time.Hour,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}

		pup1, err := upr.NewUpstream(
			"test_cb",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		ntw, err := NewNetwork(
			&log.Logger,
			"test_cb",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfgNetwork,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		var lastErr error
		var resp *common.NormalizedResponse
		for i := 0; i < 2; i++ {
			fakeReq := common.NewNormalizedRequest(requestBytes)
			resp, lastErr = ntw.Forward(ctx, fakeReq)
		}

		if lastErr == nil {
			t.Fatalf("Expected an error, got nil, resp: %v", resp)
		}
		if !strings.Contains(lastErr.Error(), "ErrUpstreamsExhausted") {
			t.Errorf("Expected error message to contain 'ErrUpstreamsExhausted', got %v", lastErr.Error())
		}
	})

	t.Run("ForwardCBClosesAfterUpstreamIsBackUp", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Times(3).
			Reply(200).
			JSON([]byte(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

		gock.New("http://rpc1.localhost").
			Post("").
			Times(3).
			Reply(503).
			JSON([]byte(`{"error":{"message":"some random provider issue"}}`))

		gock.New("http://rpc1.localhost").
			Post("").
			Times(3).
			Reply(200).
			JSON([]byte(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfg := &common.FailsafeConfig{
			CircuitBreaker: &common.CircuitBreakerPolicyConfig{
				FailureThresholdCount:    2,
				FailureThresholdCapacity: 4,
				HalfOpenAfter:            "2s",
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("test_cb", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "upstream1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"test_cb",
			[]*common.UpstreamConfig{up1},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(
			"test_cb",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl

		ntw, err := NewNetwork(
			&log.Logger,
			"test_cb",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		var lastErr error
		for i := 0; i < 4+2; i++ {
			fakeReq := common.NewNormalizedRequest(requestBytes)
			_, lastErr = ntw.Forward(ctx, fakeReq)
		}

		if lastErr == nil {
			t.Fatalf("Expected an error, got nil")
		}

		time.Sleep(2 * time.Second)

		var resp *common.NormalizedResponse
		for i := 0; i < 3; i++ {
			fakeReq := common.NewNormalizedRequest(requestBytes)
			resp, lastErr = ntw.Forward(ctx, fakeReq)
		}

		if lastErr != nil {
			t.Fatalf("Expected nil error, got %v", lastErr)
		}

		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}
		hash, err := jrr.PeekStringByPath("hash")
		if err != nil || hash == "" {
			t.Fatalf("Expected hash to exist and be non-empty, got %v", hash)
		}
	})

	t.Run("ForwardCorrectResultForUnknownEndpointError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Reply(500).
			JSON([]byte(`{"error":{"code":-39999,"message":"my funky random error"}}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfg := &common.FailsafeConfig{}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}

		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		fakeReq := common.NewNormalizedRequest(requestBytes)
		_, err = ntw.Forward(ctx, fakeReq)

		if err == nil {
			t.Fatalf("Expected non-nil error, got nil")
		}

		ser, ok := err.(common.StandardError)
		if !ok {
			t.Fatalf("Expected error to be StandardError, got %T", err)
		}
		sum := common.ErrorSummary(ser.GetCause())

		if !strings.Contains(sum, "ErrEndpointServerSideException") {
			t.Fatalf("Expected error code ErrEndpointServerSideException, got %v", sum)
		}
		if !strings.Contains(sum, "my funky random error") {
			t.Fatalf("Expected error text 'my funky random error', but was missing %v", sum)
		}
	})

	t.Run("ForwardEndpointServerSideExceptionSuccess", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Reply(500).
			JSON([]byte(`{"error":{"message":"Internal error"}}`))

		gock.New("http://rpc2.localhost").
			Post("").
			Reply(200).
			JSON([]byte(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9","fromHost":"rpc2"}}`))

		log.Logger.Info().Msgf("Mocks registered: %d", len(gock.Pending()))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfg := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 2,
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}

		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		up2 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(
			"prjA",
			up2,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		fakeReq := common.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)

		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		if jrr.Result == nil {
			t.Fatalf("Expected result, got nil")
		}

		// Debug logging
		t.Logf("jrr.Result type: %T", jrr.Result)
		t.Logf("jrr.Result content: %+v", jrr.Result)

		fromHost, err := jrr.PeekStringByPath("fromHost")
		if err != nil || fromHost != "rpc2" {
			t.Errorf("Expected fromHost to be %v, got %v", "rpc2", fromHost)
		}
	})

	t.Run("ForwardIgnoredMethod", func(t *testing.T) {
		var requestBytes = []byte(`{"jsonrpc":"2.0","id":1,"method":"ignored_method","params":["0x1273c18",false]}`)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfg := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 2,
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}

		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			IgnoreMethods: []string{"ignored_method"},
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		fakeReq := common.NewNormalizedRequest(requestBytes)
		_, err = ntw.Forward(ctx, fakeReq)

		if err == nil {
			t.Fatalf("Expected non-nil error, got nil")
		}

		if !common.HasErrorCode(err, common.ErrCodeUpstreamMethodIgnored) {
			t.Fatalf("Expected error code %v, got %v", common.ErrCodeUpstreamMethodIgnored, err)
		}
	})

	t.Run("ForwardEthGetLogsEmptyArrayResponseSuccess", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc": "2.0","method": "eth_getLogs","params":[{"address":"0x1234567890abcdef1234567890abcdef12345678"}],"id": 1}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Reply(200).
			JSON([]byte(`{"result":[]}`))

		gock.New("http://rpc2.localhost").
			Post("").
			Reply(200).
			JSON([]byte(`{"result":[{"logIndex":444,"fromHost":"rpc2"}]}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfg := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 2,
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		up2 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(
			"prjA",
			up1,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(
			"prjA",
			up2,
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		fakeReq := common.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)

		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Failed to get JSON-RPC response: %v", err)
		}

		if jrr.Result == nil {
			t.Fatalf("Expected non-nil result")
		}

		fromHost, err := jrr.PeekStringByPath(0, "fromHost")
		if err != nil || fromHost != "rpc2" {
			t.Errorf("Expected fromHost to be %q, got %q", "rpc2", fromHost)
		}
	})

	t.Run("ForwardEthGetLogsBothEmptyArrayResponse", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc": "2.0","method": "eth_getLogs","params":[{"address":"0x1234567890abcdef1234567890abcdef12345678"}],"id": 1}`)

		emptyResponse := []byte(`{"jsonrpc": "2.0","id": 1,"result":[]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			JSON(emptyResponse)

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			JSON(emptyResponse)

		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		fsCfg := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 2,
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatalf("Failed to create rate limiters registry: %v", err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		up2 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatalf("Failed to bootstrap upstreams registry: %v", err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatalf("Failed to prepare upstreams for network: %v", err)
		}

		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatalf("Failed to create network: %v", err)
		}

		fakeReq := common.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)

		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Failed to get JSON-RPC response: %v", err)
		}

		if jrr.Result == nil {
			t.Fatalf("Expected non-nil result")
		}

		if len(jrr.Result) != 2 || jrr.Result[0] != '[' || jrr.Result[1] != ']' {
			t.Errorf("Expected empty array result, got %s", string(jrr.Result))
		}
	})

	t.Run("ForwardQuicknodeEndpointRateLimitResponse", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":["0x1273c18"]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Reply(429).
			JSON([]byte(`{"error":{"code":-32007,"message":"300/second request limit reached - reduce calls per second or upgrade your account at quicknode.com"}}`))

		log.Logger.Info().Msgf("Mocks registered: %d", len(gock.Pending()))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		fsCfg := &common.FailsafeConfig{}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		FALSE := false
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			VendorName: "quicknode",
			JsonRpc: &common.JsonRpcUpstreamConfig{
				SupportsBatch: &FALSE,
			},
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatalf("Failed to bootstrap upstreams registry: %v", err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatalf("Failed to prepare upstreams for network: %v", err)
		}

		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		fakeReq := common.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)

		if err == nil {
			t.Errorf("Expected non-nil error, got nil")
			return
		}

		if resp != nil {
			t.Errorf("Expected nil response, got %v", resp)
			return
		}

		if !common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded) {
			t.Errorf("Expected error code %v, got %+v", common.ErrCodeEndpointCapacityExceeded, err)
		}
	})

	t.Run("ForwardLlamaRPCEndpointRateLimitResponseSingle", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getBalance")
			}).
			Reply(200).
			BodyString(`error code: 1015`)

		log.Logger.Info().Msgf("Mocks registered: %d", len(gock.Pending()))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		fsCfg := &common.FailsafeConfig{}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			JsonRpc: &common.JsonRpcUpstreamConfig{
				SupportsBatch: &common.FALSE,
			},
			VendorName: "llama",
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatalf("Failed to bootstrap upstreams registry: %v", err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatalf("Failed to prepare upstreams for network: %v", err)
		}

		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		fakeReq := common.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)

		if err == nil {
			t.Errorf("Expected non-nil error, got nil")
			return
		}

		if resp != nil {
			t.Errorf("Expected nil response, got %v", resp)
			return
		}

		if !common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded) {
			t.Errorf("Expected error code %v, got %+v", common.ErrCodeEndpointCapacityExceeded, err)
		}
	})

	t.Run("ForwardLlamaRPCEndpointRateLimitResponseBatch", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getBalance")
			}).
			Reply(200).
			BodyString(`error code: 1015`)

		log.Logger.Info().Msgf("Mocks registered: %d", len(gock.Pending()))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		fsCfg := &common.FailsafeConfig{}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			JsonRpc: &common.JsonRpcUpstreamConfig{
				SupportsBatch: &common.TRUE,
			},
			VendorName: "llama",
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1},
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatalf("Failed to bootstrap upstreams registry: %v", err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatalf("Failed to prepare upstreams for network: %v", err)
		}

		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		fakeReq := common.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)

		if err == nil {
			t.Errorf("Expected non-nil error, got nil")
			return
		}

		if resp != nil {
			t.Errorf("Expected nil response, got %v", resp)
			return
		}

		if !common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded) {
			t.Errorf("Expected error code %v, got %+v", common.ErrCodeEndpointCapacityExceeded, err)
		}
	})

	t.Run("DynamicMethodSpecificLatencyPreference", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 9)

		projectID := "test-project"
		networkID := "evm:123"

		logger := log.Logger
		metricsTracker := health.NewTracker(projectID, 1*time.Hour)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		metricsTracker.Bootstrap(ctx)

		rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &logger)
		assert.NoError(t, err)

		upstreamConfigs := []*common.UpstreamConfig{
			{Id: "upstream-a", Endpoint: "http://upstream-a.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}},
			{Id: "upstream-b", Endpoint: "http://upstream-b.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}},
			{Id: "upstream-c", Endpoint: "http://upstream-c.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		}

		upstreamsRegistry := upstream.NewUpstreamsRegistry(
			ctx,
			&logger,
			projectID,
			upstreamConfigs,
			rateLimitersRegistry,
			vendors.NewVendorsRegistry(),
			metricsTracker,
			1*time.Second,
		)

		err = upstreamsRegistry.Bootstrap(ctx)
		assert.NoError(t, err)

		err = upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, networkID)
		assert.NoError(t, err)

		networksRegistry := NewNetworksRegistry(
			upstreamsRegistry,
			metricsTracker,
			nil,
			rateLimitersRegistry,
		)

		ntw, err := networksRegistry.RegisterNetwork(
			&logger,
			&common.ProjectConfig{Id: projectID},
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{ChainId: 123},
			},
		)
		assert.NoError(t, err)

		mockRequests := func(method string, upstreamId string, latency time.Duration) {
			gock.New("http://" + upstreamId + ".localhost").
				Persist().
				Post("/").
				Filter(func(request *http.Request) bool {
					// seek body in request without changing the original Body buffer
					body := util.SafeReadBody(request)
					return strings.Contains(body, method) && strings.Contains(request.Host, upstreamId)
				}).
				Reply(200).
				BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x1","method":"` + method + `","upstreamId":"` + upstreamId + `","latency":` + fmt.Sprintf("%d", latency.Milliseconds()) + `}`).
				Delay(latency)
		}

		// Upstream A is faster for eth_call, Upstream B is faster for eth_traceTransaction, Upstream C is faster for eth_getLogs
		mockRequests("eth_getLogs", "upstream-a", 200*time.Millisecond)
		mockRequests("eth_getLogs", "upstream-b", 100*time.Millisecond)
		mockRequests("eth_getLogs", "upstream-c", 50*time.Millisecond)
		mockRequests("eth_traceTransaction", "upstream-a", 100*time.Millisecond)
		mockRequests("eth_traceTransaction", "upstream-b", 50*time.Millisecond)
		mockRequests("eth_traceTransaction", "upstream-c", 200*time.Millisecond)
		mockRequests("eth_call", "upstream-a", 50*time.Millisecond)
		mockRequests("eth_call", "upstream-b", 200*time.Millisecond)
		mockRequests("eth_call", "upstream-c", 100*time.Millisecond)

		allMethods := []string{"eth_getLogs", "eth_traceTransaction", "eth_call"}

		upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, networkID)
		time.Sleep(2 * time.Second)

		upstreamsRegistry.RefreshUpstreamNetworkMethodScores()
		time.Sleep(2 * time.Second)

		wg := sync.WaitGroup{}
		for _, method := range allMethods {
			for i := 0; i < 100; i++ {
				wg.Add(1)
				go func(method string) {
					defer wg.Done()
					upstreamsRegistry.RefreshUpstreamNetworkMethodScores()
					req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"%s","params":[],"id":1}`, method)))
					req.SetNetwork(ntw)
					oups, err := upstreamsRegistry.GetSortedUpstreams(networkID, method)
					upstreamsRegistry.RLockUpstreams()
					ups := []*upstream.Upstream{}
					ups = append(ups, oups...)
					upstreamsRegistry.RUnlockUpstreams()
					assert.NoError(t, err)
					for _, up := range ups {
						_, err = up.Forward(ctx, req)
						assert.NoError(t, err)
					}
				}(method)
				// time.Sleep(1 * time.Millisecond)
			}
		}
		wg.Wait()

		time.Sleep(2 * time.Second)
		upstreamsRegistry.RefreshUpstreamNetworkMethodScores()

		sortedUpstreamsGetLogs, err := upstreamsRegistry.GetSortedUpstreams(networkID, "eth_getLogs")
		assert.NoError(t, err)
		assert.Equal(t, "upstream-c", sortedUpstreamsGetLogs[0].Config().Id, "Expected upstream-c to be preferred for eth_getLogs in Phase 1")

		sortedUpstreamsTraceTransaction, err := upstreamsRegistry.GetSortedUpstreams(networkID, "eth_traceTransaction")
		assert.NoError(t, err)
		assert.Equal(t, "upstream-b", sortedUpstreamsTraceTransaction[0].Config().Id, "Expected upstream-b to be preferred for eth_traceTransaction in Phase 1")

		sortedUpstreamsCall, err := upstreamsRegistry.GetSortedUpstreams(networkID, "eth_call")
		assert.NoError(t, err)
		assert.Equal(t, "upstream-a", sortedUpstreamsCall[0].Config().Id, "Expected upstream-a to be preferred for eth_call in Phase 1")
	})

	t.Run("ForwardEnvioUnsupportedNetwork", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"address":"0x1234567890abcdef1234567890abcdef12345678"}],"id": 1}`)

		gock.New("https://rpc.hypersync.xyz").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_chainId")
			}).
			Reply(500).
			BodyString(`{"error": "Internal Server Error"}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			JSON([]byte(`{"result":[{"logIndex":444}]}`))

		log.Logger.Info().Msgf("Mocks registered: %d", len(gock.Pending()))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		fsCfg := &common.FailsafeConfig{}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		vndr := vendors.NewVendorsRegistry()
		mt := health.NewTracker("prjA", 2*time.Second)

		// First upstream (Envio) with unsupported network
		upEnvio := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvmEnvio,
			Id:       "envio",
			Endpoint: "envio://rpc.hypersync.xyz",
			JsonRpc: &common.JsonRpcUpstreamConfig{
				SupportsBatch: &common.TRUE,
			},
			VendorName: "envio",
		}

		// Second upstream (RPC1)
		upRpc1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			JsonRpc: &common.JsonRpcUpstreamConfig{
				SupportsBatch: &common.TRUE,
			},
		}

		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{upEnvio, upRpc1}, // Both upstreams
			rlr,
			vndr, mt, 1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatalf("Failed to bootstrap upstreams registry: %v", err)
		}

		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatalf("Failed to prepare upstreams for network: %v", err)
		}

		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		fakeReq := common.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)
		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		// Convert the raw response to a map to access custom fields like fromHost
		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Failed to get JSON-RPC response: %v", err)
		}

		// Check that the result field is an empty array as expected
		result := []interface{}{}
		err = sonic.Unmarshal(jrr.Result, &result)
		if err != nil {
			t.Fatalf("Failed to unmarshal result: %v", err)
		}
		if len(result) == 0 {
			t.Fatalf("Expected non-empty result array")
		}
	})

	for i := 0; i < 10; i++ {
		t.Run("ResponseReleasedBeforeCacheSet", func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()
			defer util.AssertNoPendingMocks(t, 0)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			network := setupTestNetwork(t, ctx, nil, nil)
			gock.New("http://rpc1.localhost").
				Post("/").
				MatchType("json").
				JSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"method":  "eth_getTransactionReceipt",
					"params":  []interface{}{"0x1111"},
					"id":      11111,
				}).
				Reply(200).
				JSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      11111,
					"result": map[string]interface{}{
						"blockNumber": "0x1111",
					},
				})
			gock.New("http://rpc1.localhost").
				Post("/").
				MatchType("json").
				JSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"method":  "eth_getBalance",
					"params":  []interface{}{"0x2222", "0x2222"},
					"id":      22222,
				}).
				Reply(200).
				JSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      22222,
					"result":  "0x22222222222222",
				})

			// Create a slow cache to increase the chance of a race condition
			conn, errc := data.NewMockMemoryConnector(ctx, &log.Logger, &common.MemoryConnectorConfig{
				MaxItems: 1000,
			}, 100*time.Millisecond)
			if errc != nil {
				t.Fatalf("Failed to create mock memory connector: %v", errc)
			}
			slowCache := (&EvmJsonRpcCache{
				conn:   conn,
				logger: &log.Logger,
			}).WithNetwork(network)
			network.cacheDal = slowCache

			// Make the request
			req1 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getTransactionReceipt","params":["0x1111"],"id":11111}`))
			req2 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x2222", "0x2222"],"id":22222}`))

			// Use a WaitGroup to ensure both goroutines complete
			var wg sync.WaitGroup
			wg.Add(2)

			var jrr1Atomic atomic.Value
			var jrr2Atomic atomic.Value

			// Goroutine 1: Make the request and immediately release the response
			go func() {
				defer wg.Done()

				resp1, err := network.Forward(ctx, req1)
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
					return
				}
				jrr1Value, _ := resp1.JsonRpcResponse()
				jrr1Atomic.Store(jrr1Value)
				// Simulate immediate release of the response
				resp1.Release()

				resp2, err := network.Forward(ctx, req2)
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
					return
				}
				jrr2Value, _ := resp2.JsonRpcResponse()
				jrr2Atomic.Store(jrr2Value)
				resp2.Release()
			}()

			// Goroutine 2: Access the response concurrently
			go func() {
				defer wg.Done()
				time.Sleep(500 * time.Millisecond)
				var res1 string
				var res2 string
				jrr1 := jrr1Atomic.Load().(*common.JsonRpcResponse)
				jrr2 := jrr2Atomic.Load().(*common.JsonRpcResponse)
				if jrr1 != nil {
					res1 = string(jrr1.Result)
					_ = jrr1.ID()
				}
				if jrr2 != nil {
					res2 = string(jrr2.Result)
					_ = jrr2.ID()
				}
				assert.NotEmpty(t, res1)
				assert.NotEmpty(t, res2)
				assert.NotEqual(t, res1, res2)
				cache1, e1 := slowCache.Get(ctx, req1)
				cache2, e2 := slowCache.Get(ctx, req2)
				assert.NoError(t, e1)
				assert.NoError(t, e2)
				cjrr1, _ := cache1.JsonRpcResponse()
				cjrr2, _ := cache2.JsonRpcResponse()
				assert.NotNil(t, cjrr1)
				assert.NotNil(t, cjrr2)
				if cjrr1 != nil {
					assert.Equal(t, res1, string(cjrr1.Result))
				}
				if cjrr2 != nil {
					assert.Equal(t, res2, string(cjrr2.Result))
				}
			}()

			// Wait for both goroutines to complete
			wg.Wait()
		})
	}

	t.Run("BatchRequestValidationAndRetry", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Set up the test environment
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetwork(t, ctx, &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			JsonRpc: &common.JsonRpcUpstreamConfig{
				SupportsBatch: &common.TRUE,
			},
			Failsafe: &common.FailsafeConfig{
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 2,
				},
			},
		}, nil)

		// Mock the response for the batch request
		gock.New("http://rpc1.localhost").
			Post("/").
			Reply(200).
			BodyString(`[
				{
					"jsonrpc": "2.0",
					"id": 32,
					"error": {
						"code": -32602,
						"message": "Invalid params",
						"data": {
							"range": "the range 56224203 - 56274202 exceeds the range allowed for your plan (49999 > 2000)."
						}
					}
				},
				{
					"jsonrpc": "2.0",
					"id": 43,
					"error": {
						"code": -32600,
						"message": "Invalid Request",
						"data": {
							"message": "Cancelled due to validation errors in batch request"
						}
					}
				}
			]`)
		gock.New("http://rpc1.localhost").
			Post("/").
			Reply(200).
			BodyString(`[
				{
					"jsonrpc": "2.0",
					"id": 43,
					"result": "0x22222222222222"
				}
			]`)

		// Create normalized requests
		req1 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":32,"method":"eth_getLogs","params":[{"fromBlock":"0x35A35CB","toBlock":"0x35AF7CA"}]}`))
		req2 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":43,"method":"eth_getBalance","params":["0x742d35Cc6634C0532925a3b844Bc454e4438f44e", "latest"]}`))

		// Process requests
		var resp1, resp2 *common.NormalizedResponse
		var err1, err2 error

		wg := sync.WaitGroup{}
		wg.Add(2)
		go func() {
			defer wg.Done()
			resp1, err1 = network.Forward(ctx, req1)
		}()
		go func() {
			defer wg.Done()
			resp2, err2 = network.Forward(ctx, req2)
		}()
		wg.Wait()

		// Assertions for the first request (server-side error, should be retried)
		assert.Error(t, err1, "Expected an error for the first request")
		assert.Nil(t, resp1, "Expected nil response for the first request")
		assert.False(t, common.IsRetryableTowardsUpstream(err1), "Expected a retryable error for the first request")
		assert.True(t, common.HasErrorCode(err1, common.ErrCodeEndpointClientSideException), "Expected a client-side exception error for the second request")

		// Assertions for the second request (client-side error, should not be retried)
		assert.Nil(t, err2, "Expected no error for the second request")
		assert.NotNil(t, resp2, "Expected non-nil response for the second request")
	})
}

func TestNetwork_SelectionScenarios(t *testing.T) {
	t.Run("StatePollerContributesToErrorRateWhenNotResamplingExcludedUpstreams", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		selectionPolicy := &common.SelectionPolicyConfig{
			ResampleExcluded: false,
			EvalInterval:     100 * time.Millisecond,
		}
		selectionPolicy.SetDefaults()

		// Mock failing responses for evm state poller
		gock.New("http://rpc1.localhost").
			Post("").
			Times(32).
			Reply(500).
			JSON([]byte(`{"error":{"code":-32000,"message":"Internal error"}}`))

		// Now mock successful responses
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "latest")
			}).
			Reply(200).
			JSON([]byte(`{"result":{"number":"0x11118888"}}`))
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "finalized")
			}).
			Reply(200).
			JSON([]byte(`{"result":{"number":"0x11117777"}}`))
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_syncing")
			}).
			Reply(200).
			JSON([]byte(`{"result":false}`))

		// Create network with default selection policy and disabled resampling
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetwork(t, ctx, &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId:             123,
				StatePollerInterval: "50ms", // Fast polling for test
			},
			JsonRpc: &common.JsonRpcUpstreamConfig{
				SupportsBatch: &common.FALSE,
			},
		}, &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				ChainId: 123,
			},
			SelectionPolicy: selectionPolicy,
		})

		// Let the state poller run and accumulate errors
		time.Sleep(300 * time.Millisecond)

		ups1 := network.upstreamsRegistry.GetNetworkUpstreams("evm:123")[0]

		// Verify the upstream is marked as inactive due to high error rate
		err := network.selectionPolicyEvaluator.AcquirePermit(&log.Logger, ups1, "eth_getBalance")
		assert.Error(t, err, "Upstream should be inactive due to state poller errors")
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamExcludedByPolicy),
			"Expected upstream to be excluded by policy")

		// Verify metrics show high error rate from state poller requests
		metrics := network.metricsTracker.GetUpstreamMethodMetrics("rpc1", "evm:123", "*")
		assert.True(t, metrics.ErrorRate() > 0.7,
			"Expected error rate above 70%% due to state poller failures, got %.2f%%",
			metrics.ErrorRate()*100)

		// Let the state poller improve the metrics
		time.Sleep(600 * time.Millisecond)

		// Verify the upstream becomes active again as error rate improves
		err = network.selectionPolicyEvaluator.AcquirePermit(&log.Logger, ups1, "eth_getBalance")
		assert.NoError(t, err, "Upstream should be active after error rate improves")

		// Verify metrics show improved error rate
		metrics = network.metricsTracker.GetUpstreamMethodMetrics("rpc1", "evm:123", "*")
		assert.True(t, metrics.ErrorRate() < 0.7,
			"Expected error rate below 70%% after successful requests, got %.2f%%",
			metrics.ErrorRate()*100)
	})

}

var testMu sync.Mutex

func TestNetwork_InFlightRequests(t *testing.T) {
	t.Run("MultipleSuccessfulConcurrentRequests", func(t *testing.T) {
		testMu.Lock()
		defer testMu.Unlock()
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetwork(t, ctx, nil, nil)
		requestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[]}`)

		gock.New("http://rpc1.localhost").
			Post("/").
			Persist().
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			Delay(1 * time.Second). // Delay a bit so in-flight multiplexing kicks in
			BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`)

		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := common.NewNormalizedRequest(requestBytes)
				resp, err := network.Forward(ctx, req)
				assert.NoError(t, err)
				assert.NotNil(t, resp)
			}()
		}
		wg.Wait()
	})

	t.Run("MultipleConcurrentRequestsWithFailure", func(t *testing.T) {
		testMu.Lock()
		defer testMu.Unlock()
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetwork(t, ctx, nil, nil)
		requestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[]}`)

		gock.New("http://rpc1.localhost").
			Post("/").
			Times(1).
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(500).
			Delay(1 * time.Second).
			BodyString(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"Internal error"}}`)

		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := common.NewNormalizedRequest(requestBytes)
				resp, err := network.Forward(ctx, req)
				assert.Error(t, err)
				assert.Nil(t, resp)
			}()
		}
		wg.Wait()
	})

	t.Run("MultipleConcurrentRequestsWithContextTimeout", func(t *testing.T) {
		testMu.Lock()
		defer testMu.Unlock()
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetwork(t, ctx, &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			Failsafe: &common.FailsafeConfig{
				Retry: nil,
				Hedge: nil,
				Timeout: &common.TimeoutPolicyConfig{
					Duration: "50ms",
				},
			},
		}, nil)
		requestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[]}`)

		gock.New("http://rpc1.localhost").
			Post("/").
			Persist().
			Filter(func(request *http.Request) bool {
				bd := util.SafeReadBody(request)
				return strings.Contains(bd, "eth_getLogs")
			}).
			Reply(200).
			Delay(100 * time.Second).
			BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`)

		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(ctx, 10000*time.Millisecond)
				defer cancel()
				req := common.NewNormalizedRequest(requestBytes)
				resp, err := network.Forward(ctx, req)
				assert.Error(t, err)
				// TODO we should only expect ErrFailsafeTimeoutExceeded, but for now ErrEndpointRequestTimeout is also sometimes returned
				if !common.HasErrorCode(err, "ErrFailsafeTimeoutExceeded") && !common.HasErrorCode(err, "ErrEndpointRequestTimeout") {
					t.Errorf("Expected ErrFailsafeTimeoutExceeded or ErrEndpointRequestTimeout, got %v", err)
				}
				assert.Nil(t, resp)
			}()
		}
		wg.Wait()
	})

	t.Run("MixedSuccessAndFailureConcurrentRequests", func(t *testing.T) {
		testMu.Lock()
		defer testMu.Unlock()
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetwork(t, ctx, nil, nil)
		successRequestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[]}`)
		failureRequestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"]}`)

		gock.New("http://rpc1.localhost").
			Post("/").
			Times(1).
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			Delay(1 * time.Second).
			BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`)

		gock.New("http://rpc1.localhost").
			Post("/").
			Times(1).
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getBalance")
			}).
			Reply(500).
			Delay(1 * time.Second).
			BodyString(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"Internal error"}}`)

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			req := common.NewNormalizedRequest(successRequestBytes)
			resp, err := network.Forward(ctx, req)
			assert.NoError(t, err)
			assert.NotNil(t, resp)
		}()

		go func() {
			defer wg.Done()
			req := common.NewNormalizedRequest(failureRequestBytes)
			resp, err := network.Forward(ctx, req)
			assert.Error(t, err)
			assert.Nil(t, resp)
		}()

		wg.Wait()
	})

	t.Run("SequentialInFlightRequests", func(t *testing.T) {
		testMu.Lock()
		defer testMu.Unlock()
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetwork(t, ctx, nil, nil)
		requestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[]}`)

		gock.New("http://rpc1.localhost").
			Post("/").
			Times(2).
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			Delay(1 * time.Second).
			BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`)

		// First request
		req1 := common.NewNormalizedRequest(requestBytes)
		resp1, err1 := network.Forward(ctx, req1)
		assert.NoError(t, err1)
		assert.NotNil(t, resp1)

		// Second request (should not be in-flight)
		req2 := common.NewNormalizedRequest(requestBytes)
		resp2, err2 := network.Forward(ctx, req2)
		assert.NoError(t, err2)
		assert.NotNil(t, resp2)
	})

	t.Run("JsonRpcIDConsistencyOnConcurrentRequests", func(t *testing.T) {
		testMu.Lock()
		defer testMu.Unlock()
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetwork(t, ctx, nil, nil)

		// Mock the response from the upstream
		gock.New("http://rpc1.localhost").
			Post("/").
			Persist().
			Reply(200).
			Delay(3 * time.Second).
			BodyString(`{"jsonrpc":"2.0","id":4,"result":"0x1"}`)

		totalRequests := int64(100)

		// Prepare requests with different IDs
		requestTemplate := `{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x742d35Cc6634C0532925a3b844Bc454e4438f44e", "latest"],"id":%d}`
		requests := make([]*common.NormalizedRequest, totalRequests)
		for i := int64(0); i < totalRequests; i++ {
			reqBytes := []byte(fmt.Sprintf(requestTemplate, i+1))
			requests[i] = common.NewNormalizedRequest(reqBytes)
		}

		// Process requests concurrently
		var wg sync.WaitGroup
		responses := make([]*common.NormalizedResponse, totalRequests)
		errors := make([]error, totalRequests)

		for i := int64(0); i < totalRequests; i++ {
			wg.Add(1)
			go func(index int64) {
				defer wg.Done()
				responses[index], errors[index] = network.Forward(ctx, requests[index])
			}(i)
		}
		wg.Wait()

		// Verify results
		for i := int64(0); i < totalRequests; i++ {
			assert.NoError(t, errors[i], "Request %d should not return an error", i+1)
			assert.NotNil(t, responses[i], "Request %d should return a response", i+1)

			if responses[i] != nil {
				jrr, err := responses[i].JsonRpcResponse()
				assert.NoError(t, err, "Response %d should be a valid JSON-RPC response", i+1)
				assert.Equal(t, i+1, jrr.ID(), "Response ID should match the request ID for request %d", i+1)
			}
		}
	})

	t.Run("ContextCancellationDuringRequest", func(t *testing.T) {
		testMu.Lock()
		defer testMu.Unlock()
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetwork(t, ctx, nil, nil)
		requestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[]}`)

		gock.New("http://rpc1.localhost").
			Post("/").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			Delay(2 * time.Second). // Delay to ensure context cancellation occurs before response
			BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`)

		ctxLimited, cancelLimited := context.WithCancel(ctx)

		var wg sync.WaitGroup

		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(500 * time.Millisecond) // Wait a bit before cancelling
			cancelLimited()
		}()

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctxLimited, req)

		wg.Wait() // Ensure cancellation has occurred

		assert.Error(t, err)
		assert.Nil(t, resp)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointRequestTimeout))

		// Verify cleanup
		inFlightCount := 0
		network.inFlightRequests.Range(func(key, value interface{}) bool {
			inFlightCount++
			return true
		})
		assert.Equal(t, 0, inFlightCount, "in-flight requests map should be empty after context cancellation")
	})

	t.Run("LongRunningRequest", func(t *testing.T) {
		testMu.Lock()
		defer testMu.Unlock()
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetwork(t, ctx, nil, nil)
		requestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[]}`)

		gock.New("http://rpc1.localhost").
			Post("/").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			Delay(5 * time.Second). // Simulate a long-running request
			BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`)

		var wg sync.WaitGroup
		wg.Add(1)

		go func() {
			defer wg.Done()
			req := common.NewNormalizedRequest(requestBytes)
			resp, err := network.Forward(ctx, req)
			assert.NoError(t, err)
			assert.NotNil(t, resp)
		}()

		// Check in-flight requests during processing
		time.Sleep(1 * time.Second)
		inFlightCount := 0

		network.inFlightRequests.Range(func(key, value interface{}) bool {
			inFlightCount++
			return true
		})
		assert.Equal(t, 1, inFlightCount, "should have one in-flight request during processing")

		wg.Wait() // Wait for the request to complete

		// Verify cleanup after completion
		inFlightCount = 0
		network.inFlightRequests.Range(func(key, value interface{}) bool {
			inFlightCount++
			return true
		})
		assert.Equal(t, 0, inFlightCount, "in-flight requests map should be empty after request completion")
	})
}

func TestNetwork_SkippingUpstreams(t *testing.T) {

	t.Run("NotSkippedRecentBlockNumberForFullNodeUpstream", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		requestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x0000000000000000000000000000000000000000", "0x11118888"]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			JSON([]byte(`{"result":[{"value":0x1,"fromHost":"rpc1"}]}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetworkWithFullAndArchiveNodeUpstreams(t, ctx, common.EvmNodeTypeFull, 128, common.EvmNodeTypeArchive, 0)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)
		assert.NoError(t, err)
		assert.NotNil(t, resp)
		// Convert the raw response to a map to access custom fields like fromHost
		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Failed to get JSON-RPC response: %v", err)
		}

		if jrr.Result == nil {
			t.Fatalf("Expected non-nil result")
		}

		fromHost, err := jrr.PeekStringByPath(0, "fromHost")
		if err != nil {
			t.Fatalf("Failed to get fromHost from result: %v", err)
		}
		if fromHost != "rpc1" {
			t.Errorf("Expected fromHost to be %q, got %q", "rpc1", fromHost)
		}
	})

	t.Run("SkippedHistoricalBlockNumberForFullNodeUpstream", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1)

		requestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x0000000000000000000000000000000000000000", "0x1"]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			JSON([]byte(`{"result":[{"value":0x1,"fromHost":"rpc1"}]}`))```

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			JSON([]byte(`{"result":[{"value":0x1,"fromHost":"rpc2"}]}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetworkWithFullAndArchiveNodeUpstreams(t, ctx, common.EvmNodeTypeFull, 128, common.EvmNodeTypeArchive, 0)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)
		assert.NoError(t, err)
		assert.NotNil(t, resp)
		// Convert the raw response to a map to access custom fields like fromHost
		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Failed to get JSON-RPC response: %v", err)
		}

		if jrr.Result == nil {
			t.Fatalf("Expected non-nil result")
		}

		fromHost, err := jrr.PeekStringByPath(0, "fromHost")
		if err != nil {
			t.Fatalf("Failed to get fromHost from result: %v", err)
		}
		if fromHost != "rpc2" {
			t.Errorf("Expected fromHost to be %q, got %q", "rpc2", fromHost)
		}
	})

	t.Run("NotSkippedHistoricalBlockNumberForArchiveNodeUpstream", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		requestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x0000000000000000000000000000000000000000", "0x1"]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			JSON([]byte(`{"result":[{"value":0x1,"fromHost":"rpc1"}]}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetworkWithFullAndArchiveNodeUpstreams(t, ctx, common.EvmNodeTypeArchive, 0, common.EvmNodeTypeFull, 128)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)
		assert.NoError(t, err)
		assert.NotNil(t, resp)
		// Convert the raw response to a map to access custom fields like fromHost
		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Failed to get JSON-RPC response: %v", err)
		}

		if jrr.Result == nil {
			t.Fatalf("Expected non-nil result")
		}

		fromHost, err := jrr.PeekStringByPath(0, "fromHost")
		if err != nil {
			t.Fatalf("Failed to get fromHost from result: %v", err)
		}
		if fromHost != "rpc1" {
			t.Errorf("Expected fromHost to be %q, got %q", "rpc1", fromHost)
		}
	})

	t.Run("NotSkippedHistoricalBlockForUnknowneNodeUpstream", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		requestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x0000000000000000000000000000000000000000", "0x1"]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			JSON([]byte(`{"result":[{"value":0x1,"fromHost":"rpc1"}]}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetworkWithFullAndArchiveNodeUpstreams(t, ctx, "", 0, common.EvmNodeTypeArchive, 0)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)
		assert.NoError(t, err)
		assert.NotNil(t, resp)
		// Convert the raw response to a map to access custom fields like fromHost
		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Failed to get JSON-RPC response: %v", err)
		}

		if jrr.Result == nil {
			t.Fatalf("Expected non-nil result")
		}

		fromHost, err := jrr.PeekStringByPath(0, "fromHost")
		if err != nil {
			t.Fatalf("Failed to get fromHost from result: %v", err)
		}
		if fromHost != "rpc1" {
			t.Errorf("Expected fromHost to be %q, got %q", "rpc1", fromHost)
		}
	})
}

func setupTestNetwork(t *testing.T, ctx context.Context, upstreamConfig *common.UpstreamConfig, networkConfig *common.NetworkConfig) *Network {
	t.Helper()

	rateLimitersRegistry, _ := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
	metricsTracker := health.NewTracker("test", time.Minute)

	if upstreamConfig == nil {
		upstreamConfig = &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
	}
	upstreamsRegistry := upstream.NewUpstreamsRegistry(
		ctx,
		&log.Logger,
		"test",
		[]*common.UpstreamConfig{upstreamConfig},
		rateLimitersRegistry,
		vendors.NewVendorsRegistry(),
		metricsTracker,
		1*time.Second,
	)
	if networkConfig == nil {
		networkConfig = &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				ChainId: 123,
			},
		}
	}
	network, err := NewNetwork(
		&log.Logger,
		"test",
		networkConfig,
		rateLimitersRegistry,
		upstreamsRegistry,
		metricsTracker,
	)
	assert.NoError(t, err)

	err = upstreamsRegistry.Bootstrap(ctx)
	assert.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	err = upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
	assert.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	err = network.Bootstrap(ctx)
	assert.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	if upstreamConfig.Id == "test" {
		h, _ := common.HexToInt64("0x1273c18")
		network.evmStatePollers["test"].SuggestFinalizedBlock(h)
		network.evmStatePollers["test"].SuggestLatestBlock(h)
	}

	return network
}

func setupTestNetworkWithFullAndArchiveNodeUpstreams(t *testing.T, ctx context.Context, nodeType1 common.EvmNodeType, maxRecentBlocks1 int64, nodeType2 common.EvmNodeType, maxRecentBlocks2 int64) *Network {
	t.Helper()

	rateLimitersRegistry, _ := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
	metricsTracker := health.NewTracker("test", time.Minute)

	up1 := &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "rpc1",
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:                  123,
			NodeType:                 nodeType1,
			MaxAvailableRecentBlocks: maxRecentBlocks1,
		},
	}

	up2 := &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "rpc2",
		Endpoint: "http://rpc2.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:                  123,
			NodeType:                 nodeType2,
			MaxAvailableRecentBlocks: maxRecentBlocks2,
		},
	}

	upstreamsRegistry := upstream.NewUpstreamsRegistry(
		ctx,
		&log.Logger,
		"test",
		[]*common.UpstreamConfig{up1, up2},
		rateLimitersRegistry,
		vendors.NewVendorsRegistry(),
		metricsTracker,
		1*time.Second,
	)

	fsCfg := &common.FailsafeConfig{
		Hedge:   nil,
		Timeout: nil,
		Retry:   nil,
	}

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId: 123,
		},
		Failsafe: fsCfg,
	}
	network, err := NewNetwork(
		&log.Logger,
		"test",
		networkConfig,
		rateLimitersRegistry,
		upstreamsRegistry,
		metricsTracker,
	)
	assert.NoError(t, err)

	err = upstreamsRegistry.Bootstrap(ctx)
	assert.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	err = upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
	assert.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	err = network.Bootstrap(ctx)
	assert.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	fb1, _ := common.HexToInt64("0x11117777")
	network.evmStatePollers["rpc1"].SuggestFinalizedBlock(fb1)
	lb1, _ := common.HexToInt64("0x11118888")
	network.evmStatePollers["rpc1"].SuggestLatestBlock(lb1)

	fb2, _ := common.HexToInt64("0x22227777")
	network.evmStatePollers["rpc2"].SuggestFinalizedBlock(fb2)
	lb2, _ := common.HexToInt64("0x22228888")
	network.evmStatePollers["rpc2"].SuggestLatestBlock(lb2)

	return network
}
