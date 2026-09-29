package svm

import (
	"context"
	"strings"

	"github.com/erpc/erpc/common"
)

// ExtractRequestedCommitment returns the commitment the caller explicitly put
// on the request — before any network-default injection and without folding in
// Solana's server-side default. Used by failsafe matchCommitment.
//
// Semantics:
//   - No commitment field / positional token → CommitmentNone
//   - processed|confirmed|finalized (any case) → that level
//   - Malformed or unsupported value → CommitmentUnknown (does not match "none")
//   - preflightCommitment is ignored (write-path preflight only)
//   - Positional string forms (e.g. getSignaturesForAddress trailing "confirmed")
//     are recognized when the string is a known commitment token; encoding
//     strings like "base64" are not treated as commitment or as unknown
func ExtractRequestedCommitment(ctx context.Context, r *common.NormalizedRequest) common.CommitmentLevel {
	if r == nil {
		return common.CommitmentNone
	}
	rpcReq, err := r.JsonRpcRequest(ctx)
	if err != nil || rpcReq == nil {
		return common.CommitmentNone
	}

	rpcReq.RLock()
	defer rpcReq.RUnlock()

	for _, p := range rpcReq.Params {
		m, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		raw, exists := m["commitment"]
		if !exists {
			continue
		}
		switch v := raw.(type) {
		case string:
			if strings.TrimSpace(v) == "" {
				// Key present but empty: treat as none and stop — do not let a
				// trailing positional token override the object field.
				return common.CommitmentNone
			}
			return common.ParseRequestedCommitment(v)
		case nil:
			// Key present with null: same as empty — stop before positional scan.
			return common.CommitmentNone
		default:
			// Non-string commitment (number, bool, object) is malformed.
			return common.CommitmentUnknown
		}
	}

	// Positional commitment: a bare string param that is a known commitment
	// token. Prefer later params (trailing forms like getSignaturesForAddress
	// [pubkey, {opts}, "confirmed"]). Encoding strings are left alone.
	//
	// Deliberately method-agnostic (unlike commitmentOptionsIndex): Solana's
	// legacy positional-commitment form isn't confined to one method, and the
	// unknown-input fallthrough must stay safe — only exact known tokens
	// qualify; any other string (encoding, pubkey, etc.) is ignored.
	for i := len(rpcReq.Params) - 1; i >= 0; i-- {
		s, ok := rpcReq.Params[i].(string)
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "processed":
			return common.CommitmentProcessed
		case "confirmed":
			return common.CommitmentConfirmed
		case "finalized":
			return common.CommitmentFinalized
		}
	}

	return common.CommitmentNone
}

// CaptureRequestedCommitment memoizes ExtractRequestedCommitment on the
// request when not already set. Call before any commitment injection.
func CaptureRequestedCommitment(ctx context.Context, r *common.NormalizedRequest) {
	if r == nil {
		return
	}
	if _, ok := r.RequestedCommitment(); ok {
		return
	}
	r.SetRequestedCommitment(ExtractRequestedCommitment(ctx, r))
}
