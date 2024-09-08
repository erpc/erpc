package erpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/erpc/erpc/vendors"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/valyala/fasthttp"
)

var TRUE = true

func init() {
	log.Logger = log.Level(zerolog.TraceLevel).Output(zerolog.ConsoleWriter{Out: os.Stderr})
}

func TestNetwork_Forward(t *testing.T) {

	t.Run("ForwardCorrectlyRateLimitedOnNetworkLevel", func(t *testing.T) {
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()
		setupMocksForEvmStatePoller()

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

		mt := health.NewTracker("prjA", 2*time.Second)
		upsReg := upstream.NewUpstreamsRegistry(
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{},
			rateLimitersRegistry,
			vendors.NewVendorsRegistry(),
			mt,
			1*time.Second,
		)
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				RateLimitBudget: "MyLimiterBudget_Test1",
			},
			rateLimitersRegistry,
			upsReg,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

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
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

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

		mt := health.NewTracker("prjA", 2*time.Second)
		upsReg := upstream.NewUpstreamsRegistry(
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{},
			rateLimitersRegistry,
			vendors.NewVendorsRegistry(),
			mt,
			1*time.Second,
		)
		ntw, err := NewNetwork(
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				RateLimitBudget: "MyLimiterBudget_Test2",
			},
			rateLimitersRegistry,
			upsReg,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

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
		defer gock.Off()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Times(3).
			Post("").
			Reply(503).
			JSON(json.RawMessage(`{"error":{"message":"some random provider issue"}}`))

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl, err := clr.GetOrCreateClient(pup)
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

		if len(gock.Pending()) > 0 {
			t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}

		if err == nil {
			t.Errorf("Expected an error, got nil")
		} else if !strings.Contains(common.ErrorSummary(err), "ErrUpstreamsExhausted") {
			t.Errorf("Expected %v, got %v", "ErrUpstreamsExhausted", err)
		}
	})

	t.Run("ForwardRetryFailuresWithoutSuccessErrorWithCode", func(t *testing.T) {
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":9199,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Times(3).
			Post("").
			Reply(503).
			JSON(json.RawMessage(`{"jsonrpc":"2.0","id":9199,"error":{"code":-32600,"message":"some random provider issue"}}`))

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl, err := clr.GetOrCreateClient(pup)
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

		if len(gock.Pending()) > 0 {
			t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}

		if err == nil {
			t.Errorf("Expected an error, got nil")
		}

		if !strings.Contains(common.ErrorSummary(err), "ErrUpstreamsExhausted") {
			t.Errorf("Expected %v, got %v", "ErrUpstreamsExhausted", err)
		}
	})

	t.Run("ForwardSkipsNonRetryableFailuresFromUpstreams", func(t *testing.T) {
		defer gock.Off()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Times(1).
			Post("").
			Reply(401).
			JSON(json.RawMessage(`{"error":{"code":-32016,"message":"unauthorized rpc1"}}`))

		gock.New("http://rpc2.localhost").
			Times(2).
			Post("").
			Reply(503).
			JSON(json.RawMessage(`{"error":"random rpc2 unavailable"}`))

		gock.New("http://rpc2.localhost").
			Times(1).
			Post("").
			Reply(200).
			JSON(json.RawMessage(`{"result":"0x1234567"}`))

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl, err := clr.GetOrCreateClient(pup1)
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
		cl2, err := clr.GetOrCreateClient(pup2)
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
				t.Errorf("Pending mock: %v", pending)
			}
		}

		if err != nil {
			t.Errorf("Expected an nil, got error %v", err)
		}
	})

	t.Run("ForwardNotSkipsRetryableFailuresFromUpstreams", func(t *testing.T) {
		defer gock.Off()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Times(3).
			Post("").
			Reply(503).
			JSON(json.RawMessage(`{"error":"random rpc1 unavailable"}`))

		gock.New("http://rpc2.localhost").
			Times(3).
			Post("").
			Reply(503).
			JSON(json.RawMessage(`{"error":"random rpc2 unavailable"}`))

		gock.New("http://rpc2.localhost").
			Times(1).
			Post("").
			Reply(200).
			JSON(json.RawMessage(`{"result":"0x1234567"}`))

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl, err := clr.GetOrCreateClient(pup1)
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
		cl2, err := clr.GetOrCreateClient(pup2)
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
		// Clean up any gock mocks after the test runs
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		// Prepare a JSON-RPC request payload as a byte array
		var requestBytes = json.RawMessage(`{
			"jsonrpc": "2.0",
			"method": "eth_getLogs",
			"params": [{
				"address": "0x1234567890abcdef1234567890abcdef12345678",
				"fromBlock": "0x4",
				"toBlock": "0x7"
			}],
			"id": 1
		}`)

		// Mock the response for the latest block number request
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				return strings.Contains(safeReadBody(request), "latest")
			}).
			Reply(200).
			JSON(json.RawMessage(`{"result": {"number":"0x9"}}`))

		// Mock the response for the finalized block number request
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				return strings.Contains(safeReadBody(request), "finalized")
			}).
			Reply(200).
			JSON(json.RawMessage(`{"result": {"number":"0x8"}}`))

		// Mock an empty logs response from the first upstream
		gock.New("http://rpc1.localhost").
			Post("").
			Reply(200).
			JSON(json.RawMessage(`{"result":[], "fromHost":"rpc1"}`))

		// Mock a non-empty logs response from the second upstream
		gock.New("http://rpc2.localhost").
			Post("").
			Reply(200).
			JSON(json.RawMessage(`{"result":[{"logIndex":444}], "fromHost":"rpc2"}`))

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
				Syncing: &common.FALSE,
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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl1, err := clr.GetOrCreateClient(pup1)
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
		cl2, err := clr.GetOrCreateClient(pup2)
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
					ChainId:              123,
					BlockTrackerInterval: "10h",
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

		poller := ntw.evmStatePollers["rpc1"]
		poller.SuggestLatestBlock(9)
		poller.SuggestFinalizedBlock(8)

		time.Sleep(100 * time.Millisecond)

		// Create a fake request and forward it through the network
		fakeReq := common.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)

		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		// Convert the raw response to a map to access custom fields like fromHost
		var responseMap map[string]interface{}
		err = sonic.Unmarshal(resp.Body(), &responseMap)
		if err != nil {
			t.Fatalf("Failed to unmarshal response body: %v", err)
		}

		// Check if fromHost exists and is a string
		fromHost, ok := responseMap["fromHost"].(string)
		if !ok {
			t.Fatalf("Expected fromHost to be a string, got %T", responseMap["fromHost"])
		}

		// Assert the value of fromHost
		if fromHost != "rpc1" {
			t.Errorf("Expected fromHost to be %q, got %q", "rpc1", fromHost)
		}

		// Check that the result field is an empty array as expected
		result, ok := responseMap["result"].([]interface{})
		if !ok {
			t.Fatalf("Expected result to be []interface{}, got %T", responseMap["result"])
		}

		if len(result) != 0 {
			t.Fatalf("Expected empty result array")
		}
	})

	t.Run("RetryWhenNodeIsNotSynced", func(t *testing.T) {
		// Clean up any gock mocks after the test runs
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		// Prepare a JSON-RPC request payload as a byte array
		var requestBytes = json.RawMessage(`{
			"jsonrpc": "2.0",
			"method": "eth_getLogs",
			"params": [{
				"address": "0x1234567890abcdef1234567890abcdef12345678",
				"fromBlock": "0x4",
				"toBlock": "0x7"
			}],
			"id": 1
		}`)

		// Mock the response for the latest block number request
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				return strings.Contains(safeReadBody(request), "latest")
			}).
			Reply(200).
			JSON(json.RawMessage(`{"result": {"number":"0x9"}}`))

		// Mock the response for the finalized block number request
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				return strings.Contains(safeReadBody(request), "finalized")
			}).
			Reply(200).
			JSON(json.RawMessage(`{"result": {"number":"0x8"}}`))

		// Mock an empty logs response from the first upstream
		gock.New("http://rpc1.localhost").
			Post("").
			Reply(200).
			JSON(json.RawMessage(`{"result":[], "fromHost":"rpc1"}`))

		// Mock a non-empty logs response from the second upstream
		gock.New("http://rpc2.localhost").
			Post("").
			Reply(200).
			JSON(json.RawMessage(`{"result":[{"logIndex":444}], "fromHost":"rpc2"}`))

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
				Syncing: &common.TRUE,
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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl1, err := clr.GetOrCreateClient(pup1)
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
		cl2, err := clr.GetOrCreateClient(pup2)
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
					ChainId:              123,
					BlockTrackerInterval: "10h",
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

		time.Sleep(100 * time.Millisecond)

		// Create a fake request and forward it through the network
		fakeReq := common.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)

		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		// Convert the raw response to a map to access custom fields like fromHost
		var responseMap map[string]interface{}
		err = sonic.Unmarshal(resp.Body(), &responseMap)
		if err != nil {
			t.Fatalf("Failed to unmarshal response body: %v", err)
		}

		// Check if fromHost exists and is a string
		fromHost, ok := responseMap["fromHost"].(string)
		if !ok {
			t.Fatalf("Expected fromHost to be a string, got %T", responseMap["fromHost"])
		}

		// Assert the value of fromHost
		if fromHost != "rpc2" {
			t.Errorf("Expected fromHost to be %q, got %q", "rpc2", fromHost)
		}

		result, ok := responseMap["result"].([]interface{})
		if !ok {
			t.Fatalf("Expected result to be []interface{}, got %T", responseMap["result"])
		}

		if len(result) == 0 {
			t.Fatalf("Expected non-empty result array")
		}
	})

	t.Run("RetryWhenWeDoNotKnowNodeSyncState", func(t *testing.T) {
		// Clean up any gock mocks after the test runs
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		// Prepare a JSON-RPC request payload as a byte array
		var requestBytes = json.RawMessage(`{
			"jsonrpc": "2.0",
			"method": "eth_getLogs",
			"params": [{
				"address": "0x1234567890abcdef1234567890abcdef12345678",
				"fromBlock": "0x4",
				"toBlock": "0x7"
			}],
			"id": 1
		}`)

		// Mock the response for the latest block number request
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				return strings.Contains(safeReadBody(request), "latest")
			}).
			Reply(200).
			JSON(json.RawMessage(`{"result": {"number":"0x9"}}`))

		// Mock the response for the finalized block number request
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				return strings.Contains(safeReadBody(request), "finalized")
			}).
			Reply(200).
			JSON(json.RawMessage(`{"result": {"number":"0x8"}}`))

		// Mock an empty logs response from the first upstream
		gock.New("http://rpc1.localhost").
			Post("").
			Reply(200).
			JSON(json.RawMessage(`{"result":[], "fromHost":"rpc1"}`))

		// Mock a non-empty logs response from the second upstream
		gock.New("http://rpc2.localhost").
			Post("").
			Reply(200).
			JSON(json.RawMessage(`{"result":[{"logIndex":444}], "fromHost":"rpc2"}`))

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
				Syncing: nil, // means unknown state
			},
		}
		up2 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
				Syncing: nil, // means unknown state
			},
		}

		// Initialize the upstreams registry
		upr := upstream.NewUpstreamsRegistry(
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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl1, err := clr.GetOrCreateClient(pup1)
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
		cl2, err := clr.GetOrCreateClient(pup2)
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
					ChainId:              123,
					BlockTrackerInterval: "10h",
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

		time.Sleep(100 * time.Millisecond)

		// Create a fake request and forward it through the network
		fakeReq := common.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)

		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		// Convert the raw response to a map to access custom fields like fromHost
		var responseMap map[string]interface{}
		err = sonic.Unmarshal(resp.Body(), &responseMap)
		if err != nil {
			t.Fatalf("Failed to unmarshal response body: %v", err)
		}

		// Check if fromHost exists and is a string
		fromHost, ok := responseMap["fromHost"].(string)
		if !ok {
			t.Fatalf("Expected fromHost to be a string, got %T", responseMap["fromHost"])
		}

		// Assert the value of fromHost
		if fromHost != "rpc2" {
			t.Errorf("Expected fromHost to be %q, got %q", "rpc2", fromHost)
		}

		result, ok := responseMap["result"].([]interface{})
		if !ok {
			t.Fatalf("Expected result to be []interface{}, got %T", responseMap["result"])
		}

		if len(result) == 0 {
			t.Fatalf("Expected non-empty result array")
		}
	})

	t.Run("RetryWhenBlockIsNotFinalized", func(t *testing.T) {
		// Clean up any gock mocks after the test runs
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		// Prepare a JSON-RPC request payload as a byte array
		var requestBytes = json.RawMessage(`{
			"jsonrpc": "2.0",
			"method": "eth_getLogs",
			"params": [{
				"address": "0x1234567890abcdef1234567890abcdef12345678",
				"fromBlock": "0x0",
				"toBlock": "0x1273c18"
			}],
			"id": 1
		}`)

		// Mock the response for the latest block number request
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				return strings.Contains(safeReadBody(request), "latest")
			}).
			Reply(200).
			JSON(json.RawMessage(`{"result": {"number":"0x1273c17"}}`))

		// Mock the response for the finalized block number request
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				return strings.Contains(safeReadBody(request), "finalized")
			}).
			Reply(200).
			JSON(json.RawMessage(`{"result": {"number":"0x1273c17"}}`))

		// Mock an empty logs response from the first upstream
		gock.New("http://rpc1.localhost").
			Post("").
			Reply(200).
			JSON(json.RawMessage(`{"result":[]}`))

		// Mock a non-empty logs response from the second upstream
		gock.New("http://rpc2.localhost").
			Post("").
			Reply(200).
			JSON(json.RawMessage(`{"result":[{"logIndex":444, "fromHost":"rpc2"}]}`))

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl1, err := clr.GetOrCreateClient(pup1)
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
		cl2, err := clr.GetOrCreateClient(pup2)
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
					ChainId:              123,
					BlockTrackerInterval: "10h",
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

		res, err := jrr.ParsedResult()
		if err != nil {
			t.Fatalf("Failed to get parsed result: %v", err)
		}
		result, ok := res.([]interface{})
		if !ok {
			t.Fatalf("Expected Result to be []interface{}, got %T", jrr.Result)
		}

		if len(result) == 0 {
			t.Fatalf("Expected non-empty result array")
		}

		firstLog, ok := result[0].(map[string]interface{})
		if !ok {
			t.Fatalf("Expected first log to be map[string]interface{}, got %T", result[0])
		}

		fromHost, ok := firstLog["fromHost"].(string)
		if !ok {
			t.Fatalf("Expected fromHost to be string, got %T", firstLog["fromHost"])
		}

		if fromHost != "rpc2" {
			t.Errorf("Expected fromHost to be %q, got %q", "rpc2", fromHost)
		}
	})

	t.Run("RetryWhenBlockFinalizationIsNotAvailable", func(t *testing.T) {
		// Clean up any gock mocks after the test runs
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		// Prepare a JSON-RPC request payload as a byte array
		var requestBytes = json.RawMessage(`{
			"jsonrpc": "2.0",
			"method": "eth_getLogs",
			"params": [{
				"address": "0x1234567890abcdef1234567890abcdef12345678",
				"fromBlock": "0x0",
				"toBlock": "0x1273c18"
			}],
			"id": 1
		}`)

		// Mock the response for the latest block number request
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				return strings.Contains(safeReadBody(request), "latest")
			}).
			Reply(200).
			JSON(json.RawMessage(`{"result": {"number":"0x0"}}`)) // latest block not available

		// Mock the response for the finalized block number request
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				return strings.Contains(safeReadBody(request), "finalized")
			}).
			Reply(200).
			JSON(json.RawMessage(`{"result": {"number":"0x0"}}`)) //  finalzied block not available

		// Mock an empty logs response from the first upstream
		gock.New("http://rpc1.localhost").
			Post("").
			Reply(200).
			JSON(json.RawMessage(`{"result":[]}`))

		// Mock a non-empty logs response from the second upstream
		gock.New("http://rpc2.localhost").
			Post("").
			Reply(200).
			JSON(json.RawMessage(`{"result":[{"logIndex":444, "fromHost":"rpc2"}]}`))

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl1, err := clr.GetOrCreateClient(pup1)
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
		cl2, err := clr.GetOrCreateClient(pup2)
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
					ChainId:              123,
					BlockTrackerInterval: "10h",
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

		res, err := jrr.ParsedResult()
		if err != nil {
			t.Fatalf("Failed to get parsed result: %v", err)
		}
		result, ok := res.([]interface{})
		if !ok {
			t.Fatalf("Expected Result to be []interface{}, got %T", jrr.Result)
		}

		if len(result) == 0 {
			t.Fatalf("Expected non-empty result array")
		}

		firstLog, ok := result[0].(map[string]interface{})
		if !ok {
			t.Fatalf("Expected first log to be map[string]interface{}, got %T", result[0])
		}

		fromHost, ok := firstLog["fromHost"].(string)
		if !ok {
			t.Fatalf("Expected fromHost to be string, got %T", firstLog["fromHost"])
		}

		if fromHost != "rpc2" {
			t.Errorf("Expected fromHost to be %q, got %q", "rpc2", fromHost)
		}
	})

	t.Run("RetryPendingTXsWhenDirectiveIsSet", func(t *testing.T) {
		defer gock.Off()

		// Mock the response for the latest block number request
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				b := safeReadBody(request)
				return strings.Contains(b, "latest")
			}).
			Reply(200).
			JSON(json.RawMessage(`{"result": {"number":"0xA98AC7"}}`))

		// Mock the response for the finalized block number request
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				b := safeReadBody(request)
				return strings.Contains(b, "finalized")
			}).
			Reply(200).
			JSON(json.RawMessage(`{"result":{"number":"0x21E88E"}}`))

		gock.New("http://rpc2.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				b := safeReadBody(request)
				return strings.Contains(b, "latest")
			}).
			Reply(200).
			JSON(json.RawMessage(`{"result": {"number":"0x32DCD5"}}`))

		// Mock the response for the finalized block number request
		gock.New("http://rpc2.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				b := safeReadBody(request)
				return strings.Contains(b, "finalized")
			}).
			Reply(200).
			JSON(json.RawMessage(`{"result":{"number":"0x2A62B1C"}}`))

		// Mock a pending transaction response from the first upstream
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				b := safeReadBody(request)
				return strings.Contains(b, "eth_getTransactionByHash")
			}).
			Reply(200).
			JSON(json.RawMessage(`{"result":{"blockNumber":null,"hash":"0xabcdef","fromHost":"rpc1"}}`))

		// Mock a non-pending transaction response from the second upstream
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				b := safeReadBody(request)
				return strings.Contains(b, "eth_getTransactionByHash")
			}).
			Reply(200).
			JSON(json.RawMessage(`{"result":{"blockNumber":"0x54C563","hash":"0xabcdef","fromHost":"rpc2"}}`))

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl1, err := clr.GetOrCreateClient(pup1)
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
		cl2, err := clr.GetOrCreateClient(pup2)
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
					ChainId:              123,
					BlockTrackerInterval: "10h",
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
		time.Sleep(1000 * time.Millisecond)

		// Create a fake request and forward it through the network
		fakeReq := common.NewNormalizedRequest(json.RawMessage(`{
			"jsonrpc": "2.0",
			"method": "eth_getTransactionByHash",
			"params": ["0xabcdef"],
			"id": 1
		}`))
		fakeReq.ApplyDirectivesFromHttp(&fasthttp.RequestHeader{}, &fasthttp.Args{})
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

		res, err := jrr.ParsedResult()
		if err != nil {
			t.Fatalf("Failed to get parsed result: %v", err)
		}
		result, ok := res.(map[string]interface{})
		if !ok {
			t.Fatalf("Expected Result to be map[string]interface{}, got %T", jrr.Result)
		}

		blockNumber, ok := result["blockNumber"].(string)
		if !ok {
			t.Fatalf("Expected blockNumber to be string, got %T", result["blockNumber"])
		}
		if blockNumber != "0x54C563" {
			t.Errorf("Expected blockNumber to be %q, got %q", "0x54C563", blockNumber)
		}

		fromHost, ok := result["fromHost"].(string)
		if !ok {
			t.Fatalf("Expected fromHost to be string, got %T", result["fromHost"])
		}

		if fromHost != "rpc2" {
			t.Errorf("Expected fromHost to be %q, got %q", "rpc2", fromHost)
		}
	})

	t.Run("NotRetryPendingTXsWhenDirectiveIsNotSet", func(t *testing.T) {
		defer gock.Off()

		// Mock the response for the latest block number request
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				b := safeReadBody(request)
				return strings.Contains(b, "latest")
			}).
			Reply(200).
			JSON(json.RawMessage(`{"result": {"number":"0xA98AC7"}}`))

		// Mock the response for the finalized block number request
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				b := safeReadBody(request)
				return strings.Contains(b, "finalized")
			}).
			Reply(200).
			JSON(json.RawMessage(`{"result":{"number":"0x21E88E"}}`))

		gock.New("http://rpc2.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				b := safeReadBody(request)
				return strings.Contains(b, "latest")
			}).
			Reply(200).
			JSON(json.RawMessage(`{"result": {"number":"0x32DCD5"}}`))

		// Mock the response for the finalized block number request
		gock.New("http://rpc2.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				b := safeReadBody(request)
				return strings.Contains(b, "finalized")
			}).
			Reply(200).
			JSON(json.RawMessage(`{"result":{"number":"0x2A62B1C"}}`))

		// Mock a pending transaction response from the first upstream
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				b := safeReadBody(request)
				return strings.Contains(b, "eth_getTransactionByHash")
			}).
			Reply(200).
			JSON(json.RawMessage(`{"result":{"blockNumber":null,"hash":"0xabcdef","fromHost":"rpc1"}}`))

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl1, err := clr.GetOrCreateClient(pup1)
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
		cl2, err := clr.GetOrCreateClient(pup2)
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
					ChainId:              123,
					BlockTrackerInterval: "10h",
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
		time.Sleep(1000 * time.Millisecond)

		// Create a fake request and forward it through the network
		fakeReq := common.NewNormalizedRequest(json.RawMessage(`{
			"jsonrpc": "2.0",
			"method": "eth_getTransactionByHash",
			"params": ["0xabcdef"],
			"id": 1
		}`))
		hdr := &fasthttp.RequestHeader{}
		hdr.Set("x-erpc-retry-pending", "false")
		fakeReq.ApplyDirectivesFromHttp(hdr, &fasthttp.Args{})
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

		res, err := jrr.ParsedResult()
		if err != nil {
			t.Fatalf("Failed to get parsed result: %v", err)
		}
		result, ok := res.(map[string]interface{})
		if !ok {
			t.Fatalf("Expected Result to be map[string]interface{}, got %T", jrr.Result)
		}

		blockNumber, ok := result["blockNumber"].(string)
		if ok {
			t.Fatalf("Expected blockNumber to be nil, got %v", result["blockNumber"])
		}
		if blockNumber != "" {
			t.Errorf("Expected blockNumber to be empty, got %q", blockNumber)
		}

		fromHost, ok := result["fromHost"].(string)
		if !ok {
			t.Fatalf("Expected fromHost to be string, got %T", result["fromHost"])
		}

		if fromHost != "rpc1" {
			t.Errorf("Expected fromHost to be %q, got %q", "rpc1", fromHost)
		}
	})

	t.Run("ForwardMustNotReadFromCacheIfDirectiveIsSet", func(t *testing.T) {
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":9199,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		// Mock the upstream response
		gock.New("http://rpc1.localhost").
			Post("").
			Times(2). // Expect two calls
			Reply(200).
			JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9","fromHost":"rpc1"}}`))

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl1, err := clr.GetOrCreateClient(pup1)
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
		hdr := &fasthttp.RequestHeader{}
		hdr.Set("x-erpc-skip-cache-read", "true")
		fakeReq2.ApplyDirectivesFromHttp(hdr, &fasthttp.Args{})
		resp2, err := ntw.Forward(ctx, fakeReq2)
		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		// Check that both responses are not nil and different
		if resp1 == nil || resp2 == nil {
			t.Fatalf("Expected non-nil responses")
		}

		// Verify that all mocks were consumed
		if left := len(gock.Pending()); left > 0 {
			t.Errorf("Expected all mocks to be consumed, got %v left", left)
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}
	})

	t.Run("ForwardDynamicallyAddsIgnoredMethods", func(t *testing.T) {
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":9199,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Times(1).
			Post("").
			Reply(404).
			JSON(json.RawMessage(`{"jsonrpc":"2.0","id":9199,"error":{"code":-32601,"message":"Method not supported"}}`))

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
			AutoIgnoreUnsupportedMethods: &TRUE,
		}
		upr := upstream.NewUpstreamsRegistry(
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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl, err := clr.GetOrCreateClient(pup)
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

		if len(gock.Pending()) > 0 {
			t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}

		if err == nil {
			t.Errorf("Expected an error, got nil")
		}
		if !strings.Contains(err.Error(), "ErrUpstreamsExhausted") {
			t.Errorf("Expected %v, got %v", "ErrUpstreamsExhausted", err)
		}
	})

	t.Run("ForwardMustNotRetryRevertedEthCalls", func(t *testing.T) {
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":9199,"method":"eth_call","params":[{"to":"0x362fa9d0bca5d19f743db50738345ce2b40ec99f","data":"0xa4baa10c"}]}`)

		gock.New("http://rpc1.localhost").
			Times(1).
			Post("").
			Reply(404).
			JSON(json.RawMessage(`{"jsonrpc":"2.0","id":9199,"error":{"code":-32000,"message":"historical backend error: execution reverted: Dai/insufficient-balance"}}`))

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl, err := clr.GetOrCreateClient(pup)
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

		if len(gock.Pending()) > 0 {
			t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}

		if err == nil {
			t.Errorf("Expected an error, got nil")
		}
		if !strings.Contains(err.Error(), "ErrEndpointClientSideException") {
			t.Errorf("Expected %v, got %v", "ErrEndpointClientSideException", err)
		}
	})

	t.Run("ForwardMustNotRetryBillingIssues", func(t *testing.T) {
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":9199,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.alchemy.com.localhost").
			Times(1).
			Post("").
			Reply(503).
			JSON(json.RawMessage(`{"jsonrpc":"2.0","id":9179,"error":{"code":-32600,"message":"Monthly capacity limit exceeded."}}`))

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl, err := clr.GetOrCreateClient(pup)
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

		if len(gock.Pending()) > 0 {
			t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}

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
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

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

		mt := health.NewTracker("prjA", 2*time.Second)
		upsReg := upstream.NewUpstreamsRegistry(
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

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		err = upsReg.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}

		err = upsReg.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Times(3).
			Post("").
			Reply(503).
			JSON(json.RawMessage(`{"error":{"message":"some random provider issue"}}`))

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl, err := clr.GetOrCreateClient(pup)
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

		if len(gock.Pending()) > 0 {
			t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}

		if err == nil {
			t.Errorf("Expected an error, got nil")
		}
		if !strings.Contains(common.ErrorSummary(err), "ErrUpstreamsExhausted") {
			t.Errorf("Expected %v, got %v", "ErrUpstreamsExhausted", err)
		}
	})

	t.Run("ForwardRetryFailuresWithSuccess", func(t *testing.T) {
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Times(3).
			Post("").
			Reply(503).
			JSON(json.RawMessage(`{"error":{"message":"some random provider issue"}}`))

		gock.New("http://rpc1.localhost").
			Post("").
			Reply(200).
			JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl, err := clr.GetOrCreateClient(pup)
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

		if len(gock.Pending()) > 0 {
			t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}

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

		res, err := jrr.ParsedResult()
		if err != nil {
			t.Fatalf("Failed to get parsed result: %v", err)
		}
		result, ok := res.(map[string]interface{})
		if !ok {
			t.Fatalf("Expected result to be a map, got %T", jrr.Result)
		}
		if hash, ok := result["hash"]; !ok || hash == "" {
			t.Fatalf("Expected hash to exist and be non-empty, got %v", result)
		}
	})

	t.Run("ForwardTimeoutPolicyFail", func(t *testing.T) {
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Reply(200).
			Delay(100 * time.Millisecond).
			JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := upstream.NewClientRegistry(&log.Logger)
		fsCfg := &common.FailsafeConfig{
			Timeout: &common.TimeoutPolicyConfig{
				Duration: "30ms",
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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl, err := clr.GetOrCreateClient(pup)
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

		if len(gock.Pending()) > 0 {
			t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}

		if err == nil {
			t.Errorf("Expected error, got nil")
		}

		var e *common.ErrFailsafeTimeoutExceeded
		if !errors.As(err, &e) {
			t.Errorf("Expected %v, got %v", "ErrFailsafeTimeoutExceeded", err)
		} else {
			t.Logf("Got expected error: %v", err)
		}
	})

	t.Run("ForwardTimeoutPolicyPass", func(t *testing.T) {
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Reply(200).
			Delay(100 * time.Millisecond).
			JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl, err := clr.GetOrCreateClient(pup)
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

		if len(gock.Pending()) > 0 {
			t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}

		if err != nil {
			t.Errorf("Expected nil error, got %v", err)
		}

		var e *common.ErrFailsafeTimeoutExceeded
		if errors.As(err, &e) {
			t.Errorf("Did not expect %v", "ErrFailsafeTimeoutExceeded")
		}
	})

	t.Run("ForwardHedgePolicyTriggered", func(t *testing.T) {
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Reply(200).
			JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9","fromHost":"rpc1"}}`)).
			Delay(500 * time.Millisecond)

		gock.New("http://rpc2.localhost").
			Post("").
			Reply(200).
			JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9","fromHost":"rpc2"}}`)).
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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl1, err := clr.GetOrCreateClient(pup1)
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
		cl2, err := clr.GetOrCreateClient(pup2)
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

		if len(gock.Pending()) > 0 {
			t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}

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

		res, err := jrr.ParsedResult()
		if err != nil {
			t.Fatalf("Failed to get parsed result: %v", err)
		}
		result, ok := res.(map[string]interface{})
		if !ok {
			t.Fatalf("Expected Result to be map[string]interface{}, got %T", jrr.Result)
		}

		fromHost, ok := result["fromHost"].(string)
		if !ok {
			t.Fatalf("Expected fromHost to be string, got %T", result["fromHost"])
		}

		if fromHost != "rpc2" {
			t.Errorf("Expected fromHost to be %v, got %v", "rpc2", fromHost)
		}
	})

	t.Run("ForwardHedgePolicyNotTriggered", func(t *testing.T) {
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Reply(200).
			JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9","fromHost":"rpc1"}}`)).
			Delay(2000 * time.Millisecond)

		gock.New("http://alchemy.com").
			Post("").
			Reply(200).
			JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9","fromHost":"rpc2"}}`)).
			Delay(10 * time.Millisecond)

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl1, err := clr.GetOrCreateClient(pup1)
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
		cl2, err := clr.GetOrCreateClient(pup2)
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

		if len(gock.Pending()) > 0 {
			t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		} else {
			t.Logf("All mocks consumed")
		}

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

		res, err := jrr.ParsedResult()
		if err != nil {
			t.Fatalf("Failed to get parsed result: %v", err)
		}
		result, ok := res.(map[string]interface{})
		if !ok {
			t.Fatalf("Expected result to be map[string]interface{}, got %T", jrr.Result)
		}
		if result["fromHost"] != "rpc2" {
			t.Errorf("Expected fromHost to be %v, got %v", "rpc2", result["fromHost"])
		}
	})

	// t.Run("ForwardHedgePolicyIgnoresNegativeScoreUpstream", func(t *testing.T) {
	// 	defer gock.Off()
	// 	defer gock.Clean()
	// 	defer gock.CleanUnmatchedRequest()
	// 	var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)
	// 	log.Logger.Info().Msgf("Mocks registered before: %d", len(gock.Pending()))
	// 	gock.New("http://rpc1.localhost").
	// 		Post("").
	// 		Times(3).
	// 		Reply(200).
	// 		JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9","fromHost":"rpc1"}}`)).
	// 		Delay(100 * time.Millisecond)
	// 	log.Logger.Info().Msgf("Mocks registered after: %d", len(gock.Pending()))
	// 	ctx, cancel := context.WithCancel(context.Background())
	// 	defer cancel()
	// 	clr := upstream.NewClientRegistry(&log.Logger)
	// 	fsCfg := &common.FailsafeConfig{
	// 		Hedge: &common.HedgePolicyConfig{
	// 			Delay:    "30ms",
	// 			MaxCount: 2,
	// 		},
	// 	}
	// 	rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
	// 		Budgets: []*common.RateLimitBudgetConfig{},
	// 	})
	// 	if err != nil {
	// 		t.Fatal(err)
	// 	}
	// 	vndr := vendors.NewVendorsRegistry()
	// 	mt := health.NewTracker("prjA", 2*time.Second)
	// 	up1 := &common.UpstreamConfig{
	// 		Type:     common.UpstreamTypeEvm,
	// 		Id:       "rpc1",
	// 		Endpoint: "http://rpc1.localhost",
	// 		Evm: &common.EvmUpstreamConfig{
	// 			ChainId: 123,
	// 		},
	// 	}
	// 	up2 := &common.UpstreamConfig{
	// 		Type:     common.UpstreamTypeEvm,
	// 		Id:       "rpc2",
	// 		Endpoint: "http://alchemy.com",
	// 		Evm: &common.EvmUpstreamConfig{
	// 			ChainId: 123,
	// 		},
	// 	}
	// 	upr := upstream.NewUpstreamsRegistry(
	// 		&log.Logger,
	// 		"prjA",
	// 		[]*common.UpstreamConfig{up1, up2},
	// 		rlr,
	// 		vndr,
	// 		mt,
	// 	)
	// 	err = upr.Bootstrap(ctx)
	// 	if err != nil {
	// 		t.Fatal(err)
	// 	}
	// 	err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
	// 	if err != nil {
	// 		t.Fatal(err)
	// 	}
	// 	pup1, err := upr.NewUpstream(
	// 		"prjA",
	// 		up1,
	// 		&log.Logger,
	// 		mt,
	// 	)
	// 	if err != nil {
	// 		t.Fatal(err)
	// 	}
	// 	cl1, err := clr.CreateClient(pup1)
	// 	if err != nil {
	// 		t.Fatal(err)
	// 	}
	// 	pup1.Score = 2
	// 	pup1.Client = cl1
	// 	pup2, err := upr.NewUpstream(
	// 		"prjA",
	// 		up2,
	// 		&log.Logger,
	// 		mt,
	// 	)
	// 	if err != nil {
	// 		t.Fatal(err)
	// 	}
	// 	cl2, err := clr.CreateClient(pup2)
	// 	if err != nil {
	// 		t.Fatal(err)
	// 	}
	// 	pup2.Client = cl2
	// 	pup2.Score = -2
	// 	ntw, err := NewNetwork(
	// 		&log.Logger,
	// 		"prjA",
	// 		&common.NetworkConfig{
	// 			Architecture: common.ArchitectureEvm,
	// 			Evm: &common.EvmNetworkConfig{
	// 				ChainId: 123,
	// 			},
	// 			Failsafe: fsCfg,
	// 		},
	// 		rlr,
	// 		upr,
	// 		mt,
	// 	)
	// 	if err != nil {
	// 		t.Fatal(err)
	// 	}
	// 	fakeReq := common.NewNormalizedRequest(requestBytes)
	// 	resp, err := ntw.Forward(ctx, fakeReq)
	// 	time.Sleep(50 * time.Millisecond)
	// 	if len(gock.Pending()) > 0 {
	// 		t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
	// 		for _, pending := range gock.Pending() {
	// 			t.Errorf("Pending mock: %v", pending)
	// 		}
	// 	} else {
	// 		t.Logf("All mocks consumed")
	// 	}
	// 	if err != nil {
	// 		t.Fatalf("Expected nil error, got %v", err)
	// 	}
	// 	jrr, err := resp.JsonRpcResponse()
	// 	if err != nil {
	// 		t.Fatalf("Expected nil error, got %v", err)
	// 	}
	// 	if jrr.Result == nil {
	// 		t.Fatalf("Expected result, got nil")
	// 	}
	// 	result, ok := jrr.Result.(map[string]interface{})
	// 	if !ok {
	// 		t.Fatalf("Expected result to be map[string]interface{}, got %T", jrr.Result)
	// 	}
	// 	if result["fromHost"] != "rpc1" {
	// 		t.Errorf("Expected fromHost to be %v, got %v", "rpc1", result["fromHost"])
	// 	}
	// })

	t.Run("ForwardCBOpensAfterConstantFailure", func(t *testing.T) {
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Times(2).
			Reply(200).
			JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

		gock.New("http://rpc1.localhost").
			Post("").
			Times(2).
			Reply(503).
			JSON(json.RawMessage(`{"error":{"message":"some random provider issue"}}`))

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl, err := clr.GetOrCreateClient(pup1)
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

		if len(gock.Pending()) > 0 {
			t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		} else {
			t.Logf("All mocks consumed")
		}
	})

	t.Run("ForwardSkipsOpenedCB", func(t *testing.T) {
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Times(1).
			Reply(503).
			JSON(json.RawMessage(`{"error":{"message":"some random provider issue"}}`))

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl1, err := clr.GetOrCreateClient(pup1)
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
					ChainId:              123,
					BlockTrackerInterval: "10h",
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

		if len(gock.Pending()) > 0 {
			t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		} else {
			t.Logf("All mocks consumed")
		}
	})

	t.Run("ForwardCBClosesAfterUpstreamIsBackUp", func(t *testing.T) {
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Times(3).
			Reply(200).
			JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

		gock.New("http://rpc1.localhost").
			Post("").
			Times(3).
			Reply(503).
			JSON(json.RawMessage(`{"error":{"message":"some random provider issue"}}`))

		gock.New("http://rpc1.localhost").
			Post("").
			Times(3).
			Reply(200).
			JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl, err := clr.GetOrCreateClient(pup1)
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

		if len(gock.Pending()) > 0 {
			t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		} else {
			t.Logf("All mocks consumed")
		}

		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}
		res, err := jrr.ParsedResult()
		if err != nil {
			t.Fatalf("Failed to get parsed result: %v", err)
		}
		result, ok := res.(map[string]interface{})
		if !ok {
			t.Fatalf("Expected result to be map[string]interface{}, got %T", jrr.Result)
		}
		if result["hash"] == "" {
			t.Errorf("Expected hash to exist, got %v", result)
		}
	})

	t.Run("ForwardCorrectResultForUnknownEndpointError", func(t *testing.T) {
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Reply(500).
			JSON(json.RawMessage(`{"error":{"code":-39999,"message":"my funky random error"}}`))

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl1, err := clr.GetOrCreateClient(pup1)
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
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Reply(500).
			JSON(json.RawMessage(`{"error":{"message":"Internal error"}}`))

		gock.New("http://rpc2.localhost").
			Post("").
			Reply(200).
			JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9","fromHost":"rpc2"}}`))

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl1, err := clr.GetOrCreateClient(pup1)
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
		cl2, err := clr.GetOrCreateClient(pup2)
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

		if len(gock.Pending()) > 0 {
			t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}

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

		res, err := jrr.ParsedResult()
		if err != nil {
			t.Fatalf("Failed to get parsed result: %v", err)
		}
		result, ok := res.(map[string]interface{})
		if !ok {
			t.Fatalf("Expected Result to be map[string]interface{}, got %T", jrr.Result)
		}

		fromHost, ok := result["fromHost"].(string)
		if !ok {
			t.Fatalf("Expected fromHost to be string, got %T", result["fromHost"])
		}

		if fromHost != "rpc2" {
			t.Errorf("Expected fromHost to be %v, got %v", "rpc2", fromHost)
		}
	})

	t.Run("ForwardEthGetLogsEmptyArrayResponseSuccess", func(t *testing.T) {
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		var requestBytes = json.RawMessage(`{"jsonrpc": "2.0","method": "eth_getLogs","params":[{"address":"0x1234567890abcdef1234567890abcdef12345678"}],"id": 1}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Reply(200).
			JSON(json.RawMessage(`{"result":[]}`))

		gock.New("http://rpc2.localhost").
			Post("").
			Reply(200).
			JSON(json.RawMessage(`{"result":[{"logIndex":444,"fromHost":"rpc2"}]}`))

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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
		cl1, err := clr.GetOrCreateClient(pup1)
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
		cl2, err := clr.GetOrCreateClient(pup2)
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

		if len(gock.Pending()) > 0 {
			t.Errorf("Expected all mocks to be consumed, got %d left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}

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

		res, err := jrr.ParsedResult()
		if err != nil {
			t.Fatalf("Failed to get parsed result: %v", err)
		}
		result, ok := res.([]interface{})
		if !ok {
			t.Fatalf("Expected Result to be []interface{}, got %T", jrr.Result)
		}

		if len(result) == 0 {
			t.Fatalf("Expected non-empty result array")
		}

		firstLog, ok := result[0].(map[string]interface{})
		if !ok {
			t.Fatalf("Expected first log to be map[string]interface{}, got %T", result[0])
		}

		fromHost, ok := firstLog["fromHost"].(string)
		if !ok {
			t.Fatalf("Expected fromHost to be string, got %T", firstLog["fromHost"])
		}

		if fromHost != "rpc2" {
			t.Errorf("Expected fromHost to be %q, got %q", "rpc2", fromHost)
		}
	})

	t.Run("ForwardEthGetLogsBothEmptyArrayResponse", func(t *testing.T) {
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		var requestBytes = json.RawMessage(`{"jsonrpc": "2.0","method": "eth_getLogs","params":[{"address":"0x1234567890abcdef1234567890abcdef12345678"}],"id": 1}`)

		emptyResponse := json.RawMessage(`{"jsonrpc": "2.0","id": 1,"result":[]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Reply(200).
			JSON(emptyResponse)

		gock.New("http://rpc2.localhost").
			Post("").
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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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

		if len(gock.Pending()) > 0 {
			t.Errorf("Expected all mocks to be consumed, got %d left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}

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

		res, err := jrr.ParsedResult()
		if err != nil {
			t.Fatalf("Failed to get parsed result: %v", err)
		}
		result, ok := res.([]interface{})
		if !ok {
			t.Fatalf("Expected Result to be []interface{}, got %T", jrr.Result)
		}

		if len(result) != 0 {
			t.Errorf("Expected empty array result, got array of length %d", len(result))
		}
	})

	t.Run("ForwardQuicknodeEndpointRateLimitResponse", func(t *testing.T) {
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":["0x1273c18"]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Reply(429).
			JSON(json.RawMessage(`{"code":-32007,"message":"300/second request limit reached - reduce calls per second or upgrade your account at quicknode.com"}`))

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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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

		if len(gock.Pending()) > 1 {
			t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}

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
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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

		if len(gock.Pending()) > 0 {
			t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}

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
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1273c18",false]}`)

		gock.New("http://rpc1.localhost").
			Post("").
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
		err = upr.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
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

		if len(gock.Pending()) > 0 {
			t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}

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
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		projectID := "test-project"
		networkID := "evm:123"

		logger := zerolog.New(zerolog.NewConsoleWriter())
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

		err = upstreamsRegistry.PrepareUpstreamsForNetwork(networkID)
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
					body := safeReadBody(request)
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

		upstreamsRegistry.PrepareUpstreamsForNetwork(networkID)
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
}

func TestNetwork_InFlightRequests(t *testing.T) {
	t.Run("MultipleSuccessfulConcurrentRequests", func(t *testing.T) {
		resetGock()
		defer resetGock()

		network := setupTestNetwork(t)
		requestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[]}`)

		gock.New("http://rpc1.localhost").
			Post("/").
			Times(1).
			Filter(func(request *http.Request) bool {
				return strings.Contains(safeReadBody(request), "eth_getLogs")
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
				resp, err := network.Forward(context.Background(), req)
				assert.NoError(t, err)
				assert.NotNil(t, resp)
			}()
		}
		wg.Wait()

		if left := anyTestMocksLeft(); left > 0 {
			t.Errorf("Expected all test mocks to be consumed, got %v left", left)
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}
	})

	t.Run("MultipleConcurrentRequestsWithFailure", func(t *testing.T) {
		resetGock()
		defer resetGock()

		network := setupTestNetwork(t)
		requestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[]}`)

		gock.New("http://rpc1.localhost").
			Post("/").
			Times(1).
			Filter(func(request *http.Request) bool {
				return strings.Contains(safeReadBody(request), "eth_getLogs")
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
				resp, err := network.Forward(context.Background(), req)
				assert.Error(t, err)
				assert.Nil(t, resp)
			}()
		}
		wg.Wait()

		if left := anyTestMocksLeft(); left > 0 {
			t.Errorf("Expected all test mocks to be consumed, got %v left", left)
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}
	})

	t.Run("MultipleConcurrentRequestsWithContextTimeout", func(t *testing.T) {
		resetGock()
		defer resetGock()

		network := setupTestNetwork(t)
		requestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[]}`)

		gock.New("http://rpc1.localhost").
			Post("/").
			Times(1).
			Filter(func(request *http.Request) bool {
				return strings.Contains(safeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			Delay(100 * time.Second).
			BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`)

		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
				defer cancel()
				req := common.NewNormalizedRequest(requestBytes)
				resp, err := network.Forward(ctx, req)
				assert.Error(t, err)
				assert.True(t, common.HasErrorCode(err, "ErrNetworkRequestTimeout") || common.HasErrorCode(err, "ErrEndpointRequestTimeout"))
				assert.Nil(t, resp)
			}()
		}
		wg.Wait()

		if left := anyTestMocksLeft(); left > 0 {
			t.Errorf("Expected all test mocks to be consumed, got %v left", left)
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}
	})

	t.Run("MixedSuccessAndFailureConcurrentRequests", func(t *testing.T) {
		resetGock()
		defer resetGock()

		network := setupTestNetwork(t)
		successRequestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[]}`)
		failureRequestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"]}`)

		gock.New("http://rpc1.localhost").
			Post("/").
			Times(1).
			Filter(func(request *http.Request) bool {
				return strings.Contains(safeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			Delay(1 * time.Second).
			BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`)

		gock.New("http://rpc1.localhost").
			Post("/").
			Times(1).
			Filter(func(request *http.Request) bool {
				return strings.Contains(safeReadBody(request), "eth_getBalance")
			}).
			Reply(500).
			Delay(1 * time.Second).
			BodyString(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"Internal error"}}`)

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			req := common.NewNormalizedRequest(successRequestBytes)
			resp, err := network.Forward(context.Background(), req)
			assert.NoError(t, err)
			assert.NotNil(t, resp)
		}()

		go func() {
			defer wg.Done()
			req := common.NewNormalizedRequest(failureRequestBytes)
			resp, err := network.Forward(context.Background(), req)
			assert.Error(t, err)
			assert.Nil(t, resp)
		}()

		wg.Wait()

		if left := anyTestMocksLeft(); left > 0 {
			t.Errorf("Expected all test mocks to be consumed, got %v left", left)
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}
	})

	t.Run("SequentialInFlightRequests", func(t *testing.T) {
		resetGock()
		defer resetGock()

		network := setupTestNetwork(t)
		requestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[]}`)

		gock.New("http://rpc1.localhost").
			Post("/").
			Times(2).
			Filter(func(request *http.Request) bool {
				return strings.Contains(safeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			Delay(1 * time.Second).
			BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`)

		// First request
		req1 := common.NewNormalizedRequest(requestBytes)
		resp1, err1 := network.Forward(context.Background(), req1)
		assert.NoError(t, err1)
		assert.NotNil(t, resp1)

		// Second request (should not be in-flight)
		req2 := common.NewNormalizedRequest(requestBytes)
		resp2, err2 := network.Forward(context.Background(), req2)
		assert.NoError(t, err2)
		assert.NotNil(t, resp2)

		if left := anyTestMocksLeft(); left > 0 {
			t.Errorf("Expected all test mocks to be consumed, got %v left", left)
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}
	})

	t.Run("JsonRpcIDConsistencyOnConcurrentRequests", func(t *testing.T) {
		resetGock()
		defer resetGock()

		network := setupTestNetwork(t)

		// Mock the response from the upstream
		gock.New("http://rpc1.localhost").
			Post("/").
			Times(1).
			Reply(200).
			Delay(1 * time.Second).
			BodyString(`{"jsonrpc":"2.0","id":4,"result":"0x1"}`)

		totalRequests := 100

		// Prepare requests with different IDs
		requestTemplate := `{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x742d35Cc6634C0532925a3b844Bc454e4438f44e", "latest"],"id":%d}`
		requests := make([]*common.NormalizedRequest, totalRequests)
		for i := 0; i < totalRequests; i++ {
			reqBytes := []byte(fmt.Sprintf(requestTemplate, i+1))
			requests[i] = common.NewNormalizedRequest(reqBytes)
		}

		// Process requests concurrently
		var wg sync.WaitGroup
		responses := make([]*common.NormalizedResponse, totalRequests)
		errors := make([]error, totalRequests)

		for i := 0; i < totalRequests; i++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				responses[index], errors[index] = network.Forward(context.Background(), requests[index])
			}(i)
		}
		wg.Wait()

		// Verify results
		for i := 0; i < totalRequests; i++ {
			assert.NoError(t, errors[i], "Request %d should not return an error", i+1)
			assert.NotNil(t, responses[i], "Request %d should return a response", i+1)

			if responses[i] != nil {
				jrr, err := responses[i].JsonRpcResponse()
				assert.NoError(t, err, "Response %d should be a valid JSON-RPC response", i+1)
				assert.Equal(t, float64(i+1), jrr.ID, "Response ID should match the request ID for request %d", i+1)
			}
		}

		if left := anyTestMocksLeft(); left > 0 {
			t.Errorf("Expected all test mocks to be consumed, got %v left", left)
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}
	})
}

func setupTestNetwork(t *testing.T) *Network {
	t.Helper()

	setupMocksForEvmStatePoller()

	rateLimitersRegistry, _ := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
	metricsTracker := health.NewTracker("test", time.Minute)

	upstreamConfig := &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "test",
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}
	upstreamsRegistry := upstream.NewUpstreamsRegistry(
		&log.Logger,
		"test",
		[]*common.UpstreamConfig{upstreamConfig},
		rateLimitersRegistry,
		vendors.NewVendorsRegistry(),
		metricsTracker,
		1*time.Second,
	)
	network, err := NewNetwork(
		&log.Logger,
		"test",
		&common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				ChainId: 123,
			},
		},
		rateLimitersRegistry,
		upstreamsRegistry,
		metricsTracker,
	)
	assert.NoError(t, err)

	err = upstreamsRegistry.Bootstrap(context.Background())
	assert.NoError(t, err)
	time.Sleep(100 * time.Millisecond)
	err = upstreamsRegistry.PrepareUpstreamsForNetwork(util.EvmNetworkId(123))
	assert.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	return network
}

func setupMocksForEvmStatePoller() {
	resetGock()

	// Mock for evm block tracker
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			return strings.Contains(safeReadBody(request), "eth_getBlockByNumber")
		}).
		Reply(200).
		JSON(json.RawMessage(`{"result": {"number":"0x1273c18"}}`))
	gock.New("http://rpc2.localhost").
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			return strings.Contains(safeReadBody(request), "eth_getBlockByNumber")
		}).
		Reply(200).
		JSON(json.RawMessage(`{"result": {"number":"0x1273c18"}}`))
}

func anyTestMocksLeft() int {
	// We have 2 persisted mocks for evm block tracker
	return len(gock.Pending()) - 2
}

func resetGock() {
	gock.Off()
	gock.Clean()
}

func safeReadBody(request *http.Request) string {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return ""
	}
	request.Body = io.NopCloser(bytes.NewBuffer(body))
	return string(body)
}
