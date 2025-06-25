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

		// Check if this is a duplicate nonce error (transaction already in mempool or nonce too low)
		if _, isDuplicateNonce := err.(*common.ErrEndpointDuplicateNonce); isDuplicateNonce {
			txn, err := getTransactionFromRequest(ctx, rq)
			if err != nil {
				return rs, err
			}

			txnHash := "0x" + hex.EncodeToString(txn.Hash().Bytes())

			// For nonce too low, verify the transaction exists on-chain and is exactly the same
			if strings.Contains(deepestMessage, "nonce too low") {
				evmUpstream, ok := u.(common.EvmUpstream)
				if !ok {
					return rs, fmt.Errorf("upstream is not an evm upstream")
				}

				txData, err := evmUpstream.EvmGetTransactionByHash(ctx, txnHash)
				if err != nil {
					return rs, re
				}

				// Verify it's exactly the same transaction using signature values
				if !IsExactTxMatchSigNonce(txn, txData) {
					return rs, re
				}
			}

			jrr, err := common.NewJsonRpcResponse(rq.ID(), txnHash, nil)
			if err != nil {
				return rs, err
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

func IsExactTxMatchSigNonce(a, b *types.Transaction) bool {
	if a.Nonce() != b.Nonce() {
		return false
	}
	ra, sa, va := a.RawSignatureValues()
	rb, sb, vb := b.RawSignatureValues()
	return ra.Cmp(rb) == 0 && sa.Cmp(sb) == 0 && va.Cmp(vb) == 0
}
