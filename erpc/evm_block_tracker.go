package erpc

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

type EvmBlockTracker struct {
	network *Network

	mu                   sync.RWMutex
	latestBlockNumber    int64
	finalizedBlockNumber int64

	shutdownChan chan struct{}
}

func NewEvmBlockTracker(network *Network) *EvmBlockTracker {
	return &EvmBlockTracker{
		network:      network,
		shutdownChan: make(chan struct{}),
	}
}

func (e *EvmBlockTracker) Bootstrap(ctx context.Context) error {
	var blockTrackerInterval = 0 * time.Second
	var err error
	if e.network.Config.Evm != nil && e.network.Config.Evm.BlockTrackerInterval != "" {
		blockTrackerInterval, err = time.ParseDuration(e.network.Config.Evm.BlockTrackerInterval)
		if err != nil {
			return err
		}
	} else if !util.IsTest() {
		// For non-test environments, set a default interval of 60 seconds
		blockTrackerInterval = 60 * time.Second
	}

	var doesNotSupportFinalized = false

	var updateBlockNumbers = func() error {
		e.network.Logger.Debug().Msg("fetching latest and finalized block")

		lb, err := e.fetchLatestBlockNumber(ctx)
		if err != nil {
			e.network.Logger.Error().Err(err).Msg("failed to get latest block number in block tracker")
		}
		e.network.Logger.Debug().Int64("blockNumber", lb).Msg("fetched latest block")
		if lb > 0 {
			e.mu.Lock()
			e.latestBlockNumber = lb
			e.mu.Unlock()
		}

		if !doesNotSupportFinalized {
			fb, err := e.fetchFinalizedBlockNumber(ctx)
			if err != nil {
				if common.HasErrorCode(err, common.ErrCodeEndpointClientSideException) {
					doesNotSupportFinalized = true
					e.network.Logger.Warn().Err(err).Msg("this chain does not support fetching finalized block number")
				} else {
					e.network.Logger.Error().Err(err).Msg("failed to get finalized block number in block tracker")
				}
			}
			e.network.Logger.Debug().Int64("blockNumber", fb).Msg("fetched finalized block")
			if fb > 0 {
				e.mu.Lock()
				e.finalizedBlockNumber = fb
				e.mu.Unlock()
			}
		}

		// TODO should we return error here?
		return nil
	}

	if blockTrackerInterval == 0 {
		return nil
	}

	go (func() {
		ticker := time.NewTicker(blockTrackerInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				e.network.Logger.Debug().Msg("shutting down block tracker due to context cancellation")
				return
			case <-e.shutdownChan:
				e.network.Logger.Debug().Msg("shutting down block tracker via shutdown channel")
				return
			case <-ticker.C:
				updateBlockNumbers()
			}
		}
	})()

	go updateBlockNumbers()
	return nil
}

func (e *EvmBlockTracker) LatestBlock() int64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return int64(e.latestBlockNumber)
}

func (e *EvmBlockTracker) FinalizedBlock() int64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.finalizedBlockNumber
}

func (e *EvmBlockTracker) fetchLatestBlockNumber(ctx context.Context) (int64, error) {
	return e.fetchBlock(ctx, "latest")
}

func (e *EvmBlockTracker) fetchFinalizedBlockNumber(ctx context.Context) (int64, error) {
	return e.fetchBlock(ctx, "finalized")
}

func (e *EvmBlockTracker) fetchBlock(ctx context.Context, blockTag string) (int64, error) {
	randId := rand.Intn(10_000_000)
	pr := common.NewNormalizedRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBlockByNumber","params":["%s",false]}`, randId, blockTag),
	))
	pr.SetNetwork(e.network)

	resp, err := e.network.Forward(ctx, pr)
	if err != nil {
		return 0, err
	}

	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return 0, err
	}

	if jrr == nil || jrr.Error != nil {
		return 0, jrr.Error
	}

	// If result is nil or has an invalid structure, return an error
	result, err := jrr.ParsedResult()
	if err != nil {
		return 0, err
	}
	resultMap, ok := result.(map[string]interface{})
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

	return common.HexToInt64(numberStr)
}
