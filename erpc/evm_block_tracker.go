package erpc

import (
	"context"
	"fmt"
	"time"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/upstream"
)

type EvmBlockTracker struct {
	ctx       context.Context
	ctxCancel context.CancelFunc
	network   *PreparedNetwork

	LatestBlockNumber    uint64
	FinalizedBlockNumber uint64
}

func NewEvmBlockTracker(network *PreparedNetwork) *EvmBlockTracker {
	return &EvmBlockTracker{
		network: network,
	}
}

func (e *EvmBlockTracker) Bootstrap(ctx context.Context) error {
	if e.ctx != nil {
		return nil
	}

	e.ctx, e.ctxCancel = context.WithCancel(ctx)

	var blockTrackerInterval = 60 * time.Second // default value
	var err error
	if e.network.Config.Evm != nil && e.network.Config.Evm.BlockTrackerInterval != "" {
		blockTrackerInterval, err = time.ParseDuration(e.network.Config.Evm.BlockTrackerInterval)
		if err != nil {
			return err
		}
	}

	go (func() {
		for {
			e.network.Logger.Debug().Msg("fetching latest block")
			select {
			case <-e.ctx.Done():
				return
			default:
				lb, err := e.fetchLatestBlockNumber(e.ctx)
				if err != nil {
					e.network.Logger.Error().Err(err).Msg("failed to get latest block number in block tracker")
				}
				e.network.Logger.Debug().Uint64("blockNumber", lb).Msg("fetched latest block")
				if lb > 0 {
					e.LatestBlockNumber = lb
				}

				fb, err := e.fetchFinalizedBlockNumber(e.ctx)
				if err != nil {
					e.network.Logger.Error().Err(err).Msg("failed to get finalized block number in block tracker")
				}
				e.network.Logger.Debug().Uint64("blockNumber", fb).Msg("fetched finalized block")
				if fb > 0 {
					e.FinalizedBlockNumber = fb
				}
			}

			time.Sleep(blockTrackerInterval)
		}
	})()

	return nil
}

func (e *EvmBlockTracker) Shutdown() {
	if e.ctxCancel != nil {
		e.ctxCancel()
	}
}

func (e *EvmBlockTracker) fetchLatestBlockNumber(ctx context.Context) (uint64, error) {
	return e.fetchBlock(ctx, "latest")
}

func (e *EvmBlockTracker) fetchFinalizedBlockNumber(ctx context.Context) (uint64, error) {
	return e.fetchBlock(ctx, "finalized")
}

func (e *EvmBlockTracker) fetchBlock(ctx context.Context, blockTag string) (uint64, error) {
	pr := upstream.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["%s",false]}`, blockTag))).WithNetwork(e.network)
	resp, err := e.network.Forward(ctx, pr)
	if err != nil {
		return 0, err
	}

	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return 0, err
	}

	if jrr.Error != nil {
		return 0, jrr.Error
	}

	// If result is nil, return 0
	if jrr.Result == nil || jrr.Result.(map[string]interface{}) == nil || jrr.Result.(map[string]interface{})["number"] == nil {
		return 0, &common.BaseError{
			Code:    "ErrEvmBlockTracker",
			Message: "block not found",
			Details: map[string]interface{}{
				"blockTag": blockTag,
				"result":   jrr.Result,
			},
		}
	}

	return common.HexToUint64(jrr.Result.(map[string]interface{})["number"].(string))
}
