package evm

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/ethereum/go-ethereum/core/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func upstreamPostForward_eth_sendRawTransaction(ctx context.Context, n common.Network, u common.Upstream, rq *common.NormalizedRequest, rs *common.NormalizedResponse, re error, _ bool) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "Upstream.PostForwardHook.eth_sendRawTransaction", trace.WithAttributes(
		attribute.String("request.id", fmt.Sprintf("%v", rq.ID())),
		attribute.String("network.id", n.Id()),
		attribute.String("upstream.id", u.Id()),
	))
	defer span.End()

	if re != nil {
		err, ok := re.(common.StandardError)
		if !ok {
			return rs, re
		}

		deepestMessage := err.DeepestMessage()

		switch deepestMessage {
		// todo: we should probably check for nonce too low to ensure that the transaction hash used that nonce / eth_getTransactionByHash
		case "transaction already in mempool", "nonce too low":
			txn, err := getTransactionFromRequest(ctx, rq)
			if err != nil {
				return rs, err
			}

			txnHash := "0x" + hex.EncodeToString(txn.Hash().Bytes())

			jrr, err := common.NewJsonRpcResponse(rq.ID(), txnHash, nil)
			if err != nil {
				return rs, err
			}

			if deepestMessage == "nonce too low" {
				evmUpstream, ok := u.(common.EvmUpstream)
				if !ok {
					return rs, fmt.Errorf("upstream is not an evm upstream")
				}

				// todo(wparr-circle): could we short circuit this by checking the timestamp of the pre-signed transaction to avoid
				// sending a eth_sendRawTransaction request on every nonce too low error? Probably not...
				_, err := evmUpstream.EvmGetTransactionByHash(ctx, txnHash)
				if err != nil {
					break
				}

				// todo(wparr-circle): we should check if the transaction is older than t seconds ago, and if so, return the original error...
			}

			if rs != nil {
				rs.WithJsonRpcResponse(jrr)
			} else {
				rs = common.NewNormalizedResponse().
					WithJsonRpcResponse(jrr).
					WithRequest(rq)
			}

			return rs, nil
		}
	}

	return rs, re
}

func getTransactionFromRequest(ctx context.Context, rq *common.NormalizedRequest) (*types.Transaction, error) {
	req, err := rq.JsonRpcRequest(ctx)
	if err != nil {
		return nil, err
	}

	rawTxHex, ok := req.Params[0].(string)
	if !ok {
		return nil, fmt.Errorf("unable to get raw transaction hex from request params")
	}

	rawTxHex = strings.TrimPrefix(rawTxHex, "0x")

	rawTxBytes, err := hex.DecodeString(rawTxHex)
	if err != nil {
		return nil, err
	}

	var tx types.Transaction
	if err := tx.UnmarshalBinary(rawTxBytes); err != nil {
		return nil, err
	}
	return &tx, nil
}
