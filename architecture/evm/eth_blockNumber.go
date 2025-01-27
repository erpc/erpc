package evm

import (
	"context"

	"github.com/erpc/erpc/common"
)

// preForward_eth_blockNumber calls the normal forward, then compares the returned block number,
// to all EVM upstreams' StatePoller for this network and returns the highest block number across them.
func preForward_eth_blockNumber(ctx context.Context, network common.Network, nq *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	// Step 1: forward the request normally
	resp, err = network.Forward(ctx, nq)
	if err != nil {
		return true, nil, err
	}

	// Step 2: parse the block number from the existing response
	_, blockNumber, err := ExtractBlockReferenceFromResponse(resp)
	if err != nil {
		return true, nil, err
	}

	// Step 3: collect the highest block from all EVM upstream pollers for this network
	highestBlock := network.EvmHighestLatestBlockNumber()

	// Step 4: if maxBlock is larger than forwardBlock, write that in the response
	if highestBlock > blockNumber {
		network.Logger().Debug().
			Str("method", "eth_blockNumber").
			Int64("knownHighestBlock", highestBlock).
			Int64("responseBlockNumber", blockNumber).
			Str("upstreamId", resp.UpstreamId()).
			Msg("upstream returned older block than we known, falling back to highest known block")
		hbk, err := common.NormalizeHex(highestBlock)
		if err != nil {
			return true, nil, err
		}
		jrr, err := common.NewJsonRpcResponse(nq.ID(), hbk, nil)
		if err != nil {
			return true, nil, err
		}
		resp := common.NewNormalizedResponse().
			WithRequest(nq).
			WithJsonRpcResponse(jrr)

		return true, resp, nil
	}

	return true, resp, nil
}
