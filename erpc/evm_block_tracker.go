package erpc

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/upstream"
)

type EvmBlockTracker struct {
	ctx       context.Context
	ctxCancel context.CancelFunc
	network   *Network

	updateMu             sync.Mutex
	latestBlockNumber    uint64
	finalizedBlockNumber uint64
}

func NewEvmBlockTracker(network *Network) *EvmBlockTracker {
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

	var updateBlockNumbers = func() error {
		e.updateMu.Lock()
		defer e.updateMu.Unlock()

		lb, err := e.fetchLatestBlockNumber(e.ctx)
		if err != nil {
			e.network.Logger.Error().Err(err).Msg("failed to get latest block number in block tracker")
		}
		e.network.Logger.Debug().Uint64("blockNumber", lb).Msg("fetched latest block")
		if lb > 0 {
			e.latestBlockNumber = lb
		}

		fb, err := e.fetchFinalizedBlockNumber(e.ctx)
		if err != nil {
			e.network.Logger.Error().Err(err).Msg("failed to get finalized block number in block tracker")
		}
		e.network.Logger.Debug().Uint64("blockNumber", fb).Msg("fetched finalized block")
		if fb > 0 {
			e.finalizedBlockNumber = fb
		}

		// TODO should we return error here?
		return nil
	}

	go (func() {
		for {
			e.network.Logger.Debug().Msg("fetching latest block")
			select {
			case <-e.ctx.Done():
				return
			default:
				updateBlockNumbers()
			}

			time.Sleep(blockTrackerInterval)
		}
	})()

	return updateBlockNumbers()
}

func (e *EvmBlockTracker) Shutdown() {
	if e.ctxCancel != nil {
		e.ctxCancel()
	}
}

func (e *EvmBlockTracker) LatestBlock() uint64 {
	return e.latestBlockNumber
}

func (e *EvmBlockTracker) FinalizedBlock() uint64 {
	return e.finalizedBlockNumber
}

func (e *EvmBlockTracker) fetchLatestBlockNumber(ctx context.Context) (uint64, error) {
	return e.fetchBlock(ctx, "latest")
}

func (e *EvmBlockTracker) fetchFinalizedBlockNumber(ctx context.Context) (uint64, error) {
	return e.fetchBlock(ctx, "finalized")
}

func (e *EvmBlockTracker) fetchBlock(ctx context.Context, blockTag string) (uint64, error) {
	e.network.Logger.Debug().Str("blockTag", blockTag).Msg("FEEEEEEEEETCHING BLOCK")

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

	// If result is nil or has an invalid structure, return an error
	resultMap, ok := jrr.Result.(map[string]interface{})
	if !ok || resultMap == nil || resultMap["number"] == nil {
		return 0, &common.BaseError{
			Code:    "ErrEvmBlockTracker",
			Message: "block not found",
			Details: map[string]interface{}{
				"blockTag": blockTag,
				"result":   jrr.Result,
			},
		}
	}

	numberStr, ok := resultMap["number"].(string)
	if !ok {
		return 0, &common.BaseError{
			Code:    "ErrEvmBlockTracker",
			Message: "block number is not a string",
			Details: map[string]interface{}{
				"blockTag": blockTag,
				"result":   jrr.Result,
			},
		}
	}

	return common.HexToUint64(numberStr)
}
