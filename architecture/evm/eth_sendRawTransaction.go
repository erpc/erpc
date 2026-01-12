package evm

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/erpc/erpc/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	"go.opentelemetry.io/otel/attribute"
)

// upstreamPostForward_eth_sendRawTransaction handles idempotency for eth_sendRawTransaction.
// It converts "already known" errors into success and verifies "nonce too low" errors
// by checking if the transaction already exists on-chain.
func upstreamPostForward_eth_sendRawTransaction(
	ctx context.Context,
	n common.Network,
	u common.Upstream,
	rq *common.NormalizedRequest,
	rs *common.NormalizedResponse,
	re error,
) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "Upstream.PostForwardHook.eth_sendRawTransaction")
	defer span.End()

	// Check if idempotent transaction broadcast is disabled
	if cfg := n.Config(); cfg != nil && cfg.Evm != nil && cfg.Evm.IdempotentTransactionBroadcast != nil && !*cfg.Evm.IdempotentTransactionBroadcast {
		span.SetAttributes(attribute.Bool("idempotent_broadcast_disabled", true))
		return rs, re
	}

	// If there's no error, no special handling needed
	if re == nil {
		return rs, re
	}

	// Check if the error is a duplicate nonce error
	if !common.HasErrorCode(re, common.ErrCodeEndpointNonceException) {
		return rs, re
	}

	// Extract the duplicate nonce reason from the error
	var nonceErr *common.ErrEndpointNonceException
	if !errors.As(re, &nonceErr) {
		return rs, re
	}

	reason := common.NonceExceptionReason("")
	if nonceErr.Details != nil {
		if r, ok := nonceErr.Details["nonceExceptionReason"].(string); ok {
			reason = common.NonceExceptionReason(r)
		}
	}

	span.SetAttributes(attribute.String("nonce_exception_reason", string(reason)))

	// Parse the transaction from request to get the txHash
	txHash, err := extractTxHashFromSendRawTransaction(ctx, rq)
	if err != nil {
		span.SetAttributes(attribute.String("parse_error", err.Error()))
		// If we can't parse the transaction, return the original error
		return rs, re
	}

	span.SetAttributes(attribute.String("tx_hash", txHash))

	switch reason {
	case common.NonceExceptionReasonAlreadyKnown:
		// For "already known" errors, we can safely return success with the txHash
		// because the same transaction is already in the mempool or has been processed
		return createSyntheticSuccessResponse(ctx, rq, txHash)

	case common.NonceExceptionReasonNonceTooLow:
		// For "nonce too low" errors, we need to verify the transaction on-chain
		// to distinguish between:
		// - Same transaction already mined (idempotent success)
		// - Different transaction with same nonce (error)
		return verifyAndHandleNonceTooLow(ctx, n, u, rq, txHash, re)

	default:
		// Unknown reason, return original error
		return rs, re
	}
}

// extractTxHashFromSendRawTransaction extracts the transaction hash from the request
func extractTxHashFromSendRawTransaction(ctx context.Context, rq *common.NormalizedRequest) (string, error) {
	_, span := common.StartDetailSpan(ctx, "extractTxHashFromSendRawTransaction")
	defer span.End()

	jrpc, err := rq.JsonRpcRequest()
	if err != nil {
		return "", fmt.Errorf("failed to get JSON-RPC request: %w", err)
	}

	params := jrpc.Params
	if len(params) == 0 {
		return "", fmt.Errorf("no params in eth_sendRawTransaction request")
	}

	// The first param should be the signed transaction hex
	rawTxHex, ok := params[0].(string)
	if !ok {
		return "", fmt.Errorf("first param is not a string")
	}

	// Remove 0x prefix if present
	rawTxHex = strings.TrimPrefix(rawTxHex, "0x")

	// Decode the hex string
	rawTx, err := hex.DecodeString(rawTxHex)
	if err != nil {
		return "", fmt.Errorf("failed to decode transaction hex: %w", err)
	}

	// Decode the RLP-encoded transaction
	tx := new(ethtypes.Transaction)
	if err := rlp.DecodeBytes(rawTx, tx); err != nil {
		return "", fmt.Errorf("failed to decode RLP transaction: %w", err)
	}

	// Get the transaction hash
	txHash := tx.Hash()
	return txHash.Hex(), nil
}

// createSyntheticSuccessResponse creates a JSON-RPC success response with the txHash
func createSyntheticSuccessResponse(ctx context.Context, rq *common.NormalizedRequest, txHash string) (*common.NormalizedResponse, error) {
	_, span := common.StartDetailSpan(ctx, "createSyntheticSuccessResponse")
	defer span.End()

	span.SetAttributes(attribute.String("tx_hash", txHash))

	// Create a JSON-RPC response with the transaction hash as the result
	jrr, err := common.NewJsonRpcResponse(rq.ID(), txHash, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create JSON-RPC response: %w", err)
	}

	resp := common.NewNormalizedResponse().
		WithRequest(rq).
		WithJsonRpcResponse(jrr)

	return resp, nil
}

// verifyAndHandleNonceTooLow verifies the transaction on-chain for "nonce too low" errors
func verifyAndHandleNonceTooLow(
	ctx context.Context,
	n common.Network,
	u common.Upstream,
	rq *common.NormalizedRequest,
	txHash string,
	originalErr error,
) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "verifyAndHandleNonceTooLow")
	defer span.End()

	span.SetAttributes(attribute.String("tx_hash", txHash))

	// Create a request for eth_getTransactionByHash
	getTxReq := common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%d,"method":"eth_getTransactionByHash","params":[%q]}`,
		rq.ID(),
		txHash,
	)))

	// Forward the request to the same upstream
	resp, err := u.Forward(ctx, getTxReq, true)
	if err != nil {
		span.SetAttributes(attribute.String("forward_error", err.Error()))
		// If we can't verify, return the original error
		return nil, createNormalizedNonceTooLowError(originalErr)
	}

	// Check if the transaction was found
	if resp == nil || resp.IsResultEmptyish() {
		span.SetAttributes(attribute.Bool("tx_found", false))
		// Transaction not found - this is a different transaction with same nonce
		return nil, createNormalizedNonceTooLowError(originalErr)
	}

	// Transaction found - verify it's the same transaction
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		span.SetAttributes(attribute.String("parse_response_error", err.Error()))
		return nil, createNormalizedNonceTooLowError(originalErr)
	}

	// Parse the transaction response to verify it matches
	if jrr == nil || jrr.Error != nil {
		span.SetAttributes(attribute.Bool("response_has_error", true))
		return nil, createNormalizedNonceTooLowError(originalErr)
	}

	// The transaction exists on-chain with the same hash, so this is idempotent
	span.SetAttributes(attribute.Bool("tx_found", true))
	span.SetAttributes(attribute.Bool("idempotent_success", true))

	return createSyntheticSuccessResponse(ctx, rq, txHash)
}

// createNormalizedNonceTooLowError creates a normalized error for nonce-too-low mismatch cases.
// Per the plan: use JSON-RPC code -32003 (Transaction rejected) while preserving the upstream message.
func createNormalizedNonceTooLowError(originalErr error) error {
	// Extract the original message from the error
	var message string
	var dupErr *common.ErrEndpointNonceException
	if errors.As(originalErr, &dupErr) {
		if cause := dupErr.GetCause(); cause != nil {
			var jrpcErr *common.ErrJsonRpcExceptionInternal
			if errors.As(cause, &jrpcErr) {
				message = jrpcErr.Message
			} else {
				message = cause.Error()
			}
		} else {
			message = dupErr.Message
		}
	} else {
		message = "nonce too low"
	}

	// Return a client-side exception with normalized code -32003 (Transaction rejected)
	return common.NewErrEndpointClientSideException(
		common.NewErrJsonRpcExceptionInternal(
			int(common.JsonRpcErrorTransactionRejected), // -32003
			common.JsonRpcErrorTransactionRejected,
			message,
			nil,
			map[string]interface{}{
				"retryableTowardNetwork": false,
			},
		),
	).WithRetryableTowardNetwork(false)
}
