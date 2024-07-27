package erpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/health"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/flair-sdk/erpc/util"
	"github.com/flair-sdk/erpc/vendors"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
)

func TestNetwork(t *testing.T) {
	t.Run("ForwardCorrectlyRateLimitedOnNetworkLevel", func(t *testing.T) {
		defer gock.Off()
		defer gock.Clean()
		defer gock.CleanUnmatchedRequest()

		rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(
			&common.RateLimiterConfig{
				Budgets: []*common.RateLimitBudgetConfig{
					{
						Id: "MyLimiterBudget_Test1",
						Rules: []*common.RateLimitRuleConfig{
							{
								Scope:    "instance",
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
								Scope:    "instance",
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
								Scope:    "instance",
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

		var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1273c18",false]}`)

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
				Times(1000).
				// Post("/").
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

func safeReadBody(request *http.Request) string {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return ""
	}
	request.Body = io.NopCloser(bytes.NewBuffer(body))
	return string(body)
}
