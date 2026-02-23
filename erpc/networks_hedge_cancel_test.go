package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for hedge CancelIf behavior with transient errors in consensus mode.
//
// Architecture: The network-level failsafe policy order is
// timeout → consensus → retry → hedge (innermost).
// In consensus mode each hedge execution tries ONE upstream (maxLoopIterations=1).
//
// Hedge CancelIf decides if a completed execution's result is "satisfactory":
//   - true  → use this result, cancel pending/running hedge executions
//   - false → keep waiting for other hedge executions to complete
//
// BUG: CancelIf returns true for ALL non-exhaustion errors, including transient
// ones like MissingData and ServerSideException. This means:
//   - Consensus participant's first hedge exec → upstream with MissingData → CancelIf(true)
//   - The participant's recovery hedge (which could try another upstream) is cancelled
//   - The participant is stuck with the MissingData error
//   - If ALL participants hit bad upstreams first, consensus cannot be reached
//
// FIX: CancelIf should return false for transient/retryable errors so the hedge
// continues and the participant can recover via a subsequent hedge execution.

func TestHedgeConsensus_AllUpstreamsMissingData_HedgeRecovery(t *testing.T) {
	// All 3 upstreams return MissingData on first call, then success on second call.
	// With 3 consensus participants and hedge maxCount=1:
	//   - Each participant's first hedge exec hits one upstream → MissingData (fast)
	//   - CancelIf evaluates the MissingData error
	//   - BUG: CancelIf returns true → cancels recovery hedge → participant stuck with error
	//   - FIX: CancelIf returns false → recovery hedge fires → tries upstream again → success
	//
	// With the bug: 0 valid results → consensus fails
	// After fix: 3 valid results → consensus succeeds (3/3 agree)

	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	var rpc1Calls, rpc2Calls, rpc3Calls atomic.Int32

	for i, host := range []string{"rpc1", "rpc2", "rpc3"} {
		counter := []*atomic.Int32{&rpc1Calls, &rpc2Calls, &rpc3Calls}[i]
		endpoint := fmt.Sprintf("http://%s.localhost", host)

		// First call: MissingData (consumed by participant's initial hedge exec)
		gock.New(endpoint).
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Times(1).
			Reply(200).
			Map(func(r *http.Response) *http.Response { counter.Add(1); return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "header not found",
				},
			})

		// Second call: success (consumed by participant's recovery hedge exec)
		gock.New(endpoint).
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Times(1).
			Reply(200).
			Map(func(r *http.Response) *http.Response { counter.Add(1); return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xde0b6b3a7640000",
			})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkWithHedgeAndConsensus(t, ctx,
		3,
		&common.HedgePolicyConfig{
			Delay:    common.Duration(200 * time.Millisecond),
			MaxCount: 1,
		},
		&common.ConsensusPolicyConfig{
			MaxParticipants:         3,
			AgreementThreshold:      2,
			DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
			LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
			PreferNonEmpty:          &common.TRUE,
			PreferLargerResponses:   &common.TRUE,
		},
		&common.RetryPolicyConfig{
			MaxAttempts: 1,
		},
	)

	requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)
	req := common.NewNormalizedRequest(requestBytes)
	req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})

	resp, err := network.Forward(ctx, req)

	t.Logf("rpc1=%d rpc2=%d rpc3=%d err=%v", rpc1Calls.Load(), rpc2Calls.Load(), rpc3Calls.Load(), err)

	// BUG: All participants' recovery hedges are cancelled by CancelIf → 0 valid results
	// → consensus fails because no agreement can be reached.
	//
	// EXPECTED (after fix): Recovery hedges fire → all participants get success on second
	// call → consensus has 3 agreeing results → success.
	require.NoError(t, err,
		"All participants should recover via hedge after initial MissingData, "+
			"but CancelIf cancels the recovery hedges for transient errors")
	require.NotNil(t, resp)

	jrr, jrrErr := resp.JsonRpcResponse()
	require.NoError(t, jrrErr)
	assert.Contains(t, jrr.GetResultString(), "0xde0b6b3a7640000")

	// Each upstream should be called twice: first MissingData, then success via hedge
	totalCalls := rpc1Calls.Load() + rpc2Calls.Load() + rpc3Calls.Load()
	assert.GreaterOrEqual(t, totalCalls, int32(4),
		"at least 4 upstream calls expected (3 initial + at least 1 hedge recovery)")
}

func TestHedgeConsensus_ServerError_HedgeRecovery(t *testing.T) {
	// Same pattern as MissingData but with HTTP 500 errors.
	// Server-side errors are also transient — CancelIf should NOT cancel hedges.

	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	var rpc1Calls, rpc2Calls, rpc3Calls atomic.Int32

	for i, host := range []string{"rpc1", "rpc2", "rpc3"} {
		counter := []*atomic.Int32{&rpc1Calls, &rpc2Calls, &rpc3Calls}[i]
		endpoint := fmt.Sprintf("http://%s.localhost", host)

		// First call: 500 error
		gock.New(endpoint).
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Times(1).
			Reply(500).
			Map(func(r *http.Response) *http.Response { counter.Add(1); return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "Internal server error",
				},
			})

		// Second call: success
		gock.New(endpoint).
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Times(1).
			Reply(200).
			Map(func(r *http.Response) *http.Response { counter.Add(1); return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x5678",
			})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkWithHedgeAndConsensus(t, ctx,
		3,
		&common.HedgePolicyConfig{
			Delay:    common.Duration(200 * time.Millisecond),
			MaxCount: 1,
		},
		&common.ConsensusPolicyConfig{
			MaxParticipants:         3,
			AgreementThreshold:      2,
			DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
			LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
			PreferNonEmpty:          &common.TRUE,
			PreferLargerResponses:   &common.TRUE,
		},
		&common.RetryPolicyConfig{
			MaxAttempts: 1,
		},
	)

	requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)
	req := common.NewNormalizedRequest(requestBytes)
	req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})

	resp, err := network.Forward(ctx, req)

	t.Logf("rpc1=%d rpc2=%d rpc3=%d err=%v", rpc1Calls.Load(), rpc2Calls.Load(), rpc3Calls.Load(), err)

	require.NoError(t, err,
		"Server errors are transient — hedge recovery should not be cancelled")
	require.NotNil(t, resp)

	jrr, jrrErr := resp.JsonRpcResponse()
	require.NoError(t, jrrErr)
	assert.Contains(t, jrr.GetResultString(), "0x5678")
}

func TestHedgeConsensus_ExecutionReverted_CancelsHedge(t *testing.T) {
	// Execution reverted is deterministic — all upstreams would return the same
	// result. CancelIf SHOULD cancel hedges for terminal errors to avoid wasting
	// resources. This test verifies the fix doesn't break terminal error cancellation.

	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	var rpc1Calls, rpc2Calls, rpc3Calls atomic.Int32

	for i, host := range []string{"rpc1", "rpc2", "rpc3"} {
		counter := []*atomic.Int32{&rpc1Calls, &rpc2Calls, &rpc3Calls}[i]
		endpoint := fmt.Sprintf("http://%s.localhost", host)

		// All upstreams return execution reverted (deterministic)
		gock.New(endpoint).
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response { counter.Add(1); return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    3,
					"message": "execution reverted",
					"data":    "0x08c379a0",
				},
			})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkWithHedgeAndConsensus(t, ctx,
		3,
		&common.HedgePolicyConfig{
			Delay:    common.Duration(200 * time.Millisecond),
			MaxCount: 1,
		},
		&common.ConsensusPolicyConfig{
			MaxParticipants:         3,
			AgreementThreshold:      2,
			DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
			LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
			PreferNonEmpty:          &common.TRUE,
			PreferLargerResponses:   &common.TRUE,
		},
		&common.RetryPolicyConfig{
			MaxAttempts: 1,
		},
	)

	requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"latest"]}`)
	req := common.NewNormalizedRequest(requestBytes)
	req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})

	start := time.Now()
	_, err := network.Forward(ctx, req)
	elapsed := time.Since(start)

	t.Logf("rpc1=%d rpc2=%d rpc3=%d elapsed=%v err=%v",
		rpc1Calls.Load(), rpc2Calls.Load(), rpc3Calls.Load(), elapsed, err)

	require.Error(t, err, "execution reverted is a terminal error that should propagate")
	assert.True(t,
		common.HasErrorCode(err, common.ErrCodeEndpointExecutionException),
		"should contain execution exception, got: %v", err)

	// Should complete fast — terminal error cancels hedges, no need to wait
	assert.Less(t, elapsed, 400*time.Millisecond,
		"terminal error should cancel hedges promptly")
}

func TestHedgeConsensus_OneUpstreamMissingData_OthersSucceed_StillWorks(t *testing.T) {
	// When only one upstream returns MissingData and others succeed,
	// the consensus should work regardless of CancelIf behavior because
	// enough participants succeed independently.

	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	// rpc1: always MissingData
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Persist().
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"error": map[string]interface{}{
				"code":    -32000,
				"message": "header not found",
			},
		})

	// rpc2 and rpc3: always success
	for _, host := range []string{"rpc2", "rpc3"} {
		gock.New(fmt.Sprintf("http://%s.localhost", host)).
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xabc",
			})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkWithHedgeAndConsensus(t, ctx,
		3,
		&common.HedgePolicyConfig{
			Delay:    common.Duration(100 * time.Millisecond),
			MaxCount: 1,
		},
		&common.ConsensusPolicyConfig{
			MaxParticipants:         3,
			AgreementThreshold:      2,
			DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
			LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
			PreferNonEmpty:          &common.TRUE,
			PreferLargerResponses:   &common.TRUE,
		},
		&common.RetryPolicyConfig{
			MaxAttempts: 2,
			Delay:       common.Duration(50 * time.Millisecond),
		},
	)

	requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)
	req := common.NewNormalizedRequest(requestBytes)
	req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})

	resp, err := network.Forward(ctx, req)

	// 2 of 3 participants succeed independently → consensus should pass
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, jrrErr := resp.JsonRpcResponse()
	require.NoError(t, jrrErr)
	assert.Contains(t, jrr.GetResultString(), "0xabc")
}

func setupTestNetworkWithHedgeAndConsensus(
	t *testing.T,
	ctx context.Context,
	numUpstreams int,
	hedgeConfig *common.HedgePolicyConfig,
	consensusConfig *common.ConsensusPolicyConfig,
	retryConfig *common.RetryPolicyConfig,
) *Network {
	t.Helper()

	upstreamConfigs := make([]*common.UpstreamConfig, numUpstreams)
	for i := 0; i < numUpstreams; i++ {
		upstreamConfigs[i] = &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       fmt.Sprintf("rpc%d", i+1),
			Endpoint: fmt.Sprintf("http://rpc%d.localhost", i+1),
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
	}

	if consensusConfig != nil {
		if err := consensusConfig.SetDefaults(); err != nil {
			t.Fatalf("failed to set defaults on consensus config: %v", err)
		}
	}

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId: 123,
		},
		Failsafe: []*common.FailsafeConfig{{
			MatchMethod: "*",
			Hedge:       hedgeConfig,
			Consensus:   consensusConfig,
			Retry:       retryConfig,
		}},
	}

	return setupTestNetwork(t, ctx, upstreamConfigs, networkConfig)
}
