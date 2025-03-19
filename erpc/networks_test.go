package erpc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
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

		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Id:       "test",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upsReg := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			ssr,
			rateLimitersRegistry,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		ntw, err := NewNetwork(
			ctx,
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

		upstream.ReorderUpstreams(upsReg)

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

		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Id:       "test",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upsReg := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			ssr,
			rateLimitersRegistry,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		ntw, err := NewNetwork(
			ctx,
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

		upstream.ReorderUpstreams(upsReg)

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

		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)

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
		vndr := thirdparty.NewVendorsRegistry()
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Id:       "test",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			Failsafe: fsCfg,
		}
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			ssr,
			rlr,
			vndr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup)
		if err != nil {
			t.Fatal(err)
		}
		pup.Client = cl
		ntw, err := NewNetwork(
			ctx,
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
			health.NewTracker(&log.Logger, "prjA", 2*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		upstream.ReorderUpstreams(upr)
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

		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)

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
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Id:       "test",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			Failsafe: fsCfg,
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup)
		if err != nil {
			t.Fatal(err)
		}
		pup.Client = cl
		ntw, err := NewNetwork(
			ctx,
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
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		upstream.ReorderUpstreams(upr)
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

		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)

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
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Id:       "rpc1",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			Failsafe: upsFsCfg,
		}
		up2 := &common.UpstreamConfig{
			Id:       "rpc2",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			Failsafe: upsFsCfg,
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
				up2,
			},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl

		pup2, err := upr.NewUpstream(up2)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		ntw, err := NewNetwork(
			ctx,
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
			health.NewTracker(&log.Logger, "prjA", 2*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		upstream.ReorderUpstreams(upr)
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

		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)

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
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Id:       "rpc1",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			Failsafe: upsFsCfg,
		}
		up2 := &common.UpstreamConfig{
			Id:       "rpc2",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			Failsafe: upsFsCfg,
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
				up2,
			},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl

		pup2, err := upr.NewUpstream(up2)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		ntw, err := NewNetwork(
			ctx,
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
			health.NewTracker(&log.Logger, "prjA", 2*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		upstream.ReorderUpstreams(upr)
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
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)

		// Set up upstream configurations
		up1 := &common.UpstreamConfig{
			Id:       "rpc1",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		up2 := &common.UpstreamConfig{
			Id:       "rpc2",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		// Initialize the upstreams registry
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
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
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(up2)
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
			ctx,
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

		poller := pup1.EvmStatePoller()
		poller.SuggestLatestBlock(9)
		poller.SuggestFinalizedBlock(8)

		// TODO pup1.SetEvmSyncingState(common.EvmSyncingStateNotSyncing)
		// TODO pup2.SetEvmSyncingState(common.EvmSyncingStateNotSyncing)

		upstream.ReorderUpstreams(upr)

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
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
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
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)

		// Set up upstream configurations
		up1 := &common.UpstreamConfig{
			Id:       "rpc1",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		up2 := &common.UpstreamConfig{
			Id:       "rpc2",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		// Initialize the upstreams registry
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
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
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(up2)
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
			ctx,
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

		poller1 := pup1.EvmStatePoller()
		poller1.SuggestLatestBlock(9)
		poller1.SuggestFinalizedBlock(8)

		poller2 := pup2.EvmStatePoller()
		poller2.SuggestLatestBlock(9)
		poller2.SuggestFinalizedBlock(8)

		pup1.EvmStatePoller().SetSyncingState(common.EvmSyncingStateSyncing)

		time.Sleep(100 * time.Millisecond)

		upstream.ReorderUpstreams(upr)

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

	t.Run("ForwardRetriesOnPendingBlockIsNotAvailableClientError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		// We expect no leftover mocks after both calls
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{
			"jsonrpc": "2.0",
			"id": 1,
			"method": "eth_call",
			"params": [
				{
					"to": "0x123"
				},
				"pending"
			]
		}`)

		// First upstream returns the retryable client error
		gock.New("http://rpc1.localhost").
			Post("").
			Reply(400).
			JSON([]byte(`{
				"jsonrpc": "2.0",
				"id": 1,
				"error": {
					"code": -32000,
					"message": "pending block is not available"
				}
			}`))

		// Second upstream responds successfully
		gock.New("http://rpc2.localhost").
			Post("").
			Reply(200).
			JSON([]byte(`{
				"jsonrpc": "2.0",
				"id": 1,
				"result": "0x123"
			}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}

		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)

		up1 := &common.UpstreamConfig{
			Id:       "rpc1",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		up2 := &common.UpstreamConfig{
			Id:       "rpc2",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}

		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(up2)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		ntw, err := NewNetwork(
			ctx,
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: &common.FailsafeConfig{
					Retry: &common.RetryPolicyConfig{
						MaxAttempts: 3,
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

		ntw.Bootstrap(ctx)
		time.Sleep(100 * time.Millisecond)

		poller1 := pup1.EvmStatePoller()
		poller1.SuggestLatestBlock(9)
		poller1.SuggestFinalizedBlock(8)

		poller2 := pup2.EvmStatePoller()
		poller2.SuggestLatestBlock(9)
		poller2.SuggestFinalizedBlock(8)

		upstream.ReorderUpstreams(upr)

		fakeReq := common.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)

		// Since "pending block is not available" client error is considered retryable,
		// aggregator should try the second upstream and succeed.
		if err != nil {
			t.Fatalf("Expected no error (success from second upstream), got %v", err)
		}
		if resp == nil {
			t.Fatal("Expected a non-nil response from second upstream, got nil")
		}

		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Unable to parse JSON-RPC response: %v", err)
		}
		if jrr.Result == nil {
			t.Fatal("Expected a successful 'result' field, got nil")
		}

		assert.Equal(t, "\"0x123\"", strings.ToLower(string(jrr.Result)))
	})

	t.Run("ForwardretriesOnInvalidArgumentCodeClientError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		// We expect no leftover mocks after both calls
		defer util.AssertNoPendingMocks(t, 0)

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"}]}`)

		// Mock a client error response (400) and code -32602, meaning invalid argument
		gock.New("http://rpc1.localhost").
			Post("").
			Reply(400).
			JSON([]byte(`{
				"jsonrpc": "2.0",
				"id": 1,
				"error": {
					"code": -32602,
					"message": "value is not an object"
				}
			}`))

		// Second upstream responds successfully
		gock.New("http://rpc2.localhost").
			Post("").
			Reply(200).
			JSON([]byte(`{
				"jsonrpc": "2.0",
				"id": 1,
				"result": "0x123"
			}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}

		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)

		up1 := &common.UpstreamConfig{
			Id:       "rpc1",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		up2 := &common.UpstreamConfig{
			Id:       "rpc2",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}

		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(up2)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		ntw, err := NewNetwork(
			ctx,
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: &common.FailsafeConfig{
					Retry: &common.RetryPolicyConfig{
						MaxAttempts: 3,
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

		ntw.Bootstrap(ctx)
		time.Sleep(100 * time.Millisecond)

		poller1 := pup1.EvmStatePoller()
		poller1.SuggestLatestBlock(9)
		poller1.SuggestFinalizedBlock(8)

		poller2 := pup2.EvmStatePoller()
		poller2.SuggestLatestBlock(9)
		poller2.SuggestFinalizedBlock(8)

		upstream.ReorderUpstreams(upr)

		fakeReq := common.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)

		// Since "pending block is not available" client error is considered retryable,
		// aggregator should try the second upstream and succeed.
		if err != nil {
			t.Fatalf("Expected no error (success from second upstream), got %v", err)
		}
		if resp == nil {
			t.Fatal("Expected a non-nil response from second upstream, got nil")
		}

		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Unable to parse JSON-RPC response: %v", err)
		}
		if jrr.Result == nil {
			t.Fatal("Expected a successful 'result' field, got nil")
		}

		assert.Equal(t, "\"0x123\"", strings.ToLower(string(jrr.Result)))
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
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
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
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)

		// Set up upstream configurations
		up1 := &common.UpstreamConfig{
			Id:       "rpc1",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		up2 := &common.UpstreamConfig{
			Id:       "rpc2",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		// Initialize the upstreams registry
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
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
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(up2)
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
			ctx,
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

		poller1 := pup1.EvmStatePoller()
		poller1.SuggestLatestBlock(9)
		poller1.SuggestFinalizedBlock(8)

		poller2 := pup2.EvmStatePoller()
		poller2.SuggestLatestBlock(9)
		poller2.SuggestFinalizedBlock(8)

		time.Sleep(100 * time.Millisecond)

		upstream.ReorderUpstreams(upr)

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
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
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
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)

		// Set up upstream configurations
		up1 := &common.UpstreamConfig{
			Id:       "rpc1",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		up2 := &common.UpstreamConfig{
			Id:       "rpc2",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		// Initialize the upstreams registry
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
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
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(up2)
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
			ctx,
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

		upstream.ReorderUpstreams(upr)

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
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
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
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)

		// Set up upstream configurations
		up1 := &common.UpstreamConfig{
			Id:       "rpc1",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		up2 := &common.UpstreamConfig{
			Id:       "rpc2",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		// Initialize the upstreams registry
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
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
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(up2)
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
			ctx,
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

		upstream.ReorderUpstreams(upr)

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
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
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
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)

		// Set up upstream configurations
		up1 := &common.UpstreamConfig{
			Id:       "rpc1",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		up2 := &common.UpstreamConfig{
			Id:       "rpc2",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		// Initialize the upstreams registry
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			ssr,
			rlr,
			vr,
			pr,
			nil,
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

		upstream.ReorderUpstreams(upr)

		// Create and register clients for both upstreams
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(up2)
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
			ctx,
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

		upstream.ReorderUpstreams(upr)

		// Create a fake request and forward it through the network
		fakeReq := common.NewNormalizedRequest([]byte(`{
				"jsonrpc": "2.0",
				"method": "eth_getTransactionByHash",
				"params": ["0xabcdef"],
				"id": 1
			}`))
		fakeReq.ApplyDirectivesFromHttp(http.Header{
			"X-Erpc-Retry-Pending": []string{"true"},
		}, url.Values{})
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

	t.Run("RetryPendingDirectiveFromDefaults", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Mock responses similar to RetryPendingTXsWhenDirectiveIsSet
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getTransactionByHash")
			}).
			Reply(200).
			JSON([]byte(`{"result":{"blockNumber":null,"hash":"0xabcdef","fromHost":"rpc1"}}`))

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getTransactionByHash")
			}).
			Reply(200).
			JSON([]byte(`{"result":{"blockNumber":"0x54C563","hash":"0xabcdef","fromHost":"rpc2"}}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Initialize test components
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
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

		// Set up test environment
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Id:       "rpc1",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		up2 := &common.UpstreamConfig{
			Id:       "rpc2",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		// Set up registry with both upstreams
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			0,
		)
		if err := upr.Bootstrap(ctx); err != nil {
			t.Fatal(err)
		}
		if err := upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)); err != nil {
			t.Fatal(err)
		}

		// Create and register clients
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(up2)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		// Set up network with directiveDefaults
		retryPending := true
		ntw, err := NewNetwork(
			ctx,
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: fsCfg,
				DirectiveDefaults: &common.DirectiveDefaultsConfig{
					RetryPending: &retryPending,
				},
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		ntw.Bootstrap(ctx)
		time.Sleep(100 * time.Millisecond)

		upstream.ReorderUpstreams(upr)

		// Create request without explicit directives
		fakeReq := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getTransactionByHash",
			"params": ["0xabcdef"],
			"id": 1
		}`))

		resp, err := ntw.Forward(ctx, fakeReq)
		if err != nil {
			t.Fatalf("Expected nil error, got %v", err)
		}

		// Verify response shows retry behavior from defaults
		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Fatalf("Failed to get JSON-RPC response: %v", err)
		}

		blockNumber, err := jrr.PeekStringByPath("blockNumber")
		if err != nil {
			t.Fatalf("Failed to get blockNumber: %v", err)
		}
		if blockNumber != "0x54C563" {
			t.Errorf("Expected blockNumber %q, got %q", "0x54C563", blockNumber)
		}

		fromHost, err := jrr.PeekStringByPath("fromHost")
		if err != nil {
			t.Fatalf("Failed to get fromHost: %v", err)
		}
		if fromHost != "rpc2" {
			t.Errorf("Expected fromHost %q, got %q", "rpc2", fromHost)
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
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
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
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)

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

		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		// Initialize the upstreams registry
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			ssr,
			rlr,
			vr,
			pr,
			nil,
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
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(up2)
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
			ctx,
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

		upstream.ReorderUpstreams(upr)

		// Create a fake request and forward it through the network
		fakeReq := common.NewNormalizedRequest([]byte(`{
				"jsonrpc": "2.0",
				"method": "eth_getTransactionByHash",
				"params": ["0xabcdef"],
				"id": 1
			}`))

		headers := http.Header{
			"X-Erpc-Retry-Pending": []string{"true"},
		}
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
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
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
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)

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

		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		// Initialize the upstreams registry
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			ssr,
			rlr,
			vr,
			pr,
			nil,
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
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(up2)
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
			ctx,
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

		upstream.ReorderUpstreams(upr)

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

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
		fsCfg := &common.FailsafeConfig{}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		ntw, err := NewNetwork(
			ctx,
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

		upstream.ReorderUpstreams(upr)

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

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)

		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			AutoIgnoreUnsupportedMethods: &common.TRUE,
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup)
		if err != nil {
			t.Fatal(err)
		}
		pup.Client = cl
		ntw, err := NewNetwork(
			ctx,
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
			health.NewTracker(&log.Logger, "prjA", 2*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}

		upstream.ReorderUpstreams(upr)

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

	t.Run("ForwardMustNotRetryRevertedEthCallsMultiUpstreams", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 2) // 2 not-called upstreams

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":9199,"method":"eth_call","params":[{"to":"0x362fa9d0bca5d19f743db50738345ce2b40ec99f","data":"0xa4baa10c"}]}`)

		gock.New("http://rpc1.localhost").
			Times(1).
			Post("").
			Reply(200).
			JSON([]byte(`{"jsonrpc":"2.0","id":9199,"error":{"code":-32000,"message":"historical backend error: execution reverted: Dai/insufficient-balance"}}`))

		gock.New("http://rpc2.localhost").
			Times(1).
			Post("").
			Reply(200).
			JSON([]byte(`{"jsonrpc":"2.0","id":9199,"error":{"code":-32000,"message":"historical backend error: execution reverted: Dai/insufficient-balance"}}`))

		gock.New("http://rpc3.localhost").
			Times(1).
			Post("").
			Reply(200).
			JSON([]byte(`{"jsonrpc":"2.0","id":9199,"error":{"code":-32000,"message":"historical backend error: execution reverted: Dai/insufficient-balance"}}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)

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
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			Failsafe: fsCfg,
		}
		up2 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			Failsafe: fsCfg,
		}
		up3 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test3",
			Endpoint: "http://rpc3.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			Failsafe: fsCfg,
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
				up2,
				up3,
			},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1
		pup2, err := upr.NewUpstream(up2)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2
		pup3, err := upr.NewUpstream(up3)
		if err != nil {
			t.Fatal(err)
		}
		cl3, err := clr.GetOrCreateClient(ctx, pup3)
		if err != nil {
			t.Fatal(err)
		}
		pup3.Client = cl3
		ntw, err := NewNetwork(
			ctx,
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
			health.NewTracker(&log.Logger, "prjA", 2*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}

		upstream.ReorderUpstreams(upr)

		fakeReq := common.NewNormalizedRequest(requestBytes)

		_, err = ntw.Forward(ctx, fakeReq)

		if err == nil {
			t.Errorf("Expected an error, got nil")
		}
		if !strings.Contains(err.Error(), "ErrEndpointExecutionException") {
			t.Errorf("Expected %v, got: %s", "ErrEndpointExecutionException", err.Error())
		}
		if strings.Contains(err.Error(), "ErrUpstreamsExhausted") {
			t.Errorf("Did not expect ErrUpstreamsExhausted, got: %s", err.Error())
		}
	})

	t.Run("ForwardMustNotRetryRevertedEthCallsSingleUpstream", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1) // 1 not-called upstream

		var requestBytes = []byte(`{"jsonrpc":"2.0","id":9199,"method":"eth_call","params":[{"to":"0x362fa9d0bca5d19f743db50738345ce2b40ec99f","data":"0xa4baa10c"}]}`)

		gock.New("http://rpc1.localhost").
			Times(1).
			Post("").
			Reply(200).
			JSON([]byte(`{"jsonrpc":"2.0","id":9199,"error":{"code":-32000,"message":"historical backend error: execution reverted: Dai/insufficient-balance"}}`))

		gock.New("http://rpc1.localhost").
			Times(1).
			Post("").
			Reply(200).
			JSON([]byte(`{"jsonrpc":"2.0","id":9199,"error":{"code":-32000,"message":"historical backend error: execution reverted: Dai/insufficient-balance"}}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)

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
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			Failsafe: fsCfg,
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1
		ntw, err := NewNetwork(
			ctx,
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
			health.NewTracker(&log.Logger, "prjA", 2*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}

		upstream.ReorderUpstreams(upr)

		fakeReq := common.NewNormalizedRequest(requestBytes)

		_, err = ntw.Forward(ctx, fakeReq)

		if err == nil {
			t.Errorf("Expected an error, got nil")
		}
		if !strings.Contains(err.Error(), "ErrEndpointExecutionException") {
			t.Errorf("Expected %v, got: %s", "ErrEndpointExecutionException", err.Error())
		}
		if strings.Contains(err.Error(), "ErrUpstreamsExhausted") {
			t.Errorf("Did not expect ErrUpstreamsExhausted, got: %s", err.Error())
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

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)

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
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
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
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup)
		if err != nil {
			t.Fatal(err)
		}
		pup.Client = cl
		ntw, err := NewNetwork(
			ctx,
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
			health.NewTracker(&log.Logger, "prjA", 10*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}

		upstream.ReorderUpstreams(upr)

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

		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
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
			ssr,
			rateLimitersRegistry,
			vr,
			pr,
			nil,
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
			ctx,
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

		upstream.ReorderUpstreams(upsReg)

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

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)

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
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup)
		if err != nil {
			t.Fatal(err)
		}
		pup.Client = cl
		ntw, err := NewNetwork(
			ctx,
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
		upstream.ReorderUpstreams(upr)
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

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
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
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup)
		if err != nil {
			t.Fatal(err)
		}
		pup.Client = cl
		ntw, err := NewNetwork(
			ctx,
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
		upstream.ReorderUpstreams(upr)
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

		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup)
		if err != nil {
			t.Fatal(err)
		}
		pup.Client = cl
		ntw, err := NewNetwork(
			ctx,
			&log.Logger,
			"prjA",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: &common.FailsafeConfig{
					Timeout: &common.TimeoutPolicyConfig{
						Duration: common.Duration(30 * time.Millisecond),
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

		upstream.ReorderUpstreams(upr)
		fakeReq := common.NewNormalizedRequest(requestBytes)
		_, err = ntw.Forward(ctx, fakeReq)

		if err == nil {
			t.Errorf("Expected error, got nil")
		}

		// TODO in here we should ONLY see failsafe timeout error but currently sometimes we see upstream timeout, we must fix this
		if !common.HasErrorCode(err, common.ErrCodeFailsafeTimeoutExceeded) &&
			!common.HasErrorCode(err, common.ErrCodeEndpointRequestTimeout) &&
			!common.HasErrorCode(err, common.ErrCodeNetworkRequestTimeout) {
			t.Errorf("Expected %v or %v or %v, got %v", common.ErrCodeFailsafeTimeoutExceeded,
				common.ErrCodeEndpointRequestTimeout,
				common.ErrCodeNetworkRequestTimeout,
				err,
			)
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

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
		fsCfg := &common.FailsafeConfig{
			Timeout: &common.TimeoutPolicyConfig{
				Duration: common.Duration(1 * time.Second),
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{
				up1,
			},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup)
		if err != nil {
			t.Fatal(err)
		}
		pup.Client = cl
		ntw, err := NewNetwork(
			ctx,
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

		upstream.ReorderUpstreams(upr)
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

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
		fsCfg := &common.FailsafeConfig{
			Hedge: &common.HedgePolicyConfig{
				Delay:    common.Duration(200 * time.Millisecond),
				MaxCount: 1,
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
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
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(up2)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		ntw, err := NewNetwork(
			ctx,
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

		upstream.ReorderUpstreams(upr)

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

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
		fsCfg := &common.FailsafeConfig{
			Hedge: &common.HedgePolicyConfig{
				Delay:    common.Duration(100 * time.Millisecond),
				MaxCount: 5,
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
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
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(up2)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		ntw, err := NewNetwork(
			ctx,
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

		upstream.ReorderUpstreams(upr)

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

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
		fsCfg := &common.FailsafeConfig{
			Hedge: &common.HedgePolicyConfig{
				Delay:    common.Duration(100 * time.Millisecond),
				MaxCount: 5,
			},
		}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
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
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(up2)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		ntw, err := NewNetwork(
			ctx,
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

		upstream.ReorderUpstreams(upr)

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

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
		fsCfg := &common.FailsafeConfig{
			CircuitBreaker: &common.CircuitBreakerPolicyConfig{
				FailureThresholdCount:    2,
				FailureThresholdCapacity: 4,
				HalfOpenAfter:            common.Duration(2 * time.Second),
			},
		}

		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		mt := health.NewTracker(&log.Logger, "test_cb", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "upstream1",
			Endpoint: "http://rpc1.localhost",
			Failsafe: fsCfg,
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"test_cb",
			[]*common.UpstreamConfig{up1},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl
		ntw, err := NewNetwork(
			ctx,
			&log.Logger,
			"test_cb",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: nil,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		upstream.ReorderUpstreams(upr)

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

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
		fsCfgNetwork := &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 1,
			},
		}
		fsCfgUp1 := &common.FailsafeConfig{
			CircuitBreaker: &common.CircuitBreakerPolicyConfig{
				FailureThresholdCount:    1,
				FailureThresholdCapacity: 1,
				HalfOpenAfter:            common.Duration(20 * time.Second),
			},
		}

		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		mt := health.NewTracker(&log.Logger, "test_cb", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "upstream1",
			Endpoint: "http://rpc1.localhost",
			Failsafe: fsCfgUp1,
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"test_cb",
			[]*common.UpstreamConfig{up1},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Hour,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}

		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		ntw, err := NewNetwork(
			ctx,
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

		upstream.ReorderUpstreams(upr)

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
			Times(2).
			Reply(200).
			JSON([]byte(`{"result":{"hash":"0x111340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

		gock.New("http://rpc1.localhost").
			Post("").
			Times(2).
			Reply(503).
			JSON([]byte(`{"error":{"message":"some random provider issue"}}`))

		gock.New("http://rpc1.localhost").
			Post("").
			Times(2).
			Reply(200).
			JSON([]byte(`{"result":{"hash":"0x222340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}
		mt := health.NewTracker(&log.Logger, "test_cb", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "upstream1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			Failsafe: &common.FailsafeConfig{
				CircuitBreaker: &common.CircuitBreakerPolicyConfig{
					FailureThresholdCount:    2,
					FailureThresholdCapacity: 4,
					HalfOpenAfter:            common.Duration(500 * time.Millisecond),
					SuccessThresholdCount:    2,
					SuccessThresholdCapacity: 2,
				},
			},
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"test_cb",
			[]*common.UpstreamConfig{up1},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
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
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl

		ntw, err := NewNetwork(
			ctx,
			&log.Logger,
			"test_cb",
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 123,
				},
				Failsafe: nil,
			},
			rlr,
			upr,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}

		upstream.ReorderUpstreams(upr)

		r1, e1 := ntw.Forward(ctx, common.NewNormalizedRequest(requestBytes))
		r2, e2 := ntw.Forward(ctx, common.NewNormalizedRequest(requestBytes))
		_, e3 := ntw.Forward(ctx, common.NewNormalizedRequest(requestBytes))
		_, e4 := ntw.Forward(ctx, common.NewNormalizedRequest(requestBytes))

		if r1 == nil || r2 == nil {
			t.Fatalf("Expected a response on first two attempts, got %v, %v", r1, r2)
		}
		if e1 != nil || e2 != nil {
			t.Fatalf("Did not expect an error on first two attempts, got %v, %v", e1, e2)
		}
		if e3 == nil || e4 == nil {
			t.Fatalf("Expected an error on last two attempts, got %v, %v", e3, e4)
		}

		time.Sleep(500 * time.Millisecond)

		var resp *common.NormalizedResponse
		var lastErr error
		for i := 0; i < 2; i++ {
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

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
		fsCfg := &common.FailsafeConfig{}
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &log.Logger)
		if err != nil {
			t.Fatal(err)
		}

		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		ntw, err := NewNetwork(
			ctx,
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

		upstream.ReorderUpstreams(upr)

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

	t.Run("ForwardEndpointServerSideExceptionRetrySuccess", func(t *testing.T) {
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

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
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

		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
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
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(up2)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		ntw, err := NewNetwork(
			ctx,
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

		upstream.ReorderUpstreams(upr)

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

	t.Run("ForwardIgnoredMethod", func(t *testing.T) {
		var requestBytes = []byte(`{"jsonrpc":"2.0","id":1,"method":"ignored_method","params":["0x1273c18",false]}`)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
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

		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			IgnoreMethods: []string{"ignored_method"},
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		ntw, err := NewNetwork(
			ctx,
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

		upstream.ReorderUpstreams(upr)

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

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		clr := clients.NewClientRegistry(&log.Logger, "prjA", nil)
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
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
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
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		if err != nil {
			t.Fatal(err)
		}
		pup1, err := upr.NewUpstream(up1)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1

		pup2, err := upr.NewUpstream(up2)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2

		ntw, err := NewNetwork(
			ctx,
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

		upstream.ReorderUpstreams(upr)

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
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
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
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1, up2},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
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
			ctx,
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

		upstream.ReorderUpstreams(upr)

		time.Sleep(300 * time.Millisecond)

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
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
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
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
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
			ctx,
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

		upstream.ReorderUpstreams(upr)

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
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
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
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
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
			ctx,
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

		upstream.ReorderUpstreams(upr)

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
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
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
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1},
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
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
			ctx,
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

		upstream.ReorderUpstreams(upr)

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
		metricsTracker := health.NewTracker(&logger, projectID, 1*time.Hour)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		metricsTracker.Bootstrap(ctx)

		rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{},
		}, &logger)
		assert.NoError(t, err)

		upstreamConfigs := []*common.UpstreamConfig{
			{Id: "upstream-a", Endpoint: "http://upstream-a.localhost", Type: common.UpstreamTypeEvm, Evm: &common.EvmUpstreamConfig{ChainId: 123}},
			{Id: "upstream-b", Endpoint: "http://upstream-b.localhost", Type: common.UpstreamTypeEvm, Evm: &common.EvmUpstreamConfig{ChainId: 123}},
			{Id: "upstream-c", Endpoint: "http://upstream-c.localhost", Type: common.UpstreamTypeEvm, Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		}

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upstreamsRegistry := upstream.NewUpstreamsRegistry(
			ctx,
			&logger,
			projectID,
			upstreamConfigs,
			ssr,
			rateLimitersRegistry,
			vr,
			pr,
			nil,
			metricsTracker,
			1*time.Second,
		)

		err = upstreamsRegistry.Bootstrap(ctx)
		assert.NoError(t, err)

		err = upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, networkID)
		assert.NoError(t, err)

		prj := &PreparedProject{
			Config: &common.ProjectConfig{
				Id:       projectID,
				Networks: []*common.NetworkConfig{},
			},
			Logger: &logger,
		}
		networksRegistry := NewNetworksRegistry(
			prj,
			ctx,
			upstreamsRegistry,
			metricsTracker,
			nil,
			rateLimitersRegistry,
			&logger,
		)

		ntw, err := networksRegistry.prepareNetwork(
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

		upstream.ReorderUpstreams(upstreamsRegistry)

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
						_, err = up.Forward(ctx, req, false)
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
		mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)

		// First upstream (Envio) with unsupported network
		upEnvio := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "envio",
			Endpoint: "https://rpc.hypersync.xyz",
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

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(
			&log.Logger,
			vr,
			[]*common.ProviderConfig{},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		upr := upstream.NewUpstreamsRegistry(
			ctx,
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{upEnvio, upRpc1}, // Both upstreams
			ssr,
			rlr,
			vr,
			pr,
			nil,
			mt,
			1*time.Second,
		)
		err = upr.Bootstrap(ctx)
		if err == nil {
			t.Fatalf("Expected error on registry bootstrap, got nil")
		}

		ntw, err := NewNetwork(
			ctx,
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

		upstream.ReorderUpstreams(upr)

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

	t.Run("ResponseReleasedBeforeCacheSet", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		cacheCfg := &common.CacheConfig{
			Connectors: []*common.ConnectorConfig{
				{
					Id:     "mock",
					Driver: "mock",
					Mock: &common.MockConnectorConfig{
						MemoryConnectorConfig: common.MemoryConnectorConfig{
							MaxItems: 100_000,
						},
						// GetDelay: 10 * time.Second,
						// SetDelay: 10 * time.Second,
					},
				},
			},
			Policies: []*common.CachePolicyConfig{
				{
					Network:   "*",
					Method:    "*",
					TTL:       common.Duration(5 * time.Minute),
					Connector: "mock",
				},
			},
		}
		cacheCfg.SetDefaults()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetworkSimple(t, ctx, nil, nil)
		gock.New("http://rpc1.localhost").
			Post("/").
			MatchType("json").
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"method":  "eth_getBalance",
				"params":  []interface{}{"0x11", "0x11"},
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
				"params":  []interface{}{"0x22", "0x22"},
				"id":      22222,
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      22222,
				"result":  "0x22222222222222",
			})
		slowCache, err := evm.NewEvmJsonRpcCache(ctx, &log.Logger, cacheCfg)
		if err != nil {
			t.Fatalf("Failed to create evm json rpc cache: %v", err)
		}
		network.cacheDal = slowCache.WithProjectId("prjA")

		// Make the request
		req1 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x11", "0x11"],"id":11111}`))
		req1.SetCacheDal(slowCache)
		req1.SetNetwork(network)
		req2 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x22", "0x22"],"id":22222}`))
		req2.SetCacheDal(slowCache)
		req2.SetNetwork(network)

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
			jrr1Value, err := resp1.JsonRpcResponse()
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}
			jrr1Atomic.Store(jrr1Value)
			// Simulate immediate release of the response
			resp1.Release()

			resp2, err := network.Forward(ctx, req2)
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}
			jrr2Value, err := resp2.JsonRpcResponse()
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}
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

	t.Run("BatchRequestValidationAndRetry", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Set up the test environment
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetworkSimple(t, ctx, &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "test",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId:              123,
				GetLogsMaxBlockRange: 100_000,
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
						"code": -32000,
						"message": "method not found"
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
		req1 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":32,"method":"eth_trace","params":[{"fromBlock":"0x35A35CB","toBlock":"0x35AF7CA"}]}`))
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
		time.Sleep(100 * time.Millisecond)
		go func() {
			defer wg.Done()
			resp2, err2 = network.Forward(ctx, req2)
		}()
		wg.Wait()

		// Assertions for the first request (server-side error, should be retried)
		assert.Error(t, err1, "Expected an error for the first request")
		assert.Nil(t, resp1, "Expected nil response for the first request")
		assert.False(t, common.IsRetryableTowardsUpstream(err1), "Expected a retryable error for the first request")
		assert.True(t, common.HasErrorCode(err1, common.ErrCodeEndpointUnsupported), "Expected a unsupported method error for the first request")

		// Assertions for the second request (client-side error, should not be retried)
		assert.Nil(t, err2, "Expected no error for the second request")
		assert.NotNil(t, resp2, "Expected non-nil response for the second request")
	})
}

func TestNetwork_SelectionScenarios(t *testing.T) {
	t.Run("StatePollerContributesToErrorRateWhenNotResamplingExcludedUpstreams", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		evalFn, _ := common.CompileFunction(`
			(upstreams) => {
				return upstreams.filter(u => u.metrics.errorRate < 0.7);
			}
		`)
		selectionPolicy := &common.SelectionPolicyConfig{
			ResampleExcluded: false,
			EvalInterval:     common.Duration(100 * time.Millisecond),
			EvalFunction:     evalFn,
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
		network := setupTestNetworkSimple(t, ctx, &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId:             123,
				StatePollerInterval: common.Duration(50 * time.Millisecond), // Fast polling for test
				StatePollerDebounce: common.Duration(1 * time.Millisecond),  // Small debounce for test
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
		network := setupTestNetworkSimple(t, ctx, nil, nil)
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
		network := setupTestNetworkSimple(t, ctx, nil, nil)
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
		network := setupTestNetworkSimple(t, ctx, &common.UpstreamConfig{
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
					Duration: common.Duration(50 * time.Millisecond),
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
				if !common.HasErrorCode(err, "ErrFailsafeTimeoutExceeded") {
					t.Errorf("Expected ErrFailsafeTimeoutExceeded, got %v", err)
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
		network := setupTestNetworkSimple(t, ctx, nil, nil)
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
		network := setupTestNetworkSimple(t, ctx, nil, nil)
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
		network := setupTestNetworkSimple(t, ctx, nil, nil)

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
		network := setupTestNetworkSimple(t, ctx, nil, nil)
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
		assert.True(t, errors.Is(err, context.Canceled))

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
		network := setupTestNetworkSimple(t, ctx, nil, nil)
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
			JSON([]byte(`{"result":[{"value":0x1,"fromHost":"rpc1"}]}`))

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
		req.SetNetwork(network)
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

func TestNetwork_EvmGetLogs(t *testing.T) {
	t.Run("EnforceLatestBlockUpdateWhenRangeEndIsHigherThanLatestBlock", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Mock eth_getLogs request with toBlock higher than latest block
		requestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x11118899","address":"0x0000000000000000000000000000000000000000"}]}`)

		// Mock the eth_getBlockByNumber response for latest block force update
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "latest")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"number": "0x11119999", // Now higher than end range
				},
			})

		// Mock the eth_getLogs response
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs")
			}).
			Reply(200).
			JSON([]byte(`{"result":[{"value":0x1,"fromHost":"rpc1"}]}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetworkWithFullAndArchiveNodeUpstreams(t, ctx, common.EvmNodeTypeArchive, 0, common.EvmNodeTypeFull, 128)

		network.cfg.Evm.Integrity = &common.EvmIntegrityConfig{
			EnforceGetLogsBlockRange: util.BoolPtr(true),
		}

		// Wait for state poller debounce to pass
		time.Sleep(1010 * time.Millisecond)

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

		assert.True(t, len(gock.Pending()) == 0, "Expected no pending mocks")
	})

	t.Run("FailEvenAfterEnforceLatestBlockUpdateWhenRangeEndIsHigherThanLatestBlock", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Mock eth_getLogs request with toBlock higher than latest block
		requestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x11118899","address":"0x0000000000000000000000000000000000000000"}]}`)

		// Mock the eth_getBlockByNumber response for latest block force update
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "latest")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"number": "0x11118889", // Still lower than end range
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetworkWithFullAndArchiveNodeUpstreams(t, ctx, common.EvmNodeTypeArchive, 0, common.EvmNodeTypeFull, 128)

		network.cfg.Evm.Integrity = &common.EvmIntegrityConfig{
			EnforceGetLogsBlockRange: util.BoolPtr(true),
		}

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)
		assert.Error(t, err)
		assert.Nil(t, resp)

		assert.Contains(t, err.Error(), "less than toBlock")
	})

	t.Run("AvoidLatestBlockUpdateWhenRangeEndIsLowerThanLatestBlock", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Mock eth_getLogs request with toBlock lower than latest block
		requestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x100","address":"0x0000000000000000000000000000000000000000"}]}`)

		// Mock the eth_getLogs response
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs")
			}).
			Reply(200).
			JSON([]byte(`{"result":[{"value":0x1,"fromHost":"rpc1"}]}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetworkWithFullAndArchiveNodeUpstreams(t, ctx, common.EvmNodeTypeArchive, 0, common.EvmNodeTypeFull, 128)

		network.cfg.Evm.Integrity = &common.EvmIntegrityConfig{
			EnforceGetLogsBlockRange: util.BoolPtr(true),
		}

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

		assert.True(t, len(gock.Pending()) == 0, "Expected no pending mocks")
	})

	t.Run("SkipToUpstreamWithCorrectLatestBlockToCoverBlockRangeEnd", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Mock eth_getLogs request with a block range
		requestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x22228880","toBlock":"0x22228887","address":"0x0000000000000000000000000000000000000000"}]}`)

		// Mock the eth_getLogs response for rpc2 (should be used since the range is within its bounds)
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs")
			}).
			Reply(200).
			JSON([]byte(`{"result":[{"value":0x1,"fromHost":"rpc2"}]}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Setup network with two upstreams:
		// - rpc1: Archive node with latest block 0x11119999 but only 128 blocks available
		// - rpc2: Full node with latest block 0x11118888 but 1000 blocks available
		network := setupTestNetworkWithFullAndArchiveNodeUpstreams(
			t,
			ctx,
			common.EvmNodeTypeArchive, // rpc1 type
			128,                       // rpc1 max recent blocks
			common.EvmNodeTypeFull,    // rpc2 type
			1000,                      // rpc2 max recent blocks
		)

		network.cfg.Evm.Integrity = &common.EvmIntegrityConfig{
			EnforceGetLogsBlockRange: util.BoolPtr(true),
		}

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

		assert.True(t, len(gock.Pending()) == 0, "Expected no pending mocks")
	})

	t.Run("SkipDueToMaxAvailableRecentBlocksWhenLowerEndTooEarly", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Mock eth_getLogs request with fromBlock that's too early compared to maxAvailableRecentBlocks
		// Latest block is 0x11118888, with 128 max recent blocks, so anything before 0x11118888-128 is too early
		requestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x11117000","toBlock":"0x11118800","address":"0x0000000000000000000000000000000000000000"}]}`)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Setup network with a Full node that has limited block history (128 blocks)
		network := setupTestNetworkWithFullAndArchiveNodeUpstreams(t, ctx, common.EvmNodeTypeArchive, 0, common.EvmNodeTypeFull, 128)

		network.cfg.Evm.Integrity = &common.EvmIntegrityConfig{
			EnforceGetLogsBlockRange: util.BoolPtr(true),
		}

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Verify that the request was skipped with appropriate error
		assert.Error(t, err)
		assert.Nil(t, resp)
		assert.Contains(t, err.Error(), "is < than upstream latest block")

		assert.True(t, len(gock.Pending()) == 0, "Expected no pending mocks")
	})

	t.Run("BypassMaxAvailableRecentBlocksIfLowerEndStillInRange", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Mock eth_getLogs request with fromBlock that's within the maxAvailableRecentBlocks range
		// Latest block is 0x11118888, with 128 max recent blocks, so anything after 0x11118888-128 is fine
		requestBytes := []byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x11118800","toBlock":"0x11118850","address":"0x0000000000000000000000000000000000000000"}]}`)

		// Mock the eth_getLogs response since we expect the request to succeed
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs")
			}).
			Reply(200).
			JSON([]byte(`{"result":[{"value":0x1,"fromHost":"rpc1"}]}`))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Setup network with a Full node that has limited block history (128 blocks)
		network := setupTestNetworkWithFullAndArchiveNodeUpstreams(t, ctx, common.EvmNodeTypeArchive, 0, common.EvmNodeTypeFull, 128)

		network.cfg.Evm.Integrity = &common.EvmIntegrityConfig{
			EnforceGetLogsBlockRange: util.BoolPtr(true),
		}

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Verify that the request succeeded
		assert.NoError(t, err)
		assert.NotNil(t, resp)

		// Convert the raw response to a map to access custom fields like fromHost
		jrr, err := resp.JsonRpcResponse()
		assert.NoError(t, err)
		assert.NotNil(t, jrr.Result)

		fromHost, err := jrr.PeekStringByPath(0, "fromHost")
		assert.NoError(t, err)
		assert.Equal(t, "rpc1", fromHost)

		assert.True(t, len(gock.Pending()) == 0, "Expected no pending mocks")
	})

	t.Run("SplitIntoSubRequestsIfRangeTooBig", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Mock eth_getLogs request with a large block range
		requestBytes := []byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getLogs",
			"params": [{
				"fromBlock": "0x11118000",
				"toBlock": "0x11118500",
				"address": "0x0000000000000000000000000000000000000000",
				"topics": ["0x1234567890123456789012345678901234567890123456789012345678901234"]
			}]
		}`)

		// Mock responses for the sub-requests
		// First sub-request
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, "0x11118000") &&
					strings.Contains(body, "0x111180ff")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": []map[string]interface{}{
					{"logIndex": "0x1", "blockNumber": "0x11118001"},
				},
			})

		// Second sub-request
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, "0x11118100") &&
					strings.Contains(body, "0x111181ff")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      2,
				"result": []map[string]interface{}{
					{"logIndex": "0x2", "blockNumber": "0x11118102"},
				},
			})

		// Third sub-request
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, "0x11118200") &&
					strings.Contains(body, "0x111182ff")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      3,
				"result": []map[string]interface{}{
					{"logIndex": "0x3", "blockNumber": "0x11118203"},
				},
			})

		// Fourth sub-request
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, "0x11118300") &&
					strings.Contains(body, "0x111183ff")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      4,
				"result": []map[string]interface{}{
					{"logIndex": "0x4", "blockNumber": "0x11118304"},
				},
			})

		// Fifth sub-request
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, "0x11118400") &&
					strings.Contains(body, "0x111184ff")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      5,
				"result": []map[string]interface{}{
					{"logIndex": "0x5", "blockNumber": "0x11118405"},
				},
			})

		// Last sub-request
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, "0x11118500") &&
					strings.Contains(body, "0x11118500")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      5,
				"result": []map[string]interface{}{
					{"logIndex": "0x6", "blockNumber": "0x11118506"},
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Setup network with a node that has a small GetLogsMaxBlockRange
		network := setupTestNetworkWithFullAndArchiveNodeUpstreams(t, ctx, common.EvmNodeTypeArchive, 0, common.EvmNodeTypeFull, 1000)

		// Configure a small GetLogsMaxBlockRange to force splitting
		network.cfg.Evm.Integrity = &common.EvmIntegrityConfig{
			EnforceGetLogsBlockRange: util.BoolPtr(true),
		}
		upsList := network.upstreamsRegistry.GetNetworkUpstreams(util.EvmNetworkId(123))
		upsList[0].Config().Evm.GetLogsMaxBlockRange = 0x100 // Small range to force splitting

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Verify the merged response
		assert.NoError(t, err)
		assert.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		assert.NoError(t, err)
		w := bytes.NewBuffer(nil)
		jrr.WriteTo(w)
		result := w.Bytes()
		t.Logf("merged response: %s", result)

		// Parse the result to verify all logs from sub-requests are present
		var respObject map[string]interface{}
		err = sonic.Unmarshal(result, &respObject)
		if err != nil {
			t.Fatalf("Cannot parse response err: %s: %s", err, string(result))
		}

		// Verify we got all logs from all sub-requests
		logs := respObject["result"].([]interface{})
		assert.Equal(t, 6, len(logs))

		// Verify logs are from different blocks as expected
		blockNumbers := make([]string, len(logs))
		for i, l := range logs {
			log := l.(map[string]interface{})
			blockNumbers[i] = log["blockNumber"].(string)
		}
		assert.Contains(t, blockNumbers, "0x11118001")
		assert.Contains(t, blockNumbers, "0x11118102")
		assert.Contains(t, blockNumbers, "0x11118203")
		assert.Contains(t, blockNumbers, "0x11118304")
		assert.Contains(t, blockNumbers, "0x11118405")
		assert.Contains(t, blockNumbers, "0x11118506")
		assert.True(t, len(gock.Pending()) == 0, "Expected no pending mocks")
	})

	t.Run("SplitOnErrorWhenHedgePolicyExistsWithoutRaceCondition", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 3)

		// Mock eth_getLogs request with a large block range
		requestBytes := []byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getLogs",
			"params": [{
				"fromBlock": "0x18000",
				"toBlock": "0x18500",
				"address": "0x0000000000000000000000000000000000000000",
				"topics": ["0x1234567890123456789012345678901234567890123456789012345678901234"]
			}]
		}`)

		// Mock responses for the main request
		gock.New("http://rpc1.localhost").
			Post("").
			Persist(). // Not exact number because requests might be multiplexed
			Filter(func(request *http.Request) bool {
				body := strings.ToLower(util.SafeReadBody(request))
				return strings.Contains(body, "eth_getlogs") &&
					strings.Contains(body, "0x18000") &&
					strings.Contains(body, "0x18500")
			}).
			Reply(429).
			Delay(2 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "Request exceeds the range",
				},
			})

		// First sub-request
		gock.New("http://rpc1.localhost").
			Post("").
			Persist(). // Not exact number because sub-requests might be multiplexed for a hedged splitted getLogs request
			Filter(func(request *http.Request) bool {
				body := strings.ToLower(util.SafeReadBody(request))
				return strings.Contains(body, "eth_getlogs") &&
					strings.Contains(body, "0x18000") &&
					strings.Contains(body, "0x1827f")
			}).
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      2,
				"result": []map[string]interface{}{
					{"logIndex": "0x2", "blockNumber": "0x18101"},
				},
			})

		// Second sub-request
		gock.New("http://rpc1.localhost").
			Post("").
			Persist(). // Not exact number because sub-requests might be multiplexed for a hedged splitted getLogs request
			Filter(func(request *http.Request) bool {
				body := strings.ToLower(util.SafeReadBody(request))
				return strings.Contains(body, "eth_getlogs") &&
					strings.Contains(body, "0x18280") &&
					strings.Contains(body, "0x18500")
			}).
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      3,
				"result": []map[string]interface{}{
					{"logIndex": "0x3", "blockNumber": "0x18202"},
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Setup network with a node that has a small GetLogsMaxBlockRange
		network := setupTestNetworkSimple(t, ctx, nil, &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				ChainId: 123,
				Integrity: &common.EvmIntegrityConfig{
					EnforceGetLogsBlockRange: util.BoolPtr(true),
				},
			},
			Failsafe: &common.FailsafeConfig{
				Hedge: &common.HedgePolicyConfig{
					Delay:    common.Duration(1 * time.Millisecond),
					MaxCount: 10,
				},
			},
		})

		upsList := network.upstreamsRegistry.GetNetworkUpstreams(util.EvmNetworkId(123))
		upsList[0].Config().Evm.GetLogsMaxBlockRange = 0x10000000 // Large range to avoid auto-splitting since we want error-based splitting

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Verify the merged response
		assert.NoError(t, err)
		assert.NotNil(t, resp)
		if resp == nil {
			t.Fatalf("merged response is nil")
			return
		}

		jrr, err := resp.JsonRpcResponse()
		assert.NoError(t, err)
		if jrr == nil {
			t.Fatalf("merged response is nil")
			return
		}
		w := bytes.NewBuffer(nil)
		jrr.WriteTo(w)
		result := w.Bytes()

		// Parse the result to verify all logs from sub-requests are present
		var respObject map[string]interface{}
		err = sonic.Unmarshal(result, &respObject)
		if err != nil {
			t.Fatalf("Cannot parse response err: %s: %s", err, string(result))
		}

		// Verify we got all logs from all sub-requests
		logs := respObject["result"].([]interface{})
		assert.Equal(t, 2, len(logs))

		// Verify logs are from different blocks as expected
		blockNumbers := make([]string, len(logs))
		for i, l := range logs {
			log := l.(map[string]interface{})
			blockNumbers[i] = log["blockNumber"].(string)
		}
		assert.Contains(t, blockNumbers, "0x18101")
		assert.Contains(t, blockNumbers, "0x18202")
	})

	t.Run("SplitCorrectlyWhenMaxRangeIsOne", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Mock eth_getLogs request with a small block range
		requestBytes := []byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getLogs",
			"params": [{
				"fromBlock": "0x11118000",
				"toBlock": "0x11118002",
				"address": "0x0000000000000000000000000000000000000000",
				"topics": ["0x1234567890123456789012345678901234567890123456789012345678901234"]
			}]
		}`)

		// Mock responses for each individual block
		// First block (0x11118000)
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, `"fromBlock":"0x11118000"`) &&
					strings.Contains(body, `"toBlock":"0x11118000"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": []map[string]interface{}{
					{"logIndex": "0x1", "blockNumber": "0x11118000", "data": "0x1"},
				},
			})

		// Second block (0x11118001)
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, `"fromBlock":"0x11118001"`) &&
					strings.Contains(body, `"toBlock":"0x11118001"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      2,
				"result": []map[string]interface{}{
					{"logIndex": "0x2", "blockNumber": "0x11118001", "data": "0x2"},
				},
			})

		// Third block (0x11118002)
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, `"fromBlock":"0x11118002"`) &&
					strings.Contains(body, `"toBlock":"0x11118002"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      3,
				"result": []map[string]interface{}{
					{"logIndex": "0x3", "blockNumber": "0x11118002", "data": "0x3"},
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Setup network with a node that has GetLogsMaxBlockRange = 1
		network := setupTestNetworkWithFullAndArchiveNodeUpstreams(t, ctx, common.EvmNodeTypeArchive, 0, common.EvmNodeTypeFull, 120)

		// Configure GetLogsMaxBlockRange = 1 to force splitting into individual blocks
		network.cfg.Evm.Integrity = &common.EvmIntegrityConfig{
			EnforceGetLogsBlockRange: util.BoolPtr(true),
		}
		upsList := network.upstreamsRegistry.GetNetworkUpstreams(util.EvmNetworkId(123))
		upsList[0].Config().Evm.GetLogsMaxBlockRange = 1 // Force splitting into individual blocks

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Verify the merged response
		assert.NoError(t, err)
		assert.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		assert.NoError(t, err)
		w := bytes.NewBuffer(nil)
		jrr.WriteTo(w)
		result := w.Bytes()
		t.Logf("merged response: %s", result)

		// Parse the result to verify all logs from individual blocks are present
		var respObject map[string]interface{}
		err = sonic.Unmarshal(result, &respObject)
		assert.NoError(t, err, "Failed to unmarshal response: %v", err)

		// Verify we got all logs from all blocks
		logs := respObject["result"].([]interface{})
		assert.Equal(t, 3, len(logs), "Expected exactly 3 logs (one from each block)")

		// Verify logs are from different blocks and in correct order
		blockNumbers := make([]string, len(logs))
		data := make([]string, len(logs))
		for i, l := range logs {
			log := l.(map[string]interface{})
			blockNumbers[i] = log["blockNumber"].(string)
			data[i] = log["data"].(string)
		}

		// Verify block numbers are in sequence
		assert.Contains(t, blockNumbers, "0x11118000", "First log should be from block 0x11118000")
		assert.Contains(t, blockNumbers, "0x11118001", "Second log should be from block 0x11118001")
		assert.Contains(t, blockNumbers, "0x11118002", "Third log should be from block 0x11118002")

		// Verify data values are in sequence
		assert.Contains(t, data, "0x1", "First log should have data 0x1")
		assert.Contains(t, data, "0x2", "Second log should have data 0x2")
		assert.Contains(t, data, "0x3", "Third log should have data 0x3")

		assert.True(t, len(gock.Pending()) == 0, "Expected no pending mocks")
	})

	t.Run("SkipSplitWhenRangeIsWithinBounds", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Mock eth_getLogs request with a range smaller than max range
		requestBytes := []byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getLogs",
			"params": [{
				"fromBlock": "0x11118000",
				"toBlock": "0x11118050",
				"address": "0x0000000000000000000000000000000000000000",
				"topics": ["0x1234567890123456789012345678901234567890123456789012345678901234"]
			}]
		}`)

		// Mock single response since we expect no splitting
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, `"fromBlock":"0x11118000"`) &&
					strings.Contains(body, `"toBlock":"0x11118050"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": []map[string]interface{}{
					{"logIndex": "0x1", "blockNumber": "0x11118025", "data": "0x1"},
					{"logIndex": "0x2", "blockNumber": "0x11118035", "data": "0x2"},
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Setup network with GetLogsMaxBlockRange = 0x100
		network := setupTestNetworkWithFullAndArchiveNodeUpstreams(t, ctx, common.EvmNodeTypeArchive, 0, common.EvmNodeTypeFull, 1000)

		network.cfg.Evm.Integrity = &common.EvmIntegrityConfig{
			EnforceGetLogsBlockRange: util.BoolPtr(true),
		}
		upsList := network.upstreamsRegistry.GetNetworkUpstreams(util.EvmNetworkId(123))
		upsList[0].Config().Evm.GetLogsMaxBlockRange = 0x100 // Range that's larger than our test range

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Verify the response
		assert.NoError(t, err)
		assert.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		assert.NoError(t, err)
		w := bytes.NewBuffer(nil)
		jrr.WriteTo(w)
		result := w.Bytes()

		// Parse and verify the response
		var respObject map[string]interface{}
		err = sonic.Unmarshal(result, &respObject)
		assert.NoError(t, err)

		logs := respObject["result"].([]interface{})
		assert.Equal(t, 2, len(logs), "Expected exactly 2 logs")

		// Verify log block numbers
		blockNumbers := make([]string, len(logs))
		for i, l := range logs {
			log := l.(map[string]interface{})
			blockNumbers[i] = log["blockNumber"].(string)
		}
		assert.Contains(t, blockNumbers, "0x11118025")
		assert.Contains(t, blockNumbers, "0x11118035")

		// Verify only one request was made (no splitting)
		assert.True(t, len(gock.Pending()) == 0, "Expected no pending mocks")
	})

	t.Run("SkipSplitWhenRangeIsExactlyEqualToMaxRange", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Mock eth_getLogs request with range exactly equal to max range (0x100)
		requestBytes := []byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getLogs",
			"params": [{
				"fromBlock": "0x11118000",
				"toBlock": "0x111180ff",
				"address": "0x0000000000000000000000000000000000000000",
				"topics": ["0x1234567890123456789012345678901234567890123456789012345678901234"]
			}]
		}`)

		// Mock single response since we expect no splitting
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, `"fromBlock":"0x11118000"`) &&
					strings.Contains(body, `"toBlock":"0x111180ff"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": []map[string]interface{}{
					{"logIndex": "0x1", "blockNumber": "0x11118050", "data": "0x1"},
					{"logIndex": "0x2", "blockNumber": "0x11118080", "data": "0x2"},
					{"logIndex": "0x3", "blockNumber": "0x111180f0", "data": "0x3"},
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Setup network with GetLogsMaxBlockRange = 0x100
		network := setupTestNetworkWithFullAndArchiveNodeUpstreams(t, ctx, common.EvmNodeTypeArchive, 0, common.EvmNodeTypeFull, 1000)

		network.cfg.Evm.Integrity = &common.EvmIntegrityConfig{
			EnforceGetLogsBlockRange: util.BoolPtr(true),
		}
		upsList := network.upstreamsRegistry.GetNetworkUpstreams(util.EvmNetworkId(123))
		upsList[0].Config().Evm.GetLogsMaxBlockRange = 0x100 // Range exactly equal to our test range

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Verify the response
		assert.NoError(t, err)
		assert.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		assert.NoError(t, err)
		w := bytes.NewBuffer(nil)
		jrr.WriteTo(w)
		result := w.Bytes()

		// Parse and verify the response
		var respObject map[string]interface{}
		err = sonic.Unmarshal(result, &respObject)
		assert.NoError(t, err)

		logs := respObject["result"].([]interface{})
		assert.Equal(t, 3, len(logs), "Expected exactly 3 logs")

		// Verify log block numbers
		blockNumbers := make([]string, len(logs))
		for i, l := range logs {
			log := l.(map[string]interface{})
			blockNumbers[i] = log["blockNumber"].(string)
		}
		assert.Contains(t, blockNumbers, "0x11118050")
		assert.Contains(t, blockNumbers, "0x11118080")
		assert.Contains(t, blockNumbers, "0x111180f0")

		// Verify only one request was made (no splitting)
		assert.True(t, len(gock.Pending()) == 0, "Expected no pending mocks")
	})

	t.Run("UseCacheWhenOneOfSubRequestsIsAlreadyCached", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Setup cache configuration
		cacheCfg := &common.CacheConfig{
			Connectors: []*common.ConnectorConfig{
				{
					Id:     "mock",
					Driver: "mock",
					Mock: &common.MockConnectorConfig{
						MemoryConnectorConfig: common.MemoryConnectorConfig{
							MaxItems: 100_000,
						},
						// GetDelay: 10 * time.Second,
						// SetDelay: 10 * time.Second,
					},
				},
			},
			Policies: []*common.CachePolicyConfig{
				{
					Network:   "*",
					Method:    "*",
					TTL:       common.Duration(5 * time.Minute),
					Connector: "mock",
					Finality:  common.DataFinalityStateUnfinalized,
				},
			},
		}
		cacheCfg.SetDefaults()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetworkWithFullAndArchiveNodeUpstreams(t, ctx, common.EvmNodeTypeArchive, 0, common.EvmNodeTypeFull, 1000)

		// Configure network for splitting
		network.cfg.Evm.Integrity = &common.EvmIntegrityConfig{
			EnforceGetLogsBlockRange: util.BoolPtr(true),
		}
		upsList := network.upstreamsRegistry.GetNetworkUpstreams(util.EvmNetworkId(123))
		upsList[0].Config().Evm.GetLogsMaxBlockRange = 0x100 // Force splitting into ranges of 256 blocks

		// Create and set cache
		slowCache, err := evm.NewEvmJsonRpcCache(ctx, &log.Logger, cacheCfg)
		if err != nil {
			t.Fatalf("Failed to create evm json rpc cache: %v", err)
		}
		network.cacheDal = slowCache.WithProjectId("prjA")

		// First, make a request that will be cached for the middle range
		middleRangeRequest := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getLogs",
			"params": [{
				"fromBlock": "0x11118100",
				"toBlock": "0x111181ff",
				"address": "0x0000000000000000000000000000000000000000",
				"topics": ["0x1234567890123456789012345678901234567890123456789012345678901234"]
			}]
		}`))
		middleRangeRequest.SetCacheDal(slowCache)
		middleRangeRequest.SetNetwork(network)

		// Mock response for the middle range that will be cached
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, `"fromBlock":"0x11118100"`) &&
					strings.Contains(body, `"toBlock":"0x111181ff"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": []map[string]interface{}{
					{"logIndex": "0x2", "blockNumber": "0x11118150", "data": "0x2"},
				},
			})

		// Make the request to cache the middle range
		resp, err := network.Forward(ctx, middleRangeRequest)
		assert.NoError(t, err)
		assert.NotNil(t, resp)
		resp.Release()

		// Now make the full request that should be split into three parts
		fullRangeRequest := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getLogs",
			"params": [{
				"fromBlock": "0x11118000",
				"toBlock": "0x11118300",
				"address": "0x0000000000000000000000000000000000000000",
				"topics": ["0x1234567890123456789012345678901234567890123456789012345678901234"]
			}]
		}`))
		fullRangeRequest.SetCacheDal(slowCache)
		fullRangeRequest.SetNetwork(network)

		// Mock responses for the first and last ranges
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, `"fromBlock":"0x11118000"`) &&
					strings.Contains(body, `"toBlock":"0x111180ff"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      2,
				"result": []map[string]interface{}{
					{"logIndex": "0x1", "blockNumber": "0x11118050", "data": "0x1"},
				},
			})

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, `"fromBlock":"0x11118200"`) &&
					strings.Contains(body, `"toBlock":"0x111182ff"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      3,
				"result": []map[string]interface{}{
					{"logIndex": "0x3", "blockNumber": "0x11118250", "data": "0x3"},
				},
			})

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, `"fromBlock":"0x11118300"`) &&
					strings.Contains(body, `"toBlock":"0x11118300"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      3,
				"result": []map[string]interface{}{
					{"logIndex": "0x4", "blockNumber": "0x11118300", "data": "0x4"},
				},
			})

		// Make the full request
		resp, err = network.Forward(ctx, fullRangeRequest)
		assert.NoError(t, err)
		assert.NotNil(t, resp)

		// Verify the merged response
		jrr, err := resp.JsonRpcResponse()
		assert.NoError(t, err)
		w := bytes.NewBuffer(nil)
		jrr.WriteTo(w)
		result := w.Bytes()

		// Parse and verify the response contains logs from all three ranges
		var respObject map[string]interface{}
		err = sonic.Unmarshal(result, &respObject)
		assert.NoError(t, err)

		logs := respObject["result"].([]interface{})
		assert.Equal(t, 4, len(logs), "Expected exactly 4 logs (one from each range)")

		// Verify log block numbers and data
		blockNumbers := make([]string, len(logs))
		data := make([]string, len(logs))
		for i, l := range logs {
			log := l.(map[string]interface{})
			blockNumbers[i] = log["blockNumber"].(string)
			data[i] = log["data"].(string)
		}

		// Verify we got logs from all three ranges
		assert.Contains(t, blockNumbers, "0x11118050", "Missing log from first range")
		assert.Contains(t, blockNumbers, "0x11118150", "Missing log from cached middle range")
		assert.Contains(t, blockNumbers, "0x11118250", "Missing log from third range")
		assert.Contains(t, blockNumbers, "0x11118300", "Missing log from last range")

		// Verify data values
		assert.Contains(t, data, "0x1", "Missing data from first range")
		assert.Contains(t, data, "0x2", "Missing data from cached middle range")
		assert.Contains(t, data, "0x3", "Missing data from third range")
		assert.Contains(t, data, "0x4", "Missing data from last range")

		// Verify only two requests were made (first and last ranges)
		// The middle range should have come from cache
		pendings := gock.Pending()
		assert.True(t, len(pendings) == 0, "Expected no pending mocks")
	})
}

func setupTestNetworkSimple(t *testing.T, ctx context.Context, upstreamConfig *common.UpstreamConfig, networkConfig *common.NetworkConfig) *Network {
	t.Helper()

	rateLimitersRegistry, _ := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
	metricsTracker := health.NewTracker(&log.Logger, "test", time.Minute)

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
	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(
		&log.Logger,
		vr,
		[]*common.ProviderConfig{},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems: 100_000,
			},
		},
	})
	if err != nil {
		panic(err)
	}
	upstreamsRegistry := upstream.NewUpstreamsRegistry(
		ctx,
		&log.Logger,
		"test",
		[]*common.UpstreamConfig{upstreamConfig},
		ssr,
		rateLimitersRegistry,
		vr,
		pr,
		nil,
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
		ctx,
		&log.Logger,
		"test",
		networkConfig,
		rateLimitersRegistry,
		upstreamsRegistry,
		metricsTracker,
	)
	assert.NoError(t, err)

	upstreamsRegistry.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)

	err = network.Bootstrap(ctx)
	assert.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	if upstreamConfig.Id == "test" {
		h, _ := common.HexToInt64("0x1273c18")
		upsList := upstreamsRegistry.GetNetworkUpstreams(util.EvmNetworkId(123))
		upsList[0].EvmStatePoller().SuggestFinalizedBlock(h)
		upsList[0].EvmStatePoller().SuggestLatestBlock(h)
	}

	upstream.ReorderUpstreams(upstreamsRegistry)

	return network
}

func setupTestNetworkWithFullAndArchiveNodeUpstreams(t *testing.T, ctx context.Context, nodeType1 common.EvmNodeType, maxRecentBlocks1 int64, nodeType2 common.EvmNodeType, maxRecentBlocks2 int64) *Network {
	t.Helper()

	rateLimitersRegistry, _ := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
	metricsTracker := health.NewTracker(&log.Logger, "test", time.Minute)

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

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(
		&log.Logger,
		vr,
		[]*common.ProviderConfig{},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems: 100_000,
			},
		},
	})
	if err != nil {
		panic(err)
	}
	upstreamsRegistry := upstream.NewUpstreamsRegistry(
		ctx,
		&log.Logger,
		"test",
		[]*common.UpstreamConfig{up1, up2},
		ssr,
		rateLimitersRegistry,
		vr,
		pr,
		nil,
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
		ctx,
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

	upstream.ReorderUpstreams(upstreamsRegistry)

	fb1, _ := common.HexToInt64("0x11117777")
	upsList := upstreamsRegistry.GetNetworkUpstreams(util.EvmNetworkId(123))
	upsList[0].EvmStatePoller().SuggestFinalizedBlock(fb1)
	lb1, _ := common.HexToInt64("0x11118888")
	upsList[0].EvmStatePoller().SuggestLatestBlock(lb1)

	fb2, _ := common.HexToInt64("0x22227777")
	upsList[1].EvmStatePoller().SuggestFinalizedBlock(fb2)
	lb2, _ := common.HexToInt64("0x22228888")
	upsList[1].EvmStatePoller().SuggestLatestBlock(lb2)

	return network
}
