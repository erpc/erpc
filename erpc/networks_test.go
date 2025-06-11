package erpc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"runtime"
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
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
					MaxItems: 100_000, MaxTotalSize: "1GB",
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
			nil,
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
					MaxItems: 100_000, MaxTotalSize: "1GB",
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
			nil,
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
					MaxItems: 100_000, MaxTotalSize: "1GB",
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
			nil,
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
					MaxItems: 100_000, MaxTotalSize: "1GB",
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
			nil,
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
					MaxItems: 100_000, MaxTotalSize: "1GB",
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
			nil,
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
					MaxItems: 100_000, MaxTotalSize: "1GB",
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
			nil,
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
					MaxItems: 100_000, MaxTotalSize: "1GB",
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
			nil,
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
		err = pup1.Bootstrap(ctx)
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
		err = pup2.Bootstrap(ctx)
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
					MaxItems: 100_000, MaxTotalSize: "1GB",
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
			nil,
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
		err = pup1.Bootstrap(ctx)
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
		err = pup2.Bootstrap(ctx)
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
				DirectiveDefaults: &common.DirectiveDefaultsConfig{
					RetryEmpty: &common.TRUE,
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
		fakeReq.ApplyDirectiveDefaults(ntw.Config().DirectiveDefaults)
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
					MaxItems: 100_000, MaxTotalSize: "1GB",
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
			nil,
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
		err = pup1.Bootstrap(ctx)
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
		err = pup2.Bootstrap(ctx)
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

	t.Run("ForwardRetriesOnInvalidArgumentCodeClientError", func(t *testing.T) {
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
					MaxItems: 100_000, MaxTotalSize: "1GB",
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
			nil,
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
		err = pup1.Bootstrap(ctx)
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
		err = pup2.Bootstrap(ctx)
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
					MaxItems: 100_000, MaxTotalSize: "1GB",
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
			nil,
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
		err = pup1.Bootstrap(ctx)
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
		err = pup2.Bootstrap(ctx)
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
				DirectiveDefaults: &common.DirectiveDefaultsConfig{
					RetryEmpty: &common.TRUE,
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
		fakeReq.ApplyDirectiveDefaults(ntw.Config().DirectiveDefaults)
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

		fromHost, err := jrr.PeekStringByPath(context.TODO(), 0, "fromHost")
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
					MaxItems: 100_000, MaxTotalSize: "1GB",
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
			nil,
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
				DirectiveDefaults: &common.DirectiveDefaultsConfig{
					RetryEmpty: &common.TRUE,
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

		upstream.ReorderUpstreams(upr)

		// Create a fake request and forward it through the network
		fakeReq := common.NewNormalizedRequest(requestBytes)
		fakeReq.ApplyDirectiveDefaults(ntw.Config().DirectiveDefaults)
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

		fromHost, err := jrr.PeekStringByPath(context.TODO(), 0, "fromHost")
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
					MaxItems: 100_000, MaxTotalSize: "1GB",
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
			nil,
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
				DirectiveDefaults: &common.DirectiveDefaultsConfig{
					RetryEmpty: &common.TRUE,
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

		upstream.ReorderUpstreams(upr)

		// Create a fake request and forward it through the network
		fakeReq := common.NewNormalizedRequest(requestBytes)
		fakeReq.ApplyDirectiveDefaults(ntw.Config().DirectiveDefaults)
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

		fromHost, err := jrr.PeekStringByPath(context.TODO(), 0, "fromHost")
		if err != nil {
			t.Fatalf("Failed to get fromHost from result: %v", err)
		}
		if fromHost != "rpc2" {
			t.Errorf("Expected fromHost to be %q, got %q", "rpc2", fromHost)
		}
	})
