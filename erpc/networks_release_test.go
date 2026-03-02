package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	evm "github.com/erpc/erpc/architecture/evm"
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

func init() {
	util.ConfigureTestLogger()
}

// TestNetworkForward_CacheSetDoesNotBlockForward proves that the cache-set
// goroutine no longer holds an RLock on the response, so Forward returns
// immediately even when the cache write is slow. Before the fix, this would
// deadlock because the cache-set goroutine held resp.RLock and then tried
// to acquire resp.Lock inside cacheDal.Set → resp.JsonRpcResponse().
func TestNetworkForward_CacheSetDoesNotBlockForward(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	cacheCfg := &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{
			{
				Id:     "slow-mock",
				Driver: "mock",
				Mock: &common.MockConnectorConfig{
					MemoryConnectorConfig: common.MemoryConnectorConfig{
						MaxItems: 100_000, MaxTotalSize: "1GB",
					},
					SetDelay: 2 * time.Second,
				},
			},
		},
		Policies: []*common.CachePolicyConfig{
			{
				Network:   "*",
				Method:    "*",
				TTL:       common.Duration(5 * time.Minute),
				Connector: "slow-mock",
			},
		},
	}
	cacheCfg.SetDefaults()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkSimple(t, ctx, nil, nil)

	slowCache, err := evm.NewEvmJsonRpcCache(ctx, &log.Logger, cacheCfg)
	require.NoError(t, err)
	network.cacheDal = slowCache.WithProjectId("test")

	gock.New("http://rpc1.localhost").
		Post("/").
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getBalance")
		}).
		Times(1).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0xdeadbeef",
		})

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0xabc","latest"],"id":1}`))
	req.SetNetwork(network)

	start := time.Now()
	resp, err := network.Forward(ctx, req)
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, jrrErr := resp.JsonRpcResponse()
	require.NoError(t, jrrErr)
	assert.Equal(t, "\"0xdeadbeef\"", jrr.GetResultString())

	// Forward must return well under the 2s cache SetDelay.
	// If it blocks on cache-set, it would take >=2s.
	assert.Less(t, elapsed, 1*time.Second,
		"Forward should not block on slow cache-set goroutine")

	// Release the response (simulates what http_server does).
	// This will block until cache-set finishes (pendingOps.Wait), but since
	// releaseOnce makes it idempotent, calling it again is safe.
	go resp.Release()

	// Allow background cache-set to finish
	time.Sleep(3 * time.Second)
}

// TestNetworkForward_DoubleReleaseIsSafe proves that calling Release() twice
// on the same NormalizedResponse does not panic or corrupt state. This is
// critical for flows where consensus and the HTTP handler may both release.
func TestNetworkForward_DoubleReleaseIsSafe(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkSimple(t, ctx, nil, nil)

	gock.New("http://rpc1.localhost").
		Post("/").
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getBalance")
		}).
		Times(1).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x1234",
		})

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0xabc","latest"],"id":1}`))
	req.SetNetwork(network)
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, _ := resp.JsonRpcResponse()
	require.NotNil(t, jrr)
	assert.Equal(t, "\"0x1234\"", jrr.GetResultString())

	// First release
	resp.Release()

	// Second release — must be a no-op, no panic
	assert.NotPanics(t, func() {
		resp.Release()
	}, "double Release() must not panic")

	// Third release — still safe
	assert.NotPanics(t, func() {
		resp.Release()
	}, "triple Release() must not panic")
}

// TestNetworkForward_ConsensusErrorPathClearsLVRWithoutDoubleFree proves that
// when all consensus participants return errors, the LVR is cleared without
// double-releasing. Before the fix, network.Forward would call lvr.Release()
// on a response that the consensus executor had already released.
func TestNetworkForward_ConsensusErrorPathClearsLVRWithoutDoubleFree(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)
	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "100MB"},
		},
	})
	require.NoError(t, err)

	upstreams := createTestUpstreams(3)
	upsReg := upstream.NewUpstreamsRegistry(
		ctx, &log.Logger, "prjA", upstreams,
		ssr, nil, vr, pr, nil, mt, 1*time.Second, nil, nil,
	)

	consensusCfg := &common.ConsensusPolicyConfig{
		MaxParticipants:    3,
		AgreementThreshold: 2,
	}
	require.NoError(t, consensusCfg.SetDefaults())

	ntw, err := NewNetwork(
		ctx, &log.Logger, "prjA",
		&common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{ChainId: 123},
			Failsafe: []*common.FailsafeConfig{
				{
					MatchMethod: "*",
					Consensus:   consensusCfg,
				},
			},
		},
		nil, upsReg, mt,
	)
	require.NoError(t, err)

	upsReg.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, upsReg.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	upstream.ReorderUpstreams(upsReg)
	time.Sleep(200 * time.Millisecond)

	// All 3 upstreams return JSON-RPC errors (execution reverts)
	for i := 1; i <= 3; i++ {
		gock.New(fmt.Sprintf("http://rpc%d.localhost", i)).
			Post("/").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_call")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    3,
					"message": "execution reverted",
				},
			})
	}

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_call","params":[{"to":"0x1234"},"latest"],"id":1}`))
	req.SetNetwork(ntw)

	// This must not panic from double-release of LVR
	assert.NotPanics(t, func() {
		resp, fwdErr := ntw.Forward(ctx, req)
		// Consensus with all errors should return an error
		assert.Error(t, fwdErr)
		assert.Nil(t, resp)
		// LVR should be cleared
		assert.Nil(t, req.LastValidResponse())
	}, "consensus error path must not panic from double-release")
}

// TestNetworkForward_ConcurrentForwardAndReleaseWithCache runs multiple
// concurrent Forward calls with a slow cache to prove there are no data races.
// Run with: go test -race -run TestNetworkForward_ConcurrentForwardAndReleaseWithCache
func TestNetworkForward_ConcurrentForwardAndReleaseWithCache(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	cacheCfg := &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{
			{
				Id:     "mock",
				Driver: "mock",
				Mock: &common.MockConnectorConfig{
					MemoryConnectorConfig: common.MemoryConnectorConfig{
						MaxItems: 100_000, MaxTotalSize: "1GB",
					},
					SetDelay: 200 * time.Millisecond,
				},
			},
		},
		Policies: []*common.CachePolicyConfig{
			{
				Network:   "*",
				Method:    "*",
				TTL:       common.Duration(5 * time.Minute),
				Connector: "mock",
			},
		},
	}
	cacheCfg.SetDefaults()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkSimple(t, ctx, nil, nil)

	cache, err := evm.NewEvmJsonRpcCache(ctx, &log.Logger, cacheCfg)
	require.NoError(t, err)
	network.cacheDal = cache.WithProjectId("test")

	const numRequests = 10

	// Each request gets its own unique gock mock
	for i := 0; i < numRequests; i++ {
		gock.New("http://rpc1.localhost").
			Post("/").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xabc",
			})
	}

	var wg sync.WaitGroup
	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0xabc","latest"],"id":1}`))
			req.SetNetwork(network)
			resp, fwdErr := network.Forward(ctx, req)
			if fwdErr != nil {
				return
			}
			if resp == nil {
				return
			}
			// Read from the response (concurrent with cache-set goroutine reading it)
			jrr, _ := resp.JsonRpcResponse()
			if jrr != nil {
				_ = jrr.GetResultString()
			}
			// Immediately release (concurrent with cache-set goroutine)
			resp.Release()
		}()
	}

	wg.Wait()
	// Allow cache-set goroutines to drain
	time.Sleep(500 * time.Millisecond)
}
