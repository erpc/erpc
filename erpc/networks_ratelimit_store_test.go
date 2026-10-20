package erpc

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// A network budget on a Redis store that dies decides by onFailure through the
// whole forward path, the same way for EVM and SVM networks.
func TestNetwork_RateLimitStoreFailureFollowsOnFailure(t *testing.T) {
	archs := []struct {
		name    string
		host    string
		mocks   func()
		network *common.NetworkConfig
		ups     *common.UpstreamConfig
		netId   string
		method  string
		forward func(context.Context, *Network, *common.NormalizedRequest) (*common.NormalizedResponse, error)
	}{
		{
			name:    "evm",
			host:    "rpc1.localhost",
			mocks:   util.SetupMocksForEvmStatePoller,
			network: &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}},
			ups: &common.UpstreamConfig{
				Id: "rpc1", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost",
				Evm: &common.EvmUpstreamConfig{ChainId: 123},
			},
			netId:  util.EvmNetworkId(123),
			method: "eth_call",
			forward: func(ctx context.Context, n *Network, r *common.NormalizedRequest) (*common.NormalizedResponse, error) {
				return n.Forward(ctx, r)
			},
		},
		{
			name:  "svm",
			host:  "svm-rlstore-rpc1.localhost",
			mocks: func() { util.SetupMocksForSvmStatePoller("svm-rlstore-rpc1.localhost", 1000, 990) },
			network: &common.NetworkConfig{
				Architecture: common.ArchitectureSvm,
				Svm:          &common.SvmNetworkConfig{Cluster: "mainnet-beta", Commitment: "confirmed"},
			},
			ups:     svmUpstreamConfig("rpc1", "svm-rlstore-rpc1.localhost"),
			netId:   util.SvmNetworkId("", "mainnet-beta"),
			method:  "getAccountInfo",
			forward: svmProjectForward,
		},
	}
	modes := []struct {
		mode    common.RateLimitStoreFailureMode
		allowed bool
	}{
		{common.RateLimitStoreFailureAllow, true},
		{common.RateLimitStoreFailureDeny, false},
	}
	for _, a := range archs {
		for _, m := range modes {
			t.Run(a.name+"/"+string(m.mode), func(t *testing.T) {
				util.ResetGock()
				defer util.ResetGock()
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
				defer cancel()

				redis := miniredis.RunT(t)
				rlCfg := &common.RateLimiterConfig{
					Store: &common.RateLimitStoreConfig{
						Driver: "redis",
						Redis: &common.RedisConnectorConfig{
							Addr: redis.Addr(), ConnPoolSize: 4,
							GetTimeout: common.Duration(50 * time.Millisecond),
						},
						CacheKeyPrefix: t.Name() + "_",
						OnFailure:      m.mode,
					},
					Budgets: []*common.RateLimitBudgetConfig{{
						Id:    "net",
						Rules: []*common.RateLimitRuleConfig{{Method: "*", MaxCount: 100, Period: common.RateLimitPeriodHour}},
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
				mt := health.NewTracker(&log.Logger, "test", time.Minute)
				upsReg := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "test",
					[]*common.UpstreamConfig{a.ups}, ssr, rlr, vr, pr, nil, mt, nil)
				netCfg := *a.network
				netCfg.RateLimitBudget = "net"
				ntw, err := NewNetwork(ctx, &log.Logger, "test", &netCfg, rlr, upsReg, mt, nil)
				require.NoError(t, err)
				upsReg.Bootstrap(ctx)
				time.Sleep(100 * time.Millisecond)
				require.NoError(t, upsReg.PrepareUpstreamsForNetwork(ctx, a.netId))
				time.Sleep(150 * time.Millisecond)

				req := func() error {
					_, err := a.forward(ctx, ntw, common.NewNormalizedRequest(
						[]byte(`{"jsonrpc":"2.0","id":1,"method":"`+a.method+`","params":[]}`)))
					return err
				}
				require.NoError(t, req(), "healthy store admits within the limit")

				redis.Close()
				err = req()
				var exceeded *common.ErrNetworkRateLimitRuleExceeded
				if m.allowed {
					require.NoError(t, err, "dead store under %s must admit", m.mode)
				} else {
					require.True(t, errors.As(err, &exceeded), "dead store under %s must reject as rate limited, got %v", m.mode, err)
				}
			})
		}
	}
}
