package evm

import (
	"errors"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// binarySearchEarliest must report what it observed. When no block in the
// range answers, or the upstream does not support the probe, the search has
// learned no earliest block, so it must not return one. Returning the value
// the loop converges on records the tip as the earliest retained block, and
// a lower bound of earliestBlockPlus then refuses every historical read.
func TestBinarySearchEarliest_ReportsOnlyWhatItObserved(t *testing.T) {
	t.Run("NothingAvailableIsAnError", func(t *testing.T) {
		// Latest is 1000, and every height answers null.
		p, _, ctx := covPollerNew(t, "earliest-none", covPollerHistory(1000, 2000))

		v, err := p.binarySearchEarliest(ctx, common.EvmProbeBlockHeader, 0, 1000)
		require.Error(t, err, "no block answered, so there is no earliest block; got %d", v)
		assert.True(t, errors.Is(err, errEarliestBlockUnavailable), "got %v", err)
	})

	t.Run("NothingAvailableDoesNotRecordTheTip", func(t *testing.T) {
		p, _, ctx := covPollerNew(t, "earliest-none-poll", covPollerHistory(1000, 2000))

		_, err := p.PollEarliestBlockNumber(ctx, common.EvmProbeBlockHeader, time.Minute)
		require.Error(t, err)
		assert.Zero(t, p.EarliestBlock(common.EvmProbeBlockHeader),
			"the tip must not be recorded as the earliest retained block")
	})

	t.Run("UnsupportedProbeIsReportedAsUnsupported", func(t *testing.T) {
		// Every block exists, but the node has no eth_getLogs.
		p, up, ctx := covPollerNew(t, "earliest-unsupported", func(method string, params []interface{}) (*common.NormalizedResponse, error) {
			switch method {
			case "eth_getBlockByNumber":
				if covPollerBlockTag(params) == "latest" {
					return covPollerBlock(1000), nil
				}
				n, err := common.HexToInt64(covPollerBlockTag(params))
				if err != nil {
					return nil, err
				}
				return covPollerBlock(n), nil
			case "eth_getLogs":
				return covPollerRpcError(-32601, "the method eth_getLogs does not exist/is not available"), nil
			}
			return nil, errors.New("unexpected " + method)
		})

		v, err := p.binarySearchEarliest(ctx, common.EvmProbeEventLogs, 0, 1000)
		require.Error(t, err, "a probe the node does not support says nothing about pruning; got %d", v)
		assert.True(t, errors.Is(err, errEarliestProbeUnsupported), "got %v", err)
		assert.Equal(t, 1, up.callCount("eth_getLogs"), "the first unsupported answer settles the search")
	})

	t.Run("EarliestEqualToHighIsStillFound", func(t *testing.T) {
		// The node retains exactly the tip. No midpoint answers, so only a
		// probe of the converged value can find it.
		p, _, ctx := covPollerNew(t, "earliest-at-tip", covPollerHistory(1000, 1000))

		v, err := p.binarySearchEarliest(ctx, common.EvmProbeBlockHeader, 0, 1000)
		require.NoError(t, err)
		assert.Equal(t, int64(1000), v)
	})

	t.Run("EarliestInsideTheRangeIsFound", func(t *testing.T) {
		p, _, ctx := covPollerNew(t, "earliest-inside", covPollerHistory(1000, 321))

		v, err := p.binarySearchEarliest(ctx, common.EvmProbeBlockHeader, 0, 1000)
		require.NoError(t, err)
		assert.Equal(t, int64(321), v)
	})
}
