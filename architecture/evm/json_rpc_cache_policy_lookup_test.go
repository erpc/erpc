package evm

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestFindGetPolicies_PreservesOrderingAcrossIndexBuckets(t *testing.T) {
	connectorA := data.NewMockConnector("connector-a")
	connectorB := data.NewMockConnector("connector-b")
	connectorC := data.NewMockConnector("connector-c")

	policy0 := mustNewTestCachePolicy(t, connectorB, &common.CachePolicyConfig{
		Connector: connectorB.Id(),
		Network:   "*",
		Method:    "eth_getBalance",
		Finality:  common.DataFinalityStateFinalized,
		TTL:       common.Duration(time.Minute),
	})
	policy1 := mustNewTestCachePolicy(t, connectorA, &common.CachePolicyConfig{
		Connector: connectorA.Id(),
		Network:   "evm:1",
		Method:    "eth_getBalance",
		Finality:  common.DataFinalityStateFinalized,
		TTL:       common.Duration(time.Minute),
	})
	policy2 := mustNewTestCachePolicy(t, connectorC, &common.CachePolicyConfig{
		Connector: connectorC.Id(),
		Network:   "evm:1",
		Method:    "eth_*",
		Finality:  common.DataFinalityStateFinalized,
		TTL:       common.Duration(time.Minute),
	})
	// Same connector as policy1, should be de-duped after policy1 matches.
	policy3 := mustNewTestCachePolicy(t, connectorA, &common.CachePolicyConfig{
		Connector: connectorA.Id(),
		Network:   "evm:*",
		Method:    "eth_getBalance",
		Finality:  common.DataFinalityStateFinalized,
		TTL:       common.Duration(time.Minute),
	})

	cache := newTestCacheWithPolicies(t, []*data.CachePolicy{policy0, policy1, policy2, policy3})
	policies, err := cache.findGetPolicies(
		"evm:1",
		"eth_getBalance",
		nil,
		common.DataFinalityStateFinalized,
	)

	require.NoError(t, err)
	require.Equal(t, []*data.CachePolicy{policy0, policy1, policy2}, policies)
}

func TestFindGetPolicies_UnknownFinalityMatchesAllPolicyFinalities(t *testing.T) {
	connectorA := data.NewMockConnector("connector-a")
	connectorB := data.NewMockConnector("connector-b")
	connectorC := data.NewMockConnector("connector-c")
	connectorD := data.NewMockConnector("connector-d")

	policyFinalized := mustNewTestCachePolicy(t, connectorA, &common.CachePolicyConfig{
		Connector: connectorA.Id(),
		Network:   "evm:1",
		Method:    "eth_getBlockByHash",
		Finality:  common.DataFinalityStateFinalized,
		TTL:       common.Duration(time.Minute),
	})
	policyUnfinalized := mustNewTestCachePolicy(t, connectorB, &common.CachePolicyConfig{
		Connector: connectorB.Id(),
		Network:   "evm:1",
		Method:    "eth_getBlockByHash",
		Finality:  common.DataFinalityStateUnfinalized,
		TTL:       common.Duration(time.Minute),
	})
	policyRealtime := mustNewTestCachePolicy(t, connectorC, &common.CachePolicyConfig{
		Connector: connectorC.Id(),
		Network:   "evm:1",
		Method:    "eth_getBlockByHash",
		Finality:  common.DataFinalityStateRealtime,
		TTL:       common.Duration(time.Minute),
	})
	policyUnknown := mustNewTestCachePolicy(t, connectorD, &common.CachePolicyConfig{
		Connector: connectorD.Id(),
		Network:   "evm:1",
		Method:    "eth_getBlockByHash",
		Finality:  common.DataFinalityStateUnknown,
		TTL:       common.Duration(time.Minute),
	})

	cache := newTestCacheWithPolicies(t, []*data.CachePolicy{
		policyFinalized,
		policyUnfinalized,
		policyRealtime,
		policyUnknown,
	})

	policies, err := cache.findGetPolicies(
		"evm:1",
		"eth_getBlockByHash",
		[]interface{}{"0xabc"},
		common.DataFinalityStateUnknown,
	)

	require.NoError(t, err)
	require.Equal(t, []*data.CachePolicy{
		policyFinalized,
		policyUnfinalized,
		policyRealtime,
		policyUnknown,
	}, policies)
}

func TestCurrentPolicySnapshot_LegacyFallbackConcurrent(t *testing.T) {
	connector := data.NewMockConnector("connector-a")
	policy := mustNewTestCachePolicy(t, connector, &common.CachePolicyConfig{
		Connector: connector.Id(),
		Network:   "evm:1",
		Method:    "eth_chainId",
		Finality:  common.DataFinalityStateFinalized,
		TTL:       common.Duration(time.Minute),
	})

	logger := zerolog.Nop()
	cache := &EvmJsonRpcCache{
		logger:   &logger,
		policies: []*data.CachePolicy{policy},
	}

	const workers = 32
	snapshots := make(chan *cachePolicySnapshot, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snapshots <- cache.currentPolicySnapshot()
		}()
	}
	wg.Wait()
	close(snapshots)

	var first *cachePolicySnapshot
	for snap := range snapshots {
		require.NotNil(t, snap)
		if first == nil {
			first = snap
			continue
		}
		require.Same(t, first, snap)
	}
	require.NotNil(t, first)
	require.Len(t, first.policies, 1)
}

func TestFindGetPolicies_MultiMatchConnectorDedupUsesFirstMatchingPolicy(t *testing.T) {
	connectorA := data.NewMockConnector("connector-a")
	connectorB := data.NewMockConnector("connector-b")

	policyNoMatch := mustNewTestCachePolicy(t, connectorA, &common.CachePolicyConfig{
		Connector: connectorA.Id(),
		Network:   "evm:1",
		Method:    "eth_call",
		Params:    []interface{}{"0xdeadbeef"},
		Finality:  common.DataFinalityStateRealtime,
		TTL:       common.Duration(time.Minute),
	})
	policyMatchFirst := mustNewTestCachePolicy(t, connectorA, &common.CachePolicyConfig{
		Connector: connectorA.Id(),
		Network:   "evm:1",
		Method:    "eth_call",
		Params:    []interface{}{"0x*"},
		Finality:  common.DataFinalityStateRealtime,
		TTL:       common.Duration(time.Minute),
	})
	// Matches too, but same connector as policyMatchFirst; should be skipped.
	policyMatchSecondSameConnector := mustNewTestCachePolicy(t, connectorA, &common.CachePolicyConfig{
		Connector: connectorA.Id(),
		Network:   "evm:1",
		Method:    "eth_call",
		Finality:  common.DataFinalityStateRealtime,
		TTL:       common.Duration(time.Minute),
	})
	policyMatchOtherConnector := mustNewTestCachePolicy(t, connectorB, &common.CachePolicyConfig{
		Connector: connectorB.Id(),
		Network:   "evm:1",
		Method:    "eth_call",
		Params:    []interface{}{"0x*"},
		Finality:  common.DataFinalityStateRealtime,
		TTL:       common.Duration(time.Minute),
	})

	cache := newTestCacheWithPolicies(t, []*data.CachePolicy{
		policyNoMatch,
		policyMatchFirst,
		policyMatchSecondSameConnector,
		policyMatchOtherConnector,
	})

	policies, err := cache.findGetPolicies(
		"evm:1",
		"eth_call",
		[]interface{}{"0xbeef"},
		common.DataFinalityStateRealtime,
	)

	require.NoError(t, err)
	require.Equal(t, []*data.CachePolicy{policyMatchFirst, policyMatchOtherConnector}, policies)
}

func TestFindGetPolicies_SetPoliciesRebuildsLookup(t *testing.T) {
	connectorA := data.NewMockConnector("connector-a")
	connectorB := data.NewMockConnector("connector-b")

	initialPolicy := mustNewTestCachePolicy(t, connectorA, &common.CachePolicyConfig{
		Connector: connectorA.Id(),
		Network:   "evm:2",
		Method:    "eth_getBalance",
		Finality:  common.DataFinalityStateFinalized,
		TTL:       common.Duration(time.Minute),
	})
	updatedPolicy := mustNewTestCachePolicy(t, connectorB, &common.CachePolicyConfig{
		Connector: connectorB.Id(),
		Network:   "evm:1",
		Method:    "eth_getBalance",
		Finality:  common.DataFinalityStateFinalized,
		TTL:       common.Duration(time.Minute),
	})

	cache := newTestCacheWithPolicies(t, []*data.CachePolicy{initialPolicy})

	before, err := cache.findGetPolicies("evm:1", "eth_getBalance", nil, common.DataFinalityStateFinalized)
	require.NoError(t, err)
	require.Len(t, before, 0)

	cache.SetPolicies([]*data.CachePolicy{updatedPolicy})

	after, err := cache.findGetPolicies("evm:1", "eth_getBalance", nil, common.DataFinalityStateFinalized)
	require.NoError(t, err)
	require.Equal(t, []*data.CachePolicy{updatedPolicy}, after)
}

func BenchmarkFindGetPolicies_Indexed50(b *testing.B) {
	cache := newBenchmarkCacheWithPolicies(b, 50)
	params := []interface{}{"0xabc"}
	finality := common.DataFinalityStateFinalized

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		policies, err := cache.findGetPolicies("evm:1", "eth_getBalance", params, finality)
		if err != nil {
			b.Fatalf("findGetPolicies failed: %v", err)
		}
		if len(policies) != 1 {
			b.Fatalf("expected exactly 1 policy, got %d", len(policies))
		}
	}
}

func BenchmarkFindGetPolicies_Linear50(b *testing.B) {
	cache := newBenchmarkCacheWithPolicies(b, 50)
	allPolicies := cache.currentPolicySnapshot().policies
	params := []interface{}{"0xabc"}
	finality := common.DataFinalityStateFinalized

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		policies, err := findGetPoliciesLinearForBenchmark(allPolicies, "evm:1", "eth_getBalance", params, finality)
		if err != nil {
			b.Fatalf("linear benchmark failed: %v", err)
		}
		if len(policies) != 1 {
			b.Fatalf("expected exactly 1 policy, got %d", len(policies))
		}
	}
}

func newBenchmarkCacheWithPolicies(tb testing.TB, policyCount int) *EvmJsonRpcCache {
	tb.Helper()
	if policyCount < 2 {
		tb.Fatalf("policyCount must be >= 2")
	}

	policies := make([]*data.CachePolicy, 0, policyCount)
	for i := 0; i < policyCount-1; i++ {
		connector := data.NewMockConnector(fmt.Sprintf("connector-%d", i))
		policies = append(policies, mustNewTestCachePolicy(tb, connector, &common.CachePolicyConfig{
			Connector: connector.Id(),
			Network:   fmt.Sprintf("evm:%d", 1000+i),
			Method:    fmt.Sprintf("eth_method_%d", i),
			Params:    []interface{}{"0x*"},
			Finality:  common.DataFinalityStateFinalized,
			TTL:       common.Duration(time.Minute),
		}))
	}

	matchingConnector := data.NewMockConnector("connector-match")
	policies = append(policies, mustNewTestCachePolicy(tb, matchingConnector, &common.CachePolicyConfig{
		Connector: matchingConnector.Id(),
		Network:   "evm:1",
		Method:    "eth_getBalance",
		Params:    []interface{}{"0x*"},
		Finality:  common.DataFinalityStateFinalized,
		TTL:       common.Duration(time.Minute),
	}))

	return newTestCacheWithPolicies(tb, policies)
}

func findGetPoliciesLinearForBenchmark(
	policies []*data.CachePolicy,
	networkId string,
	method string,
	params []interface{},
	finality common.DataFinalityState,
) ([]*data.CachePolicy, error) {
	var matched []*data.CachePolicy
	visitedConnectorsMap := make(map[data.Connector]bool)
	for _, policy := range policies {
		match, err := policy.MatchesForGet(networkId, method, params, finality)
		if err != nil {
			return nil, err
		}
		if match {
			if c := policy.GetConnector(); !visitedConnectorsMap[c] {
				matched = append(matched, policy)
				visitedConnectorsMap[c] = true
			}
		}
	}
	return matched, nil
}

func newTestCacheWithPolicies(tb testing.TB, policies []*data.CachePolicy) *EvmJsonRpcCache {
	tb.Helper()
	logger := zerolog.Nop()
	cache := &EvmJsonRpcCache{
		logger: &logger,
	}
	cache.SetPolicies(policies)
	return cache
}

func mustNewTestCachePolicy(tb testing.TB, connector data.Connector, cfg *common.CachePolicyConfig) *data.CachePolicy {
	tb.Helper()
	policy, err := data.NewCachePolicy(cfg, connector)
	require.NoError(tb, err)
	return policy
}
