package erpc

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/internal/policy/stdlib"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// TestHealthCheckLastEvaluation pins: after the policy engine has run at
// least one tick, the network exposes a non-zero "last eval" timestamp.
// Health-check exporters use this signal to flag stale selection state.
func TestHealthCheckLastEvaluation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	network := createTestNetworkWithSelectionPolicy(t, ctx)

	// Network.Bootstrap registers the network with the engine, which runs
	// an initial synchronous eval — that decision is the LastEvalAt anchor
	// health-check exporters use.
	last := network.policyEngine.LastEvalAt("evm:123", "*", "*")
	assert.False(t, last.IsZero(), "LastEvalAt should be set after Bootstrap's initial tick")
	assert.WithinDuration(t, time.Now(), last, time.Second,
		"LastEvalAt should be approximately now")

	// A subsequent tick advances the timestamp.
	first := last
	time.Sleep(5 * time.Millisecond)
	policy.TickForTest(network.policyEngine, "evm:123", "*")
	last = network.policyEngine.LastEvalAt("evm:123", "*", "*")
	assert.True(t, last.After(first), "LastEvalAt should advance after another tick")
}

func createTestNetworkWithSelectionPolicy(t *testing.T, ctx context.Context) *Network {
	tracker := health.NewTracker(&log.Logger, "test", time.Minute)
	tracker.Bootstrap(ctx)

	upr := upstream.NewUpstreamsRegistry(
		ctx, &log.Logger, "test",
		[]*common.UpstreamConfig{},
		nil, nil, nil, nil, nil,
		tracker,
		nil,
	)

	engine := policy.NewEngine(ctx, &log.Logger, "test", tracker, stdlib.Install, nil)

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		SelectionPolicy: &common.SelectionPolicyConfig{
			EvalInterval: common.Duration(0), // frozen — tests drive ticks manually
			EvalTimeout:  common.Duration(50 * time.Millisecond),
		},
	}
	require.NoError(t, networkConfig.SelectionPolicy.SetDefaults())

	network, err := NewNetwork(ctx, &log.Logger, "test", networkConfig, nil, upr, tracker, engine)
	require.NoError(t, err)

	require.NoError(t, network.Bootstrap(ctx))
	network.PinUpstreamOrderForTest()
	return network
}

// TestHealthCheckSvmNetworkScope pins: domain-aliased SVM proxies scope healthcheck
// by architecture+chainId the same way EVM proxies do, returning id/state in
// networks mode instead of an empty list.
func TestHealthCheckSvmNetworkScope(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForSvmStatePoller("svm-hc-rpc1.localhost", 1000, 990)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := &log.Logger
	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(logger, vr, nil, nil)
	require.NoError(t, err)
	ssr, err := data.NewSharedStateRegistry(ctx, logger, &common.SharedStateConfig{
		ClusterKey: "test",
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems: 100_000, MaxTotalSize: "1GB",
			},
		},
	})
	require.NoError(t, err)
	mtk := health.NewTracker(logger, "test", time.Second)

	svmUp := &common.UpstreamConfig{
		Id:       "svm-upstream",
		Type:     common.UpstreamTypeSvm,
		Endpoint: "http://svm-hc-rpc1.localhost",
		Svm: &common.SvmUpstreamConfig{
			Cluster: "mainnet-beta",
		},
	}

	pp := &PreparedProject{
		Config: &common.ProjectConfig{
			Id:        "solana-mainnet-beta",
			Upstreams: []*common.UpstreamConfig{svmUp},
			Networks: []*common.NetworkConfig{
				{
					Architecture: common.ArchitectureSvm,
					Svm:          &common.SvmNetworkConfig{Cluster: "mainnet-beta"},
				},
			},
		},
		upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, logger, "", []*common.UpstreamConfig{svmUp}, ssr, nil, vr, pr, nil, mtk, nil),
	}
	pp.upstreamsRegistry.Bootstrap(ctx)
	pp.networksRegistry = NewNetworksRegistry(pp, ctx, pp.upstreamsRegistry, mtk, nil, nil, nil, nil, logger)

	s := &HttpServer{
		logger: logger,
		erpc: &ERPC{
			projectsRegistry: &ProjectsRegistry{
				preparedProjects: map[string]*PreparedProject{
					"solana-mainnet-beta": pp,
				},
			},
		},
		healthCheckCfg: &common.HealthCheckConfig{
			Mode: common.HealthCheckModeNetworks,
		},
		draining: &atomic.Bool{},
	}

	time.Sleep(200 * time.Millisecond)

	w := httptest.NewRecorder()
	startTime := time.Now()
	encoder := common.SonicCfg.NewEncoder(w)
	s.handleHealthCheck(
		ctx, w,
		&http.Request{Method: "GET", URL: &url.URL{Path: "/"}},
		&startTime,
		"solana-mainnet-beta", "svm", "mainnet-beta",
		encoder,
		func(ctx context.Context, statusCode int, body error) {
			w.WriteHeader(statusCode)
			_ = encoder.Encode(map[string]string{"error": body.Error()})
		},
	)

	resp := w.Result()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), `"id":"svm:mainnet-beta"`)
	assert.Contains(t, string(body), `"state":"OK"`)
}
