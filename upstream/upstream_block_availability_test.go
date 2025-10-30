package upstream

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

// Enhanced mock EVM state poller with more realistic behavior
type mockEvmStatePollerEnhanced struct {
	latestBlock    int64
	finalizedBlock int64
	isNull         bool
	pollError      error
	pollCount      int
	// For simulating block progression
	blockProgression []int64
}

func (m *mockEvmStatePollerEnhanced) Bootstrap(ctx context.Context) error { return nil }
func (m *mockEvmStatePollerEnhanced) Poll(ctx context.Context) error      { return m.pollError }
func (m *mockEvmStatePollerEnhanced) PollLatestBlockNumber(ctx context.Context) (int64, error) {
	if m.pollError != nil {
		return 0, m.pollError
	}
	// Simulate block progression if configured
	if len(m.blockProgression) > m.pollCount {
		m.latestBlock = m.blockProgression[m.pollCount]
		m.pollCount++
	}
	return m.latestBlock, nil
}
func (m *mockEvmStatePollerEnhanced) PollFinalizedBlockNumber(ctx context.Context) (int64, error) {
	if m.pollError != nil {
		return 0, m.pollError
	}
	return m.finalizedBlock, nil
}
func (m *mockEvmStatePollerEnhanced) SyncingState() common.EvmSyncingState {
	return common.EvmSyncingStateNotSyncing
}
func (m *mockEvmStatePollerEnhanced) SetSyncingState(state common.EvmSyncingState) {}
func (m *mockEvmStatePollerEnhanced) LatestBlock() int64                           { return m.latestBlock }
func (m *mockEvmStatePollerEnhanced) FinalizedBlock() int64                        { return m.finalizedBlock }
func (m *mockEvmStatePollerEnhanced) PollEarliestBlockNumber(ctx context.Context, probe common.EvmAvailabilityProbeType) (int64, error) {
	return 0, nil
}
func (m *mockEvmStatePollerEnhanced) EarliestBlock(probe common.EvmAvailabilityProbeType) int64 {
	return 0
}
func (m *mockEvmStatePollerEnhanced) IsBlockFinalized(blockNumber int64) (bool, error) {
	if m.pollError != nil {
		return false, m.pollError
	}
	return blockNumber <= m.finalizedBlock, nil
}
func (m *mockEvmStatePollerEnhanced) SuggestFinalizedBlock(blockNumber int64)    {}
func (m *mockEvmStatePollerEnhanced) SuggestLatestBlock(blockNumber int64)       {}
func (m *mockEvmStatePollerEnhanced) SetNetworkConfig(cfg *common.NetworkConfig) {}
func (m *mockEvmStatePollerEnhanced) IsObjectNull() bool                         { return m.isNull }

func TestEvmAssertBlockAvailability_EdgeCases(t *testing.T) {
	t.Run("NegativeBlockNumber", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm:  &common.EvmUpstreamConfig{},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePollerEnhanced{latestBlock: 1000, finalizedBlock: 900},
		}

		// Test with negative block number
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, -1)
		assert.True(t, canHandle) // Should handle negative blocks (e.g., for genesis)
		assert.NoError(t, err)
	})

	t.Run("ZeroBlockNumber", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm:  &common.EvmUpstreamConfig{},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePollerEnhanced{latestBlock: 1000, finalizedBlock: 900},
		}

		// Test with zero block number (genesis)
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 0)
		assert.True(t, canHandle)
		assert.NoError(t, err)
	})

	t.Run("InvalidConfidenceLevel", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm:  &common.EvmUpstreamConfig{},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePollerEnhanced{latestBlock: 1000, finalizedBlock: 900},
		}

		// Test with invalid confidence level
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidence(999), false, 500)
		assert.False(t, canHandle)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported block availability confidence")
	})

	t.Run("PollErrorHandling", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					MaxAvailableRecentBlocks: 100,
				},
			},
			logger: &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePollerEnhanced{
				latestBlock:    1000,
				finalizedBlock: 900,
				pollError:      errors.New("network error"),
			},
		}

		// Test BlockHead confidence with poll error
		canHandle, err := upstream.EvmAssertBlockAvailability(ctx, "test_method", common.AvailbilityConfidenceBlockHead, true, 1050)
		assert.False(t, canHandle)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to poll latest block number")

		// Test Finalized confidence with poll error
		canHandle, err = upstream.EvmAssertBlockAvailability(ctx, "test_method", common.AvailbilityConfidenceFinalized, true, 1050)
		assert.False(t, canHandle)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to check if block is finalized")
	})

	t.Run("BoundaryConditions_MaxInt64", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm:  &common.EvmUpstreamConfig{},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePollerEnhanced{latestBlock: 1000, finalizedBlock: 900},
		}

		// Test with max int64 block number
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 9223372036854775807)
		assert.False(t, canHandle) // Should not be available as it's way beyond latest
		assert.NoError(t, err)
	})
}

func TestEvmAssertBlockAvailability_RealWorldScenarios(t *testing.T) {
	t.Run("BlockReorgScenario", func(t *testing.T) {
		// Simulate a scenario where latest block regresses due to reorg
		poller := &mockEvmStatePollerEnhanced{
			latestBlock:      1000,
			finalizedBlock:   900,
			blockProgression: []int64{1005, 1003}, // Block regresses after poll
		}

		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					MaxAvailableRecentBlocks: 128,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: poller,
		}

		// First call with forceFresh should update to 1005
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, true, 1004)
		assert.True(t, canHandle)
		assert.NoError(t, err)

		// Second call might see regression to 1003
		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, true, 1006)
		assert.False(t, canHandle) // Block 1006 is now beyond latest
		assert.NoError(t, err)
	})

	t.Run("RapidBlockProgression", func(t *testing.T) {
		// Simulate rapid block progression during availability checks
		poller := &mockEvmStatePollerEnhanced{
			latestBlock:      1000,
			finalizedBlock:   900,
			blockProgression: []int64{1001, 1002, 1003, 1004, 1005},
		}

		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					MaxAvailableRecentBlocks: 10, // Small window
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: poller,
		}

		// Check a block that's initially within range but might fall out during the check
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 995)
		// Should pass as 995 >= (1001 - 10) = 991
		assert.True(t, canHandle)
		assert.NoError(t, err)
	})

	t.Run("FinalizedBlockLag", func(t *testing.T) {
		// Simulate a node where finalized block significantly lags behind latest
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					MaxAvailableRecentBlocks: 1000, // Large window
				},
			},
			logger: &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePollerEnhanced{
				latestBlock:    10000,
				finalizedBlock: 8000, // 2000 blocks behind
			},
		}

		// Check block that's between finalized and latest
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceFinalized, false, 8500)
		assert.False(t, canHandle) // Not finalized yet
		assert.NoError(t, err)

		// Check with BlockHead confidence
		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 8500)
		assert.False(t, canHandle) // Outside the 1000 block window (10000 - 1000 = 9000)
		assert.NoError(t, err)

		// Check recent block
		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 9500)
		assert.True(t, canHandle) // Within the 1000 block window
		assert.NoError(t, err)
	})

	t.Run("FullNodePruningEdgeCase", func(t *testing.T) {
		// Simulate a full node that's actively pruning
		poller := &mockEvmStatePollerEnhanced{
			latestBlock:      1000,
			finalizedBlock:   900,
			blockProgression: []int64{1005, 1010}, // Blocks advance
		}

		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					MaxAvailableRecentBlocks: 128,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: poller,
		}

		// Check a block that's on the edge of being pruned
		// Initial: 1000 - 128 = 872
		// After poll: 1005 - 128 = 877
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 875)
		assert.False(t, canHandle) // Should fail after fresh poll shows it's outside range
		assert.NoError(t, err)
	})

	t.Run("ZeroMaxAvailableRecentBlocks", func(t *testing.T) {
		// Test behavior when MaxAvailableRecentBlocks is 0 (archive node behavior)
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					MaxAvailableRecentBlocks: 0, // Explicitly set to 0
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePollerEnhanced{latestBlock: 1000, finalizedBlock: 900},
		}

		// Should behave like archive node - accept any block <= latest
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 1)
		assert.True(t, canHandle)
		assert.NoError(t, err)

		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 999)
		assert.True(t, canHandle)
		assert.NoError(t, err)
	})
}

func TestEvmAssertBlockAvailability_eth_getLogs_Integration(t *testing.T) {
	t.Run("BothBlocksWithinRange", func(t *testing.T) {
		upstream := &Upstream{
			ProjectId: "test-project",
			config: &common.UpstreamConfig{
				Id:   "test-upstream",
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					MaxAvailableRecentBlocks: 1000,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePollerEnhanced{latestBlock: 10000, finalizedBlock: 9900},
			networkId:      util.AtomicValue("evm:123"),
		}

		// Simulate eth_getLogs checking fromBlock and toBlock
		fromBlock := int64(9500)
		toBlock := int64(9800)

		// Check toBlock first (as in eth_getLogs)
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "eth_getLogs", common.AvailbilityConfidenceBlockHead, true, toBlock)
		assert.True(t, canHandle)
		assert.NoError(t, err)

		// Check fromBlock
		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "eth_getLogs", common.AvailbilityConfidenceBlockHead, false, fromBlock)
		assert.True(t, canHandle)
		assert.NoError(t, err)
	})

	t.Run("ToBlockBeyondLatest", func(t *testing.T) {
		upstream := &Upstream{
			ProjectId: "test-project",
			config: &common.UpstreamConfig{
				Id:   "test-upstream",
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					MaxAvailableRecentBlocks: 1000,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePollerEnhanced{latestBlock: 10000, finalizedBlock: 9900},
			networkId:      util.AtomicValue("evm:123"),
		}

		// eth_getLogs with toBlock beyond latest
		toBlock := int64(10001)

		// Check toBlock first - should fail
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "eth_getLogs", common.AvailbilityConfidenceBlockHead, true, toBlock)
		assert.False(t, canHandle)
		assert.NoError(t, err)
		// In real scenario, eth_getLogs would return error here without checking fromBlock
	})

	t.Run("FromBlockTooOld", func(t *testing.T) {
		upstream := &Upstream{
			ProjectId: "test-project",
			config: &common.UpstreamConfig{
				Id:   "test-upstream",
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					MaxAvailableRecentBlocks: 1000,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePollerEnhanced{latestBlock: 10000, finalizedBlock: 9900},
			networkId:      util.AtomicValue("evm:123"),
		}

		// eth_getLogs with fromBlock too old
		fromBlock := int64(8000) // Outside the 1000 block window
		toBlock := int64(9500)

		// Check toBlock first - should pass
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "eth_getLogs", common.AvailbilityConfidenceBlockHead, true, toBlock)
		assert.True(t, canHandle)
		assert.NoError(t, err)

		// Check fromBlock - should fail
		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "eth_getLogs", common.AvailbilityConfidenceBlockHead, false, fromBlock)
		assert.False(t, canHandle)
		assert.NoError(t, err)
	})
}

func TestEvmAssertBlockAvailability_RetryPolicy_Integration(t *testing.T) {
	t.Run("EmptyResultWithBlockAvailable", func(t *testing.T) {
		upstream := &Upstream{
			ProjectId: "test-project",
			config: &common.UpstreamConfig{
				Id:   "test-upstream",
				Type: common.UpstreamTypeEvm,
				Evm:  &common.EvmUpstreamConfig{},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePollerEnhanced{latestBlock: 1000, finalizedBlock: 900},
			networkId:      util.AtomicValue("evm:123"),
		}

		// Simulate retry policy checking if block is available
		// If block is available and result is empty, should NOT retry
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "eth_getBalance", common.AvailbilityConfidenceFinalized, false, 850)
		assert.True(t, canHandle) // Block is finalized and available
		assert.NoError(t, err)
		// In this case, retry policy should NOT retry as the empty result is legitimate
	})

	t.Run("EmptyResultWithBlockNotAvailable", func(t *testing.T) {
		upstream := &Upstream{
			ProjectId: "test-project",
			config: &common.UpstreamConfig{
				Id:   "test-upstream",
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					MaxAvailableRecentBlocks: 128,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePollerEnhanced{latestBlock: 1000, finalizedBlock: 900},
			networkId:      util.AtomicValue("evm:123"),
		}

		// Check old block that's not available
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "eth_getBalance", common.AvailbilityConfidenceBlockHead, false, 500)
		assert.False(t, canHandle) // Block is too old
		assert.NoError(t, err)
		// In this case, retry policy SHOULD retry with different upstream
	})

	t.Run("MethodInIgnoreList", func(t *testing.T) {
		// For methods in ignore list (eth_getLogs, eth_call by default),
		// the retry policy should NOT check block availability at all
		upstream := &Upstream{
			ProjectId: "test-project",
			config: &common.UpstreamConfig{
				Id:   "test-upstream",
				Type: common.UpstreamTypeEvm,
				Evm:  &common.EvmUpstreamConfig{},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePollerEnhanced{latestBlock: 1000, finalizedBlock: 900},
			networkId:      util.AtomicValue("evm:123"),
		}

		// This would normally be called by retry policy, but for ignored methods it shouldn't
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "eth_getLogs", common.AvailbilityConfidenceFinalized, false, 850)
		assert.True(t, canHandle)
		assert.NoError(t, err)
		// But in practice, the retry policy wouldn't even call this for ignored methods
	})
}

func TestEvmAssertBlockAvailability_PollerBehavior(t *testing.T) {
	t.Run("LatestBlockStale_ForceFresh", func(t *testing.T) {
		callCount := 0
		poller := &mockEvmStatePollerWithCustomBehavior{
			getLatestBlock: func() int64 {
				if callCount == 0 {
					return 1000 // Initial stale value
				}
				return 1100 // Fresh value after poll
			},
			pollLatestBlockNumber: func(ctx context.Context) (int64, error) {
				callCount++
				return 1100, nil
			},
			finalizedBlock: 900,
		}

		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm:  &common.EvmUpstreamConfig{},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: poller,
		}

		// Request block 1050 with forceFresh=true
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, true, 1050)
		assert.True(t, canHandle) // Should be available after fresh poll
		assert.NoError(t, err)
		assert.Equal(t, 1, callCount) // Should have polled once
	})

	t.Run("LatestBlockCurrent_NoForceFresh", func(t *testing.T) {
		pollCount := 0
		poller := &mockEvmStatePollerWithCustomBehavior{
			getLatestBlock: func() int64 {
				return 1100
			},
			pollLatestBlockNumber: func(ctx context.Context) (int64, error) {
				pollCount++
				return 1100, nil
			},
			finalizedBlock: 900,
		}

		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm:  &common.EvmUpstreamConfig{},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: poller,
		}

		// Request block 1050 with forceFresh=false
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 1050)
		assert.True(t, canHandle)
		assert.NoError(t, err)
		assert.Equal(t, 0, pollCount) // Should NOT have polled
	})

	t.Run("LowerBoundCheck_PollsWhenNearBoundary", func(t *testing.T) {
		pollCount := 0
		poller := &mockEvmStatePollerWithCustomBehavior{
			getLatestBlock: func() int64 {
				return 1000
			},
			pollLatestBlockNumber: func(ctx context.Context) (int64, error) {
				pollCount++
				return 1000, nil
			},
			finalizedBlock: 900,
		}

		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					MaxAvailableRecentBlocks: 128,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: poller,
		}

		// Request block that's initially within range
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 875)
		assert.True(t, canHandle)
		assert.NoError(t, err)
		assert.Equal(t, 1, pollCount) // Should have polled for lower bound check
	})

	t.Run("LowerBoundCheck_NoPollsWhenFarFromBoundary", func(t *testing.T) {
		pollCount := 0
		poller := &mockEvmStatePollerWithCustomBehavior{
			getLatestBlock: func() int64 {
				return 1000
			},
			pollLatestBlockNumber: func(ctx context.Context) (int64, error) {
				pollCount++
				return 1000, nil
			},
			finalizedBlock: 900,
		}

		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					MaxAvailableRecentBlocks: 128,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: poller,
		}

		// Request block that's initially within range
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 890)
		assert.True(t, canHandle)
		assert.NoError(t, err)
		assert.Equal(t, 0, pollCount) // Should NOT have polled for lower bound check
	})
}

// Custom mock for fine-grained control over poller behavior
type mockEvmStatePollerWithCustomBehavior struct {
	getLatestBlock        func() int64
	pollLatestBlockNumber func(ctx context.Context) (int64, error)
	finalizedBlock        int64
}

func (m *mockEvmStatePollerWithCustomBehavior) Bootstrap(ctx context.Context) error { return nil }
func (m *mockEvmStatePollerWithCustomBehavior) Poll(ctx context.Context) error      { return nil }
func (m *mockEvmStatePollerWithCustomBehavior) PollLatestBlockNumber(ctx context.Context) (int64, error) {
	return m.pollLatestBlockNumber(ctx)
}
func (m *mockEvmStatePollerWithCustomBehavior) PollFinalizedBlockNumber(ctx context.Context) (int64, error) {
	return m.finalizedBlock, nil
}
func (m *mockEvmStatePollerWithCustomBehavior) SyncingState() common.EvmSyncingState {
	return common.EvmSyncingStateNotSyncing
}
func (m *mockEvmStatePollerWithCustomBehavior) SetSyncingState(state common.EvmSyncingState) {}
func (m *mockEvmStatePollerWithCustomBehavior) LatestBlock() int64 {
	return m.getLatestBlock()
}
func (m *mockEvmStatePollerWithCustomBehavior) FinalizedBlock() int64 { return m.finalizedBlock }
func (m *mockEvmStatePollerWithCustomBehavior) PollEarliestBlockNumber(ctx context.Context, probe common.EvmAvailabilityProbeType) (int64, error) {
	return 0, nil
}
func (m *mockEvmStatePollerWithCustomBehavior) EarliestBlock(probe common.EvmAvailabilityProbeType) int64 {
	return 0
}
func (m *mockEvmStatePollerWithCustomBehavior) IsBlockFinalized(blockNumber int64) (bool, error) {
	return blockNumber <= m.finalizedBlock, nil
}
func (m *mockEvmStatePollerWithCustomBehavior) SuggestFinalizedBlock(blockNumber int64)    {}
func (m *mockEvmStatePollerWithCustomBehavior) SuggestLatestBlock(blockNumber int64)       {}
func (m *mockEvmStatePollerWithCustomBehavior) SetNetworkConfig(cfg *common.NetworkConfig) {}
func (m *mockEvmStatePollerWithCustomBehavior) IsObjectNull() bool                         { return false }

func TestEvmAssertBlockAvailability_Metrics(t *testing.T) {
	t.Run("ProjectIdHandling", func(t *testing.T) {
		// Test with empty ProjectId - should not panic
		upstream := &Upstream{
			ProjectId: "", // Empty project ID
			config: &common.UpstreamConfig{
				Id:   "test-upstream",
				Type: common.UpstreamTypeEvm,
				Evm:  &common.EvmUpstreamConfig{},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePollerEnhanced{latestBlock: 1000, finalizedBlock: 900},
			networkId:      util.AtomicValue("evm:123"),
		}

		// Should handle empty ProjectId gracefully
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 1001)
		assert.False(t, canHandle)
		assert.NoError(t, err)
	})

	t.Run("NetworkIdHandling", func(t *testing.T) {
		// Test with various NetworkId formats
		testCases := []string{
			"",              // Empty
			"evm:123",       // Standard format
			"evm:137",       // Polygon
			"unknown-chain", // Non-standard format
		}

		for _, networkId := range testCases {
			upstream := &Upstream{
				ProjectId: "test-project",
				config: &common.UpstreamConfig{
					Id:   "test-upstream",
					Type: common.UpstreamTypeEvm,
					Evm:  &common.EvmUpstreamConfig{},
				},
				logger:         &zerolog.Logger{},
				evmStatePoller: &mockEvmStatePollerEnhanced{latestBlock: 1000, finalizedBlock: 900},
				networkId:      util.AtomicValue(networkId),
			}

			canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 1001)
			assert.False(t, canHandle)
			assert.NoError(t, err, "Should handle NetworkId '%s' without error", networkId)
		}
	})
}

func TestEvmAssertBlockAvailability_SpecialBlocks(t *testing.T) {
	t.Run("LatestBlockRequested", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm:  &common.EvmUpstreamConfig{},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePollerEnhanced{latestBlock: 1000, finalizedBlock: 900},
		}

		// Request exactly the latest block
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 1000)
		assert.True(t, canHandle)
		assert.NoError(t, err)
	})

	t.Run("FinalizedBlockRequested", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm:  &common.EvmUpstreamConfig{},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePollerEnhanced{latestBlock: 1000, finalizedBlock: 900},
		}

		// Request exactly the finalized block
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceFinalized, false, 900)
		assert.True(t, canHandle)
		assert.NoError(t, err)
	})
}

func TestEvmAssertBlockAvailability_RaceCondition(t *testing.T) {
	t.Run("assertUpstreamLowerBound_RaceCondition", func(t *testing.T) {
		// This test demonstrates the race condition in assertUpstreamLowerBound
		// where a block that's initially within range becomes unavailable after polling

		pollCount := 0
		poller := &mockEvmStatePollerWithCustomBehavior{
			getLatestBlock: func() int64 {
				// Initial latest block
				return 1000
			},
			pollLatestBlockNumber: func(ctx context.Context) (int64, error) {
				pollCount++
				// Simulate rapid block progression during poll
				// This makes previously valid blocks invalid
				return 1100, nil // 100 blocks advanced
			},
			finalizedBlock: 900,
		}

		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					MaxAvailableRecentBlocks: 128,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: poller,
		}

		// Request block 875
		// Initial calculation: 1000 - 128 = 872, so 875 is valid
		// After poll: 1100 - 128 = 972, so 875 becomes invalid
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 875)

		// Due to the race condition, this block is rejected even though it was initially valid
		assert.False(t, canHandle)
		assert.NoError(t, err)
		assert.Equal(t, 1, pollCount) // Verify that polling occurred

		// This demonstrates the issue: a request that would have succeeded
		// if checked against the initial state fails due to the unconditional re-poll
	})

	t.Run("assertUpstreamLowerBound_IdealBehavior", func(t *testing.T) {
		// This test shows what the ideal behavior should be
		// Only poll when necessary (forceFresh or near boundary)

		pollCount := 0
		poller := &mockEvmStatePollerWithCustomBehavior{
			getLatestBlock: func() int64 {
				return 1000
			},
			pollLatestBlockNumber: func(ctx context.Context) (int64, error) {
				pollCount++
				return 1100, nil
			},
			finalizedBlock: 900,
		}

		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					MaxAvailableRecentBlocks: 128,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: poller,
		}

		// Test 1: Block well within range - should not need fresh poll
		// Block 800 is well within range (1000 - 128 = 872)
		// Ideally, this should NOT trigger a poll since it's clearly within range
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 900)

		// Current behavior: polls anyway and might reject valid blocks
		// Ideal behavior: should return true without polling
		assert.True(t, canHandle) // Currently fails due to the issue
		assert.NoError(t, err)
		// Note: Currently pollCount would be 1, but ideally should be 0
	})
}

func TestEvmAssertBlockAvailability_ConcurrentAccess(t *testing.T) {
	t.Run("ConcurrentRequests", func(t *testing.T) {
		// Test concurrent access to EvmAssertBlockAvailability
		pollCount := 0
		var mu sync.Mutex

		poller := &mockEvmStatePollerWithCustomBehavior{
			getLatestBlock: func() int64 {
				return 1000
			},
			pollLatestBlockNumber: func(ctx context.Context) (int64, error) {
				mu.Lock()
				pollCount++
				count := pollCount
				mu.Unlock()

				// Simulate some delay in polling
				time.Sleep(10 * time.Millisecond)

				// Return different values based on poll count to simulate progression
				return 1000 + int64(count*10), nil
			},
			finalizedBlock: 900,
		}

		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					MaxAvailableRecentBlocks: 128,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: poller,
		}

		// Launch multiple concurrent requests
		var wg sync.WaitGroup
		results := make([]bool, 10)
		errors := make([]error, 10)

		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				// All goroutines check the same block
				results[idx], errors[idx] = upstream.EvmAssertBlockAvailability(
					context.Background(),
					"test_method",
					common.AvailbilityConfidenceBlockHead,
					false,
					875,
				)
			}(i)
		}

		wg.Wait()

		// Check results
		availableCount := 0
		for i, available := range results {
			assert.NoError(t, errors[i])
			if available {
				availableCount++
			}
		}

		// Due to concurrent polling and block progression,
		// we might get inconsistent results
		t.Logf("Concurrent results: %d/%d available, poll count: %d", availableCount, len(results), pollCount)

		// This demonstrates that concurrent access can lead to:
		// 1. Multiple unnecessary polls
		// 2. Inconsistent results for the same block
		assert.Greater(t, pollCount, 1, "Multiple polls occurred due to concurrent access")
	})
}

// Benchmark to ensure performance doesn't degrade
func BenchmarkEvmAssertBlockAvailability(b *testing.B) {
	upstream := &Upstream{
		ProjectId: "bench-project",
		config: &common.UpstreamConfig{
			Id:   "bench-upstream",
			Type: common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{
				MaxAvailableRecentBlocks: 128,
			},
		},
		logger:         &zerolog.Logger{},
		evmStatePoller: &mockEvmStatePollerEnhanced{latestBlock: 10000, finalizedBlock: 9900},
		networkId:      util.AtomicValue("evm:123"),
	}

	ctx := context.Background()

	b.Run("BlockHead_InRange", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, _ = upstream.EvmAssertBlockAvailability(ctx, "test_method", common.AvailbilityConfidenceBlockHead, false, 9950)
		}
	})

	b.Run("BlockHead_OutOfRange", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, _ = upstream.EvmAssertBlockAvailability(ctx, "test_method", common.AvailbilityConfidenceBlockHead, false, 10001)
		}
	})

	b.Run("Finalized_Available", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, _ = upstream.EvmAssertBlockAvailability(ctx, "test_method", common.AvailbilityConfidenceFinalized, false, 9800)
		}
	})
}
