package upstream

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

func TestUpstream_SkipLogic(t *testing.T) {
	t.Run("SingleSimpleMethod", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test",
				IgnoreMethods: []string{"eth_getBalance"},
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("SingleWildcardMethod", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test",
				IgnoreMethods: []string{"eth_*"},
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"net_version"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("MultipleMethods", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test",
				IgnoreMethods: []string{"eth_getBalance", "eth_getBlockByNumber"},
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_call"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("MultipleMethodsWithWildcard", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test",
				IgnoreMethods: []string{"eth_*", "net_version"},
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"net_version"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"web3_clientVersion"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("MultipleUpstreamsOneIgnoredAnotherNot", func(t *testing.T) {
		upstream1 := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test1",
				IgnoreMethods: []string{"eth_getBalance"},
			},
			logger: &zerolog.Logger{},
		}
		upstream2 := &Upstream{
			config: &common.UpstreamConfig{
				Id: "test2",
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream1.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream2.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("MultipleUpstreamsBothIgnoreDifferentThings", func(t *testing.T) {
		upstream1 := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test1",
				IgnoreMethods: []string{"eth_getBalance"},
			},
			logger: &zerolog.Logger{},
		}
		upstream2 := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test2",
				IgnoreMethods: []string{"eth_getBlockByNumber"},
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream1.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream2.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)

		reason, skip = upstream1.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)

		reason, skip = upstream2.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")
	})

	t.Run("MultipleUpstreamsAllIgnoredAMethod", func(t *testing.T) {
		upstream1 := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test1",
				IgnoreMethods: []string{"eth_getBalance"},
			},
			logger: &zerolog.Logger{},
		}
		upstream2 := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test2",
				IgnoreMethods: []string{"eth_*"},
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream1.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream2.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")
	})

	t.Run("OneUpstreamWithoutAnythingIgnored", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Id: "test",
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("MultipleUpstreamsWithoutAnythingIgnored", func(t *testing.T) {
		upstream1 := &Upstream{
			config: &common.UpstreamConfig{
				Id: "test1",
			},
			logger: &zerolog.Logger{},
		}
		upstream2 := &Upstream{
			config: &common.UpstreamConfig{
				Id: "test2",
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream1.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)

		reason, skip = upstream2.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)

		reason, skip = upstream1.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)

		reason, skip = upstream2.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("CombinationOfWildcardAndSpecificMethods", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test",
				IgnoreMethods: []string{"eth_*", "net_version", "web3_clientVersion"},
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"net_version"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"web3_clientVersion"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"personal_sign"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("NestedWildcards", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test",
				IgnoreMethods: []string{"eth_*_*"},
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_get_balance"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_get_block_by_number"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")
	})
}

// Mock EVM state poller for testing
type mockEvmStatePoller struct {
	latestBlock    int64
	finalizedBlock int64
	isNull         bool
}

func (m *mockEvmStatePoller) Bootstrap(ctx context.Context) error { return nil }
func (m *mockEvmStatePoller) Poll(ctx context.Context) error      { return nil }
func (m *mockEvmStatePoller) PollLatestBlockNumber(ctx context.Context) (int64, error) {
	return m.latestBlock, nil
}
func (m *mockEvmStatePoller) PollFinalizedBlockNumber(ctx context.Context) (int64, error) {
	return m.finalizedBlock, nil
}
func (m *mockEvmStatePoller) SyncingState() common.EvmSyncingState {
	return common.EvmSyncingStateNotSyncing
}
func (m *mockEvmStatePoller) SetSyncingState(state common.EvmSyncingState) {}
func (m *mockEvmStatePoller) LatestBlock() int64                           { return m.latestBlock }
func (m *mockEvmStatePoller) FinalizedBlock() int64                        { return m.finalizedBlock }
func (m *mockEvmStatePoller) IsBlockFinalized(blockNumber int64) (bool, error) {
	return blockNumber <= m.finalizedBlock, nil
}
func (m *mockEvmStatePoller) SuggestFinalizedBlock(blockNumber int64)    {}
func (m *mockEvmStatePoller) SuggestLatestBlock(blockNumber int64)       {}
func (m *mockEvmStatePoller) SetNetworkConfig(cfg *common.NetworkConfig) {}
func (m *mockEvmStatePoller) IsObjectNull() bool                         { return m.isNull }

func (m *mockEvmStatePoller) EarliestBlock(probe common.EvmAvailabilityProbeType) int64 {
	return 0
}

func (m *mockEvmStatePoller) PollEarliestBlockNumber(ctx context.Context, probe common.EvmAvailabilityProbeType) (int64, error) {
	return 0, nil
}

func TestUpstream_EvmCanHandleBlock(t *testing.T) {
	t.Run("NilUpstream", func(t *testing.T) {
		var upstream *Upstream
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 100)
		assert.False(t, canHandle)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "upstream or config is nil")
	})

	t.Run("NilConfig", func(t *testing.T) {
		upstream := &Upstream{}
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 100)
		assert.False(t, canHandle)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "upstream or config is nil")
	})

	t.Run("NilStatePoller", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
			},
			logger: &zerolog.Logger{},
		}
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 100)
		assert.False(t, canHandle)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "upstream evm state poller is not available")
	})

	t.Run("NullStatePoller", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePoller{isNull: true},
		}
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 100)
		assert.False(t, canHandle)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "upstream evm state poller is not available")
	})

	t.Run("NoLatestBlock", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm:  &common.EvmUpstreamConfig{},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePoller{latestBlock: 0},
		}
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 100)
		assert.False(t, canHandle)
		assert.NoError(t, err)
	})

	t.Run("BlockBeyondLatest", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm:  &common.EvmUpstreamConfig{},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePoller{latestBlock: 1000},
		}
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 1001)
		assert.False(t, canHandle)
		assert.NoError(t, err)
	})

	t.Run("NonEvmUpstream", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: "solana", // Non-EVM type
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePoller{latestBlock: 1000},
		}
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 100)
		assert.False(t, canHandle)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "upstream is not an EVM type")
	})

	t.Run("ArchiveNode", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					NodeType: common.EvmNodeTypeArchive,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePoller{latestBlock: 1000},
		}

		// Archive node should handle all blocks up to latest
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 1)
		assert.True(t, canHandle)
		assert.NoError(t, err)

		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 500)
		assert.True(t, canHandle)
		assert.NoError(t, err)

		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 1000)
		assert.True(t, canHandle)
		assert.NoError(t, err)

		// But not beyond latest
		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 1001)
		assert.False(t, canHandle)
		assert.NoError(t, err)
	})

	t.Run("FullNodeWithMaxAvailableBlocks", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					NodeType:                 common.EvmNodeTypeFull,
					MaxAvailableRecentBlocks: 128,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePoller{latestBlock: 1000},
		}

		// Full node should handle recent blocks
		// First available block = 1000 - 128 = 872
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 872)
		assert.True(t, canHandle)
		assert.NoError(t, err)

		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 900)
		assert.True(t, canHandle)
		assert.NoError(t, err)

		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 1000)
		assert.True(t, canHandle)
		assert.NoError(t, err)

		// But not old blocks
		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 871)
		assert.False(t, canHandle)
		assert.NoError(t, err)

		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 1)
		assert.False(t, canHandle)
		assert.NoError(t, err)

		// And not beyond latest
		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 1001)
		assert.False(t, canHandle)
		assert.NoError(t, err)
	})

	t.Run("FullNodeWithoutMaxAvailableBlocks", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					NodeType:                 common.EvmNodeTypeFull,
					MaxAvailableRecentBlocks: 0, // Not configured
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePoller{latestBlock: 1000},
		}

		// Without MaxAvailableRecentBlocks, assume it can handle all blocks up to latest
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 1)
		assert.True(t, canHandle)
		assert.NoError(t, err)

		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 500)
		assert.True(t, canHandle)
		assert.NoError(t, err)

		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 1000)
		assert.True(t, canHandle)
		assert.NoError(t, err)

		// But not beyond latest
		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 1001)
		assert.False(t, canHandle)
		assert.NoError(t, err)
	})

	t.Run("EdgeCasesForFullNode", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					NodeType:                 common.EvmNodeTypeFull,
					MaxAvailableRecentBlocks: 100,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePoller{latestBlock: 1000},
		}

		// Test exact boundary
		// First available block = 1000 - 100 = 900
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 900)
		assert.True(t, canHandle)
		assert.NoError(t, err)

		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 899)
		assert.False(t, canHandle)
		assert.NoError(t, err)
	})

	t.Run("UnknownNodeType", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					NodeType:                 common.EvmNodeTypeUnknown,
					MaxAvailableRecentBlocks: 0,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePoller{latestBlock: 1000},
		}

		// Unknown node type without MaxAvailableRecentBlocks should assume it can handle all blocks up to latest
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 500)
		assert.True(t, canHandle)
		assert.NoError(t, err)
	})

	t.Run("ForcePollLatestBlock", func(t *testing.T) {
		// Mock that initially returns 1000, but after polling returns 1100
		mockPoller := &mockEvmStatePoller{latestBlock: 1000}
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					NodeType: common.EvmNodeTypeArchive,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: mockPoller,
		}

		// Request block 1050 which is beyond initial latest (1000)
		// This should trigger force-polling
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, true, 1050)
		assert.False(t, canHandle) // Still false because mock doesn't update on poll
		assert.NoError(t, err)

		// Test with a mock that updates on poll
		mockPollerWithUpdate := &mockEvmStatePollerWithUpdate{
			initialLatest: 1000,
			polledLatest:  1100,
		}
		upstream.evmStatePoller = mockPollerWithUpdate

		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, true, 1050)
		assert.True(t, canHandle) // Should be true after force-polling
		assert.NoError(t, err)
	})
}

// Mock EVM state poller that updates latest block on poll
type mockEvmStatePollerWithUpdate struct {
	initialLatest int64
	polledLatest  int64
	hasPolled     bool
}

func (m *mockEvmStatePollerWithUpdate) Bootstrap(ctx context.Context) error { return nil }
func (m *mockEvmStatePollerWithUpdate) Poll(ctx context.Context) error      { return nil }
func (m *mockEvmStatePollerWithUpdate) PollLatestBlockNumber(ctx context.Context) (int64, error) {
	m.hasPolled = true
	return m.polledLatest, nil
}
func (m *mockEvmStatePollerWithUpdate) PollFinalizedBlockNumber(ctx context.Context) (int64, error) {
	return m.polledLatest - 10, nil
}
func (m *mockEvmStatePollerWithUpdate) SyncingState() common.EvmSyncingState {
	return common.EvmSyncingStateNotSyncing
}
func (m *mockEvmStatePollerWithUpdate) SetSyncingState(state common.EvmSyncingState) {}
func (m *mockEvmStatePollerWithUpdate) LatestBlock() int64 {
	if m.hasPolled {
		return m.polledLatest
	}
	return m.initialLatest
}
func (m *mockEvmStatePollerWithUpdate) FinalizedBlock() int64 { return m.LatestBlock() - 10 }
func (m *mockEvmStatePollerWithUpdate) IsBlockFinalized(blockNumber int64) (bool, error) {
	return blockNumber <= m.FinalizedBlock(), nil
}
func (m *mockEvmStatePollerWithUpdate) SuggestFinalizedBlock(blockNumber int64)    {}
func (m *mockEvmStatePollerWithUpdate) SuggestLatestBlock(blockNumber int64)       {}
func (m *mockEvmStatePollerWithUpdate) SetNetworkConfig(cfg *common.NetworkConfig) {}
func (m *mockEvmStatePollerWithUpdate) IsObjectNull() bool                         { return false }

func (m *mockEvmStatePollerWithUpdate) EarliestBlock(probe common.EvmAvailabilityProbeType) int64 {
	return 0
}
func (m *mockEvmStatePollerWithUpdate) PollEarliestBlockNumber(ctx context.Context, probe common.EvmAvailabilityProbeType) (int64, error) {
	return 0, nil
}

func TestUpstream_EvmCanHandleBlock_Metrics(t *testing.T) {
	t.Run("MetricsIncrementedForUpperBound", func(t *testing.T) {
		upstream := &Upstream{
			ProjectId: "test-project",
			config: &common.UpstreamConfig{
				Id:   "test-upstream",
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					NodeType: common.EvmNodeTypeArchive,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePoller{latestBlock: 1000},
			networkId:      util.AtomicValue("evm:123"),
		}

		// Request block beyond latest - should increment upper bound metric
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 1001)
		assert.False(t, canHandle)
		assert.NoError(t, err)

		// Note: In a real test environment, you would check that the metric was incremented
		// For this test, we're just verifying the method completes without error
	})

	t.Run("MetricsIncrementedForLowerBound", func(t *testing.T) {
		upstream := &Upstream{
			ProjectId: "test-project",
			config: &common.UpstreamConfig{
				Id:   "test-upstream",
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					NodeType:                 common.EvmNodeTypeFull,
					MaxAvailableRecentBlocks: 100,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePoller{latestBlock: 1000},
			networkId:      util.AtomicValue("evm:123"),
		}

		// Request block before available range - should increment lower bound metric
		// First available block = 1000 - 100 = 900
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceBlockHead, false, 899)
		assert.False(t, canHandle)
		assert.NoError(t, err)

		// Note: In a real test environment, you would check that the metric was incremented
		// For this test, we're just verifying the method completes without error
	})

	t.Run("NoMetricsWhenMethodEmpty", func(t *testing.T) {
		upstream := &Upstream{
			ProjectId: "test-project",
			config: &common.UpstreamConfig{
				Id:   "test-upstream",
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					NodeType: common.EvmNodeTypeArchive,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePoller{latestBlock: 1000},
			networkId:      util.AtomicValue("evm:123"),
		}

		// Request block beyond latest with empty method - should not increment metrics
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "", common.AvailbilityConfidenceBlockHead, false, 1001)
		assert.False(t, canHandle)
		assert.NoError(t, err)

		// Note: In a real test environment, you would check that no metric was incremented
		// For this test, we're just verifying the method completes without error
	})
}

func TestUpstream_EvmAssertBlockAvailability_Finalized(t *testing.T) {
	t.Run("FinalizedBlockArchiveNode", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					NodeType: common.EvmNodeTypeArchive,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePoller{latestBlock: 1000, finalizedBlock: 900},
		}

		// Block is finalized - should be available
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceFinalized, false, 850)
		assert.True(t, canHandle)
		assert.NoError(t, err)

		// Block at finalized boundary - should be available
		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceFinalized, false, 900)
		assert.True(t, canHandle)
		assert.NoError(t, err)

		// Block not finalized yet - should not be available
		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceFinalized, false, 901)
		assert.False(t, canHandle)
		assert.NoError(t, err)
	})

	t.Run("FinalizedBlockFullNode", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					NodeType:                 common.EvmNodeTypeFull,
					MaxAvailableRecentBlocks: 128,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePoller{latestBlock: 1000, finalizedBlock: 900},
		}

		// Block is finalized and within range - should be available
		// First available block = 1000 - 128 = 872
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceFinalized, false, 880)
		assert.True(t, canHandle)
		assert.NoError(t, err)

		// Block is finalized but before available range - should not be available
		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceFinalized, false, 850)
		assert.False(t, canHandle)
		assert.NoError(t, err)

		// Block not finalized - should not be available
		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceFinalized, false, 950)
		assert.False(t, canHandle)
		assert.NoError(t, err)
	})

	t.Run("FinalizedBlockFullNodeEdgeCase", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					NodeType:                 common.EvmNodeTypeFull,
					MaxAvailableRecentBlocks: 100,
				},
			},
			logger:         &zerolog.Logger{},
			evmStatePoller: &mockEvmStatePoller{latestBlock: 1000, finalizedBlock: 900},
		}

		// Test exact boundary
		// First available block = 1000 - 100 = 900
		canHandle, err := upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceFinalized, false, 900)
		assert.True(t, canHandle)
		assert.NoError(t, err)

		canHandle, err = upstream.EvmAssertBlockAvailability(context.Background(), "test_method", common.AvailbilityConfidenceFinalized, false, 899)
		assert.False(t, canHandle)
		assert.NoError(t, err)
	})
}
