package erpc

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	// "encoding/json"
	"errors"
	"io"

	// "net/http"
	"net/http"
	"net/http/httptest"

	// "strings"
	"sync"
	"testing"

	// "time"

	// "github.com/failsafe-go/failsafe-go"
	// "github.com/failsafe-go/failsafe-go"
	// "github.com/failsafe-go/failsafe-go/retrypolicy"
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/flair-sdk/erpc/vendors"

	// "github.com/flair-sdk/erpc/upstream"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
)

type ResponseRecorder struct {
	sync.Mutex
	*httptest.ResponseRecorder
	extraBodyWriters []io.Writer
}

func (r *ResponseRecorder) Write(p []byte) (int, error) {
	n, err := r.ResponseRecorder.Write(p)
	if err != nil {
		return n, err
	}
	for _, wr := range r.extraBodyWriters {
		_, err := wr.Write(p)
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (r *ResponseRecorder) AddBodyWriter(wr io.WriteCloser) {
	r.extraBodyWriters = append(r.extraBodyWriters, wr)
}

func (r *ResponseRecorder) Close() error {
	if r.extraBodyWriters != nil {
		for _, wr := range r.extraBodyWriters {
			closer, ok := wr.(io.Closer)
			if ok {
				err := closer.Close()
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (r *ResponseRecorder) AddHeader(key, value string) {
	r.Header().Add(key, value)
}

func TestNetwork_ForwardCorrectlyRateLimitedOnNetworkLevel(t *testing.T) {
	defer gock.Clean()

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
	)
	if err != nil {
		t.Error(err)
	}

	ntw, err := NewNetwork(&log.Logger, "prjA", &common.NetworkConfig{
		RateLimitBudget: "MyLimiterBudget_Test1",
	}, rateLimitersRegistry)
	if err != nil {
		t.Error(err)
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
}

func TestNetwork_ForwardNotRateLimitedOnNetworkLevel(t *testing.T) {
	defer gock.Clean()

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
	)
	if err != nil {
		t.Error(err)
	}
	ntw, err := NewNetwork(&log.Logger, "prjA", &common.NetworkConfig{
		RateLimitBudget: "MyLimiterBudget_Test2",
	}, rateLimitersRegistry)
	if err != nil {
		t.Error(err)
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
}

func TestNetwork_ForwardRetryFailuresWithoutSuccess(t *testing.T) {
	defer gock.Clean()
	defer gock.DisableNetworking()
	defer gock.DisableNetworkingFilters()

	gock.EnableNetworking()

	// Register a networking filter
	gock.NetworkingFilter(func(req *http.Request) bool {
		shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "localhost"
		return shouldMakeRealCall
	})

	var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1273c18",false]}`)

	gock.New("http://google.com").
		Times(3).
		Post("").
		Reply(503).
		JSON(json.RawMessage(`{"error":{"message":"some random provider issue"}}`))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clr := upstream.NewClientRegistry()

	fsCfg := &common.FailsafeConfig{
		Retry: &common.RetryPolicyConfig{
			MaxAttempts: 3,
		},
	}
	rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{},
	})
	if err != nil {
		t.Error(err)
	}
	vndr := vendors.NewVendorsRegistry()
	upr := upstream.NewUpstreamsRegistry(&log.Logger, &common.Config{}, rlr, vndr)
	defer upr.Shutdown()
	pup, err := upr.NewUpstream("prjA", &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "test",
		Endpoint: "http://google.com",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}, &log.Logger)
	if err != nil {
		t.Error(err)
	}
	cl, err := clr.GetOrCreateClient(pup)
	if err != nil {
		t.Error(err)
	}
	pup.Client = cl
	ntw, err := NewNetwork(&log.Logger, "prjA", &common.NetworkConfig{
		Failsafe: fsCfg,
	}, rlr)
	if err != nil {
		t.Error(err)
	}
	ntw.Upstreams = []*upstream.Upstream{pup}
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
	} else if !strings.Contains(err.Error(), "ErrFailsafeRetryExceeded") {
		t.Errorf("Expected %v, got %v", "ErrFailsafeRetryExceeded", err)
	}
}

func TestNetwork_ForwardRetryFailuresWithSuccess(t *testing.T) {
	defer gock.Clean()
	defer gock.DisableNetworking()
	defer gock.DisableNetworkingFilters()

	gock.EnableNetworking()

	// Register a networking filter
	gock.NetworkingFilter(func(req *http.Request) bool {
		shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "localhost"
		return shouldMakeRealCall
	})

	var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1273c18",false]}`)

	gock.New("http://google.com").
		Times(3).
		Post("").
		Reply(503).
		JSON(json.RawMessage(`{"error":{"message":"some random provider issue"}}`))

	gock.New("http://google.com").
		Post("").
		Reply(200).
		JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clr := upstream.NewClientRegistry()
	fsCfg := &common.FailsafeConfig{
		Retry: &common.RetryPolicyConfig{
			MaxAttempts: 4,
		},
	}
	rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{},
	})
	if err != nil {
		t.Error(err)
	}
	vndr := vendors.NewVendorsRegistry()
	upr := upstream.NewUpstreamsRegistry(&log.Logger, &common.Config{}, rlr, vndr)
	defer upr.Shutdown()
	pup, err := upr.NewUpstream("prjA", &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "test",
		Endpoint: "http://google.com",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}, &log.Logger)
	if err != nil {
		t.Error(err)
	}
	cl, err := clr.GetOrCreateClient(pup)
	if err != nil {
		t.Error(err)
	}
	pup.Client = cl
	ntw, err := NewNetwork(&log.Logger, "prjA", &common.NetworkConfig{
		Failsafe: fsCfg,
	}, rlr)
	if err != nil {
		t.Error(err)
	}
	ntw.Upstreams = []*upstream.Upstream{pup}
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

	if jrr.Result.(map[string]interface{})["hash"] == "" {
		t.Errorf("Expected hash to exist, got %v", jrr)
	}
}

func TestNetwork_ForwardTimeoutPolicyFail(t *testing.T) {
	defer gock.Clean()

	defer gock.DisableNetworking()
	defer gock.DisableNetworkingFilters()

	gock.EnableNetworking()

	// Register a networking filter
	gock.NetworkingFilter(func(req *http.Request) bool {
		shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "localhost"
		return shouldMakeRealCall
	})

	var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1273c18",false]}`)

	gock.New("http://google.com").
		Post("").
		Reply(200).
		Delay(100 * time.Millisecond).
		JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clr := upstream.NewClientRegistry()
	fsCfg := &common.FailsafeConfig{
		Timeout: &common.TimeoutPolicyConfig{
			Duration: "30ms",
		},
	}
	rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{},
	})
	if err != nil {
		t.Error(err)
	}
	vndr := vendors.NewVendorsRegistry()
	upr := upstream.NewUpstreamsRegistry(&log.Logger, &common.Config{}, rlr, vndr)
	defer upr.Shutdown()
	pup, err := upr.NewUpstream("prjA", &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "test",
		Endpoint: "http://google.com",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}, &log.Logger)
	if err != nil {
		t.Error(err)
	}
	cl, err := clr.GetOrCreateClient(pup)
	if err != nil {
		t.Error(err)
	}
	pup.Client = cl
	ntw, err := NewNetwork(&log.Logger, "prjA", &common.NetworkConfig{
		Failsafe: fsCfg,
	}, rlr)
	if err != nil {
		t.Error(err)
	}
	ntw.Upstreams = []*upstream.Upstream{pup}

	fakeReq := upstream.NewNormalizedRequest(requestBytes)
	_, err = ntw.Forward(ctx, fakeReq)

	if len(gock.Pending()) > 0 {
		t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
		for _, pending := range gock.Pending() {
			t.Errorf("Pending mock: %v", pending)
		}
	}

	if err == nil {
		t.Errorf("Expected error, got %v", err)
	}

	var e *common.ErrFailsafeTimeoutExceeded
	if !errors.As(err, &e) {
		t.Errorf("Expected %v, got %v", "ErrFailsafeTimeoutExceeded", err)
	} else {
		t.Logf("Got expected error: %v", err)
	}
}

func TestNetwork_ForwardTimeoutPolicyPass(t *testing.T) {
	defer gock.Clean()
	defer gock.DisableNetworking()
	defer gock.DisableNetworkingFilters()

	gock.EnableNetworking()

	// Register a networking filter
	gock.NetworkingFilter(func(req *http.Request) bool {
		shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "localhost"
		return shouldMakeRealCall
	})

	var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1273c18",false]}`)

	gock.New("http://google.com").
		Post("").
		Reply(200).
		Delay(100 * time.Millisecond).
		JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clr := upstream.NewClientRegistry()
	fsCfg := &common.FailsafeConfig{
		Timeout: &common.TimeoutPolicyConfig{
			Duration: "1s",
		},
	}
	rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{},
	})
	if err != nil {
		t.Error(err)
	}
	upr := upstream.NewUpstreamsRegistry(&log.Logger, &common.Config{}, rlr, vendors.NewVendorsRegistry())
	defer upr.Shutdown()
	pup, err := upr.NewUpstream("prjA", &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "test",
		Endpoint: "http://google.com",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}, &log.Logger)
	if err != nil {
		t.Error(err)
	}
	cl, err := clr.GetOrCreateClient(pup)
	if err != nil {
		t.Error(err)
	}
	pup.Client = cl

	ntw, err := NewNetwork(&log.Logger, "prjA", &common.NetworkConfig{
		Failsafe: fsCfg,
	}, rlr)
	if err != nil {
		t.Error(err)
	}
	ntw.Upstreams = []*upstream.Upstream{pup}

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
}

func TestNetwork_ForwardHedgePolicyTriggered(t *testing.T) {
	defer gock.Clean()
	defer gock.DisableNetworking()
	defer gock.DisableNetworkingFilters()

	gock.EnableNetworking()

	// Register a networking filter
	gock.NetworkingFilter(func(req *http.Request) bool {
		shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "localhost"
		return shouldMakeRealCall
	})

	var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1273c18",false]}`)

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

	clr := upstream.NewClientRegistry()
	fsCfg := &common.FailsafeConfig{
		Hedge: &common.HedgePolicyConfig{
			Delay:    "200ms",
			MaxCount: 1,
		},
	}
	rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{},
	})
	if err != nil {
		t.Error(err)
	}
	upr := upstream.NewUpstreamsRegistry(&log.Logger, &common.Config{}, rlr, vendors.NewVendorsRegistry())
	defer upr.Shutdown()
	pup1, err := upr.NewUpstream("prjA", &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "rpc1",
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}, &log.Logger)
	if err != nil {
		t.Error(err)
	}
	cl1, err := clr.GetOrCreateClient(pup1)
	if err != nil {
		t.Error(err)
	}
	pup1.Client = cl1

	pup2, err := upr.NewUpstream("prjA", &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "rpc2",
		Endpoint: "http://rpc2.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}, &log.Logger)
	if err != nil {
		t.Error(err)
	}
	cl2, err := clr.GetOrCreateClient(pup2)
	if err != nil {
		t.Error(err)
	}
	pup2.Client = cl2

	ntw, err := NewNetwork(&log.Logger, "prjA", &common.NetworkConfig{
		Failsafe: fsCfg,
	}, rlr)
	if err != nil {
		t.Error(err)
	}
	ntw.Upstreams = []*upstream.Upstream{pup1, pup2}

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
		return
	}

	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		t.Errorf("Expected nil error, got %v", err)
		return
	}

	if jrr.Result == nil {
		t.Errorf("Expected result, got nil")
		return
	}

	// Type assertion with check
	result, ok := jrr.Result.(map[string]interface{})
	if !ok {
		t.Errorf("Expected Result to be map[string]interface{}, got %T", jrr.Result)
		return
	}

	fromHost, ok := result["fromHost"].(string)
	if !ok {
		t.Errorf("Expected fromHost to be string, got %T", result["fromHost"])
		return
	}

	if fromHost != "rpc2" {
		t.Errorf("Expected fromHost to be %v, got %v", "rpc2", fromHost)
	}
}

func TestNetwork_ForwardHedgePolicyNotTriggered(t *testing.T) {
	defer gock.Clean()

	var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1273c18",false]}`)

	gock.New("http://google.com").
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

	clr := upstream.NewClientRegistry()
	fsCfg := &common.FailsafeConfig{
		Hedge: &common.HedgePolicyConfig{
			Delay:    "100ms",
			MaxCount: 5,
		},
	}
	rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{},
	})
	if err != nil {
		t.Error(err)
	}
	upr := upstream.NewUpstreamsRegistry(&log.Logger, &common.Config{}, rlr, vendors.NewVendorsRegistry())
	defer upr.Shutdown()
	pup1, err := upr.NewUpstream("prjA", &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "rpc1",
		Endpoint: "http://google.com",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}, &log.Logger)
	if err != nil {
		t.Error(err)
	}
	cl1, err := clr.GetOrCreateClient(pup1)
	if err != nil {
		t.Error(err)
	}
	pup1.Client = cl1
	pup2, err := upr.NewUpstream("prjA", &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "rpc2",
		Endpoint: "http://alchemy.com",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}, &log.Logger)
	if err != nil {
		t.Error(err)
	}
	cl2, err := clr.GetOrCreateClient(pup2)
	if err != nil {
		t.Error(err)
	}
	pup2.Client = cl2

	ntw, err := NewNetwork(&log.Logger, "prjA", &common.NetworkConfig{
		Failsafe: fsCfg,
	}, rlr)
	if err != nil {
		t.Error(err)
	}
	ntw.Upstreams = []*upstream.Upstream{pup1, pup2}

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
		t.Errorf("Expected nil error, got %v", err)
	}

	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		t.Errorf("Expected nil error, got %v", err)
	}
	if jrr.Result == nil {
		t.Errorf("Expected result, got %v", jrr)
	}
	if jrr.Result.(map[string]interface{})["fromHost"] != "rpc2" {
		t.Errorf("Expected fromHost to be %v, got %v", "rpc2", jrr.Result.(map[string]interface{})["fromHost"])
	}
}

func TestNetwork_ForwardHedgePolicyIgnoresNegativeScoreUpstream(t *testing.T) {
	defer gock.Clean()
	var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1273c18",false]}`)

	log.Logger.Info().Msgf("Mocks registered before: %d", len(gock.Pending()))
	gock.New("http://google.com").
		Post("").
		Times(3).
		Reply(200).
		JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9","fromHost":"rpc1"}}`)).
		Delay(100 * time.Millisecond)
	log.Logger.Info().Msgf("Mocks registered after: %d", len(gock.Pending()))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clr := upstream.NewClientRegistry()
	fsCfg := &common.FailsafeConfig{
		Hedge: &common.HedgePolicyConfig{
			Delay:    "30ms",
			MaxCount: 2,
		},
	}
	rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{},
	})
	if err != nil {
		t.Error(err)
	}
	upr := upstream.NewUpstreamsRegistry(&log.Logger, &common.Config{}, rlr, vendors.NewVendorsRegistry())
	defer upr.Shutdown()

	// policies, err := upstream.CreateFailSafePolicies("eth_getBlockByNumber", fsCfg)
	// if err != nil {
	// 	t.Error(err)
	// }
	// pup1 := &upstream.Upstream{
	// 	Logger:       log.Logger,
	// 	Id:           "rpc1",
	// 	Endpoint:     "http://google.com",
	// 	Architecture: common.ArchitectureEvm,
	// 	Metadata: map[string]string{
	// 		"evmChainId": "123",
	// 	},
	// 	NetworkIds: []string{"123"},
	// 	Score:      2,
	// }
	pup1, err := upr.NewUpstream("prjA", &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "rpc1",
		Endpoint: "http://google.com",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}, &log.Logger)
	if err != nil {
		t.Error(err)
	}
	cl1, err := clr.CreateClient(pup1)
	if err != nil {
		t.Error(err)
	}
	pup1.Score = 2
	pup1.Client = cl1

	pup2, err := upr.NewUpstream("prjA", &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "rpc2",
		Endpoint: "http://alchemy.com",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}, &log.Logger)
	if err != nil {
		t.Error(err)
	}
	cl2, err := clr.CreateClient(pup2)
	if err != nil {
		t.Error(err)
	}
	pup2.Client = cl2
	pup2.Score = -2

	ntw, err := NewNetwork(&log.Logger, "prjA", &common.NetworkConfig{
		Failsafe: fsCfg,
	}, rlr)
	if err != nil {
		t.Error(err)
	}
	ntw.Upstreams = []*upstream.Upstream{pup1, pup2}

	fakeReq := upstream.NewNormalizedRequest(requestBytes)
	resp, err := ntw.Forward(ctx, fakeReq)

	time.Sleep(50 * time.Millisecond)

	if len(gock.Pending()) > 0 {
		t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
		for _, pending := range gock.Pending() {
			t.Errorf("Pending mock: %v", pending)
		}
	} else {
		t.Logf("All mocks consumed")
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
	if jrr.Result.(map[string]interface{})["fromHost"] != "rpc1" {
		t.Errorf("Expected fromHost to be %v, got %v", "rpc1", jrr.Result.(map[string]interface{})["fromHost"])
	}
}

func TestNetwork_ForwardCBOpensAfterConstantFailure(t *testing.T) {
	defer gock.Clean()
	defer gock.DisableNetworking()
	defer gock.DisableNetworkingFilters()

	gock.EnableNetworking()

	// Register a networking filter
	gock.NetworkingFilter(func(req *http.Request) bool {
		shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "localhost"
		return shouldMakeRealCall
	})

	var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1273c18",false]}`)

	gock.New("http://google.com").
		Post("").
		Times(2).
		Reply(200).
		JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

	gock.New("http://google.com").
		Post("").
		Times(2).
		Reply(503).
		JSON(json.RawMessage(`{"error":{"message":"some random provider issue"}}`))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clr := upstream.NewClientRegistry()
	fsCfg := &common.FailsafeConfig{
		CircuitBreaker: &common.CircuitBreakerPolicyConfig{
			FailureThresholdCount:    2,
			FailureThresholdCapacity: 4,
			HalfOpenAfter:            "2s",
		},
	}

	rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{},
	})
	if err != nil {
		t.Error(err)
	}
	upr := upstream.NewUpstreamsRegistry(&log.Logger, &common.Config{}, rlr, vendors.NewVendorsRegistry())
	defer upr.Shutdown()
	pup1, err := upr.NewUpstream("test_cb", &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "upstream1",
		Endpoint: "http://google.com",
		Failsafe: fsCfg,
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}, &log.Logger)
	if err != nil {
		t.Error(err)
	}
	cl, err := clr.GetOrCreateClient(pup1)
	if err != nil {
		t.Error(err)
	}
	pup1.Client = cl
	ntw, err := NewNetwork(&log.Logger, "test_cb", &common.NetworkConfig{
		Failsafe: fsCfg,
	}, rlr)
	if err != nil {
		t.Error(err)
	}
	ntw.Upstreams = []*upstream.Upstream{pup1}

	for i := 0; i < 10; i++ {
		fakeReq := upstream.NewNormalizedRequest(requestBytes)
		_, err = ntw.Forward(ctx, fakeReq)
	}

	if err == nil {
		t.Errorf("Expected an error, got nil")
	}

	var e *common.ErrFailsafeCircuitBreakerOpen
	if !errors.As(err, &e) {
		t.Errorf("Expected %v, got %v", "ErrFailsafeCircuitBreakerOpen", err)
	}

	if len(gock.Pending()) > 0 {
		t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
		for _, pending := range gock.Pending() {
			t.Errorf("Pending mock: %v", pending)
		}
	} else {
		t.Logf("All mocks consumed")
	}
}

func TestNetwork_ForwardCBClosesAfterUpstreamIsBackUp(t *testing.T) {
	defer gock.Clean()
	defer gock.DisableNetworking()
	defer gock.DisableNetworkingFilters()

	gock.EnableNetworking()

	// Register a networking filter
	gock.NetworkingFilter(func(req *http.Request) bool {
		shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "localhost"
		return shouldMakeRealCall
	})

	var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1273c18",false]}`)

	gock.New("http://google.com").
		Post("").
		Times(3).
		Reply(200).
		JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

	gock.New("http://google.com").
		Post("").
		Times(3).
		Reply(503).
		JSON(json.RawMessage(`{"error":{"message":"some random provider issue"}}`))

	gock.New("http://google.com").
		Post("").
		Times(3).
		Reply(200).
		JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clr := upstream.NewClientRegistry()
	fsCfg := &common.FailsafeConfig{
		CircuitBreaker: &common.CircuitBreakerPolicyConfig{
			FailureThresholdCount:    2,
			FailureThresholdCapacity: 4,
			HalfOpenAfter:            "2s",
		},
	}
	rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{},
	})
	if err != nil {
		t.Error(err)
	}
	upr := upstream.NewUpstreamsRegistry(&log.Logger, &common.Config{}, rlr, vendors.NewVendorsRegistry())
	defer upr.Shutdown()
	pup1, err := upr.NewUpstream("test_cb", &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "upstream1",
		Endpoint: "http://google.com",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}, &log.Logger)
	if err != nil {
		t.Error(err)
	}
	cl, err := clr.GetOrCreateClient(pup1)
	if err != nil {
		t.Error(err)
	}
	pup1.Client = cl

	ntw, err := NewNetwork(&log.Logger, "test_cb", &common.NetworkConfig{
		Failsafe: fsCfg,
	}, rlr)
	if err != nil {
		t.Error(err)
	}
	ntw.Upstreams = []*upstream.Upstream{pup1}

	for i := 0; i < 4+2; i++ {
		fakeReq := upstream.NewNormalizedRequest(requestBytes)
		_, err = ntw.Forward(ctx, fakeReq)
	}

	if err == nil {
		t.Errorf("Expected an error, got nil")
	}

	time.Sleep(2 * time.Second)

	var resp common.NormalizedResponse
	for i := 0; i < 3; i++ {
		fakeReq := upstream.NewNormalizedRequest(requestBytes)
		resp, err = ntw.Forward(ctx, fakeReq)
	}

	if err != nil {
		t.Errorf("Expected nil error, got %v", err)
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
		t.Errorf("Expected nil error, got %v", err)
	}
	if jrr.Result.(map[string]interface{})["hash"] == "" {
		t.Errorf("Expected hash to exist, got %v", jrr)
	}
}

func TestNetwork_WeightedRandomSelect(t *testing.T) {
	// Test cases
	tests := []struct {
		name      string
		upstreams []*upstream.Upstream
		expected  map[string]int // To track selection counts for each upstream ID
	}{
		{
			name: "Basic scenario with distinct weights",
			upstreams: []*upstream.Upstream{
				{ProjectId: "upstream1", Score: 1},
				{ProjectId: "upstream2", Score: 2},
				{ProjectId: "upstream3", Score: 3},
			},
			expected: map[string]int{"upstream1": 0, "upstream2": 0, "upstream3": 0},
		},
		{
			name: "All upstreams have the same score",
			upstreams: []*upstream.Upstream{
				{ProjectId: "upstream1", Score: 1},
				{ProjectId: "upstream2", Score: 1},
				{ProjectId: "upstream3", Score: 1},
			},
			expected: map[string]int{"upstream1": 0, "upstream2": 0, "upstream3": 0},
		},
		{
			name: "Single upstream",
			upstreams: []*upstream.Upstream{
				{ProjectId: "upstream1", Score: 1},
			},
			expected: map[string]int{"upstream1": 0},
		},
		{
			name: "Upstreams with zero score",
			upstreams: []*upstream.Upstream{
				{ProjectId: "upstream1", Score: 0},
				{ProjectId: "upstream2", Score: 0},
				{ProjectId: "upstream3", Score: 1},
			},
			expected: map[string]int{"upstream1": 0, "upstream2": 0, "upstream3": 0},
		},
	}

	// Number of iterations to perform for weighted selection
	const iterations = 10000

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test WeightedRandomSelect
			for i := 0; i < iterations; i++ {
				selected := weightedRandomSelect(tt.upstreams)
				tt.expected[selected.ProjectId]++
			}

			// Check if the distribution matches the expected ratios
			totalScore := 0
			for _, upstream := range tt.upstreams {
				totalScore += upstream.Score
			}

			for _, upstream := range tt.upstreams {
				if upstream.Score > 0 {
					expectedCount := (upstream.Score * iterations) / totalScore
					actualCount := tt.expected[upstream.ProjectId]
					if actualCount < expectedCount*9/10 || actualCount > expectedCount*11/10 {
						t.Errorf("upstream %s selected %d times, expected approximately %d times", upstream.ProjectId, actualCount, expectedCount)
					}
				}
			}
		})
	}
}

func TestNetwork_ForwardEndpointServerSideExceptionSuccess(t *testing.T) {
	defer gock.Clean()
	defer gock.DisableNetworking()
	defer gock.DisableNetworkingFilters()

	gock.EnableNetworking()

	// Register a networking filter
	gock.NetworkingFilter(func(req *http.Request) bool {
		shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "localhost"
		return shouldMakeRealCall
	})

	var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1273c18",false]}`)

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

	clr := upstream.NewClientRegistry()
	fsCfg := &common.FailsafeConfig{
		Retry: &common.RetryPolicyConfig{
			MaxAttempts: 2,
		},
	}
	rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{},
	})
	if err != nil {
		t.Error(err)
	}

	upr := upstream.NewUpstreamsRegistry(&log.Logger, &common.Config{}, rlr, vendors.NewVendorsRegistry())
	defer upr.Shutdown()
	pup1, err := upr.NewUpstream("prjA", &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "rpc1",
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}, &log.Logger)
	if err != nil {
		t.Error(err)
	}
	cl1, err := clr.GetOrCreateClient(pup1)
	if err != nil {
		t.Error(err)
	}
	pup1.Client = cl1

	pup2, err := upr.NewUpstream("prjA", &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "rpc2",
		Endpoint: "http://rpc2.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}, &log.Logger)
	if err != nil {
		t.Error(err)
	}
	cl2, err := clr.GetOrCreateClient(pup2)
	if err != nil {
		t.Error(err)
	}
	pup2.Client = cl2

	ntw, err := NewNetwork(&log.Logger, "prjA", &common.NetworkConfig{
		Failsafe: fsCfg,
	}, rlr)
	if err != nil {
		t.Error(err)
	}
	ntw.Upstreams = []*upstream.Upstream{pup1, pup2}

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
		return
	}

	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		t.Errorf("Expected nil error, got %v", err)
		return
	}

	if jrr.Result == nil {
		t.Errorf("Expected result, got nil")
		return
	}

	// Debug logging
	t.Logf("jrr.Result type: %T", jrr.Result)
	t.Logf("jrr.Result content: %+v", jrr.Result)

	// Type assertion with check
	result, ok := jrr.Result.(map[string]interface{})
	if !ok {
		t.Errorf("Expected Result to be map[string]interface{}, got %T", jrr.Result)
		return
	}

	fromHost, ok := result["fromHost"].(string)
	if !ok {
		t.Errorf("Expected fromHost to be string, got %T", result["fromHost"])
		return
	}

	if fromHost != "rpc2" {
		t.Errorf("Expected fromHost to be %v, got %v", "rpc2", fromHost)
	}
}

func TestNetwork_ForwardEthGetLogsEmptyArrayResponseSuccess(t *testing.T) {
	defer gock.Clean()
	defer gock.DisableNetworking()
	defer gock.DisableNetworkingFilters()

	gock.EnableNetworking()

	// Register a networking filter
	gock.NetworkingFilter(func(req *http.Request) bool {
		shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "localhost"
		return shouldMakeRealCall
	})

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

	// Setup test network
	clr := upstream.NewClientRegistry()
	fsCfg := &common.FailsafeConfig{
		Retry: &common.RetryPolicyConfig{
			MaxAttempts: 2,
		},
	}
	rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{},
	})
	if err != nil {
		t.Fatalf("Failed to create rate limiters registry: %v", err)
	}
	upr := upstream.NewUpstreamsRegistry(&log.Logger, &common.Config{}, rlr, vendors.NewVendorsRegistry())
	defer upr.Shutdown()
	pup1, err := upr.NewUpstream("prjA", &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "rpc1",
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}, &log.Logger)
	if err != nil {
		t.Fatalf("Failed to create upstream 1: %v", err)
	}
	cl1, err := clr.GetOrCreateClient(pup1)
	if err != nil {
		t.Fatalf("Failed to get or create client for upstream 1: %v", err)
	}
	pup1.Client = cl1

	pup2, err := upr.NewUpstream("prjA", &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "rpc2",
		Endpoint: "http://rpc2.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}, &log.Logger)
	if err != nil {
		t.Fatalf("Failed to create upstream 2: %v", err)
	}
	cl2, err := clr.GetOrCreateClient(pup2)
	if err != nil {
		t.Fatalf("Failed to get or create client for upstream 2: %v", err)
	}
	pup2.Client = cl2

	ntw, err := NewNetwork(&log.Logger, "prjA", &common.NetworkConfig{
		Failsafe: fsCfg,
	}, rlr)
	if err != nil {
		t.Fatalf("Failed to create network: %v", err)
	}
	ntw.Upstreams = []*upstream.Upstream{pup1, pup2}

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

	fromHost, ok := result[0].(map[string]interface{})["fromHost"].(string)
	if !ok {
		t.Fatalf("Expected fromHost to be string, got %T", result[0].(map[string]interface{})["fromHost"])
	}

	if fromHost != "rpc2" {
		t.Errorf("Expected fromHost to be %q, got %q", "rpc2", fromHost)
	}
}

func TestNetwork_ForwardEthGetLogsBothEmptyArrayResponse(t *testing.T) {
	defer gock.Clean()
	defer gock.DisableNetworking()
	defer gock.DisableNetworkingFilters()

	gock.EnableNetworking()

	gock.NetworkingFilter(func(req *http.Request) bool {
		return strings.Split(req.URL.Host, ":")[0] == "localhost"
	})

	var requestBytes = json.RawMessage(`{"jsonrpc": "2.0","method": "eth_getLogs","params":[{"address":"0x1234567890abcdef1234567890abcdef12345678"}],"id": 1}`)

	emptyResponse := json.RawMessage(`{"jsonrpc": "2.0","id": 1,"result":[]}`)

	gock.New("http://rpc1.localhost").
		Post("").
		Reply(200).
		JSON(emptyResponse).
		Delay(500 * time.Millisecond)

	gock.New("http://rpc2.localhost").
		Post("").
		Reply(200).
		JSON(emptyResponse).
		Delay(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	// Setup test network
	clr := upstream.NewClientRegistry()
	fsCfg := &common.FailsafeConfig{
		Hedge: &common.HedgePolicyConfig{
			Delay:    "200ms",
			MaxCount: 1,
		},
	}
	rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{},
	})
	if err != nil {
		t.Fatalf("Failed to create rate limiters registry: %v", err)
	}
	upr := upstream.NewUpstreamsRegistry(&log.Logger, &common.Config{}, rlr, vendors.NewVendorsRegistry())
	defer upr.Shutdown()
	pup1, err := upr.NewUpstream("prjA", &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "rpc1",
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}, &log.Logger)
	if err != nil {
		t.Fatalf("Failed to create upstream 1: %v", err)
	}
	cl1, err := clr.GetOrCreateClient(pup1)
	if err != nil {
		t.Fatalf("Failed to get or create client for upstream 1: %v", err)
	}
	pup1.Client = cl1

	pup2, err := upr.NewUpstream("prjA", &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "rpc2",
		Endpoint: "http://rpc2.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}, &log.Logger)
	if err != nil {
		t.Fatalf("Failed to create upstream 2: %v", err)
	}
	cl2, err := clr.GetOrCreateClient(pup2)
	if err != nil {
		t.Fatalf("Failed to get or create client for upstream 2: %v", err)
	}
	pup2.Client = cl2

	ntw, err := NewNetwork(&log.Logger, "prjA", &common.NetworkConfig{
		Failsafe: fsCfg,
	}, rlr)
	if err != nil {
		t.Fatalf("Failed to create network: %v", err)
	}
	ntw.Upstreams = []*upstream.Upstream{pup1, pup2}

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
}
