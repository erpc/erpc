package common

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestIsRetryableTowardNetwork_EmptyUpstreamsExhausted(t *testing.T) {
	t.Run("ErrUpstreamsExhausted_NoErrors_ShouldNotBeRetryable", func(t *testing.T) {
		// When ErrUpstreamsExhausted has no underlying errors, it should NOT be retryable
		errMap := &sync.Map{}

		exhaustedErr := NewErrUpstreamsExhausted(
			nil, errMap, "project", "evm:1", "eth_call",
			500*time.Millisecond, 1, 0, 0, 0,
		)

		result := IsRetryableTowardNetwork(exhaustedErr)

		assert.False(t, result,
			"ErrUpstreamsExhausted with no underlying errors should NOT be retryable")
	})
}

func TestIsRetryableTowardNetwork_MissingDataError(t *testing.T) {
	t.Run("SingleMissingDataError_ShouldBeRetryable", func(t *testing.T) {
		missingDataErr := NewErrEndpointMissingData(
			NewErrJsonRpcExceptionInternal(-32000, -32014, "missing trie node", nil, nil),
			nil,
		)

		result := IsRetryableTowardNetwork(missingDataErr)

		assert.True(t, result, "Single ErrEndpointMissingData should be retryable toward network")
	})

	t.Run("MissingDataError_RetryableTowardUpstream", func(t *testing.T) {
		missingDataErr := NewErrEndpointMissingData(
			NewErrJsonRpcExceptionInternal(-32000, -32014, "missing trie node", nil, nil),
			nil,
		)

		result := IsRetryableTowardsUpstream(missingDataErr)

		assert.True(t, result, "ErrEndpointMissingData should be retryable toward upstream (respects emptyResultDelay)")
	})

	t.Run("ErrUpstreamsExhausted_AllMissingData_ShouldBeRetryable", func(t *testing.T) {
		err1 := NewErrEndpointMissingData(
			NewErrJsonRpcExceptionInternal(-32000, -32014, "missing trie node 1", nil, nil),
			nil,
		)
		err2 := NewErrEndpointMissingData(
			NewErrJsonRpcExceptionInternal(-32000, -32014, "missing trie node 2", nil, nil),
			nil,
		)

		errMap := &sync.Map{}
		errMap.Store("upstream1", err1)
		errMap.Store("upstream2", err2)

		exhaustedErr := NewErrUpstreamsExhausted(
			nil, errMap, "project", "evm:42161", "eth_call",
			500*time.Millisecond, 1, 0, 0, 2,
		)

		result := IsRetryableTowardNetwork(exhaustedErr)

		assert.True(t, result,
			"ErrUpstreamsExhausted with all ErrEndpointMissingData should be retryable toward network")
	})

	t.Run("ErrUpstreamsExhausted_MixedErrors_ShouldBeRetryable", func(t *testing.T) {
		timeoutErr := NewErrEndpointRequestTimeout(time.Second, nil)
		missingDataErr := NewErrEndpointMissingData(
			NewErrJsonRpcExceptionInternal(-32000, -32014, "missing trie node", nil, nil),
			nil,
		)

		errMap := &sync.Map{}
		errMap.Store("upstream1", timeoutErr)
		errMap.Store("upstream2", missingDataErr)

		exhaustedErr := NewErrUpstreamsExhausted(
			nil, errMap, "project", "evm:1", "eth_call",
			500*time.Millisecond, 1, 0, 0, 2,
		)

		result := IsRetryableTowardNetwork(exhaustedErr)

		assert.True(t, result,
			"ErrUpstreamsExhausted with mixed errors should be retryable toward network")
	})
}

func TestIsRetryableTowardNetwork_ServerSideException(t *testing.T) {
	t.Run("SingleServerSideException_ShouldBeRetryable", func(t *testing.T) {
		serverErr := NewErrEndpointServerSideException(
			NewErrJsonRpcExceptionInternal(-32000, -32000, "Internal server error", nil, nil),
			map[string]interface{}{"statusCode": 500},
			500,
		)

		result := IsRetryableTowardNetwork(serverErr)

		assert.True(t, result, "Single ErrEndpointServerSideException should be retryable toward network")
	})

	t.Run("ErrUpstreamsExhausted_AllServerSideExceptions_ShouldBeRetryable", func(t *testing.T) {
		err1 := NewErrEndpointServerSideException(
			NewErrJsonRpcExceptionInternal(-32000, -32000, "Internal server error", nil, nil),
			map[string]interface{}{"statusCode": 500},
			500,
		)
		err2 := NewErrEndpointServerSideException(
			NewErrJsonRpcExceptionInternal(-32000, -32000, "Internal server error", nil, nil),
			map[string]interface{}{"statusCode": 500},
			500,
		)

		errMap := &sync.Map{}
		errMap.Store("upstream1", err1)
		errMap.Store("upstream2", err2)

		exhaustedErr := NewErrUpstreamsExhausted(
			nil, errMap, "project", "evm:42161", "eth_call",
			500*time.Millisecond, 1, 0, 0, 2,
		)

		result := IsRetryableTowardNetwork(exhaustedErr)

		assert.True(t, result,
			"ErrUpstreamsExhausted with all server-side exceptions should be retryable toward network")
	})
}

func TestIsRetryableTowardNetwork_ExecutionException(t *testing.T) {
	t.Run("ExecutionException_NotRetryableTowardNetwork", func(t *testing.T) {
		execErr := NewErrEndpointExecutionException(
			NewErrJsonRpcExceptionInternal(3, 3, "execution reverted", nil, nil),
		)

		result := IsRetryableTowardNetwork(execErr)

		assert.False(t, result, "ErrEndpointExecutionException should NOT be retryable toward network")
	})

	t.Run("ErrUpstreamsExhausted_AllExecutionExceptions_ShouldNotRetry", func(t *testing.T) {
		err1 := NewErrEndpointExecutionException(
			NewErrJsonRpcExceptionInternal(3, 3, "execution reverted", nil, nil),
		)
		err2 := NewErrEndpointExecutionException(
			NewErrJsonRpcExceptionInternal(3, 3, "execution reverted", nil, nil),
		)

		errMap := &sync.Map{}
		errMap.Store("upstream1", err1)
		errMap.Store("upstream2", err2)

		exhaustedErr := NewErrUpstreamsExhausted(
			nil, errMap, "project", "evm:1", "eth_call",
			500*time.Millisecond, 1, 0, 0, 2,
		)

		result := IsRetryableTowardNetwork(exhaustedErr)

		assert.False(t, result,
			"ErrUpstreamsExhausted with all execution exceptions should NOT be retryable")
	})
}

func TestIsRetryableTowardNetwork_ClientError(t *testing.T) {
	t.Run("ClientError_IsRetryable", func(t *testing.T) {
		clientErr := NewErrEndpointClientSideException(
			NewErrJsonRpcExceptionInternal(-32600, -32600, "Invalid Request", nil, nil),
		)

		result := IsRetryableTowardNetwork(clientErr)

		// Client errors don't have retryableTowardNetwork: false set explicitly,
		// so they're retryable toward network by default
		assert.True(t, result, "Client-side errors are retryable toward network by default")
	})
}

func TestIsRetryableTowardNetwork_Timeout(t *testing.T) {
	t.Run("TimeoutError_ShouldBeRetryable", func(t *testing.T) {
		timeoutErr := NewErrEndpointRequestTimeout(time.Second, nil)

		result := IsRetryableTowardNetwork(timeoutErr)

		assert.True(t, result, "Timeout errors should be retryable toward network")
	})

	t.Run("ErrUpstreamsExhausted_AllTimeouts_ShouldBeRetryable", func(t *testing.T) {
		err1 := NewErrEndpointRequestTimeout(time.Second, nil)
		err2 := NewErrEndpointRequestTimeout(time.Second, nil)

		errMap := &sync.Map{}
		errMap.Store("upstream1", err1)
		errMap.Store("upstream2", err2)

		exhaustedErr := NewErrUpstreamsExhausted(
			nil, errMap, "project", "evm:1", "eth_call",
			500*time.Millisecond, 1, 0, 0, 2,
		)

		result := IsRetryableTowardNetwork(exhaustedErr)

		assert.True(t, result,
			"ErrUpstreamsExhausted with all timeouts should be retryable toward network")
	})
}

func TestIsRetryableTowardNetwork_UserReportedScenario(t *testing.T) {
	t.Run("MissingTrieNode_ArbitrumScenario", func(t *testing.T) {
		innerErr := NewErrJsonRpcExceptionInternal(
			-32000,
			-32014,
			"missing trie node 0000000000000000000000000000000000000000000000000000000000000000 (path ) <nil>",
			nil,
			map[string]interface{}{"statusCode": 200},
		)

		missingDataErr := NewErrEndpointMissingData(innerErr, nil)

		errMap := &sync.Map{}
		errMap.Store("repository-1-evm:42161", missingDataErr)

		exhaustedErr := NewErrUpstreamsExhausted(
			nil, errMap, "main", "evm:42161", "eth_call",
			132*time.Millisecond, 1, 0, 0, 1,
		)

		result := IsRetryableTowardNetwork(exhaustedErr)

		assert.True(t, result,
			"eth_call with missing trie node should trigger network-level retry")
	})

	t.Run("InternalServerError_ArbitrumScenario", func(t *testing.T) {
		innerErr := NewErrJsonRpcExceptionInternal(
			-32000, -32000, "Internal server error", nil,
			map[string]interface{}{"statusCode": 500},
		)

		serverErr := NewErrEndpointServerSideException(
			innerErr,
			map[string]interface{}{"statusCode": 500},
			500,
		)

		errMap := &sync.Map{}
		errMap.Store("repository-1-evm:42161", serverErr)

		exhaustedErr := NewErrUpstreamsExhausted(
			nil, errMap, "main", "evm:42161", "eth_call",
			73*time.Millisecond, 1, 0, 0, 1,
		)

		result := IsRetryableTowardNetwork(exhaustedErr)

		assert.True(t, result,
			"Internal server error (HTTP 500) should trigger network-level retry")
	})
}

func TestHasErrorCode_NestedErrors(t *testing.T) {
	t.Run("FindsCodeInNestedError", func(t *testing.T) {
		innerErr := NewErrJsonRpcExceptionInternal(-32000, -32014, "missing trie node", nil, nil)
		missingDataErr := NewErrEndpointMissingData(innerErr, nil)

		assert.True(t, HasErrorCode(missingDataErr, ErrCodeEndpointMissingData))
		assert.True(t, HasErrorCode(missingDataErr, ErrCodeJsonRpcExceptionInternal))
	})
}

func BenchmarkIsRetryableTowardNetwork(b *testing.B) {
	missingDataErr := NewErrEndpointMissingData(
		NewErrJsonRpcExceptionInternal(-32000, -32014, "missing trie node", nil, nil),
		nil,
	)

	errMap := &sync.Map{}
	errMap.Store("upstream1", missingDataErr)

	exhaustedErr := NewErrUpstreamsExhausted(
		nil, errMap, "project", "evm:1", "eth_call",
		100*time.Millisecond, 1, 0, 0, 1,
	)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		IsRetryableTowardNetwork(exhaustedErr)
	}
}
