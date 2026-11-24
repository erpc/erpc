package erpc

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNetworkForward_FilecoinNullRound(t *testing.T) {
	const (
		nullHex   = "0x643970"
		nullEpoch = 6568208
	)
	nullBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"error": map[string]interface{}{
			"code":    12,
			"message": "requested epoch was a null round (6568208)",
			"data":    nullEpoch,
		},
	}

	t.Run("EachUpstreamAskedOnce_NoRetryNoErrorPenalty", func(t *testing.T) {
		util.ResetGock()
		t.Cleanup(util.ResetGock)
		util.SetupMocksForEvmStatePoller()

		var rpc1Calls, rpc2Calls atomic.Int64
		for _, tc := range []struct {
			url   string
			count *atomic.Int64
		}{
			{"http://rpc1.localhost", &rpc1Calls},
			{"http://rpc2.localhost", &rpc2Calls},
		} {
			counter := tc.count
			gock.New(tc.url).
				Post("").
				Filter(func(r *http.Request) bool {
					body := util.SafeReadBody(r)
					return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, nullHex)
				}).
				Persist().
				Reply(200).
				Map(func(r *http.Response) *http.Response {
					counter.Add(1)
					return r
				}).
				JSON(nullBody)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		upsCfg := func(id, endpoint string) *common.UpstreamConfig {
			return &common.UpstreamConfig{
				Id:       id,
				Type:     common.UpstreamTypeEvm,
				Endpoint: endpoint,
				Evm:      &common.EvmUpstreamConfig{ChainId: 123},
				JsonRpc:  &common.JsonRpcUpstreamConfig{SupportsBatch: &common.FALSE},
				Failsafe: []*common.FailsafeConfig{{
					Retry: &common.RetryPolicyConfig{MaxAttempts: 3},
					CircuitBreaker: &common.CircuitBreakerPolicyConfig{
						FailureThresholdCount:    1,
						FailureThresholdCapacity: 1,
						HalfOpenAfter:            common.Duration(time.Minute),
					},
				}},
			}
		}

		network := setupTestNetworkWithCustomUpstreams(
			t,
			ctx,
			[]*common.UpstreamConfig{
				upsCfg("rpc1", "http://rpc1.localhost"),
				upsCfg("rpc2", "http://rpc2.localhost"),
			},
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{MaxAttempts: 4, EmptyResultMaxAttempts: 4},
		)
		network.PinUpstreamOrderForTest("rpc1", "rpc2")

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["` + nullHex + `",false]}`))
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)
		require.Error(t, err)
		assert.Nil(t, resp)

		assert.Equal(t, int64(1), rpc1Calls.Load(), "rpc1 must be asked exactly once (no upstream or network retry)")
		assert.Equal(t, int64(1), rpc2Calls.Load(), "rpc2 must be asked exactly once (no upstream or network retry)")

		translated := common.TranslateToJsonRpcException(err)
		var jre *common.ErrJsonRpcExceptionInternal
		require.True(t, errors.As(translated, &jre))
		assert.Equal(t, common.JsonRpcErrorNumber(12), jre.NormalizedCode())
		assert.Equal(t, "requested epoch was a null round (6568208)", jre.Message)

		for _, u := range network.AllUpstreams() {
			m := network.metricsTracker.GetUpstreamMethodMetrics(u, "eth_getBlockByNumber", common.DataFinalityStateAll)
			assert.Equal(t, int64(0), m.ErrorsTotal.Load(), "null round must not increment upstream ErrorsTotal for %s", u.Id())
		}
	})

	t.Run("MinorityForkNullRound_FailsOverWithoutMisbehavior", func(t *testing.T) {
		util.ResetGock()
		t.Cleanup(util.ResetGock)
		util.SetupMocksForEvmStatePoller()

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, nullHex)
			}).
			Times(1).
			Reply(200).
			JSON(nullBody)

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, nullHex)
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"number":     nullHex,
					"hash":       "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
					"parentHash": "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(
			t,
			ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{MaxAttempts: 3},
		)
		network.PinUpstreamOrderForTest("rpc1", "rpc2")

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["` + nullHex + `",false]}`))
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, jerr := resp.JsonRpcResponse()
		require.NoError(t, jerr)
		assert.Contains(t, jrr.GetResultString(), "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

		for _, u := range network.AllUpstreams() {
			m := network.metricsTracker.GetUpstreamMethodMetrics(u, "eth_getBlockByNumber", common.DataFinalityStateAll)
			assert.Equal(t, int64(0), m.MisbehaviorsTotal.Load(), "null round on a minority fork must not record wrong-empty misbehavior for %s", u.Id())
		}
	})

	t.Run("Consensus_TwoOfThreeNullRounds_ShortCircuitsAndReturnsCode12", func(t *testing.T) {
		util.ResetGock()
		t.Cleanup(util.ResetGock)
		util.SetupMocksForEvmStatePoller()

		var rpc1Calls, rpc2Calls atomic.Int64
		for _, tc := range []struct {
			url   string
			count *atomic.Int64
		}{
			{"http://rpc1.localhost", &rpc1Calls},
			{"http://rpc2.localhost", &rpc2Calls},
		} {
			counter := tc.count
			gock.New(tc.url).
				Post("").
				Filter(func(r *http.Request) bool {
					body := util.SafeReadBody(r)
					return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, nullHex)
				}).
				Persist().
				Reply(200).
				Map(func(r *http.Response) *http.Response {
					counter.Add(1)
					return r
				}).
				JSON(nullBody)
		}

		// rpc3 is slow so we can verify consensus short-circuits on rpc1 + rpc2.
		gock.New("http://rpc3.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, nullHex)
			}).
			Persist().
			Reply(200).
			Delay(2 * time.Second).
			JSON(nullBody)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		upstreamConfigs := []*common.UpstreamConfig{
			{
				Id:       "rpc1",
				Type:     common.UpstreamTypeEvm,
				Endpoint: "http://rpc1.localhost",
				Evm:      &common.EvmUpstreamConfig{ChainId: 123},
				JsonRpc:  &common.JsonRpcUpstreamConfig{SupportsBatch: &common.FALSE},
			},
			{
				Id:       "rpc2",
				Type:     common.UpstreamTypeEvm,
				Endpoint: "http://rpc2.localhost",
				Evm:      &common.EvmUpstreamConfig{ChainId: 123},
				JsonRpc:  &common.JsonRpcUpstreamConfig{SupportsBatch: &common.FALSE},
			},
			{
				Id:       "rpc3",
				Type:     common.UpstreamTypeEvm,
				Endpoint: "http://rpc3.localhost",
				Evm:      &common.EvmUpstreamConfig{ChainId: 123},
				JsonRpc:  &common.JsonRpcUpstreamConfig{SupportsBatch: &common.FALSE},
			},
		}

		networkConfig := &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{ChainId: 123},
			Failsafe: []*common.FailsafeConfig{{
				Retry: &common.RetryPolicyConfig{MaxAttempts: 3, EmptyResultMaxAttempts: 3},
				Consensus: &common.ConsensusPolicyConfig{
					MaxParticipants:         3,
					AgreementThreshold:      2,
					DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
					LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				},
			}},
		}

		rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
		require.NoError(t, err)
		metricsTracker := health.NewTracker(&log.Logger, "test", time.Minute)
		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
		require.NoError(t, err)
		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
			},
		})
		require.NoError(t, err)

		upstreamsRegistry := upstream.NewUpstreamsRegistry(
			ctx, &log.Logger, "test", upstreamConfigs, ssr, rateLimitersRegistry, vr, pr, nil, metricsTracker, nil,
		)
		upstreamsRegistry.Bootstrap(ctx)
		time.Sleep(100 * time.Millisecond)

		network, err := NewNetwork(ctx, &log.Logger, "test", networkConfig, rateLimitersRegistry, upstreamsRegistry, metricsTracker, nil)
		stopAtEnd(t, network)
		require.NoError(t, err)
		require.NoError(t, upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, networkConfig.NetworkId()))
		require.NoError(t, network.Bootstrap(ctx))
		network.PinUpstreamOrderForTest("rpc1", "rpc2", "rpc3")

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["` + nullHex + `",false]}`))
		start := time.Now()
		resp, err := network.Forward(ctx, req)
		elapsed := time.Since(start)

		require.Error(t, err)
		assert.Nil(t, resp)
		assert.Less(t, elapsed, time.Second, "consensus must short-circuit on 2 agreeing null rounds instead of waiting for rpc3")
		assert.Equal(t, int64(1), rpc1Calls.Load(), "agreed permanent null round must not trigger network retries")
		assert.Equal(t, int64(1), rpc2Calls.Load(), "agreed permanent null round must not trigger network retries")

		translated := common.TranslateToJsonRpcException(err)
		var jre *common.ErrJsonRpcExceptionInternal
		require.True(t, errors.As(translated, &jre))
		assert.Equal(t, common.JsonRpcErrorNumber(12), jre.NormalizedCode())
		assert.Equal(t, "requested epoch was a null round (6568208)", jre.Message)
	})
}
