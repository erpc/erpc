package common

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTranslateToJsonRpcException_UnsupportedMethod_Returns32601 pins that
// "method not found" upstream errors surface as the JSON-RPC standard -32601
// rather than the generic -32603 wrap.
//
// Discovered via dogfood against a Solana mainnet endpoint: clients calling a
// method the upstream doesn't implement received -32603 (Internal error) with
// a "Method not found" string, breaking spec-compliant clients that match on
// `error.code === -32601`.
//
// EVM's normalizer already wraps with ErrJsonRpcExceptionInternal carrying
// the upstream's original code, so the early-return-on-existing-internal
// branch in TranslateToJsonRpcException keeps EVM behavior identical. This
// fix only changes behavior for callers that wrap ErrEndpointUnsupported
// with a plain cause (e.g. future non-EVM architectures).
func TestTranslateToJsonRpcException_UnsupportedMethod_Returns32601(t *testing.T) {
	t.Run("plain_cause_returns_32601", func(t *testing.T) {
		// Direct ErrEndpointUnsupported with a plain string cause (the case
		// where no inner ErrJsonRpcExceptionInternal exists in the chain).
		err := NewErrEndpointUnsupported(errors.New("Method not found"))

		out := TranslateToJsonRpcException(err)
		require.NotNil(t, out)

		jre := &ErrJsonRpcExceptionInternal{}
		require.True(t, errors.As(out, &jre), "expected a *ErrJsonRpcExceptionInternal")
		assert.Equal(t, JsonRpcErrorUnsupportedException, jre.NormalizedCode(),
			"ErrEndpointUnsupported with plain cause must surface as -32601, not the default -32603")
	})

	t.Run("wrapped_in_failsafe_returns_32601", func(t *testing.T) {
		// Real-world chain: ErrFailsafeRetryExceeded → ErrUpstreamRequest →
		// ErrEndpointUnsupported(plain cause). HasErrorCode walks the chain.
		base := NewErrEndpointUnsupported(errors.New("Method not found"))
		now := time.Now()
		wrapped := NewErrFailsafeRetryExceeded(ScopeUpstream, base, &now)

		out := TranslateToJsonRpcException(wrapped)
		require.NotNil(t, out)

		jre := &ErrJsonRpcExceptionInternal{}
		require.True(t, errors.As(out, &jre))
		assert.Equal(t, JsonRpcErrorUnsupportedException, jre.NormalizedCode(),
			"ErrEndpointUnsupported deep in the cause chain must still surface as -32601")
	})

	t.Run("preserves_existing_internal_jsonrpc_code", func(t *testing.T) {
		// EVM-style chain: the cause is already ErrJsonRpcExceptionInternal
		// carrying the upstream's original code (e.g. a vendor that returned
		// a non-standard code with "method not found" semantics). The early
		// return in TranslateToJsonRpcException must keep that code intact —
		// the new -32601 case must NOT override it.
		inner := NewErrJsonRpcExceptionInternal(
			-32601, // originalCode (vendor-supplied)
			JsonRpcErrorEvmLargeRange, // normalizedCode used as a sentinel here
			"vendor-specific message",
			nil,
			nil,
		)
		base := NewErrEndpointUnsupported(inner)

		out := TranslateToJsonRpcException(base)
		require.NotNil(t, out)

		jre := &ErrJsonRpcExceptionInternal{}
		require.True(t, errors.As(out, &jre))
		assert.Equal(t, JsonRpcErrorEvmLargeRange, jre.NormalizedCode(),
			"existing ErrJsonRpcExceptionInternal in chain must take precedence over the new -32601 mapping")
	})
}
