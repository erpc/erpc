package upstream

import (
	"context"
	"errors"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/failsafe"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubClient stands in for the transport: it answers each SendRequest with
// whatever fn decides, counting calls so tests can tell whether the upstream
// actually dialed.
type stubClient struct {
	typ   clients.ClientType
	calls atomic.Int32
	fn    func(ctx context.Context, req *common.NormalizedRequest, call int32) (*common.NormalizedResponse, error)
}

func (c *stubClient) GetType() clients.ClientType { return c.typ }

func (c *stubClient) SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return c.fn(ctx, req, c.calls.Add(1))
}

func newStubClient(fn func(ctx context.Context, req *common.NormalizedRequest, call int32) (*common.NormalizedResponse, error)) *stubClient {
	return &stubClient{typ: clients.ClientTypeHttpJsonRpc, fn: fn}
}

func stubResult(req *common.NormalizedRequest, result string) *common.NormalizedResponse {
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(result), nil)
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
}

func stubRpcError(req *common.NormalizedRequest) *common.NormalizedResponse {
	jrr := common.MustNewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(-32000, "boom", ""))
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
}

func fwdReq(method string) *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":[]}`))
}

type fwdEnv struct {
	ctx     context.Context
	logger  zerolog.Logger
	tracker *health.Tracker
	rlr     *RateLimitersRegistry
	vr      *thirdparty.VendorsRegistry
	cr      *clients.ClientRegistry
	ssr     data.SharedStateRegistry
}

func newFwdEnv(t *testing.T, rl *common.RateLimiterConfig) *fwdEnv {
	t.Helper()
	ctx := t.Context()
	lg := zerolog.Nop()
	if rl == nil {
		rl = &common.RateLimiterConfig{}
	}
	rlr, err := NewRateLimitersRegistry(ctx, rl, &lg)
	require.NoError(t, err)
	ssr, err := data.NewSharedStateRegistry(ctx, &lg, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 10_000, MaxTotalSize: "64MB"},
		},
	})
	require.NoError(t, err)
	return &fwdEnv{
		ctx:     ctx,
		logger:  lg,
		tracker: health.NewTracker(&lg, "prj", time.Minute),
		rlr:     rlr,
		vr:      thirdparty.NewVendorsRegistry(),
		cr:      clients.NewClientRegistry(&lg, "prj", nil, NewCompositeJsonRpcErrorExtractor()),
		ssr:     ssr,
	}
}

func (e *fwdEnv) newUpstream(t *testing.T, cfg *common.UpstreamConfig) (*Upstream, error) {
	t.Helper()
	if cfg.Endpoint == "" && cfg.VendorName == "" {
		cfg.Endpoint = "http://rpc1.localhost"
	}
	if cfg.Type == "" {
		cfg.Type = common.UpstreamTypeEvm
	}
	return NewUpstream(e.ctx, "prj", cfg, e.cr, e.rlr, e.vr, &e.logger, e.tracker, e.ssr)
}

// mustUpstream builds an upstream through NewUpstream and swaps its transport
// for the stub.
func (e *fwdEnv) mustUpstream(t *testing.T, cfg *common.UpstreamConfig, sc *stubClient) *Upstream {
	t.Helper()
	u, err := e.newUpstream(t, cfg)
	require.NoError(t, err)
	u.Client = sc
	return u
}

func breakerCfg() *common.CircuitBreakerPolicyConfig {
	return &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    1,
		FailureThresholdCapacity: 1,
		HalfOpenAfter:            common.Duration(time.Hour),
		SuccessThresholdCount:    1,
		SuccessThresholdCapacity: 1,
	}
}

func TestUpstreamForward_SuccessRecordsAttemptAndLastValidResponse(t *testing.T) {
	env := newFwdEnv(t, nil)
	sc := newStubClient(func(_ context.Context, req *common.NormalizedRequest, _ int32) (*common.NormalizedResponse, error) {
		return stubResult(req, `"0xabc"`), nil
	})
	u := env.mustUpstream(t, &common.UpstreamConfig{Id: "fwd-ok"}, sc)

	req := fwdReq("eth_call")
	resp, err := u.Forward(env.ctx, req, false, false)
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Same(t, u, resp.Upstream(), "the response is attributed to the upstream that served it")
	assert.Same(t, resp, req.LastValidResponse())
	assert.Same(t, u, req.LastUpstream())

	log := req.ExecState().UpstreamAttemptLog()
	require.Len(t, log, 1)
	assert.Equal(t, common.UpstreamOutcomeSuccess, log[0].Outcome)
	assert.Equal(t, "fwd-ok", log[0].UpstreamId)
	assert.Equal(t, common.SelectionReasonPrimary, log[0].Reason)
	assert.Equal(t, int64(1), log[0].CreditUnits, "a vendor without pricing costs one credit per request")
	assert.Empty(t, log[0].ErrorCode)

	m := env.tracker.GetUpstreamMethodMetrics(u, "eth_call", common.DataFinalityStateAll)
	assert.Equal(t, int64(1), m.RequestsTotal.Load())
	assert.Equal(t, int64(0), m.ErrorsTotal.Load())
}

func TestUpstreamForward_ResponseShapesClassifyOutcome(t *testing.T) {
	cases := []struct {
		name          string
		resp          func(req *common.NormalizedRequest) *common.NormalizedResponse
		wantOutcome   common.UpstreamAttemptOutcome
		wantLastValid bool
	}{
		{"empty hex result", func(r *common.NormalizedRequest) *common.NormalizedResponse { return stubResult(r, `"0x"`) }, common.UpstreamOutcomeEmpty, true},
		{"null result", func(r *common.NormalizedRequest) *common.NormalizedResponse { return stubResult(r, `null`) }, common.UpstreamOutcomeEmpty, true},
		// An error body has no result: it reads as empty, and is never kept as the last valid response.
		{"json-rpc error body", stubRpcError, common.UpstreamOutcomeEmpty, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newFwdEnv(t, nil)
			sc := newStubClient(func(_ context.Context, req *common.NormalizedRequest, _ int32) (*common.NormalizedResponse, error) {
				return tc.resp(req), nil
			})
			u := env.mustUpstream(t, &common.UpstreamConfig{Id: "fwd-shape"}, sc)
			req := fwdReq("eth_getLogs")
			resp, err := u.Forward(env.ctx, req, false, false)
			require.NoError(t, err)
			require.NotNil(t, resp)
			log := req.ExecState().UpstreamAttemptLog()
			require.Len(t, log, 1)
			assert.Equal(t, tc.wantOutcome, log[0].Outcome)
			assert.Equal(t, tc.wantLastValid, req.LastValidResponse() != nil)
		})
	}
}

func TestUpstreamForward_ClientErrorsAreWrappedAndAccounted(t *testing.T) {
	endpoint, _ := url.Parse("http://rpc1.localhost")
	cases := []struct {
		name           string
		err            error
		wantCode       common.ErrorCode
		wantOutcome    common.UpstreamAttemptOutcome
		wantErrorCount int64
		wantRemoteRL   int64
		wantCredits    int64
	}{
		{"capacity exceeded", common.NewErrEndpointCapacityExceeded(errors.New("429")), common.ErrCodeEndpointCapacityExceeded, common.UpstreamOutcomeRateLimited, 0, 1, 1},
		{"missing data", common.NewErrEndpointMissingData(errors.New("no block"), nil), common.ErrCodeEndpointMissingData, common.UpstreamOutcomeMissingData, 0, 0, 1},
		{"request canceled", common.NewErrEndpointRequestCanceled(context.Canceled), common.ErrCodeEndpointRequestCanceled, common.UpstreamOutcomeCancelled, 0, 0, 1},
		{"execution reverted", common.NewErrEndpointExecutionException(errors.New("revert")), common.ErrCodeEndpointExecutionException, common.UpstreamOutcomeExecRevert, 0, 0, 1},
		{"server side", common.NewErrEndpointServerSideException(errors.New("500"), nil, 500), common.ErrCodeEndpointServerSideException, common.UpstreamOutcomeServerError, 1, 0, 1},
		{"transport failure", common.NewErrEndpointTransportFailure(endpoint, errors.New("refused")), common.ErrCodeEndpointTransportFailure, common.UpstreamOutcomeTransportError, 1, 0, 1},
		{"client side", common.NewErrEndpointClientSideException(errors.New("bad params")), common.ErrCodeEndpointClientSideException, common.UpstreamOutcomeClientError, 0, 0, 1},
		{"skipped by client", common.NewErrUpstreamRequestSkipped(errors.New("nope"), "fwd-err"), common.ErrCodeUpstreamRequestSkipped, common.UpstreamOutcomeSkipped, 0, 0, 0},
		{"unclassified error", errors.New("weird"), common.ErrCodeUpstreamRequest, common.UpstreamOutcomeServerError, 1, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newFwdEnv(t, nil)
			sc := newStubClient(func(_ context.Context, req *common.NormalizedRequest, _ int32) (*common.NormalizedResponse, error) {
				// A revert carries a body; it must be released and the slot cleared.
				return stubResult(req, `"0x1"`), tc.err
			})
			u := env.mustUpstream(t, &common.UpstreamConfig{Id: "fwd-err"}, sc)
			req := fwdReq("eth_call")
			// Seed a previous valid response: only a revert must clear it.
			prev := stubResult(req, `"0x2"`)
			req.SetLastValidResponse(env.ctx, prev)

			resp, err := u.Forward(env.ctx, req, false, false)
			require.Error(t, err)
			assert.Nil(t, resp)
			assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamRequest), "every client error is wrapped as ErrUpstreamRequest: %v", err)
			assert.True(t, common.HasErrorCode(err, tc.wantCode), "cause code %s preserved: %v", tc.wantCode, err)

			log := req.ExecState().UpstreamAttemptLog()
			require.Len(t, log, 1)
			assert.Equal(t, tc.wantOutcome, log[0].Outcome)
			assert.NotEmpty(t, log[0].ErrorCode)
			assert.Equal(t, tc.wantCredits, log[0].CreditUnits)

			m := env.tracker.GetUpstreamMethodMetrics(u, "eth_call", common.DataFinalityStateAll)
			assert.Equal(t, tc.wantErrorCount, m.ErrorsTotal.Load(), "error-rate counter")
			assert.Equal(t, tc.wantRemoteRL, m.RemoteRateLimitedTotal.Load(), "remote rate-limit counter")

			if tc.wantCode == common.ErrCodeEndpointExecutionException {
				assert.Nil(t, req.LastValidResponse(), "a revert invalidates the last valid response")
			} else {
				assert.Same(t, prev, req.LastValidResponse())
			}
		})
	}
}

func TestUpstreamForward_AttemptErrorDetailIsTruncated(t *testing.T) {
	old := AttemptErrorDetailMaxLen
	AttemptErrorDetailMaxLen = 16
	t.Cleanup(func() { AttemptErrorDetailMaxLen = old })

	env := newFwdEnv(t, nil)
	sc := newStubClient(func(context.Context, *common.NormalizedRequest, int32) (*common.NormalizedResponse, error) {
		return nil, common.NewErrEndpointServerSideException(errors.New("a very long upstream failure message"), nil, 500)
	})
	u := env.mustUpstream(t, &common.UpstreamConfig{Id: "fwd-trunc"}, sc)
	req := fwdReq("eth_call")
	_, err := u.Forward(env.ctx, req, false, false)
	require.Error(t, err)
	log := req.ExecState().UpstreamAttemptLog()
	require.Len(t, log, 1)
	assert.Len(t, log[0].ErrorDetail, 16)
	assert.Greater(t, len(err.Error()), 16, "the caller still gets the full error")
}

func TestUpstreamForward_UnresolvableMethodFailsBeforeDialing(t *testing.T) {
	env := newFwdEnv(t, nil)
	sc := newStubClient(func(context.Context, *common.NormalizedRequest, int32) (*common.NormalizedResponse, error) {
		t.Fatal("must not dial")
		return nil, nil
	})
	u := env.mustUpstream(t, &common.UpstreamConfig{Id: "fwd-bad"}, sc)
	_, err := u.Forward(env.ctx, common.NewNormalizedRequest([]byte(`{not json`)), false, false)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamRequest))
	assert.Equal(t, int32(0), sc.calls.Load())
}

type syncingPoller struct{ mockEvmStatePollerEnhanced }

func (p *syncingPoller) SyncingState() common.EvmSyncingState { return common.EvmSyncingStateSyncing }

func TestUpstreamForward_SkipRules(t *testing.T) {
	okClient := func() *stubClient {
		return newStubClient(func(_ context.Context, req *common.NormalizedRequest, _ int32) (*common.NormalizedResponse, error) {
			return stubResult(req, `"0x1"`), nil
		})
	}
	cases := []struct {
		name      string
		cfg       *common.UpstreamConfig
		poller    common.EvmStatePoller
		useUps    string
		bypass    bool
		wantCause common.ErrorCode // empty = forwarded
	}{
		{name: "shadow upstream never serves", cfg: &common.UpstreamConfig{Id: "sk", Shadow: &common.ShadowUpstreamConfig{Enabled: true}}, wantCause: common.ErrCodeUpstreamShadowing},
		{name: "syncing upstream skipped when skipWhenSyncing", cfg: &common.UpstreamConfig{Id: "sk", Evm: &common.EvmUpstreamConfig{SkipWhenSyncing: util.BoolPtr(true)}}, poller: &syncingPoller{}, wantCause: common.ErrCodeUpstreamSyncing},
		{name: "syncing upstream served when skipWhenSyncing off", cfg: &common.UpstreamConfig{Id: "sk", Evm: &common.EvmUpstreamConfig{SkipWhenSyncing: util.BoolPtr(false)}}, poller: &syncingPoller{}},
		{name: "ignored method", cfg: &common.UpstreamConfig{Id: "sk", IgnoreMethods: []string{"eth_call"}}, wantCause: common.ErrCodeUpstreamMethodIgnored},
		{name: "ignored method bypassed", cfg: &common.UpstreamConfig{Id: "sk", IgnoreMethods: []string{"eth_call"}}, bypass: true},
		{name: "invalid ignore pattern skips with its error", cfg: &common.UpstreamConfig{Id: "sk", IgnoreMethods: []string{"(eth_"}}, wantCause: ""},
		{name: "use-upstream directive mismatch", cfg: &common.UpstreamConfig{Id: "sk"}, useUps: "other-*", wantCause: common.ErrCodeUpstreamNotAllowed},
		{name: "use-upstream directive match", cfg: &common.UpstreamConfig{Id: "sk"}, useUps: "s*"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newFwdEnv(t, nil)
			sc := okClient()
			u := env.mustUpstream(t, tc.cfg, sc)
			if tc.poller != nil {
				u.evmStatePoller = tc.poller
			}
			req := fwdReq("eth_call")
			if tc.useUps != "" {
				req.SetDirectives(&common.RequestDirectives{UseUpstream: tc.useUps})
			}
			resp, err := u.Forward(env.ctx, req, tc.bypass, false)
			skipped := tc.wantCause != "" || tc.name == "invalid ignore pattern skips with its error"
			if !skipped {
				require.NoError(t, err)
				require.NotNil(t, resp)
				assert.Equal(t, int32(1), sc.calls.Load())
				return
			}
			require.Error(t, err)
			assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamRequestSkipped), "%v", err)
			if tc.wantCause != "" {
				assert.True(t, common.HasErrorCode(err, tc.wantCause), "%v", err)
			}
			assert.Equal(t, int32(0), sc.calls.Load(), "a skipped request never dials")
		})
	}
}

func TestUpstreamForward_RateLimitBudget(t *testing.T) {
	rl := &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*common.RateLimitBudgetConfig{{
			Id:    "one-call",
			Rules: []*common.RateLimitRuleConfig{{Method: "eth_call", MaxCount: 1, Period: common.RateLimitPeriodMinute}},
		}},
	}
	ok := func() *stubClient {
		return newStubClient(func(_ context.Context, req *common.NormalizedRequest, _ int32) (*common.NormalizedResponse, error) {
			return stubResult(req, `"0x1"`), nil
		})
	}

	t.Run("exhausted budget rejects without dialing", func(t *testing.T) {
		env := newFwdEnv(t, rl)
		sc := ok()
		u := env.mustUpstream(t, &common.UpstreamConfig{Id: "rl-a", RateLimitBudget: "one-call"}, sc)
		_, err := u.Forward(env.ctx, fwdReq("eth_call"), false, false)
		require.NoError(t, err)
		_, err = u.Forward(env.ctx, fwdReq("eth_call"), false, false)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamRateLimitRuleExceeded), "%v", err)
		assert.Equal(t, int32(1), sc.calls.Load())
	})

	t.Run("methods without a rule are unlimited", func(t *testing.T) {
		env := newFwdEnv(t, rl)
		sc := ok()
		u := env.mustUpstream(t, &common.UpstreamConfig{Id: "rl-b", RateLimitBudget: "one-call"}, sc)
		for range 3 {
			_, err := u.Forward(env.ctx, fwdReq("eth_getBalance"), false, false)
			require.NoError(t, err)
		}
		assert.Equal(t, int32(3), sc.calls.Load())
	})

	t.Run("unknown budget is an error", func(t *testing.T) {
		env := newFwdEnv(t, rl)
		sc := ok()
		u := env.mustUpstream(t, &common.UpstreamConfig{Id: "rl-c", RateLimitBudget: "missing"}, sc)
		_, err := u.Forward(env.ctx, fwdReq("eth_call"), false, false)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, "ErrRateLimitBudgetNotFound"), "%v", err)
		assert.Equal(t, int32(0), sc.calls.Load())
	})

	t.Run("credit mode charges the method's credit units", func(t *testing.T) {
		env := newFwdEnv(t, &common.RateLimiterConfig{
			Store: &common.RateLimitStoreConfig{Driver: "memory"},
			Budgets: []*common.RateLimitBudgetConfig{{
				Id:    "wallet",
				Rules: []*common.RateLimitRuleConfig{{Method: "*", MaxCount: 10, Period: common.RateLimitPeriodMinute}},
			}},
		})
		sc := ok()
		u := env.mustUpstream(t, &common.UpstreamConfig{
			Id:                 "rl-credit",
			RateLimitBudget:    "wallet",
			RateLimitCountMode: common.RateLimitCountModeCredit,
			CreditUnits:        map[string]int64{"eth_call": 5, "eth_chainId": 0},
		}, sc)
		for i := range 2 {
			_, err := u.Forward(env.ctx, fwdReq("eth_call"), false, false)
			require.NoError(t, err, "call %d fits in 10 credits", i)
		}
		_, err := u.Forward(env.ctx, fwdReq("eth_call"), false, false)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamRateLimitRuleExceeded))
		// A 0-credit method draws nothing from the exhausted wallet.
		_, err = u.Forward(env.ctx, fwdReq("eth_chainId"), false, false)
		require.NoError(t, err)
	})
}

func TestUpstream_RateLimitCost(t *testing.T) {
	cases := []struct {
		name string
		cfg  *common.UpstreamConfig
		want PermitCost
	}{
		{"request mode is one hit", &common.UpstreamConfig{}, requestPermit},
		{"credit mode uses the method cost", &common.UpstreamConfig{RateLimitCountMode: common.RateLimitCountModeCredit, CreditUnits: map[string]int64{"eth_call": 26}}, PermitCost{Hits: 26, Mode: common.RateLimitCountModeCredit}},
		{"credit mode zero cost is exempt", &common.UpstreamConfig{RateLimitCountMode: common.RateLimitCountModeCredit, CreditUnits: map[string]int64{"*": 0}}, PermitCost{Hits: 0, Mode: common.RateLimitCountModeCredit}},
		{"credit mode clamps to uint32", &common.UpstreamConfig{RateLimitCountMode: common.RateLimitCountModeCredit, CreditUnits: map[string]int64{"*": 1 << 40}}, PermitCost{Hits: 1<<32 - 1, Mode: common.RateLimitCountModeCredit}},
		{"credit mode without table falls back to one credit", &common.UpstreamConfig{RateLimitCountMode: common.RateLimitCountModeCredit}, PermitCost{Hits: 1, Mode: common.RateLimitCountModeCredit}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &Upstream{config: tc.cfg}
			assert.Equal(t, tc.want, u.rateLimitCost(fwdReq("eth_call")))
		})
	}
	var nilUps *Upstream
	assert.Equal(t, requestPermit, nilUps.rateLimitCost(fwdReq("eth_call")))
	assert.Equal(t, int64(0), nilUps.attemptCreditUnits(fwdReq("eth_call")))
}

func TestUpstreamForward_UnsupportedClientType(t *testing.T) {
	env := newFwdEnv(t, nil)
	sc := newStubClient(nil)
	sc.typ = clients.ClientType("Carrier-Pigeon")
	u := env.mustUpstream(t, &common.UpstreamConfig{Id: "fwd-pigeon"}, sc)
	_, err := u.Forward(env.ctx, fwdReq("eth_call"), false, false)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, "ErrUpstreamClientInitialization"), "%v", err)
	assert.Contains(t, err.Error(), "could not initialize upstream client")
}

func TestUpstreamForward_RetryPolicy(t *testing.T) {
	serverErr := func() error { return common.NewErrEndpointServerSideException(errors.New("503"), nil, 503) }
	retry := []*common.FailsafeConfig{{Retry: &common.RetryPolicyConfig{MaxAttempts: 3}}}

	t.Run("transient failures are retried until success", func(t *testing.T) {
		env := newFwdEnv(t, nil)
		sc := newStubClient(func(_ context.Context, req *common.NormalizedRequest, call int32) (*common.NormalizedResponse, error) {
			if call < 3 {
				return nil, serverErr()
			}
			return stubResult(req, `"0x1"`), nil
		})
		u := env.mustUpstream(t, &common.UpstreamConfig{Id: "rt-ok", Failsafe: retry}, sc)
		req := fwdReq("eth_call")
		resp, err := u.Forward(env.ctx, req, false, false)
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Equal(t, int32(3), sc.calls.Load())
		snap := req.ExecState().Snapshot()
		assert.Equal(t, 3, snap.UpstreamAttempts)
		assert.Equal(t, 2, snap.UpstreamRetries)
		log := req.ExecState().UpstreamAttemptLog()
		require.Len(t, log, 3)
		assert.False(t, log[0].IsRetry)
		assert.True(t, log[2].IsRetry)
		assert.Equal(t, common.SelectionReasonRetry, log[2].Reason)
		assert.Equal(t, common.UpstreamOutcomeSuccess, log[2].Outcome)
	})

	t.Run("exhausted retries surface ErrFailsafeRetryExceeded", func(t *testing.T) {
		env := newFwdEnv(t, nil)
		sc := newStubClient(func(context.Context, *common.NormalizedRequest, int32) (*common.NormalizedResponse, error) {
			return nil, serverErr()
		})
		u := env.mustUpstream(t, &common.UpstreamConfig{Id: "rt-fail", Failsafe: retry}, sc)
		_, err := u.Forward(env.ctx, fwdReq("eth_call"), false, false)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeRetryExceeded), "%v", err)
		assert.Equal(t, int32(3), sc.calls.Load())
	})

	t.Run("non-retryable errors and write methods are attempted once", func(t *testing.T) {
		cases := []struct {
			method string
			err    error
		}{
			{"eth_sendTransaction", serverErr()},
			{"eth_call", common.NewErrEndpointExecutionException(errors.New("revert"))},
			{"eth_call", common.NewErrEndpointCapacityExceeded(errors.New("429"))},
		}
		for _, tc := range cases {
			env := newFwdEnv(t, nil)
			sc := newStubClient(func(context.Context, *common.NormalizedRequest, int32) (*common.NormalizedResponse, error) {
				return nil, tc.err
			})
			u := env.mustUpstream(t, &common.UpstreamConfig{Id: "rt-once", Failsafe: retry}, sc)
			_, err := u.Forward(env.ctx, fwdReq(tc.method), false, false)
			require.Error(t, err)
			assert.Equal(t, int32(1), sc.calls.Load(), "%s / %v", tc.method, tc.err)
		}
	})
}

func TestUpstreamForward_TimeoutPolicyFires(t *testing.T) {
	env := newFwdEnv(t, nil)
	sc := newStubClient(func(ctx context.Context, _ *common.NormalizedRequest, _ int32) (*common.NormalizedResponse, error) {
		<-ctx.Done()
		return nil, context.Cause(ctx)
	})
	u := env.mustUpstream(t, &common.UpstreamConfig{
		Id:       "to-fire",
		Failsafe: []*common.FailsafeConfig{{Timeout: &common.TimeoutPolicyConfig{Duration: common.NewStaticDuration(20 * time.Millisecond)}}},
	}, sc)
	req := fwdReq("eth_call")
	_, err := u.Forward(env.ctx, req, false, false)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeTimeoutExceeded), "%v", err)
	log := req.ExecState().UpstreamAttemptLog()
	require.Len(t, log, 1)
	assert.Equal(t, common.UpstreamOutcomeTimeout, log[0].Outcome)
}

func TestUpstreamForward_CircuitBreakerOpensAndBlocks(t *testing.T) {
	env := newFwdEnv(t, nil)
	sc := newStubClient(func(context.Context, *common.NormalizedRequest, int32) (*common.NormalizedResponse, error) {
		return nil, common.NewErrEndpointServerSideException(errors.New("500"), nil, 500)
	})
	u := env.mustUpstream(t, &common.UpstreamConfig{
		Id:       "cb-forward",
		Failsafe: []*common.FailsafeConfig{{CircuitBreaker: breakerCfg()}},
	}, sc)

	transitions := telemetry.MetricUpstreamBreakerStateChange.WithLabelValues("prj", "cb-forward", "closed_to_open")
	before := testutil.ToFloat64(transitions)
	assert.Equal(t, failsafe.StateClosed, u.CircuitBreakerState())
	_, err := u.Forward(env.ctx, fwdReq("eth_call"), false, false)
	require.Error(t, err)
	assert.Equal(t, failsafe.StateOpen, u.CircuitBreakerState())
	assert.True(t, u.IsDown("eth_call", common.DataFinalityStateUnknown))

	req := fwdReq("eth_call")
	_, err = u.Forward(env.ctx, req, false, false)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeCircuitBreakerOpen), "%v", err)
	assert.Equal(t, int32(1), sc.calls.Load(), "an open breaker never dials")
	_ = req

	// The transition hook wired by NewUpstream feeds the breaker metric (async).
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(transitions) == before+1
	}, 5*time.Second, 10*time.Millisecond)
}

func TestUpstreamForward_HedgeWinsOverSlowPrimary(t *testing.T) {
	env := newFwdEnv(t, nil)
	sc := newStubClient(func(ctx context.Context, req *common.NormalizedRequest, call int32) (*common.NormalizedResponse, error) {
		if call == 1 {
			<-ctx.Done()
			return nil, common.NewErrEndpointRequestCanceled(ctx.Err())
		}
		return stubResult(req, `"0x1"`), nil
	})
	u := env.mustUpstream(t, &common.UpstreamConfig{
		Id:       "hedge-win",
		Failsafe: []*common.FailsafeConfig{{Hedge: &common.HedgePolicyConfig{MaxCount: 1, Delay: common.NewStaticDuration(10 * time.Millisecond)}}},
	}, sc)
	req := fwdReq("eth_getBalance")
	resp, err := u.Forward(env.ctx, req, false, false)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, 1, req.ExecState().Snapshot().UpstreamHedges)
	require.Eventually(t, func() bool { return len(req.ExecState().UpstreamAttemptLog()) == 2 }, 5*time.Second, 5*time.Millisecond)
	var hedged *common.UpstreamAttempt
	for _, a := range req.ExecState().UpstreamAttemptLog() {
		if a.IsHedge {
			a := a
			hedged = &a
		}
	}
	require.NotNil(t, hedged, "the hedge attempt is recorded as such")
	assert.Equal(t, common.UpstreamOutcomeSuccess, hedged.Outcome)
	assert.Equal(t, common.SelectionReasonHedge, hedged.Reason)
	// Hedges are speculative fan-out: they do not inflate the request counter.
	m := env.tracker.GetUpstreamMethodMetrics(u, "eth_getBalance", common.DataFinalityStateAll)
	assert.Equal(t, int64(1), m.RequestsTotal.Load())
}

func TestUpstreamForward_NetworkHedgeFlagSkipsRateCounters(t *testing.T) {
	env := newFwdEnv(t, nil)
	sc := newStubClient(func(context.Context, *common.NormalizedRequest, int32) (*common.NormalizedResponse, error) {
		return nil, common.NewErrEndpointServerSideException(errors.New("500"), nil, 500)
	})
	u := env.mustUpstream(t, &common.UpstreamConfig{Id: "net-hedge"}, sc)
	req := fwdReq("eth_call")
	_, err := u.Forward(env.ctx, req, false, true)
	require.Error(t, err)
	m := env.tracker.GetUpstreamMethodMetrics(u, "eth_call", common.DataFinalityStateAll)
	assert.Equal(t, int64(0), m.RequestsTotal.Load())
	assert.Equal(t, int64(0), m.ErrorsTotal.Load())
	log := req.ExecState().UpstreamAttemptLog()
	require.Len(t, log, 1)
	assert.True(t, log[0].IsHedge)
}

func TestUpstreamForward_AutoTunerShrinksBudgetUnderRemoteThrottling(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantMax uint32
	}{
		{"remote 429s shrink the budget", common.NewErrEndpointCapacityExceeded(errors.New("429")), 50},
		{"clean traffic grows the budget", nil, 150},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule := &common.RateLimitRuleConfig{Method: "eth_call", MaxCount: 100, Period: common.RateLimitPeriodMinute}
			env := newFwdEnv(t, &common.RateLimiterConfig{
				Store:   &common.RateLimitStoreConfig{Driver: "memory"},
				Budgets: []*common.RateLimitBudgetConfig{{Id: "tuned", Rules: []*common.RateLimitRuleConfig{rule}}},
			})
			sc := newStubClient(func(_ context.Context, req *common.NormalizedRequest, _ int32) (*common.NormalizedResponse, error) {
				if tc.err != nil {
					return nil, tc.err
				}
				return stubResult(req, `"0x1"`), nil
			})
			u := env.mustUpstream(t, &common.UpstreamConfig{
				Id:              "tuned-ups",
				RateLimitBudget: "tuned",
				RateLimitAutoTune: &common.RateLimitAutoTuneConfig{
					Enabled:            util.BoolPtr(true),
					AdjustmentPeriod:   common.Duration(time.Hour),
					ErrorRateThreshold: 0.1,
					IncreaseFactor:     1.5,
					DecreaseFactor:     0.5,
					MinBudget:          10,
					MaxBudget:          1000,
				},
			}, sc)
			require.NotNil(t, u.rateLimiterAutoTuner)

			// The first sample opens the window; the next ten accumulate in it.
			for range 11 {
				_, _ = u.Forward(env.ctx, fwdReq("eth_call"), false, false)
			}
			assert.Equal(t, uint32(100), rule.MaxCount, "no adjustment inside the window")

			u.rateLimiterAutoTuner.mu.Lock()
			u.rateLimiterAutoTuner.lastAdjustments["eth_call"] = time.Now().Add(-2 * time.Hour)
			u.rateLimiterAutoTuner.mu.Unlock()
			_, _ = u.Forward(env.ctx, fwdReq("eth_call"), false, false)
			assert.Equal(t, tc.wantMax, rule.MaxCount)
		})
	}
}
