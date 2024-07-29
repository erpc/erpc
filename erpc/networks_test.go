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

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/erpc/erpc/vendors"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
)

func init() {
	log.Logger = log.Level(zerolog.ErrorLevel).Output(zerolog.ConsoleWriter{Out: os.Stderr})
}

func TestNetwork_Forward(t *testing.T) {

	t.Run("ForwardCorrectlyRateLimitedOnNetworkLevel", func(t *testing.T) {
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()
		setupMocksForEvmBlockTracker()

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
		var lastResp common.NormalizedResponse

		for i := 0; i < 5; i++ {
			fakeReq := upstream.NewNormalizedRequest([]byte(`{"method": "eth_chainId","params":[]}`))
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
			fakeReq := upstream.NewNormalizedRequest([]byte(`{"method": "eth_chainId","params":[]}`))
			_, lastErr = ntw.Forward(ctx, fakeReq)
		}

		var e *common.ErrNetworkRateLimitRuleExceeded
		if lastErr != nil && errors.As(lastErr, &e) {
			t.Errorf("Did not expect ErrNetworkRateLimitRuleExceeded")
		}
	})

	t.Run("ForwardRetryFailuresWithoutSuccessNoCode", func(t *testing.T) {
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
			vndr,
			mt,
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
		fakeReq := upstream.NewNormalizedRequest(requestBytes)
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
		if !strings.Contains(err.Error(), "ErrFailsafeRetryExceeded") {
			t.Errorf("Expected %v, got %v", "ErrFailsafeRetryExceeded", err)
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
			vndr,
			mt,
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
		fakeReq := upstream.NewNormalizedRequest(requestBytes)
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
		if !strings.Contains(err.Error(), "ErrFailsafeRetryExceeded") {
			t.Errorf("Expected %v, got %v", "ErrFailsafeRetryExceeded", err)
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
			fakeReq := upstream.NewNormalizedRequest([]byte(`{"method": "eth_chainId","params":[]}`))
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
			vndr,
			mt,
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
		fakeReq := upstream.NewNormalizedRequest(requestBytes)
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
		if !strings.Contains(err.Error(), "ErrFailsafeRetryExceeded") {
			t.Errorf("Expected %v, got %v", "ErrFailsafeRetryExceeded", err)
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
			vndr,
			mt,
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
		fakeReq := upstream.NewNormalizedRequest(requestBytes)
		resp, err := ntw.Forward(ctx, fakeReq)

		if len(gock.Pending()) > 0 {
			t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
			for _, pending := range gock.Pending() {
				t.Errorf("Pending mock: %v", pending)
			}
		}

		if err != nil {
			t.Errorf("Expected nil error, got %v", err)
		}

		jrr, err := resp.JsonRpcResponse()
		if err != nil {
			t.Errorf("Expected nil error, got %v", err)
		}
		if jrr.Result == nil {
			t.Errorf("Expected result, got %v", jrr)
		}

		result, ok := jrr.Result.(map[string]interface{})
		if !ok {
			t.Errorf("Expected result to be a map, got %T", jrr.Result)
		}
		if hash, ok := result["hash"]; !ok || hash == "" {
			t.Errorf("Expected hash to exist and be non-empty, got %v", result)
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
			vndr,
			mt,
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

		fakeReq := upstream.NewNormalizedRequest(requestBytes)
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
			vndr,
			mt,
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

		fakeReq := upstream.NewNormalizedRequest(requestBytes)
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
			vndr,
			mt,
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

		fakeReq := upstream.NewNormalizedRequest(requestBytes)
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

		result, ok := jrr.Result.(map[string]interface{})
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
			vndr,
			mt,
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

		fakeReq := upstream.NewNormalizedRequest(requestBytes)
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
		result, ok := jrr.Result.(map[string]interface{})
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
	// 	fakeReq := upstream.NewNormalizedRequest(requestBytes)
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
			vndr,
			mt,
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
			fakeReq := upstream.NewNormalizedRequest(requestBytes)
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
			vndr,
			mt,
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
			fakeReq := upstream.NewNormalizedRequest(requestBytes)
			_, lastErr = ntw.Forward(ctx, fakeReq)
		}

		if lastErr == nil {
			t.Fatalf("Expected an error, got nil")
		}

		time.Sleep(2 * time.Second)

		var resp common.NormalizedResponse
		for i := 0; i < 3; i++ {
			fakeReq := upstream.NewNormalizedRequest(requestBytes)
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
		result, ok := jrr.Result.(map[string]interface{})
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
			JSON(json.RawMessage(`{"error":{"code":-32603,"message":"Internal error"}}`))

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
			vndr,
			mt,
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

		fakeReq := upstream.NewNormalizedRequest(requestBytes)
		_, err = ntw.Forward(ctx, fakeReq)

		if err == nil {
			t.Fatalf("Expected non-nil error, got nil")
		}

		cc := err.(*common.ErrJsonRpcExceptionInternal).CodeChain()
		if !strings.Contains(cc, "-32603") {
			t.Fatalf("Expected error code -32603, got %v", cc)
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
			vndr,
			mt,
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

		fakeReq := upstream.NewNormalizedRequest(requestBytes)
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

		result, ok := jrr.Result.(map[string]interface{})
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
			vndr,
			mt,
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

		fakeReq := upstream.NewNormalizedRequest(requestBytes)
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

		result, ok := jrr.Result.([]interface{})
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
			vndr,
			mt,
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

		fakeReq := upstream.NewNormalizedRequest(requestBytes)
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

		result, ok := jrr.Result.([]interface{})
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
		up1 := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			VendorName: "quicknode",
		}
		upr := upstream.NewUpstreamsRegistry(
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1},
			rlr,
			vndr,
			mt,
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

		fakeReq := upstream.NewNormalizedRequest(requestBytes)
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

		if !common.HasCode(err, common.ErrCodeEndpointCapacityExceeded) {
			t.Errorf("Expected error code %v, got %+v", common.ErrCodeEndpointCapacityExceeded, err)
		}
	})

	t.Run("ForwardLlamaRPCEndpointRateLimitResponse", func(t *testing.T) {
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
			VendorName: "llama",
		}
		upr := upstream.NewUpstreamsRegistry(
			&log.Logger,
			"prjA",
			[]*common.UpstreamConfig{up1},
			rlr,
			vndr,
			mt,
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

		fakeReq := upstream.NewNormalizedRequest(requestBytes)
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

		if !common.HasCode(err, common.ErrCodeEndpointCapacityExceeded) {
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

		network, err := networksRegistry.RegisterNetwork(
			&logger,
			&common.ProjectConfig{Id: projectID},
			&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{ChainId: 123},
			},
		)
		assert.NoError(t, err)

		simulateRequests := func(method string, upstreamId string, latency time.Duration) {
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
		simulateRequests("eth_getLogs", "upstream-a", 200*time.Millisecond)
		simulateRequests("eth_getLogs", "upstream-b", 100*time.Millisecond)
		simulateRequests("eth_getLogs", "upstream-c", 50*time.Millisecond)
		simulateRequests("eth_traceTransaction", "upstream-a", 100*time.Millisecond)
		simulateRequests("eth_traceTransaction", "upstream-b", 50*time.Millisecond)
		simulateRequests("eth_traceTransaction", "upstream-c", 200*time.Millisecond)
		simulateRequests("eth_call", "upstream-a", 50*time.Millisecond)
		simulateRequests("eth_call", "upstream-b", 200*time.Millisecond)
		simulateRequests("eth_call", "upstream-c", 100*time.Millisecond)

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
					req := upstream.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"%s","params":[],"id":1}`, method)))
					_, err := network.Forward(ctx, req)
					assert.NoError(t, err)
				}(method)
				time.Sleep(10 * time.Millisecond)
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
				req := upstream.NewNormalizedRequest(requestBytes)
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
				req := upstream.NewNormalizedRequest(requestBytes)
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
			Delay(2 * time.Second).
			BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`)

		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
				defer cancel()
				req := upstream.NewNormalizedRequest(requestBytes)
				resp, err := network.Forward(ctx, req)
				assert.Error(t, err)
				assert.True(t, errors.Is(err, context.DeadlineExceeded))
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
			req := upstream.NewNormalizedRequest(successRequestBytes)
			resp, err := network.Forward(context.Background(), req)
			assert.NoError(t, err)
			assert.NotNil(t, resp)
		}()

		go func() {
			defer wg.Done()
			req := upstream.NewNormalizedRequest(failureRequestBytes)
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
		req1 := upstream.NewNormalizedRequest(requestBytes)
		resp1, err1 := network.Forward(context.Background(), req1)
		assert.NoError(t, err1)
		assert.NotNil(t, resp1)

		// Second request (should not be in-flight)
		req2 := upstream.NewNormalizedRequest(requestBytes)
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
}

func setupTestNetwork(t *testing.T) *Network {
	t.Helper()

	setupMocksForEvmBlockTracker()

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

func setupMocksForEvmBlockTracker() {
	resetGock()

	// Mock for evm block tracker
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			return strings.Contains(safeReadBody(request), "eth_getBlockByNumber")
		}).
		Reply(200).
		JSON(json.RawMessage(`{"result": "0x1273c18"}`))
	gock.New("http://rpc2.localhost").
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			return strings.Contains(safeReadBody(request), "eth_getBlockByNumber")
		}).
		Reply(200).
		JSON(json.RawMessage(`{"result": "0x1273c18"}`))
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
