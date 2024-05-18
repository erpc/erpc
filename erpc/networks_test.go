package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/resiliency"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
)

func TestPreparedNetwork_ForwardCorrectlyRateLimitedOnNetworkLevel(t *testing.T) {
	rateLimitersRegistry, err := resiliency.NewRateLimitersRegistry(
		&config.RateLimiterConfig{
			Buckets: []*config.RateLimitBucketConfig{
				{
					Id: "MyLimiterBucket_Test1",
					Rules: []*config.RateLimitRuleConfig{
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

	ntw := &PreparedNetwork{
		ProjectId: "test",
		NetworkId: "123",
		Config: &config.NetworkConfig{
			RateLimitBucket: "MyLimiterBucket_Test1",
		},
		rateLimitersRegistry: rateLimitersRegistry,
	}
	ctx := context.Background()

	var lastErr error
	var lastFakeRespWriter *httptest.ResponseRecorder

	for i := 0; i < 5; i++ {
		lastFakeRespWriter = &httptest.ResponseRecorder{}
		fakeReq := upstream.NewNormalizedRequest([]byte(`{"method": "eth_chainId","params":[]}`))
		lastErr = ntw.Forward(ctx, fakeReq, lastFakeRespWriter)
	}

	var e *common.ErrNetworkRateLimitRuleExceeded
	if lastErr == nil || !errors.As(lastErr, &e) {
		t.Errorf("Expected %v, got %v", "ErrNetworkRateLimitRuleExceeded", lastErr)
	}
}

func TestPreparedNetwork_ForwardNotRateLimitedOnNetworkLevel(t *testing.T) {
	rateLimitersRegistry, err := resiliency.NewRateLimitersRegistry(
		&config.RateLimiterConfig{
			Buckets: []*config.RateLimitBucketConfig{
				{
					Id: "MyLimiterBucket_Test2",
					Rules: []*config.RateLimitRuleConfig{
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

	ntw := &PreparedNetwork{
		ProjectId: "test",
		NetworkId: "123",
		Config: &config.NetworkConfig{
			RateLimitBucket: "MyLimiterBucket_Test2",
		},
		rateLimitersRegistry: rateLimitersRegistry,
	}
	ctx := context.Background()

	var lastErr error
	var lastFakeRespWriter *httptest.ResponseRecorder

	for i := 0; i < 10; i++ {
		lastFakeRespWriter = &httptest.ResponseRecorder{}
		fakeReq := upstream.NewNormalizedRequest([]byte(`{"method": "eth_chainId","params":[]}`))
		lastErr = ntw.Forward(ctx, fakeReq, lastFakeRespWriter)
	}

	var e *common.ErrNetworkRateLimitRuleExceeded
	if lastErr != nil && errors.As(lastErr, &e) {
		t.Errorf("Did not expect ErrNetworkRateLimitRuleExceeded")
	}
}

func TestPreparedNetwork_ForwardRetryFailuresWithoutSuccess(t *testing.T) {
	defer gock.Disable()
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
		// MatchType("json").
		// JSON(requestBytes).
		Reply(503).
		JSON(json.RawMessage(`{"error":{"message":"some random provider issue"}}`))

	ctx := context.Background()

	clr := upstream.NewClientRegistry()

	fsCfg := &config.FailsafeConfig{
		Retry: &config.RetryPolicyConfig{
			MaxAttempts: 3,
		},
	}
	policies, err := resiliency.CreateFailSafePolicies("eth_getBlockByNumber", fsCfg)
	if err != nil {
		t.Error(err)
	}
	pup := &upstream.PreparedUpstream{
		Logger:       log.Logger,
		Id:           "test",
		Endpoint:     "http://google.com",
		Architecture: upstream.ArchitectureEvm,
		Metadata: map[string]string{
			"evmChainId": "123",
		},
		NetworkIds: []string{"123"},
	}
	cl, err := clr.GetOrCreateClient(pup)
	if err != nil {
		t.Error(err)
	}
	pup.Client = cl
	ntw := &PreparedNetwork{
		Logger:    log.Logger,
		NetworkId: "123",
		Config: &config.NetworkConfig{
			Failsafe: fsCfg,
		},
		FailsafePolicies: policies,
		Upstreams:        []*upstream.PreparedUpstream{pup},
		failsafeExecutor: failsafe.NewExecutor(policies...),
	}

	respWriter := &httptest.ResponseRecorder{}
	fakeReq := upstream.NewNormalizedRequest(requestBytes)
	err = ntw.Forward(ctx, fakeReq, respWriter)

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

func TestPreparedNetwork_ForwardRetryFailuresWithSuccess(t *testing.T) {
	defer gock.Disable()
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

	ctx := context.Background()
	clr := upstream.NewClientRegistry()
	fsCfg := &config.FailsafeConfig{
		Retry: &config.RetryPolicyConfig{
			MaxAttempts: 4,
		},
	}
	policies, err := resiliency.CreateFailSafePolicies("eth_getBlockByNumber", fsCfg)
	if err != nil {
		t.Error(err)
	}
	pup := &upstream.PreparedUpstream{
		Logger:       log.Logger,
		Id:           "test",
		Endpoint:     "http://google.com",
		Architecture: upstream.ArchitectureEvm,
		Metadata: map[string]string{
			"evmChainId": "123",
		},
		NetworkIds: []string{"123"},
	}
	cl, err := clr.GetOrCreateClient(pup)
	if err != nil {
		t.Error(err)
	}
	pup.Client = cl
	ntw := &PreparedNetwork{
		Logger:    log.Logger,
		NetworkId: "123",
		Config: &config.NetworkConfig{
			Failsafe: fsCfg,
		},
		FailsafePolicies: policies,
		Upstreams:        []*upstream.PreparedUpstream{pup},
		failsafeExecutor: failsafe.NewExecutor(policies...),
	}

	respWriter := httptest.NewRecorder()
	fakeReq := upstream.NewNormalizedRequest(requestBytes)
	err = ntw.Forward(ctx, fakeReq, respWriter)

	if len(gock.Pending()) > 0 {
		t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
		for _, pending := range gock.Pending() {
			t.Errorf("Pending mock: %v", pending)
		}
	}

	resp := respWriter.Result()

	if err != nil {
		t.Errorf("Expected nil error, got %v", err)
	}

	if resp.StatusCode != 200 {
		t.Errorf("Expected status 200, got %v", resp.StatusCode)
	}

	var respObject map[string]interface{}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("error reading response: %s", err)
	}
	err = json.Unmarshal(body, &respObject)
	if err != nil {
		t.Fatalf("error unmarshalling: %s response body: %s", err, body)
	}
	if respObject["hash"] != "0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9" {
		t.Errorf("Expected %v, got %v", "0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9", respObject["hash"])
	}
}

func TestPreparedNetwork_ForwardTimeoutPolicyFail(t *testing.T) {
	defer gock.Disable()
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

	ctx := context.Background()
	clr := upstream.NewClientRegistry()
	fsCfg := &config.FailsafeConfig{
		Timeout: &config.TimeoutPolicyConfig{
			Duration: "30ms",
		},
	}
	policies, err := resiliency.CreateFailSafePolicies("eth_getBlockByNumber", fsCfg)
	if err != nil {
		t.Error(err)
	}
	pup := &upstream.PreparedUpstream{
		Logger:       log.Logger,
		Id:           "test",
		Endpoint:     "http://google.com",
		Architecture: upstream.ArchitectureEvm,
		Metadata: map[string]string{
			"evmChainId": "123",
		},
		NetworkIds: []string{"123"},
	}
	cl, err := clr.GetOrCreateClient(pup)
	if err != nil {
		t.Error(err)
	}
	pup.Client = cl
	ntw := &PreparedNetwork{
		Logger:    log.Logger,
		NetworkId: "123",
		Config: &config.NetworkConfig{
			Failsafe: fsCfg,
		},
		FailsafePolicies: policies,
		Upstreams:        []*upstream.PreparedUpstream{pup},
		failsafeExecutor: failsafe.NewExecutor(policies...),
	}

	respWriter := httptest.NewRecorder()
	fakeReq := upstream.NewNormalizedRequest(requestBytes)
	err = ntw.Forward(ctx, fakeReq, respWriter)

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

func TestPreparedNetwork_ForwardTimeoutPolicyPass(t *testing.T) {
	defer gock.Disable()
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

	ctx := context.Background()
	clr := upstream.NewClientRegistry()
	fsCfg := &config.FailsafeConfig{
		Timeout: &config.TimeoutPolicyConfig{
			Duration: "1s",
		},
	}
	policies, err := resiliency.CreateFailSafePolicies("eth_getBlockByNumber", fsCfg)
	if err != nil {
		t.Error(err)
	}
	pup := &upstream.PreparedUpstream{
		Logger:       log.Logger,
		Id:           "test",
		Endpoint:     "http://google.com",
		Architecture: upstream.ArchitectureEvm,
		Metadata: map[string]string{
			"evmChainId": "123",
		},
		NetworkIds: []string{"123"},
	}
	cl, err := clr.GetOrCreateClient(pup)
	if err != nil {
		t.Error(err)
	}
	pup.Client = cl
	ntw := &PreparedNetwork{
		Logger:    log.Logger,
		NetworkId: "123",
		Config: &config.NetworkConfig{
			Failsafe: fsCfg,
		},
		FailsafePolicies: policies,
		Upstreams:        []*upstream.PreparedUpstream{pup},
		failsafeExecutor: failsafe.NewExecutor(policies...),
	}

	respWriter := httptest.NewRecorder()
	fakeReq := upstream.NewNormalizedRequest(requestBytes)
	err = ntw.Forward(ctx, fakeReq, respWriter)

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

func TestPreparedNetwork_ForwardHedgePolicyTriggered(t *testing.T) {
	defer gock.Disable()
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

	ctx := context.Background()
	clr := upstream.NewClientRegistry()
	fsCfg := &config.FailsafeConfig{
		Hedge: &config.HedgePolicyConfig{
			Delay:    "200ms",
			MaxCount: 1,
		},
	}
	policies, err := resiliency.CreateFailSafePolicies("eth_getBlockByNumber", fsCfg)
	if err != nil {
		t.Error(err)
	}
	pup1 := &upstream.PreparedUpstream{
		Logger:       log.Logger,
		Id:           "rpc1",
		Endpoint:     "http://rpc1.localhost",
		Architecture: upstream.ArchitectureEvm,
		Metadata: map[string]string{
			"evmChainId": "123",
		},
		NetworkIds: []string{"123"},
	}
	cl1, err := clr.GetOrCreateClient(pup1)
	if err != nil {
		t.Error(err)
	}
	pup1.Client = cl1
	pup2 := &upstream.PreparedUpstream{
		Logger:       log.Logger,
		Id:           "rpc2",
		Endpoint:     "http://rpc2.localhost",
		Architecture: upstream.ArchitectureEvm,
		Metadata: map[string]string{
			"evmChainId": "123",
		},
		NetworkIds: []string{"123"},
	}
	cl2, err := clr.GetOrCreateClient(pup2)
	if err != nil {
		t.Error(err)
	}
	pup2.Client = cl2

	ntw := &PreparedNetwork{
		Logger:    log.Logger,
		NetworkId: "123",
		Config: &config.NetworkConfig{
			Failsafe: fsCfg,
		},
		FailsafePolicies: policies,
		Upstreams:        []*upstream.PreparedUpstream{pup1, pup2},
		failsafeExecutor: failsafe.NewExecutor(policies...),
	}

	respWriter := httptest.NewRecorder()
	fakeReq := upstream.NewNormalizedRequest(requestBytes)
	err = ntw.Forward(ctx, fakeReq, respWriter)

	if len(gock.Pending()) > 0 {
		t.Errorf("Expected all mocks to be consumed, got %v left", len(gock.Pending()))
		for _, pending := range gock.Pending() {
			t.Errorf("Pending mock: %v", pending)
		}
	}

	if err != nil {
		t.Errorf("Expected nil error, got %v", err)
	}

	resp := respWriter.Result()

	if resp.StatusCode != 200 {
		t.Errorf("Expected status 200, got %v", resp.StatusCode)
	}

	var respObject map[string]interface{}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("error reading response: %s", err)
	}
	err = json.Unmarshal(body, &respObject)
	if err != nil {
		t.Fatalf("error unmarshalling: %s response body: %s", err, body)
	}
	if respObject["fromHost"] != "rpc2" {
		t.Errorf("Expected fromHost to be %v, got %v", "rpc2", respObject["fromHost"])
	}
}

func TestPreparedNetwork_ForwardHedgePolicyNotTriggered(t *testing.T) {
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

	ctx := context.Background()
	clr := upstream.NewClientRegistry()
	fsCfg := &config.FailsafeConfig{
		Hedge: &config.HedgePolicyConfig{
			Delay:    "100ms",
			MaxCount: 5,
		},
	}
	policies, err := resiliency.CreateFailSafePolicies("eth_getBlockByNumber", fsCfg)
	if err != nil {
		t.Error(err)
	}
	pup1 := &upstream.PreparedUpstream{
		Logger:       log.Logger,
		Id:           "rpc1",
		Endpoint:     "http://google.com",
		Architecture: upstream.ArchitectureEvm,
		Metadata: map[string]string{
			"evmChainId": "123",
		},
		NetworkIds: []string{"123"},
	}
	cl1, err := clr.GetOrCreateClient(pup1)
	if err != nil {
		t.Error(err)
	}
	pup1.Client = cl1
	pup2 := &upstream.PreparedUpstream{
		Logger:       log.Logger,
		Id:           "rpc2",
		Endpoint:     "http://alchemy.com",
		Architecture: upstream.ArchitectureEvm,
		Metadata: map[string]string{
			"evmChainId": "123",
		},
		NetworkIds: []string{"123"},
	}
	cl2, err := clr.GetOrCreateClient(pup2)
	if err != nil {
		t.Error(err)
	}
	pup2.Client = cl2

	ntw := &PreparedNetwork{
		Logger:    log.Logger,
		NetworkId: "123",
		Config: &config.NetworkConfig{
			Failsafe: fsCfg,
		},
		FailsafePolicies: policies,
		Upstreams:        []*upstream.PreparedUpstream{pup1, pup2},
		failsafeExecutor: failsafe.NewExecutor(policies...),
	}

	respWriter := httptest.NewRecorder()
	fakeReq := upstream.NewNormalizedRequest(requestBytes)
	err = ntw.Forward(ctx, fakeReq, respWriter)

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

	resp := respWriter.Result()

	if resp.StatusCode != 200 {
		t.Errorf("Expected status 200, got %v", resp.StatusCode)
	}

	var respObject map[string]interface{}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("error reading response: %s", err)
	}
	err = json.Unmarshal(body, &respObject)
	if err != nil {
		t.Fatalf("error unmarshalling: %s response body: %s", err, body)
	}
	if respObject["fromHost"] != "rpc2" {
		t.Errorf("Expected fromHost to be %v, got %v", "rpc2", respObject["fromHost"])
	}
}

func TestPreparedNetwork_ForwardHedgePolicyIgnoresNegativeScoreUpstream(t *testing.T) {
	var requestBytes = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1273c18",false]}`)

	gock.New("http://google.com").
		Post("").
		Times(3).
		Reply(200).
		JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9","fromHost":"rpc1"}}`)).
		Delay(300 * time.Millisecond)

	log.Logger.Info().Msgf("Mocks registered: %d", len(gock.Pending()))

	ctx := context.Background()
	clr := upstream.NewClientRegistry()
	fsCfg := &config.FailsafeConfig{
		Hedge: &config.HedgePolicyConfig{
			Delay:    "100ms",
			MaxCount: 3,
		},
	}
	policies, err := resiliency.CreateFailSafePolicies("eth_getBlockByNumber", fsCfg)
	if err != nil {
		t.Error(err)
	}
	pup1 := &upstream.PreparedUpstream{
		Logger:       log.Logger,
		Id:           "rpc1",
		Endpoint:     "http://google.com",
		Architecture: upstream.ArchitectureEvm,
		Metadata: map[string]string{
			"evmChainId": "123",
		},
		NetworkIds: []string{"123"},
	}
	cl1, err := clr.GetOrCreateClient(pup1)
	if err != nil {
		t.Error(err)
	}
	pup1.Client = cl1
	pup2 := &upstream.PreparedUpstream{
		Logger:       log.Logger,
		Id:           "rpc2",
		Endpoint:     "http://alchemy.com",
		Architecture: upstream.ArchitectureEvm,
		Metadata: map[string]string{
			"evmChainId": "123",
		},
		NetworkIds: []string{"123"},
		Score:      -1,
	}
	cl2, err := clr.GetOrCreateClient(pup2)
	if err != nil {
		t.Error(err)
	}
	pup2.Client = cl2

	ntw := &PreparedNetwork{
		Logger:    log.Logger,
		NetworkId: "123",
		Config: &config.NetworkConfig{
			Failsafe: fsCfg,
		},
		FailsafePolicies: policies,
		Upstreams:        []*upstream.PreparedUpstream{pup1, pup2},
		failsafeExecutor: failsafe.NewExecutor(policies...),
	}

	respWriter := httptest.NewRecorder()
	fakeReq := upstream.NewNormalizedRequest(requestBytes)
	err = ntw.Forward(ctx, fakeReq, respWriter)

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

	resp := respWriter.Result()

	if resp.StatusCode != 200 {
		t.Errorf("Expected status 200, got %v", resp.StatusCode)
	}

	var respObject map[string]interface{}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("error reading response: %s", err)
	}

	err = json.Unmarshal(body, &respObject)
	if err != nil {
		t.Fatalf("error unmarshalling: %s response body: %s", err, body)
	}
	if respObject["fromHost"] != "rpc1" {
		t.Errorf("Expected fromHost to be %v, got %v", "rpc1", respObject["fromHost"])
	}
}
