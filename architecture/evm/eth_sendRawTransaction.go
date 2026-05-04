package evm

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/rs/zerolog"
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

	lg := n.Logger().With().Str("hook", "eth_sendRawTransaction").Logger()

	// Check if idempotent transaction broadcast is disabled
	if cfg := n.Config(); cfg != nil && cfg.Evm != nil && cfg.Evm.IdempotentTransactionBroadcast != nil && !*cfg.Evm.IdempotentTransactionBroadcast {
		span.SetAttributes(attribute.Bool("idempotent_broadcast_disabled", true))
		lg.Debug().Msg("idempotent transaction broadcast is disabled, skipping")
		return rs, re
	}

	// If there's no error, no special handling needed
	if re == nil {
		lg.Debug().Msg("no error from upstream, returning success as-is")
		return rs, re
	}

	lg.Debug().Err(re).Msg("received error from upstream, checking for idempotency handling")

	// Check if the error is a duplicate nonce error
	if !common.HasErrorCode(re, common.ErrCodeEndpointNonceException) {
		lg.Debug().Str("errorCode", string(common.ErrorCode(re.Error()))).Msg("error is not a nonce exception, returning original error")
		return rs, re
	}

	// Extract the duplicate nonce reason from the error
	var nonceErr *common.ErrEndpointNonceException
	if !errors.As(re, &nonceErr) {
		lg.Debug().Msg("could not extract nonce exception details, returning original error")
		return rs, re
	}

	reason := common.NonceExceptionReason("")
	if nonceErr.Details != nil {
		if r, ok := nonceErr.Details["nonceExceptionReason"].(string); ok {
			reason = common.NonceExceptionReason(r)
		}
	}

	lg.Debug().Str("nonceExceptionReason", string(reason)).Msg("detected nonce exception")
	span.SetAttributes(attribute.String("nonce_exception_reason", string(reason)))

	// Parse the transaction from request to get the txHash
	txHash, err := extractTxHashFromSendRawTransaction(ctx, rq)
	if err != nil {
		span.SetAttributes(attribute.String("parse_error", err.Error()))
		lg.Debug().Err(err).Msg("failed to extract txHash from request, returning original error")
		return rs, re
	}

	lg.Debug().Str("txHash", txHash).Msg("extracted txHash from raw transaction")
	span.SetAttributes(attribute.String("tx_hash", txHash))

	switch reason {
	case common.NonceExceptionReasonAlreadyKnown:
		// For "already known" errors, we can safely return success with the txHash
		// because the same transaction is already in the mempool or has been processed
		lg.Info().Str("txHash", txHash).Msg("converting 'already known' error to idempotent success")
		return createSyntheticSuccessResponse(ctx, rq, txHash)

	case common.NonceExceptionReasonNonceTooLow:
		// For "nonce too low" errors, we need to verify the transaction on-chain
		// to distinguish between:
		// - Same transaction already mined (idempotent success)
		// - Different transaction with same nonce (error)
		lg.Debug().Str("txHash", txHash).Msg("verifying 'nonce too low' error by checking on-chain")
		return verifyAndHandleNonceTooLow(ctx, u, rq, txHash, re, &lg)

	default:
		lg.Debug().Str("reason", string(reason)).Msg("unknown nonce exception reason, returning original error")
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

	// Decode the transaction using UnmarshalBinary which handles both:
	// - Legacy (type-0) transactions: RLP-encoded
	// - Typed (EIP-2718) transactions: type byte + RLP-encoded (EIP-1559, EIP-2930, etc.)
	tx := new(ethtypes.Transaction)
	if err := tx.UnmarshalBinary(rawTx); err != nil {
		return "", fmt.Errorf("failed to decode transaction: %w", err)
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
	u common.Upstream,
	rq *common.NormalizedRequest,
	txHash string,
	originalErr error,
	lg *zerolog.Logger,
) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "verifyAndHandleNonceTooLow")
	defer span.End()

	span.SetAttributes(attribute.String("tx_hash", txHash))

	// Create a request for eth_getTransactionByHash
	// Use a new random ID since this is an internal verification request
	getTxReq := common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%d,"method":"eth_getTransactionByHash","params":[%q]}`,
		util.RandomID(),
		txHash,
	)))

	lg.Debug().Str("txHash", txHash).Str("upstream", u.Id()).Msg("sending eth_getTransactionByHash to verify tx exists")

	// Forward the request to the same upstream
	resp, err := u.Forward(ctx, getTxReq, true)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil {
		span.SetAttributes(attribute.String("forward_error", err.Error()))
		lg.Debug().Err(err).Str("txHash", txHash).Msg("failed to verify tx on-chain, returning original error")
		return nil, createNormalizedNonceTooLowError(originalErr)
	}

	// Check if the transaction was found
	if resp == nil || resp.IsResultEmptyish() {
		span.SetAttributes(attribute.Bool("tx_found", false))
		lg.Debug().Str("txHash", txHash).Msg("tx NOT found on-chain - different tx with same nonce, returning error")
		return nil, createNormalizedNonceTooLowError(originalErr)
	}

	// Transaction found - verify it's the same transaction
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		span.SetAttributes(attribute.String("parse_response_error", err.Error()))
		lg.Debug().Err(err).Str("txHash", txHash).Msg("failed to parse verification response, returning original error")
		return nil, createNormalizedNonceTooLowError(originalErr)
	}

	// Parse the transaction response to verify it matches
	if jrr == nil || jrr.Error != nil {
		span.SetAttributes(attribute.Bool("response_has_error", true))
		lg.Debug().Str("txHash", txHash).Msg("verification response has error, returning original error")
		return nil, createNormalizedNonceTooLowError(originalErr)
	}

	// The transaction exists on-chain with the same hash, so this is idempotent
	span.SetAttributes(attribute.Bool("tx_found", true))
	span.SetAttributes(attribute.Bool("idempotent_success", true))
	lg.Info().Str("txHash", txHash).Msg("tx FOUND on-chain - converting 'nonce too low' to idempotent success")

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
