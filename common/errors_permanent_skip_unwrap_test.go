package common

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func permanentSkipEE(code int, msg string) error {
	err := NewErrEndpointExecutionException(
		NewErrJsonRpcExceptionInternal(code, JsonRpcErrorNumber(code), msg, nil, nil),
	)
	ee := err.(*ErrEndpointExecutionException)
	ee.WithRetryableTowardNetwork(true)
	ee.WithPermanentMissingData(true)
	return err
}

func TestIsPermanentSkipExecutionException(t *testing.T) {
	skip := permanentSkipEE(-32007, "Slot 1 was skipped")
	assert.True(t, IsPermanentSkipExecutionException(skip))

	revert := NewErrEndpointExecutionException(
		NewErrJsonRpcExceptionInternal(3, 3, "execution reverted", nil, nil),
	)
	assert.False(t, IsPermanentSkipExecutionException(revert),
		"ordinary reverts are not permanent skips")

	md := NewErrEndpointMissingData(errors.New("missing"), nil)
	md.(*ErrEndpointMissingData).WithPermanentMissingData(true)
	assert.False(t, IsPermanentSkipExecutionException(md),
		"permanent MissingData is not an ExecutionException")
}

func TestUnwrapUnanimousPermanentSkipExecution(t *testing.T) {
	s07 := permanentSkipEE(-32007, "skipped ledger jump")
	s09 := permanentSkipEE(-32009, "skipped long-term storage")

	t.Run("unanimous skips unwrap", func(t *testing.T) {
		exhausted := NewErrUpstreamsExhaustedWithCause(errors.Join(s07, s09))
		got, ok := UnwrapUnanimousPermanentSkipExecution(exhausted)
		require.True(t, ok)
		assert.True(t, IsPermanentSkipExecutionException(got))
		assert.True(t, HasErrorCode(got, ErrCodeEndpointExecutionException))
	})

	t.Run("failsafe-retry-exceeded peels then unwraps", func(t *testing.T) {
		exhausted := NewErrUpstreamsExhaustedWithCause(errors.Join(s07, s09))
		fre := NewErrFailsafeRetryExceeded(ScopeNetwork, exhausted, nil)
		got, ok := UnwrapUnanimousPermanentSkipExecution(fre)
		require.True(t, ok)
		assert.True(t, IsPermanentSkipExecutionException(got))
	})

	t.Run("mixed exhausted stays wrapped", func(t *testing.T) {
		transient := NewErrEndpointMissingData(errors.New("-32004 not yet"), nil)
		exhausted := NewErrUpstreamsExhaustedWithCause(errors.Join(s07, transient))
		_, ok := UnwrapUnanimousPermanentSkipExecution(exhausted)
		assert.False(t, ok, "must not unwrap when a non-skip cause is present")
	})

	t.Run("plain skip is not Exhausted", func(t *testing.T) {
		_, ok := UnwrapUnanimousPermanentSkipExecution(s07)
		assert.False(t, ok)
	})
}
