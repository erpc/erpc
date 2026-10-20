package erpc

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

type rlStoreArch struct {
	name    string
	host    string
	mocks   func()
	network func() *common.NetworkConfig
	ups     func() *common.UpstreamConfig
	netId   string
	method  string
	forward func(context.Context, *Network, *common.NormalizedRequest) (*common.NormalizedResponse, error)
}

var rlStoreArchs = []rlStoreArch{
	{
		name:  "evm",
		host:  "rpc1.localhost",
		mocks: util.SetupMocksForEvmStatePoller,
		network: func() *common.NetworkConfig {
			return &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
		},
		ups: func() *common.UpstreamConfig {
			return &common.UpstreamConfig{
				Id: "rpc1", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost",
				Evm: &common.EvmUpstreamConfig{ChainId: 123},
			}
		},
		netId:  util.EvmNetworkId(123),
		method: "eth_call", // eth_chainId is answered before rate limiting
		forward: func(ctx context.Context, n *Network, r *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return n.Forward(ctx, r)
		},
	},
	{
		name:  "svm",
		host:  "svm-rlstore-rpc1.localhost",
		mocks: func() { util.SetupMocksForSvmStatePoller("svm-rlstore-rpc1.localhost", 1000, 990) },
		network: func() *common.NetworkConfig {
			return &common.NetworkConfig{
				Architecture: common.ArchitectureSvm,
				Svm:          &common.SvmNetworkConfig{Cluster: "mainnet-beta", Commitment: "confirmed"},
			}
		},
		ups:     func() *common.UpstreamConfig { return svmUpstreamConfig("rpc1", "svm-rlstore-rpc1.localhost") },
		netId:   util.SvmNetworkId("", "mainnet-beta"),
		method:  "getAccountInfo",
		forward: svmProjectForward,
	},
}

// A budget of 3/hour on a real Redis client, walked through a healthy, slow,
// dead and restarted store, on each architecture, at network and upstream
// level, under each onFailure mode. Requests run one at a time.
func TestNetwork_RateLimitStoreFailureFollowsOnFailure(t *testing.T) {
	type phases struct{ healthy, slow, down, recovered int }
	const healthy, slow, down, recovered = 1, 3, 1, 2
	modes := []struct {
		mode common.RateLimitStoreFailureMode
		// admitted per phase; the decision label store failures carry
		want     phases
		decision string
	}{
		// Failed checks are admitted. Recovered: the slow calls still landed
		// in Redis (1+3 > 3), so Redis denies again.
		{common.RateLimitStoreFailureAllow, phases{1, 3, 1, 0}, "allow"},
		{common.RateLimitStoreFailureDeny, phases{1, 0, 0, 0}, "deny"},
		// The local counter saw the healthy request: 2 more fit in 3/hour.
		{common.RateLimitStoreFailureLocal, phases{1, 2, 0, 0}, "local_"},
	}
	for _, a := range rlStoreArchs {
		for _, level := range []string{"network", "upstream"} {
			for _, m := range modes {
				t.Run(a.name+"/"+level+"/"+string(m.mode), func(t *testing.T) {
					budget := "rl-" + strings.ReplaceAll(t.Name(), "/", "-")
					redis := miniredis.RunT(t)
					proxy := util.NewSlowTCPProxy(t, redis.Addr())
					ntw := setupRateLimitStoreNetwork(t, a, level, budget, proxy.Addr(), m.mode)
					ctx := context.Background()

					failures := func(reason string) float64 {
						sum := 0.0
						for _, d := range []string{"allow", "deny", "local_allow", "local_deny"} {
							if strings.HasPrefix(d, m.decision) {
								sum += testutil.ToFloat64(telemetry.MetricRateLimiterStoreFailureTotal.WithLabelValues(budget, reason, d))
							}
						}
						return sum
					}
					// admitted forwards n requests; every rejection must be a
					// rate-limit error, never an unrelated failure.
					admitted := func(n int) int {
						ok := 0
						for range n {
							_, err := a.forward(ctx, ntw, common.NewNormalizedRequest(
								[]byte(`{"jsonrpc":"2.0","id":1,"method":"`+a.method+`","params":[]}`)))
							if err == nil {
								ok++
								continue
							}
							require.True(t, common.HasErrorCode(err,
								common.ErrCodeNetworkRateLimitRuleExceeded,
								common.ErrCodeUpstreamRateLimitRuleExceeded,
							), "rejection must be a rate limit, got %v", err)
						}
						return ok
					}
					inflightDrained := func() bool {
						return testutil.ToFloat64(telemetry.MetricRateLimiterRemoteInflight.WithLabelValues(budget)) == 0
					}

					require.Equal(t, m.want.healthy, admitted(healthy), "healthy store")

					proxy.SetDelay(200 * time.Millisecond) // > getTimeout 50ms
					before := failures("limit_timeout")
					require.Equal(t, m.want.slow, admitted(slow), "slow store")
					require.Greater(t, failures("limit_timeout"), before, "slow store must count limit_timeout failures")
					proxy.SetDelay(0)
					require.Eventually(t, inflightDrained, 10*time.Second, 10*time.Millisecond)

					redis.Close()
					before = failures("store_error")
					require.Equal(t, m.want.down, admitted(down), "dead store")
					require.Greater(t, failures("store_error"), before, "dead store must count store_error failures")

					require.NoError(t, redis.Restart())
					// The pool redials on its own; wait until Redis decides again.
					require.Eventually(t, func() bool {
						b := failures("store_error")
						admitted(1)
						return failures("store_error") == b
					}, 10*time.Second, 50*time.Millisecond, "restarted store must decide again")
					require.Equal(t, m.want.recovered, admitted(recovered), "restarted store")
				})
			}
		}
	}
}

func setupRateLimitStoreNetwork(t *testing.T, a rlStoreArch, level, budget, redisAddr string, mode common.RateLimitStoreFailureMode) *Network {
	t.Helper()
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	a.mocks()
	gock.New("http://" + a.host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), `"method":"`+a.method+`"`)
		}).
		Reply(200).
		BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store := &common.RateLimitStoreConfig{
		Driver: "redis",
		Redis: &common.RedisConnectorConfig{
			Addr: redisAddr, ConnPoolSize: 4,
			GetTimeout: common.Duration(50 * time.Millisecond),
		},
		CacheKeyPrefix: budget + "_",
		OnFailure:      mode,
	}
	if mode == common.RateLimitStoreFailureLocal {
		store.LocalLimitFactor = 1
	}
	rlCfg := &common.RateLimiterConfig{
		Store: store,
		Budgets: []*common.RateLimitBudgetConfig{{
			Id:    budget,
			Rules: []*common.RateLimitRuleConfig{{Method: "*", MaxCount: 3, Period: common.RateLimitPeriodHour}},
		}},
	}
	require.NoError(t, rlCfg.SetDefaults())
	require.NoError(t, rlCfg.Validate())
	rlr, err := upstream.NewRateLimitersRegistry(ctx, rlCfg, &log.Logger)
	require.NoError(t, err)
	require.NotNil(t, rlr.GetCache(), "redis limiter must connect")

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)
	ssCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	}
	ssCfg.SetDefaults("test")
	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, ssCfg)
	require.NoError(t, err)

	ups := a.ups()
	netCfg := a.network()
	if level == "network" {
		netCfg.RateLimitBudget = budget
	} else {
		ups.RateLimitBudget = budget
	}
	mt := health.NewTracker(&log.Logger, "test", time.Minute)
	upsReg := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "test",
		[]*common.UpstreamConfig{ups}, ssr, rlr, vr, pr, nil, mt, nil)
	ntw, err := NewNetwork(ctx, &log.Logger, "test", netCfg, rlr, upsReg, mt, nil)
	require.NoError(t, err)
	upsReg.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, upsReg.PrepareUpstreamsForNetwork(ctx, a.netId))
	time.Sleep(150 * time.Millisecond)
	return ntw
}
