package erpc

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/consensus"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// countingCacheDAL records Get/Set calls for Forward cache-path assertions.
// When admitDefaultAllow is true, Set only increments when
// AllowsCacheWrite(defaultAllow, resp.Cacheable()) — mirrors real cache
// admission so Forward+consensus mark e2e can assert skipped writes.
type countingCacheDAL struct {
	gets              atomic.Int32
	sets              atomic.Int32
	admittedSets      atomic.Int32
	admitDefaultAllow bool
	hit               *common.NormalizedResponse
}

func (c *countingCacheDAL) Get(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	c.gets.Add(1)
	if c.hit == nil {
		return nil, nil
	}
	return c.hit.WithRequest(req), nil
}

func (c *countingCacheDAL) Set(_ context.Context, _ *common.NormalizedRequest, resp *common.NormalizedResponse) error {
	c.sets.Add(1)
	if c.admitDefaultAllow {
		var mark *bool
		if resp != nil {
			mark = resp.Cacheable()
		}
		if common.AllowsCacheWrite(common.CachingStrategyDefaultAllow, mark) {
			c.admittedSets.Add(1)
		}
	}
	return nil
}

func (c *countingCacheDAL) IsObjectNull() bool { return false }

// fakeSelectorConsensus makes consensusPolicySelectorEnabled return true
// without running a real consensus round. It stamps cacheable=true so the
// Forward unmarked-under-selector Set gate does not skip the write (mirrors
// Consensus.Run stamping the inline/default policy).
type fakeSelectorConsensus struct{}

func (fakeSelectorConsensus) Run(
	ctx context.Context,
	req *common.NormalizedRequest,
	inner func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error),
) (*common.NormalizedResponse, error) {
	resp, err := inner(ctx, req)
	if resp != nil {
		yes := true
		resp.SetCacheable(&yes)
	}
	return resp, err
}

func (fakeSelectorConsensus) HasSelector() bool { return true }

// unmarkedPassThroughConsensus has a selector but never stamps a mark —
// models a miswired path; Forward must not Set under selector + nil mark.
type unmarkedPassThroughConsensus struct{}

func (unmarkedPassThroughConsensus) Run(
	ctx context.Context,
	req *common.NormalizedRequest,
	inner func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error),
) (*common.NormalizedResponse, error) {
	return inner(ctx, req)
}

func (unmarkedPassThroughConsensus) HasSelector() bool { return true }

// TestNetworkForward_SelectorReopensCache locks the write-side admission
// redesign: under a customPolicy selector, Forward consults and writes the
// cache again (historical !selectorOn skip removed). Multiplexing stays gated.
func TestNetworkForward_SelectorReopensCache(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0xupstream"}`))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := setupTestNetworkSimple(t, ctx, nil, &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	})
	// Attach selector+stamp to every executor so the matched failsafe path
	// both enables selectorOn and marks the winner (as Consensus.Run would).
	// HasConsensus requires cfg.Consensus non-nil.
	for _, fe := range network.failsafeExecutors {
		if fe.cfg == nil {
			fe.cfg = &common.FailsafeConfig{}
		}
		if fe.cfg.Consensus == nil {
			fe.cfg.Consensus = &common.ConsensusPolicyConfig{}
		}
		fe.consensus = fakeSelectorConsensus{}
	}
	require.True(t, network.consensusPolicySelectorEnabled())

	forwardBalance := func() (*common.NormalizedResponse, error) {
		req := common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","0x1"]}`,
		))
		req.SetNetwork(network)
		return network.Forward(ctx, req)
	}

	t.Run("selector_cacheHitServed", func(t *testing.T) {
		hit := common.NewNormalizedResponse().
			WithJsonRpcResponse(common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`"0xcached"`), nil))
		cache := &countingCacheDAL{hit: hit}
		network.cacheDal = cache

		resp, err := forwardBalance()
		require.NoError(t, err)
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, `"0xcached"`, jrr.GetResultString())
		assert.Equal(t, int32(1), cache.gets.Load())
		assert.Equal(t, int32(0), cache.sets.Load())
	})

	t.Run("selector_missWritesThrough", func(t *testing.T) {
		cache := &countingCacheDAL{}
		network.cacheDal = cache

		resp, err := forwardBalance()
		require.NoError(t, err)
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, `"0xupstream"`, jrr.GetResultString())
		assert.Equal(t, int32(1), cache.gets.Load())
		require.Eventually(t, func() bool { return cache.sets.Load() >= 1 }, 2*time.Second, 10*time.Millisecond,
			"selector-enabled networks must write through Cache.Set (admission lives in Set)")
	})
}

// TestNetworkForward_SelectorFallbackDoesNotAdmitWrite locks the ticket AC
// that selector+fallback must not persist: a real Consensus stamps
// cacheableResponses:false on the fallback winner, and a Set that applies
// AllowsCacheWrite(defaultAllow, mark) must not admit the write.
func TestNetworkForward_SelectorFallbackDoesNotAdmitWrite(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0xupstream"}`))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := setupTestNetworkSimple(t, ctx, nil, &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	})

	yes, no := true, false
	lg := zerolog.Nop()
	cns, err := consensus.NewConsensus(&common.ConsensusPolicyConfig{
		MaxParticipants:         1,
		AgreementThreshold:      1,
		LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
		CacheableResponses:      &yes,
		CustomPolicy: &common.ConsensusCustomPolicyConfig{
			EvalFunction: `(ctx) => "fallback"`,
		},
		Policies: map[string]*common.ConsensusPolicyConfig{
			"fallback": {
				MaxParticipants:         1,
				AgreementThreshold:      1,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				CacheableResponses:      &no,
			},
		},
	}, &lg, "test")
	require.NoError(t, err)
	require.True(t, cns.HasSelector())

	// Attach the real consensus to every executor so Forward's failsafe path
	// stamps the mark (HasConsensus requires cfg.Consensus non-nil).
	for _, fe := range network.failsafeExecutors {
		if fe.cfg == nil {
			fe.cfg = &common.FailsafeConfig{}
		}
		if fe.cfg.Consensus == nil {
			fe.cfg.Consensus = &common.ConsensusPolicyConfig{}
		}
		fe.consensus = cns
	}
	require.True(t, network.consensusPolicySelectorEnabled())

	cache := &countingCacheDAL{admitDefaultAllow: true}
	network.cacheDal = cache

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","0x1"]}`,
	))
	req.SetNetwork(network)
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp.Cacheable())
	assert.False(t, *resp.Cacheable(), "fallback policy must stamp false")

	require.Eventually(t, func() bool { return cache.sets.Load() >= 1 }, 2*time.Second, 10*time.Millisecond,
		"Forward still invokes Cache.Set; admission lives inside Set")
	assert.Equal(t, int32(0), cache.admittedSets.Load(), "defaultAllow+false must not admit the write")
}

// TestNetworkForward_SelectorSkipsUnmarkedSet locks the Codex P1 that a
// selector-enabled network must not admit nil-mark responses into the shared
// store (defaultAllow would otherwise write them for later selector Gets).
func TestNetworkForward_SelectorSkipsUnmarkedSet(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0xupstream"}`))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := setupTestNetworkSimple(t, ctx, nil, &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	})
	for _, fe := range network.failsafeExecutors {
		if fe.cfg == nil {
			fe.cfg = &common.FailsafeConfig{}
		}
		if fe.cfg.Consensus == nil {
			fe.cfg.Consensus = &common.ConsensusPolicyConfig{}
		}
		fe.consensus = unmarkedPassThroughConsensus{}
	}
	require.True(t, network.consensusPolicySelectorEnabled())

	cache := &countingCacheDAL{admitDefaultAllow: true}
	network.cacheDal = cache

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","0x1"]}`,
	))
	req.SetNetwork(network)
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	assert.Nil(t, resp.Cacheable(), "pass-through left mark unset")
	assert.Equal(t, int32(1), cache.gets.Load())
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(0), cache.sets.Load(), "Forward must not invoke Set for unmarked responses under selector")
	assert.Equal(t, int32(0), cache.admittedSets.Load())
}

// TestNetworkForward_NoConsensusUnchanged keeps the non-selector path green:
// cache Get/Set still run when no customPolicy selector is configured.
func TestNetworkForward_NoConsensusUnchanged(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0xupstream"}`))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := setupTestNetworkSimple(t, ctx, nil, &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	})
	require.False(t, network.consensusPolicySelectorEnabled())

	cache := &countingCacheDAL{}
	network.cacheDal = cache

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","0x1"]}`,
	))
	req.SetNetwork(network)
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Equal(t, `"0xupstream"`, jrr.GetResultString())
	assert.Equal(t, int32(1), cache.gets.Load())
	require.Eventually(t, func() bool { return cache.sets.Load() >= 1 }, 2*time.Second, 10*time.Millisecond)
}
