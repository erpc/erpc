package common

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestErrorSummary_CompactMode_CompoundLabels(t *testing.T) {
	// Switch to compact mode for these tests, restore afterwards
	SetErrorLabelMode(ErrorLabelModeCompact)
	t.Cleanup(func() { SetErrorLabelMode(ErrorLabelModeVerbose) })

	t.Run("RetryExceeded_WithStandardCause", func(t *testing.T) {
		cause := NewErrEndpointRequestTimeout(5*time.Second, nil)
		err := NewErrFailsafeRetryExceeded(Scope("connector"), cause, nil)

		summary := ErrorSummary(err)
		assert.Equal(t, "ErrFailsafeRetryExceeded/ErrEndpointRequestTimeout", summary)
	})

	t.Run("TimeoutExceeded_WithStandardCause", func(t *testing.T) {
		cause := NewErrEndpointRequestTimeout(5*time.Second, nil)
		err := NewErrFailsafeTimeoutExceeded(Scope("connector"), cause, nil)

		summary := ErrorSummary(err)
		assert.Equal(t, "ErrFailsafeTimeoutExceeded/ErrEndpointRequestTimeout", summary)
	})

	t.Run("CircuitBreakerOpen_WithStandardCause", func(t *testing.T) {
		cause := NewErrEndpointRequestTimeout(5*time.Second, nil)
		err := NewErrFailsafeCircuitBreakerOpen(Scope("connector"), cause, nil)

		summary := ErrorSummary(err)
		assert.Equal(t, "ErrFailsafeCircuitBreakerOpen/ErrEndpointRequestTimeout", summary)
	})

	t.Run("RetryExceeded_NilCause", func(t *testing.T) {
		err := NewErrFailsafeRetryExceeded(Scope("connector"), nil, nil)

		summary := ErrorSummary(err)
		assert.Equal(t, "ErrFailsafeRetryExceeded", summary)
	})

	t.Run("RetryExceeded_NonStandardCause", func(t *testing.T) {
		cause := fmt.Errorf("raw network error")
		err := NewErrFailsafeRetryExceeded(Scope("connector"), cause, nil)

		summary := ErrorSummary(err)
		// Non-StandardError cause → no compound, just the wrapper code
		assert.Equal(t, "ErrFailsafeRetryExceeded", summary)
	})

	t.Run("NonFailsafeError_NoCompound", func(t *testing.T) {
		err := NewErrEndpointRequestTimeout(5*time.Second, nil)

		summary := ErrorSummary(err)
		// Regular errors should just have their code, no compound
		assert.Equal(t, "ErrEndpointRequestTimeout", summary)
	})
}

func TestErrorSummary_VerboseMode_FailsafeErrors(t *testing.T) {
	// Ensure verbose mode (default)
	SetErrorLabelMode(ErrorLabelModeVerbose)
	t.Cleanup(func() { SetErrorLabelMode(ErrorLabelModeVerbose) })

	t.Run("RetryExceeded_VerboseDoesNotCompound", func(t *testing.T) {
		cause := NewErrEndpointRequestTimeout(5*time.Second, nil)
		err := NewErrFailsafeRetryExceeded(Scope("connector"), cause, nil)

		summary := ErrorSummary(err)
		// In verbose mode, should use CodeChain: DeepestMessage format, not compound
		assert.Contains(t, summary, "ErrFailsafeRetryExceeded")
		assert.Contains(t, summary, "ErrEndpointRequestTimeout")
		assert.NotContains(t, summary, "/") // no slash-compound format
	})
}
