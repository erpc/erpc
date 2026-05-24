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
	resp, err := u.Forward(ctx, getTxReq, true, false)
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

// networkPostForward_eth_sendRawTransaction is the LAST-LINE idempotency check.
//
// Context: upstreamPostForward_eth_sendRawTransaction only fires when an upstream
// returns a recognized nonce exception ("already known" / "nonce too low") via the
// string-match list in error_normalizer.go. When upstreams are degraded and return
// generic HTTP 5xx, transport errors, or vendor-specific wordings outside that list,
// the per-upstream hook is bypassed. The failsafe loop then exhausts all retries
// and surfaces ErrUpstreamsExhausted (-32603 "all upstream attempts failed") to the
// client — even though the tx may already be in mempool or mined on some upstream.
//
// This network-level hook runs once after the failsafe loop has finished. If the
// final error is an exhausted-class failure, it issues a single eth_getTransactionByHash
// against the network: if the tx is present anywhere, the broadcast effectively
// succeeded and we return a synthetic success. If the tx is genuinely missing,
// the original error propagates unchanged.
func networkPostForward_eth_sendRawTransaction(
	ctx context.Context,
	n common.Network,
	nq *common.NormalizedRequest,
	nr *common.NormalizedResponse,
	re error,
) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "Network.PostForwardHook.eth_sendRawTransaction")
	defer span.End()

	// No error — let the response flow through untouched.
	if re == nil {
		return nr, nil
	}

	lg := n.Logger().With().Str("hook", "network.eth_sendRawTransaction").Logger()

	// Only intervene on exhausted-class failures. Clean client-side rejections
	// (insufficient funds, replacement underpriced, normalized -32003 nonce-too-low
	// where on-chain verification already happened, etc.) must propagate as-is —
	// and we shouldn't even consult the network config to make that decision, so
	// this gate runs before any other inspection.
	if !common.HasErrorCode(re,
		common.ErrCodeUpstreamsExhausted,
		common.ErrCodeFailsafeRetryExceeded,
	) {
		lg.Debug().Str("errorCode", string(common.ErrorFingerprint(re))).Msg("error is not exhausted-class, skipping verification")
		return nr, re
	}

	// Respect the same opt-out as the per-upstream hook. When idempotent broadcast
	// is explicitly disabled, do not synthesize success from a verification probe.
	if cfg := n.Config(); cfg != nil && cfg.Evm != nil && cfg.Evm.IdempotentTransactionBroadcast != nil && !*cfg.Evm.IdempotentTransactionBroadcast {
		span.SetAttributes(attribute.Bool("idempotent_broadcast_disabled", true))
		lg.Debug().Msg("idempotent transaction broadcast is disabled, skipping network verification")
		return nr, re
	}

	span.SetAttributes(attribute.Bool("exhausted_class_error", true))

	// Extract the tx hash from the request (deterministic from signed bytes).
	txHash, err := extractTxHashFromSendRawTransaction(ctx, nq)
	if err != nil {
		span.SetAttributes(attribute.String("parse_error", err.Error()))
		lg.Debug().Err(err).Msg("failed to extract txHash for verification, returning original error")
		return nr, re
	}
	span.SetAttributes(attribute.String("tx_hash", txHash))

	// Probe the network for the tx. Use the same network-level Forward so the
	// query is routed through normal upstream selection (any healthy upstream
	// — including ones that just rejected the broadcast — can answer this read).
	getTxReq := common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%d,"method":"eth_getTransactionByHash","params":[%q]}`,
		util.RandomID(),
		txHash,
	)))

	verifyResp, verifyErr := n.Forward(ctx, getTxReq)
	if verifyResp != nil {
		defer verifyResp.Release()
	}
	if verifyErr != nil {
		span.SetAttributes(attribute.String("verify_error", verifyErr.Error()))
		lg.Debug().Err(verifyErr).Str("txHash", txHash).Msg("network verification failed, returning original error")
		return nr, re
	}
	if verifyResp == nil || verifyResp.IsResultEmptyish(ctx) {
		span.SetAttributes(attribute.Bool("tx_found", false))
		lg.Debug().Str("txHash", txHash).Msg("tx not found in network, returning original error")
		return nr, re
	}
	jrr, jrrErr := verifyResp.JsonRpcResponse()
	if jrrErr != nil || jrr == nil || jrr.Error != nil {
		span.SetAttributes(attribute.Bool("verify_response_invalid", true))
		lg.Debug().Str("txHash", txHash).Msg("network verification response invalid, returning original error")
		return nr, re
	}

	// Tx is present. The broadcast effectively succeeded — return a synthetic success.
	span.SetAttributes(attribute.Bool("tx_found", true))
	span.SetAttributes(attribute.Bool("synthetic_success", true))
	lg.Info().Str("txHash", txHash).Msg("exhausted error overridden: tx found in network, returning synthetic success")
	return createSyntheticSuccessResponse(ctx, nq, txHash)
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
