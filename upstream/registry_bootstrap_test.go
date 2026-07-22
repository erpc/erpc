package upstream

import (
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const bootstrapTestEndpoint = "http://rpc1.localhost"

func newBootstrapTestRegistry(t *testing.T) (*UpstreamsRegistry, *common.UpstreamConfig) {
	t.Helper()

	ctx := t.Context()
	logger := zerolog.Nop()

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)

	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1GB",
			},
		},
	})
	require.NoError(t, err)

	rlr, err := NewRateLimitersRegistry(ctx, &common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{},
	}, &logger)
	require.NoError(t, err)

	mt := health.NewTracker(&logger, "test", 2*time.Second)

	cfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: bootstrapTestEndpoint,
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
			// Long interval so ticker goroutines exist but never fire in-test.
			StatePollerInterval: common.Duration(time.Hour),
		},
	}

	reg := NewUpstreamsRegistry(
		ctx, &logger, "test",
		[]*common.UpstreamConfig{cfg},
		ssr, rlr, vr, pr, nil, mt, nil,
	)
	return reg, cfg
}

func gockBodyContains(s string) func(*http.Request) bool {
	return func(r *http.Request) bool {
		return strings.Contains(util.SafeReadBody(r), s)
	}
}

// mockChainIdFailures makes the next `times` eth_chainId calls fail with a
// retryable server error, simulating a provider outage during startup.
func mockChainIdFailures(times int) {
	gock.New(bootstrapTestEndpoint).
		Post("").
		Times(times).
		Filter(gockBodyContains("eth_chainId")).
		Reply(500).
		JSON([]byte(`{"error":{"code":-32603,"message":"simulated startup failure"}}`))
}

// mockUpstreamHealthy serves everything an upstream bootstrap and its state
// poller need: chainId detection plus latest/finalized block and syncing.
func mockUpstreamHealthy() {
	gock.New(bootstrapTestEndpoint).
		Post("").
		Persist().
		Filter(gockBodyContains("eth_chainId")).
		Reply(200).
		JSON([]byte(`{"result":"0x7b"}`))
	gock.New(bootstrapTestEndpoint).
		Post("").
		Persist().
		Filter(gockBodyContains("eth_getBlockByNumber")).
		Reply(200).
		JSON([]byte(`{"result":{"number":"0x11118888","timestamp":"0x6702a8f0"}}`))
	gock.New(bootstrapTestEndpoint).
		Post("").
		Persist().
		Filter(gockBodyContains("eth_syncing")).
		Reply(200).
		JSON([]byte(`{"result":false}`))
}

func TestUpstreamBootstrapTask_RetriedAttemptsReuseSameInstance(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	// First two chainId detections fail (transient), then the endpoint heals.
	mockChainIdFailures(2)
	mockUpstreamHealthy()

	reg, cfg := newBootstrapTestRegistry(t)
	task := reg.buildUpstreamBootstrapTask(cfg)
	ctx := t.Context()

	// Attempt 1: fails, but the created instance must be parked for reuse.
	require.Error(t, task.Fn(ctx))
	v1, ok := reg.pendingUpstreams.Load(cfg.Id)
	require.True(t, ok, "failed attempt must park its Upstream instance for reuse")

	// Attempt 2: fails again and must NOT have built a second instance.
	require.Error(t, task.Fn(ctx))
	v2, ok := reg.pendingUpstreams.Load(cfg.Id)
	require.True(t, ok)
	assert.Same(t, v1, v2, "retried attempt must reuse the instance from the previous attempt")

	// Attempt 3: succeeds; the SAME instance must be the one registered.
	require.NoError(t, task.Fn(ctx))
	all := reg.GetAllUpstreams()
	require.Len(t, all, 1)
	assert.Same(t, v1, all[0], "the registered upstream must be the reused instance")
	require.NotNil(t, all[0].EvmStatePoller(), "registered upstream must have a state poller")

	_, stillPending := reg.pendingUpstreams.Load(cfg.Id)
	assert.False(t, stillPending, "successful registration must clear the pending entry")
}

func TestUpstreamBootstrapTask_ReexecutionDoesNotDuplicatePollersOrRegistrations(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	mockUpstreamHealthy()

	reg, cfg := newBootstrapTestRegistry(t)
	task := reg.buildUpstreamBootstrapTask(cfg)
	ctx := t.Context()

	require.NoError(t, task.Fn(ctx))
	all := reg.GetAllUpstreams()
	require.Len(t, all, 1)
	ups := all[0]
	poller := ups.EvmStatePoller()
	require.NotNil(t, poller)

	time.Sleep(150 * time.Millisecond)
	runtime.GC()
	before := runtime.NumGoroutine()

	// Re-execute the bootstrap task against the already-registered upstream,
	// as a retry loop (or a task re-armed for any reason) would.
	for range 10 {
		require.NoError(t, task.Fn(ctx))
	}

	time.Sleep(150 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()

	assert.True(t, poller == ups.EvmStatePoller(),
		"re-executed bootstrap must not replace the upstream's state poller")
	assert.LessOrEqual(t, after-before, 3,
		"re-executed bootstrap must not accumulate poller goroutines, got %d new goroutines", after-before)
	assert.Len(t, reg.GetAllUpstreams(), 1,
		"re-executed bootstrap must not register the upstream again")
}

func TestUpstreamBootstrapTask_ConcurrentAttemptsConvergeOnSingleInstance(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	// All concurrent attempts fail chainId detection, then the endpoint heals.
	mockChainIdFailures(8)
	mockUpstreamHealthy()

	reg, cfg := newBootstrapTestRegistry(t)
	task := reg.buildUpstreamBootstrapTask(cfg)
	ctx := t.Context()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = task.Fn(ctx)
		}()
	}
	wg.Wait()

	// However many attempts raced, at most one instance may be parked and
	// nothing may be registered yet.
	pendingCount := 0
	var pending any
	reg.pendingUpstreams.Range(func(_, v any) bool {
		pendingCount++
		pending = v
		return true
	})
	require.Equal(t, 1, pendingCount, "concurrent attempts must converge on a single parked instance")
	assert.Empty(t, reg.GetAllUpstreams())

	// Once the endpoint heals, the parked instance is the one registered.
	require.NoError(t, task.Fn(ctx))
	all := reg.GetAllUpstreams()
	require.Len(t, all, 1)
	assert.Same(t, pending, all[0])
}
