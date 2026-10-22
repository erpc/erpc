package upstream

import (
	"context"
	"errors"
	"math"
	"net/url"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/failsafe"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const errCodeClientInit = common.ErrorCode("ErrUpstreamClientInitialization")

func answering(result string) *stubClient {
	return newStubClient(func(_ context.Context, req *common.NormalizedRequest, _ int32) (*common.NormalizedResponse, error) {
		return stubResult(req, result), nil
	})
}

func TestUpstreamState_ShouldHandleMethod_IgnoreThenAllowPrecedence(t *testing.T) {
	env := newFwdEnv(t, nil)
	u := env.mustUpstream(t, &common.UpstreamConfig{
		Id:            "rules",
		IgnoreMethods: []string{"eth_*"},
		AllowMethods:  []string{"eth_getLogs"},
	}, answering(`"0x1"`))

	cases := map[string]bool{
		"eth_getLogs": true,  // allow overrides a matching ignore
		"eth_call":    false, // ignored by wildcard
		"net_version": true,  // matched by neither: allowed by default
	}
	for method, want := range cases {
		got, err := u.ShouldHandleMethod(method)
		require.NoError(t, err)
		assert.Equal(t, want, got, method)
	}
}

func TestUpstreamState_ShouldHandleMethod_InvalidPatternErrors(t *testing.T) {
	for _, cfg := range []*common.UpstreamConfig{
		{Id: "bad-ignore", IgnoreMethods: []string{"(eth_"}},
		{Id: "bad-allow", AllowMethods: []string{"(eth_"}},
	} {
		env := newFwdEnv(t, nil)
		u := env.mustUpstream(t, cfg, answering(`"0x1"`))
		ok, err := u.ShouldHandleMethod("eth_call")
		require.Error(t, err, cfg.Id)
		assert.False(t, ok, cfg.Id)
	}
}

func TestUpstreamState_IgnoreMethod_OnlyWhenAutoIgnoreEnabled(t *testing.T) {
	env := newFwdEnv(t, nil)
	off := env.mustUpstream(t, &common.UpstreamConfig{Id: "auto-off"}, answering(`"0x1"`))
	off.IgnoreMethod("trace_block")
	ok, err := off.ShouldHandleMethod("trace_block")
	require.NoError(t, err)
	assert.True(t, ok, "without autoIgnoreUnsupportedMethods the method stays allowed")
	assert.Empty(t, off.Config().IgnoreMethods)

	on := env.mustUpstream(t, &common.UpstreamConfig{Id: "auto-on", AutoIgnoreUnsupportedMethods: util.BoolPtr(true)}, answering(`"0x1"`))
	on.IgnoreMethod("trace_block")
	ok, err = on.ShouldHandleMethod("trace_block")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, on.Config().IgnoreMethods, "a learned ignore expires, so it is never written into the config")

	_, err = on.Forward(env.ctx, fwdReq("trace_block"), false, false)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamMethodIgnored), "learned ignore is enforced on Forward: %v", err)
}

func TestUpstreamState_NilUpstreamGetters(t *testing.T) {
	var u *Upstream
	assert.Equal(t, "", u.Id())
	assert.Equal(t, "n/a", u.NetworkId())
	assert.Equal(t, "n/a", u.NetworkLabel())
	assert.Equal(t, "nil", u.VendorName())
	assert.Nil(t, u.Config())
	assert.True(t, u.IsDown("eth_call", common.DataFinalityStateUnknown))
	assert.Nil(t, u.MetricsTracker())
	assert.Nil(t, u.Tracker())
	assert.Nil(t, u.Vendor())
	assert.Equal(t, failsafe.StateClosed, u.CircuitBreakerState())
}

func TestUpstreamState_GettersAndMarshal(t *testing.T) {
	env := newFwdEnv(t, nil)
	u := env.mustUpstream(t, &common.UpstreamConfig{Id: "getters", Endpoint: "https://eth.rpc.example.com"}, answering(`"0x1"`))
	assert.Equal(t, "getters", u.Id())
	assert.Equal(t, "unknown-example.com", u.VendorName(), "vendor name is guessed from the endpoint domain")
	assert.Same(t, env.tracker, u.MetricsTracker())
	assert.NotNil(t, u.Tracker())
	assert.Nil(t, u.Vendor())
	assert.Equal(t, failsafe.StateClosed, u.CircuitBreakerState())
	assert.Nil(t, u.EvmMulticall3Batcher(), "multicall3 is off unless configured")

	b, err := u.MarshalJSON()
	require.NoError(t, err)
	assert.Contains(t, string(b), `"id":"getters"`)
	assert.Contains(t, string(b), `"networkId":"n/a"`)

	mc := env.mustUpstream(t, &common.UpstreamConfig{Id: "mc", Evm: &common.EvmUpstreamConfig{Multicall3: &common.EvmMulticall3Config{MaxCalls: 10}}}, answering(`"0x1"`))
	assert.NotNil(t, mc.EvmMulticall3Batcher())
}

func TestUpstreamState_CircuitBreakerState_ReflectsCatchAllBreaker(t *testing.T) {
	env := newFwdEnv(t, nil)
	u := env.mustUpstream(t, &common.UpstreamConfig{
		Id:       "cb",
		Failsafe: []*common.FailsafeConfig{{MatchMethod: "*", CircuitBreaker: breakerCfg()}},
	}, answering(`"0x1"`))
	assert.Equal(t, failsafe.StateClosed, u.CircuitBreakerState())
	u.failsafeExecutors[0].Breaker().Record(failsafe.OutcomeFailure)
	assert.Equal(t, failsafe.StateOpen, u.CircuitBreakerState())
}

func TestUpstreamState_SetNetworkConfig(t *testing.T) {
	env := newFwdEnv(t, nil)
	u := env.mustUpstream(t, &common.UpstreamConfig{Id: "netcfg"}, answering(`"0x1"`))
	u.SetNetworkConfig(nil)
	assert.Equal(t, "n/a", u.NetworkId(), "nil config is ignored")

	u.SetNetworkConfig(&common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 1}})
	assert.Equal(t, "evm:1", u.NetworkId())
	assert.Equal(t, "evm:1", u.NetworkLabel(), "label defaults to the network id")

	u.SetNetworkConfig(&common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 1}, Alias: "mainnet"})
	assert.Equal(t, "mainnet", u.NetworkLabel(), "alias wins as label")
}

func TestUpstreamState_DetectFeatures(t *testing.T) {
	t.Run("evm chain id is detected and stored", func(t *testing.T) {
		env := newFwdEnv(t, nil)
		u := env.mustUpstream(t, &common.UpstreamConfig{Id: "df-ok"}, answering(`"0x7b"`))
		require.NoError(t, u.detectFeatures(env.ctx))
		assert.Equal(t, int64(123), u.Config().Evm.ChainId)
		assert.Equal(t, "evm:123", u.NetworkId())
	})
	t.Run("configured chain id mismatch is task-fatal", func(t *testing.T) {
		env := newFwdEnv(t, nil)
		u := env.mustUpstream(t, &common.UpstreamConfig{Id: "df-mm", Evm: &common.EvmUpstreamConfig{ChainId: 1}}, answering(`"0x7b"`))
		err := u.detectFeatures(env.ctx)
		require.Error(t, err)
		var fatal *common.TaskFatalError
		require.True(t, errors.As(err, &fatal))
		assert.True(t, common.HasErrorCode(fatal.Err, errCodeClientInit))
		assert.Contains(t, err.Error(), "ErrUpstreamChainIdMismatch")
	})
	t.Run("rpc failure is retryable init error", func(t *testing.T) {
		env := newFwdEnv(t, nil)
		sc := newStubClient(func(_ context.Context, _ *common.NormalizedRequest, _ int32) (*common.NormalizedResponse, error) {
			return nil, common.NewErrEndpointServerSideException(errors.New("down"), nil, 503)
		})
		u := env.mustUpstream(t, &common.UpstreamConfig{Id: "df-down"}, sc)
		err := u.detectFeatures(env.ctx)
		require.Error(t, err)
		var fatal *common.TaskFatalError
		assert.False(t, errors.As(err, &fatal))
		assert.True(t, common.HasErrorCode(err, errCodeClientInit))
	})
	t.Run("json-rpc error and non-hex result fail detection", func(t *testing.T) {
		env := newFwdEnv(t, nil)
		rpcErr := newStubClient(func(_ context.Context, req *common.NormalizedRequest, _ int32) (*common.NormalizedResponse, error) {
			return stubRpcError(req), nil
		})
		u := env.mustUpstream(t, &common.UpstreamConfig{Id: "df-rpcerr"}, rpcErr)
		_, err := u.EvmGetChainId(env.ctx)
		require.Error(t, err)

		u = env.mustUpstream(t, &common.UpstreamConfig{Id: "df-num"}, answering(`123`))
		_, err = u.EvmGetChainId(env.ctx)
		require.Error(t, err, "a numeric (non-string) chainId cannot be decoded")
	})
	t.Run("type checks", func(t *testing.T) {
		lg := zerolog.Nop()
		for _, tc := range []struct {
			cfg  *common.UpstreamConfig
			want string
		}{
			{&common.UpstreamConfig{Id: "no-type"}, "upstream type not set"},
			{&common.UpstreamConfig{Id: "bad-type", Type: "cosmos"}, "upstream type not supported"},
			{&common.UpstreamConfig{Id: "svm-nocfg", Type: common.UpstreamTypeSvm}, "missing svm config"},
			{&common.UpstreamConfig{Id: "svm-nocluster", Type: common.UpstreamTypeSvm, Svm: &common.SvmUpstreamConfig{}}, "missing svm.cluster"},
		} {
			u := &Upstream{config: tc.cfg, logger: &lg}
			err := u.detectFeatures(context.Background())
			require.Error(t, err, tc.cfg.Id)
			assert.Contains(t, err.Error(), tc.want, tc.cfg.Id)
		}
		u := &Upstream{config: &common.UpstreamConfig{Id: "svm-ok", Type: common.UpstreamTypeSvm, Svm: &common.SvmUpstreamConfig{Cluster: "devnet"}}, logger: &lg}
		require.NoError(t, u.detectFeatures(context.Background()))
		assert.Equal(t, "svm:devnet", u.NetworkId())
	})
}

func TestUpstreamState_SvmVerifyGenesisHash(t *testing.T) {
	const mainnet = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
	svmCfg := func(id string, s *common.SvmUpstreamConfig) *common.UpstreamConfig {
		return &common.UpstreamConfig{Id: id, Type: common.UpstreamTypeSvm, Svm: s}
	}
	t.Run("known cluster matching hash passes", func(t *testing.T) {
		env := newFwdEnv(t, nil)
		u := env.mustUpstream(t, svmCfg("g-ok", &common.SvmUpstreamConfig{Cluster: "mainnet-beta"}), answering(`"`+mainnet+`"`))
		require.NoError(t, u.svmVerifyGenesisHash(env.ctx))
		assert.Nil(t, u.SvmStatePoller())
	})
	t.Run("known cluster wrong hash is rejected", func(t *testing.T) {
		env := newFwdEnv(t, nil)
		u := env.mustUpstream(t, svmCfg("g-bad", &common.SvmUpstreamConfig{Cluster: "mainnet-beta"}), answering(`"wronghash"`))
		err := u.svmVerifyGenesisHash(env.ctx)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, errCodeClientInit))
		assert.Contains(t, err.Error(), "mismatch")
	})
	t.Run("fetch failure on a known cluster fails closed", func(t *testing.T) {
		env := newFwdEnv(t, nil)
		sc := newStubClient(func(_ context.Context, req *common.NormalizedRequest, _ int32) (*common.NormalizedResponse, error) {
			return stubRpcError(req), nil
		})
		u := env.mustUpstream(t, svmCfg("g-err", &common.SvmUpstreamConfig{Cluster: "devnet"}), sc)
		err := u.svmVerifyGenesisHash(env.ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "getGenesisHash failed")
	})
	t.Run("unknown cluster with checkGenesisHash accepts any decodable hash", func(t *testing.T) {
		env := newFwdEnv(t, nil)
		u := env.mustUpstream(t, svmCfg("g-custom", &common.SvmUpstreamConfig{Cluster: "localnet", CheckGenesisHash: true}), answering(`"anyhash"`))
		require.NoError(t, u.svmVerifyGenesisHash(env.ctx))

		u = env.mustUpstream(t, svmCfg("g-undecodable", &common.SvmUpstreamConfig{Cluster: "localnet", CheckGenesisHash: true}), answering(`42`))
		err := u.svmVerifyGenesisHash(env.ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "decode genesis hash")
	})
}

func TestUpstreamState_EvmPollerAccessors(t *testing.T) {
	lg := zerolog.Nop()
	bare := &Upstream{config: &common.UpstreamConfig{Id: "bare"}, logger: &lg}
	_, err := bare.EvmLatestBlock()
	require.Error(t, err)
	_, err = bare.EvmFinalizedBlock()
	require.Error(t, err)
	_, err = bare.EvmIsBlockFinalized(context.Background(), 1, false)
	require.Error(t, err)
	assert.Equal(t, common.EvmSyncingStateUnknown, bare.EvmSyncingState())

	sp := &mockEvmStatePollerEnhanced{latestBlock: 200, finalizedBlock: 100}
	u := &Upstream{config: &common.UpstreamConfig{Id: "polled"}, logger: &lg, evmStatePoller: sp}
	latest, err := u.EvmLatestBlock()
	require.NoError(t, err)
	assert.Equal(t, int64(200), latest)
	fin, err := u.EvmFinalizedBlock()
	require.NoError(t, err)
	assert.Equal(t, int64(100), fin)
	assert.Equal(t, common.EvmSyncingStateNotSyncing, u.EvmSyncingState())

	ok, err := u.EvmIsBlockFinalized(context.Background(), 50, false)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = u.EvmIsBlockFinalized(context.Background(), 150, true)
	require.NoError(t, err)
	assert.False(t, ok, "fresh poll still reports finalized=100 < 150")
	sp.finalizedBlock = 150
	ok, err = u.EvmIsBlockFinalized(context.Background(), 150, false)
	require.NoError(t, err)
	assert.True(t, ok)

	sp.pollError = errors.New("poll failed")
	_, err = u.EvmIsBlockFinalized(context.Background(), 150, true)
	require.Error(t, err)
}

func TestUpstreamState_ResolveAvailabilityBounds(t *testing.T) {
	lg := zerolog.Nop()
	i64 := func(v int64) *int64 { return &v }
	mk := func(evm *common.EvmUpstreamConfig, latest int64) *Upstream {
		return &Upstream{config: &common.UpstreamConfig{Id: "b", Evm: evm}, logger: &lg, evmStatePoller: &mockEvmStatePollerEnhanced{latestBlock: latest}}
	}
	cases := []struct {
		name     string
		evm      *common.EvmUpstreamConfig
		latest   int64
		min, max int64
	}{
		{"no evm config", nil, 1000, math.MinInt64, math.MaxInt64},
		{"legacy maxAvailableRecentBlocks", &common.EvmUpstreamConfig{MaxAvailableRecentBlocks: 128}, 1000, 872, math.MaxInt64},
		{"legacy without latest is unbounded", &common.EvmUpstreamConfig{MaxAvailableRecentBlocks: 128}, 0, math.MinInt64, math.MaxInt64},
		{"exact lower, latest-minus upper", &common.EvmUpstreamConfig{BlockAvailability: &common.EvmBlockAvailabilityConfig{
			Lower: &common.EvmAvailabilityBoundConfig{ExactBlock: i64(100)},
			Upper: &common.EvmAvailabilityBoundConfig{LatestBlockMinus: i64(5)},
		}}, 1000, 100, 995},
		{"latest-minus without latest is unbounded", &common.EvmUpstreamConfig{BlockAvailability: &common.EvmBlockAvailabilityConfig{
			Lower: &common.EvmAvailabilityBoundConfig{LatestBlockMinus: i64(10)},
			Upper: &common.EvmAvailabilityBoundConfig{LatestBlockMinus: i64(5)},
		}}, 0, math.MinInt64, math.MaxInt64},
		{"earliest-plus lower uses detected earliest", &common.EvmUpstreamConfig{BlockAvailability: &common.EvmBlockAvailabilityConfig{
			Lower: &common.EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(7)},
		}}, 1000, 7, math.MaxInt64},
		{"empty bound config is a no-op", &common.EvmUpstreamConfig{BlockAvailability: &common.EvmBlockAvailabilityConfig{
			Lower: &common.EvmAvailabilityBoundConfig{},
			Upper: &common.EvmAvailabilityBoundConfig{},
		}}, 1000, math.MinInt64, math.MaxInt64},
		{"inverted range fails open", &common.EvmUpstreamConfig{BlockAvailability: &common.EvmBlockAvailabilityConfig{
			Lower: &common.EvmAvailabilityBoundConfig{ExactBlock: i64(500)},
			Upper: &common.EvmAvailabilityBoundConfig{ExactBlock: i64(100)},
		}}, 1000, math.MinInt64, math.MaxInt64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lo, hi := mk(tc.evm, tc.latest).EvmBlockAvailabilityBounds()
			assert.Equal(t, tc.min, lo)
			assert.Equal(t, tc.max, hi)
		})
	}
}

func TestUpstreamState_GuessVendorName(t *testing.T) {
	cases := map[string]string{
		"":                            "",
		"http://10.0.0.1:8545":        "unknown-10.0.0.1",
		"http://rpc1.localhost":       "unknown-rpc1.localhost",
		"https://eth.rpc.example.com": "unknown-example.com",
		"https://x.rpc.b.co":          "unknown-rpc.b.co",
		"://bad":                      "",
	}
	for endpoint, want := range cases {
		u := &Upstream{config: &common.UpstreamConfig{Endpoint: endpoint}}
		assert.Equal(t, want, u.guessVendorName(), endpoint)
	}
}

func TestUpstreamState_ClassifyUpstreamOutcome(t *testing.T) {
	cause := errors.New("x")
	req := fwdReq("eth_call")
	cases := []struct {
		name string
		resp *common.NormalizedResponse
		err  error
		want common.UpstreamAttemptOutcome
	}{
		{"cancelled code", nil, common.NewErrEndpointRequestCanceled(cause), common.UpstreamOutcomeCancelled},
		{"context canceled", nil, context.Canceled, common.UpstreamOutcomeCancelled},
		{"dynamic timeout", nil, common.ErrDynamicTimeoutExceeded, common.UpstreamOutcomeTimeout},
		{"method ignored", nil, common.NewErrUpstreamMethodIgnored("m", "u"), common.UpstreamOutcomeSkipped},
		{"capacity", nil, common.NewErrEndpointCapacityExceeded(cause), common.UpstreamOutcomeRateLimited},
		{"exec revert", nil, common.NewErrEndpointExecutionException(cause), common.UpstreamOutcomeExecRevert},
		{"block unavailable", nil, common.NewErrUpstreamBlockUnavailable("u", 10, 5, 1), common.UpstreamOutcomeBlockUnavailable},
		{"transport", nil, common.NewErrEndpointTransportFailure(&url.URL{Host: "h"}, cause), common.UpstreamOutcomeTransportError},
		{"server", nil, common.NewErrEndpointServerSideException(cause, nil, 500), common.UpstreamOutcomeServerError},
		{"client", nil, common.NewErrEndpointClientSideException(cause), common.UpstreamOutcomeClientError},
		{"unclassified error", nil, cause, common.UpstreamOutcomeServerError},
		{"empty result", stubResult(req, `null`), nil, common.UpstreamOutcomeEmpty},
		{"success", stubResult(req, `"0x1"`), nil, common.UpstreamOutcomeSuccess},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, classifyUpstreamOutcome(tc.resp, tc.err), tc.name)
	}
}
