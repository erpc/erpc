package upstream

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
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// taskByName picks the named task from an InitializerStatus snapshot.
func taskByName(t *testing.T, status *util.InitializerStatus, name string) util.TaskStatus {
	t.Helper()
	require.NotNil(t, status)
	for _, ts := range status.Tasks {
		if ts.Name == name {
			return ts
		}
	}
	t.Fatalf("task %q not found in initializer status; saw: %v", name, status.Tasks)
	return util.TaskStatus{}
}

func mockFakeUpstreamEndpoint(host string, chainIdHex string) {
	gock.New(host).
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			return strings.Contains(util.SafeReadBody(request), "eth_chainId")
		}).
		Reply(200).
		JSON([]byte(`{"result":"` + chainIdHex + `"}`))
	gock.New(host).
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getBlockByNumber")
		}).
		Reply(200).
		JSON([]byte(`{"result":{"number":"0x1","timestamp":"0x6702a8f0"}}`))
	gock.New(host).
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			return strings.Contains(util.SafeReadBody(request), "eth_syncing")
		}).
		Reply(200).
		JSON([]byte(`{"result":false}`))
}

// fakeVendor is a controllable common.Vendor used to exercise the
// provider bootstrap task. The behavior of GenerateConfigs and SupportsNetwork
// is driven by atomic counters and the supplied closures, so tests can
// simulate empty results, errors, or recovery on a subsequent attempt.
type fakeVendor struct {
	name             string
	calls            atomic.Int32
	supportsCalls    atomic.Int32
	supportsNetwork  func(networkId string, attempt int32) (bool, error)
	generateConfigs  func(base *common.UpstreamConfig, attempt int32) ([]*common.UpstreamConfig, error)
	ownsUpstreamFunc func(*common.UpstreamConfig) bool
}

func (f *fakeVendor) Name() string { return f.name }

func (f *fakeVendor) OwnsUpstream(u *common.UpstreamConfig) bool {
	if f.ownsUpstreamFunc != nil {
		return f.ownsUpstreamFunc(u)
	}
	return u.VendorName == f.name
}

func (f *fakeVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, baseConfig *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	n := f.calls.Add(1)
	if f.generateConfigs != nil {
		return f.generateConfigs(baseConfig, n)
	}
	return nil, nil
}

func (f *fakeVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	n := f.supportsCalls.Add(1)
	if f.supportsNetwork != nil {
		return f.supportsNetwork(networkId, n)
	}
	return true, nil
}

func (f *fakeVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, bodyObject interface{}, details map[string]interface{}) error {
	return nil
}

func newRegistryWithFakeProvider(t *testing.T, ctx context.Context, vendor *fakeVendor, supportedNetwork string) (*UpstreamsRegistry, *thirdparty.Provider) {
	t.Helper()
	logger := log.Logger

	vr := thirdparty.NewVendorsRegistry()
	vr.Register(vendor)

	providerCfg := &common.ProviderConfig{
		Id:                 "fakeprov",
		Vendor:             vendor.name,
		UpstreamIdTemplate: "<PROVIDER>-<NETWORK>",
		OnlyNetworks:       []string{supportedNetwork},
	}
	pr, err := thirdparty.NewProvidersRegistry(&logger, vr, []*common.ProviderConfig{providerCfg}, nil)
	require.NoError(t, err)

	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	require.NoError(t, err)

	tracker := health.NewTracker(&logger, "test-project", 5*time.Second)
	tracker.Bootstrap(ctx)

	reg := NewUpstreamsRegistry(ctx, &logger, "test-project", nil, ssr, nil, vr, pr, nil, tracker,
		1*time.Second,
		&ScoringConfig{RoutingStrategy: "round-robin"},
		nil,
	)
	require.NotNil(t, reg)

	providers := pr.GetAllProviders()
	require.Len(t, providers, 1)
	return reg, providers[0]
}

// When a provider claims to support a network but yields zero upstream configs,
// the bootstrap task must return an error rather than silently succeed.
// Returning nil here used to mark the task TaskSucceeded, which made
// summarizeNetworkTasks treat the network as terminally resolved with zero
// upstreams and caused ErrNetworkInitializing to be surfaced on every request
// until the process was restarted.
func TestBuildProviderBootstrapTask_ZeroConfigsReturnsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	vendor := &fakeVendor{
		name: "fakevendor",
		generateConfigs: func(_ *common.UpstreamConfig, _ int32) ([]*common.UpstreamConfig, error) {
			return nil, nil
		},
	}
	reg, provider := newRegistryWithFakeProvider(t, ctx, vendor, "evm:4441")

	task := reg.buildProviderBootstrapTask(provider, "evm:4441")
	require.NotNil(t, task)

	err := reg.initializer.ExecuteTasks(ctx, task)
	require.Error(t, err, "ExecuteTasks should surface the task error")

	ts := taskByName(t, reg.initializer.Status(), "network/evm:4441/provider/fakeprov")
	assert.Equal(t, util.TaskFailed, ts.State,
		"task must end up TaskFailed, not TaskSucceeded, when 0 configs are produced")
	require.NotNil(t, ts.Err)
	assert.Contains(t, ts.Err.Error(), "0 upstream configs")
}

// Control: when the vendor produces at least one config the task succeeds.
func TestBuildProviderBootstrapTask_NonZeroConfigsSucceeds(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	mockFakeUpstreamEndpoint("http://fake.localhost", "0x1159")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	vendor := &fakeVendor{
		name: "fakevendor",
		generateConfigs: func(base *common.UpstreamConfig, _ int32) ([]*common.UpstreamConfig, error) {
			return []*common.UpstreamConfig{{
				Id:       base.Id,
				Endpoint: "http://fake.localhost",
				Type:     common.UpstreamTypeEvm,
				Evm:      &common.EvmUpstreamConfig{ChainId: 4441},
			}}, nil
		},
	}
	reg, provider := newRegistryWithFakeProvider(t, ctx, vendor, "evm:4441")

	task := reg.buildProviderBootstrapTask(provider, "evm:4441")
	require.NoError(t, reg.initializer.ExecuteTasks(ctx, task))

	ts := taskByName(t, reg.initializer.Status(), "network/evm:4441/provider/fakeprov")
	assert.Equal(t, util.TaskSucceeded, ts.State)
}

// Control: when the vendor returns an explicit error the original error
// is preserved (no behavior change for this path).
func TestBuildProviderBootstrapTask_VendorErrorPropagates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	vendorErr := errors.New("vendor remote-data cache not yet populated; retry shortly")
	vendor := &fakeVendor{
		name: "fakevendor",
		generateConfigs: func(_ *common.UpstreamConfig, _ int32) ([]*common.UpstreamConfig, error) {
			return nil, vendorErr
		},
	}
	reg, provider := newRegistryWithFakeProvider(t, ctx, vendor, "evm:4441")

	task := reg.buildProviderBootstrapTask(provider, "evm:4441")
	err := reg.initializer.ExecuteTasks(ctx, task)
	require.Error(t, err)

	ts := taskByName(t, reg.initializer.Status(), "network/evm:4441/provider/fakeprov")
	assert.Equal(t, util.TaskFailed, ts.State)
	require.NotNil(t, ts.Err)
	assert.Contains(t, ts.Err.Error(), "vendor remote-data cache not yet populated")
}

// When a provider does not support a network, the task should succeed without
// invoking GenerateUpstreamConfigs. This guards against accidentally promoting
// the "zero configs" check above into a false positive for unsupported networks.
func TestBuildProviderBootstrapTask_UnsupportedNetworkIsSilent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	vendor := &fakeVendor{
		name: "fakevendor",
		supportsNetwork: func(networkId string, _ int32) (bool, error) {
			return false, nil
		},
		generateConfigs: func(_ *common.UpstreamConfig, _ int32) ([]*common.UpstreamConfig, error) {
			t.Fatalf("GenerateConfigs must not be called for unsupported networks")
			return nil, nil
		},
	}
	// Provider is configured with OnlyNetworks so SupportsNetwork short-circuits
	// for any other network without consulting the vendor at all.
	reg, provider := newRegistryWithFakeProvider(t, ctx, vendor, "evm:4441")

	task := reg.buildProviderBootstrapTask(provider, "evm:9999")
	require.NoError(t, reg.initializer.ExecuteTasks(ctx, task))

	ts := taskByName(t, reg.initializer.Status(), "network/evm:9999/provider/fakeprov")
	assert.Equal(t, util.TaskSucceeded, ts.State,
		"unsupported networks should still resolve to a successful no-op task")
}

// Integration: a provider that yields zero configs on the first attempt must
// not poison the network. Re-running the bootstrap task with a now-healthy
// vendor must succeed and register the upstream.
func TestBuildProviderBootstrapTask_RecoversAfterEmptyResult(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	mockFakeUpstreamEndpoint("http://fake.localhost", "0x1159")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	vendor := &fakeVendor{
		name: "fakevendor",
		generateConfigs: func(base *common.UpstreamConfig, attempt int32) ([]*common.UpstreamConfig, error) {
			if attempt == 1 {
				return nil, nil
			}
			return []*common.UpstreamConfig{{
				Id:       base.Id,
				Endpoint: "http://fake.localhost",
				Type:     common.UpstreamTypeEvm,
				Evm:      &common.EvmUpstreamConfig{ChainId: 4441},
			}}, nil
		},
	}
	reg, provider := newRegistryWithFakeProvider(t, ctx, vendor, "evm:4441")

	// First attempt: empty result -> task must fail, leaving it eligible to retry.
	task1 := reg.buildProviderBootstrapTask(provider, "evm:4441")
	err := reg.initializer.ExecuteTasks(ctx, task1)
	require.Error(t, err)
	ts1 := taskByName(t, reg.initializer.Status(), "network/evm:4441/provider/fakeprov")
	require.Equal(t, util.TaskFailed, ts1.State)

	// Second attempt: vendor recovered -> task succeeds and upstream lands in the registry.
	task2 := reg.buildProviderBootstrapTask(provider, "evm:4441")
	require.NoError(t, reg.initializer.ExecuteTasks(ctx, task2))

	ups := reg.GetNetworkUpstreams(ctx, "evm:4441")
	assert.NotEmpty(t, ups, "upstream must be registered once the vendor recovers")
}
