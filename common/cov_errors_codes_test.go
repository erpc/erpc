package common

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var covErrorsSentinel = errors.New("cov-errors sentinel cause")

type covErrorsCtorCase struct {
	name string
	err  error
	code ErrorCode
	// status is the ErrorStatusCode() result; 0 means the type must not expose one.
	status  int
	msg     string
	details map[string]interface{}
	// wrapsSentinel asserts errors.Is(err, covErrorsSentinel) through the Cause chain.
	wrapsSentinel bool
}

func covErrorsCtorCases() []covErrorsCtorCase {
	up := NewFakeUpstream("up-1")
	return []covErrorsCtorCase{
		{name: "InvalidRequest", err: NewErrInvalidRequest(covErrorsSentinel), code: ErrCodeInvalidRequest, status: http.StatusBadRequest, msg: "invalid request body or headers", details: map[string]interface{}{"retryableTowardNetwork": false}, wrapsSentinel: true},
		{name: "InvalidUrlPath", err: NewErrInvalidUrlPath("missing project", "/x/y"), code: ErrCodeInvalidUrlPath, status: http.StatusBadRequest, msg: "url path is not valid: missing project", details: map[string]interface{}{"providedPath": "/x/y"}},
		{name: "InvalidConfig", err: NewErrInvalidConfig("bad cfg"), code: "ErrInvalidConfig", msg: "bad cfg"},
		{name: "RequestTimeout", err: NewErrRequestTimeout(1500 * time.Millisecond), code: "ErrRequestTimeout", status: http.StatusGatewayTimeout, msg: "request timed out", details: map[string]interface{}{"timeoutSeconds": 1.5}},
		{name: "InternalServerError", err: NewErrInternalServerError(covErrorsSentinel), code: "ErrInternalServerError", msg: "internal server error", wrapsSentinel: true},
		{name: "AuthUnauthorized", err: NewErrAuthUnauthorized("secret", "bad_token", "token is bad"), code: ErrCodeAuthUnauthorized, status: http.StatusUnauthorized, msg: "token is bad", details: map[string]interface{}{"strategy": "secret", "reason": "bad_token"}},
		{name: "AuthUnauthorizedReason", err: NewErrAuthUnauthorizedReason("jwt", AuthUnauthorizedReasonInvalidSignature), code: ErrCodeAuthUnauthorized, status: http.StatusUnauthorized, msg: "invalid signature", details: map[string]interface{}{"strategy": "jwt", "reason": "invalid_signature"}},
		{name: "AuthRateLimitRuleExceeded", err: NewErrAuthRateLimitRuleExceeded("prj", "jwt", "b1", "r1", "user-1", "1.2.3.4"), code: ErrCodeAuthRateLimitRuleExceeded, status: http.StatusTooManyRequests, msg: "auth-level rate limit rule exceeded", details: map[string]interface{}{"projectId": "prj", "strategy": "jwt", "budget": "b1", "rule": "r1", "userId": "user-1", "clientIp": "1.2.3.4"}},
		{name: "ProjectNotFound", err: NewErrProjectNotFound("main"), code: ErrCodeProjectNotFound, status: http.StatusNotFound, msg: "project 'main' not configured"},
		{name: "ProjectAlreadyExists", err: NewErrProjectAlreadyExists("main"), code: "ErrProjectAlreadyExists", msg: "project already exists", details: map[string]interface{}{"projectId": "main"}},
		{name: "NetworkNotFound", err: NewErrNetworkNotFound("evm:1"), code: ErrCodeNetworkNotFound, status: http.StatusNotFound, msg: "network not configured", details: map[string]interface{}{"networkId": "evm:1"}},
		{name: "UnknownNetworkID", err: NewErrUnknownNetworkID(ArchitectureEvm), code: "ErrUnknownNetworkID", status: http.StatusBadRequest, msg: "could not resolve network ID", details: map[string]interface{}{"architecture": ArchitectureEvm}},
		{name: "UnknownNetworkArchitecture", err: NewErrUnknownNetworkArchitecture("cosmos"), code: "ErrUnknownNetworkArchitecture", status: http.StatusBadRequest, msg: "unknown network architecture", details: map[string]interface{}{"architecture": NetworkArchitecture("cosmos")}},
		{name: "NotImplemented", err: NewErrNotImplemented("eth_sign is not supported"), code: ErrCodeNotImplemented, status: http.StatusNotImplemented, msg: "eth_sign is not supported"},
		{name: "InvalidEvmChainId", err: NewErrInvalidEvmChainId("abc"), code: "ErrInvalidEvmChainId", status: http.StatusBadRequest, msg: "invalid EVM chain ID", details: map[string]interface{}{"chainId": "abc"}},
		{name: "FinalizedBlockUnavailable", err: NewErrFinalizedBlockUnavailable(42), code: ErrCodeFinalizedBlockUnavailable, msg: "finalized/latest blocks are not available", details: map[string]interface{}{"blockNumber": int64(42)}},
		{name: "UpstreamClientInitialization", err: NewErrUpstreamClientInitialization(covErrorsSentinel, up), code: "ErrUpstreamClientInitialization", msg: "could not initialize upstream client", details: map[string]interface{}{"upstreamId": "up-1"}, wrapsSentinel: true},
		{name: "UpstreamMalformedResponse", err: NewErrUpstreamMalformedResponse(covErrorsSentinel, up), code: "ErrUpstreamMalformedResponse", status: http.StatusBadRequest, msg: "malformed response from upstream", details: map[string]interface{}{"upstreamId": "up-1"}, wrapsSentinel: true},
		{name: "NoUpstreamsDefined", err: NewErrNoUpstreamsDefined("main"), code: "ErrNoUpstreamsDefined", status: http.StatusNotFound, msg: "no upstreams defined for project", details: map[string]interface{}{"project": "main"}},
		{name: "NoUpstreamsFound", err: NewErrNoUpstreamsFound("main", "evm:1"), code: "ErrNoUpstreamsFound", status: http.StatusNotFound, msg: "no upstreams found for network 'evm:1' and project 'main'", details: map[string]interface{}{"project": "main", "network": "evm:1"}},
		{name: "NetworkInitializing", err: NewErrNetworkInitializing("main", "evm:1"), code: ErrCodeNetworkInitializing, status: http.StatusServiceUnavailable, msg: "network 'evm:1' for project 'main' is initializing", details: map[string]interface{}{"project": "main", "network": "evm:1"}},
		{name: "NetworkNoUpstreamsAvailable", err: NewErrNetworkNoUpstreamsAvailable("main", "evm:1"), code: ErrCodeNetworkNoUpstreamsAvailable, status: http.StatusNotFound, msg: "no RPC providers are available for network 'evm:1' in project 'main'", details: map[string]interface{}{"project": "main", "network": "evm:1"}},
		{name: "NetworkNotSupported", err: NewErrNetworkNotSupported("main", "evm:1"), code: ErrCodeNetworkNotSupported, status: http.StatusNotFound, msg: "network 'evm:1' is not supported for project 'main'", details: map[string]interface{}{"project": "main", "network": "evm:1"}},
		{name: "UpstreamNetworkNotDetected", err: NewErrUpstreamNetworkNotDetected("main", up), code: "ErrUpstreamNetworkNotDetected", msg: "network not detected for upstream", details: map[string]interface{}{"projectId": "main", "upstreamId": "up-1"}},
		{name: "UpstreamInitialization", err: NewErrUpstreamInitialization(covErrorsSentinel, "up-1"), code: "ErrUpstreamInitialization", msg: "failed to initialize upstream", details: map[string]interface{}{"upstreamId": "up-1"}, wrapsSentinel: true},
		{name: "UpstreamRequestSkipped", err: NewErrUpstreamRequestSkipped(covErrorsSentinel, "up-1"), code: ErrCodeUpstreamRequestSkipped, status: http.StatusNotAcceptable, msg: "skipped forwarding request to upstream", details: map[string]interface{}{"upstreamId": "up-1"}, wrapsSentinel: true},
		{name: "UpstreamBlockUnavailable", err: NewErrUpstreamBlockUnavailable("up-1", 100, 90, 80), code: ErrCodeUpstreamBlockUnavailable, status: http.StatusServiceUnavailable, msg: "upstream does not have the requested block yet", details: map[string]interface{}{"upstreamId": "up-1", "blockNumber": int64(100), "latestBlock": int64(90), "finalizedBlock": int64(80)}},
		{name: "UpstreamMethodIgnored", err: NewErrUpstreamMethodIgnored("eth_call", "up-1"), code: ErrCodeUpstreamMethodIgnored, status: http.StatusNotAcceptable, msg: "method ignored by upstream configuration", details: map[string]interface{}{"method": "eth_call", "upstreamId": "up-1"}},
		{name: "UpstreamSyncing", err: NewErrUpstreamSyncing("up-1"), code: ErrCodeUpstreamSyncing, status: http.StatusUnprocessableEntity, msg: "upstream is syncing", details: map[string]interface{}{"upstreamId": "up-1"}},
		{name: "UpstreamShadowing", err: NewErrUpstreamShadowing("up-1"), code: ErrCodeUpstreamShadowing, msg: "upstream is shadowing", details: map[string]interface{}{"upstreamId": "up-1"}},
		{name: "UpstreamNotAllowed", err: NewErrUpstreamNotAllowed("alchemy-*", "up-1"), code: ErrCodeUpstreamNotAllowed, msg: "upstream not allowed based on use-upstream directive", details: map[string]interface{}{"required": "alchemy-*", "upstreamId": "up-1"}},
		{name: "UpstreamHedgeCancelled", err: NewErrUpstreamHedgeCancelled("up-1", covErrorsSentinel), code: ErrCodeUpstreamHedgeCancelled, msg: "hedged request cancelled", details: map[string]interface{}{"upstreamId": "up-1"}, wrapsSentinel: true},
		{name: "ResponseWriteLock", err: NewErrResponseWriteLock("w-1"), code: "ErrResponseWriteLock", msg: "failed to acquire write lock", details: map[string]interface{}{"writerId": "w-1"}},
		{name: "JsonRpcRequestUnresolvableMethod", err: NewErrJsonRpcRequestUnresolvableMethod("raw"), code: "ErrJsonRpcRequestUnresolvableMethod", msg: "could not resolve method", details: map[string]interface{}{"request": "raw", "retryableTowardNetwork": false}},
		{name: "FailsafeConfiguration", err: NewErrFailsafeConfiguration(covErrorsSentinel, map[string]interface{}{"policy": "retry"}), code: "ErrFailsafeConfiguration", msg: "failed to configure failsafe policy", details: map[string]interface{}{"policy": "retry"}, wrapsSentinel: true},
		{name: "RateLimitBudgetNotFound", err: NewErrRateLimitBudgetNotFound("b1"), code: "ErrRateLimitBudgetNotFound", msg: "rate limit budget not found", details: map[string]interface{}{"budgetId": "b1"}},
		{name: "RateLimitRuleNotFound", err: NewErrRateLimitRuleNotFound("b1", "eth_call"), code: "ErrRateLimitRuleNotFound", msg: "rate limit rule not found", details: map[string]interface{}{"budgetId": "b1", "method": "eth_call"}},
		{name: "ProjectRateLimitRuleExceeded", err: NewErrProjectRateLimitRuleExceeded("main", "b1", "r1"), code: ErrCodeProjectRateLimitRuleExceeded, status: http.StatusTooManyRequests, msg: "project-level rate limit rule exceeded", details: map[string]interface{}{"project": "main", "budget": "b1", "rule": "r1"}},
		{name: "NetworkRateLimitRuleExceeded", err: NewErrNetworkRateLimitRuleExceeded("main", "evm:1", "b1", "r1"), code: ErrCodeNetworkRateLimitRuleExceeded, status: http.StatusTooManyRequests, msg: "network-level rate limit rule exceeded", details: map[string]interface{}{"project": "main", "network": "evm:1", "budget": "b1", "rule": "r1"}},
		{name: "NetworkRequestTimeout", err: NewErrNetworkRequestTimeout(250*time.Millisecond, covErrorsSentinel), code: ErrCodeNetworkRequestTimeout, status: http.StatusGatewayTimeout, msg: "timed out after 250ms", wrapsSentinel: true},
		{name: "UpstreamRateLimitRuleExceeded", err: NewErrUpstreamRateLimitRuleExceeded("up-1", "b1", "r1"), code: ErrCodeUpstreamRateLimitRuleExceeded, status: http.StatusTooManyRequests, msg: "upstream-level rate limit rule exceeded", details: map[string]interface{}{"upstreamId": "up-1", "budget": "b1", "rule": "r1"}},
		{name: "UpstreamExcludedByPolicy", err: NewErrUpstreamExcludedByPolicy("up-1"), code: ErrCodeUpstreamExcludedByPolicy, msg: "upstream excluded by selection policy", details: map[string]interface{}{"upstreamId": "up-1"}},
		{name: "EndpointUnauthorized", err: NewErrEndpointUnauthorized(covErrorsSentinel), code: ErrCodeEndpointUnauthorized, status: http.StatusUnauthorized, msg: "remote endpoint responded with Unauthorized", wrapsSentinel: true},
		{name: "EndpointChainIdMismatch", err: NewErrEndpointChainIdMismatch(10, 1), code: ErrCodeEndpointChainIdMismatch, msg: "cross-wired endpoint", details: map[string]interface{}{"detectedChainId": uint64(10), "expectedChainId": uint64(1)}},
		{name: "EndpointUnsupported", err: NewErrEndpointUnsupported(covErrorsSentinel), code: ErrCodeEndpointUnsupported, status: http.StatusNotAcceptable, msg: "does not support requested method", wrapsSentinel: true},
		{name: "EndpointExecutionException", err: NewErrEndpointExecutionException(covErrorsSentinel), code: ErrCodeEndpointExecutionException, status: http.StatusOK, msg: "execution exception on node", details: map[string]interface{}{"retryableTowardNetwork": false}, wrapsSentinel: true},
		{name: "EndpointServerSideException_DefaultStatus", err: NewErrEndpointServerSideException(covErrorsSentinel, map[string]interface{}{"k": "v"}, 0), code: ErrCodeEndpointServerSideException, status: http.StatusInternalServerError, msg: "an internal error on remote endpoint", details: map[string]interface{}{"k": "v"}, wrapsSentinel: true},
		{name: "EndpointServerSideException_OriginalStatus", err: NewErrEndpointServerSideException(nil, nil, http.StatusBadGateway), code: ErrCodeEndpointServerSideException, status: http.StatusBadGateway, msg: "an internal error on remote endpoint"},
		{name: "EndpointRequestTimeout", err: NewErrEndpointRequestTimeout(3*time.Second, covErrorsSentinel), code: ErrCodeEndpointRequestTimeout, status: http.StatusGatewayTimeout, msg: "remote endpoint request timeout", details: map[string]interface{}{"durationMs": int64(3000)}, wrapsSentinel: true},
		{name: "EndpointRequestCanceled", err: NewErrEndpointRequestCanceled(covErrorsSentinel), code: ErrCodeEndpointRequestCanceled, msg: "remote endpoint request canceled", wrapsSentinel: true},
		{name: "EndpointCapacityExceeded", err: NewErrEndpointCapacityExceeded(covErrorsSentinel), code: ErrCodeEndpointCapacityExceeded, status: http.StatusTooManyRequests, msg: "remote endpoint capacity exceeded", wrapsSentinel: true},
		{name: "EndpointBillingIssue", err: NewErrEndpointBillingIssue(covErrorsSentinel), code: ErrCodeEndpointBillingIssue, status: http.StatusPaymentRequired, msg: "remote endpoint billing issue", wrapsSentinel: true},
		{name: "EndpointMissingData_NoUpstream", err: NewErrEndpointMissingData(covErrorsSentinel, nil), code: ErrCodeEndpointMissingData, status: http.StatusOK, msg: "remote endpoint does not have this data", wrapsSentinel: true},
		{name: "UpstreamNodeTypeMismatch", err: NewErrUpstreamNodeTypeMismatch(covErrorsSentinel, EvmNodeTypeArchive, EvmNodeTypeFull), code: ErrCodeUpstreamNodeTypeMismatch, msg: "node type does not match", details: map[string]interface{}{"expected": EvmNodeTypeArchive, "actual": EvmNodeTypeFull}, wrapsSentinel: true},
		{name: "EndpointRequestTooLarge", err: NewErrEndpointRequestTooLarge(covErrorsSentinel, EvmAddressesTooLarge), code: ErrCodeEndpointRequestTooLarge, status: http.StatusRequestEntityTooLarge, msg: "too large request", details: map[string]interface{}{"complaint": EvmAddressesTooLarge}, wrapsSentinel: true},
		{name: "InvalidConnectorDriver", err: NewErrInvalidConnectorDriver(DriverRedis), code: ErrCodeInvalidConnectorDriver, msg: "invalid store driver", details: map[string]interface{}{"driver": DriverRedis}},
		{name: "RecordNotFound", err: NewErrRecordNotFound("pk", "rk", "memory"), code: ErrCodeRecordNotFound, msg: "record not found", details: map[string]interface{}{"partitionKey": "pk", "rangeKey": "rk", "driver": "memory"}},
		{name: "RecordExpired", err: NewErrRecordExpired("pk", "rk", "redis", 200, 100), code: ErrCodeRecordExpired, msg: "record expired", details: map[string]interface{}{"partitionKey": "pk", "rangeKey": "rk", "driver": "redis", "now": int64(200), "expiration": int64(100)}},
		{name: "ConsensusDispute", err: NewErrConsensusDispute("dispute", nil, []error{covErrorsSentinel}), code: ErrCodeConsensusDispute, status: http.StatusConflict, msg: "dispute", wrapsSentinel: true},
		{name: "ConsensusCompositionDispute", err: NewErrConsensusCompositionDispute("composition", nil, []error{covErrorsSentinel}), code: ErrCodeConsensusCompositionDispute, status: http.StatusConflict, msg: "composition", wrapsSentinel: true},
		{name: "ConsensusLowParticipants", err: NewErrConsensusLowParticipants("too few", nil, []error{covErrorsSentinel}), code: ErrCodeConsensusLowParticipants, status: http.StatusPreconditionFailed, msg: "too few", wrapsSentinel: true},
		{name: "GetLogsExceededMaxAllowedResponseSize", err: NewErrGetLogsExceededMaxAllowedResponseSize(2048, 1024), code: ErrCodeGetLogsExceededMaxAllowedResponseSize, status: http.StatusRequestEntityTooLarge, msg: "getLogs response exceeded max allowed size", details: map[string]interface{}{"responseBytes": int64(2048), "maxAllowedBytes": int64(1024)}},
		{name: "GetLogsExceededMaxAllowedRange", err: NewErrGetLogsExceededMaxAllowedRange(20000, 10000), code: ErrCodeGetLogsExceededMaxAllowedRange, status: http.StatusRequestEntityTooLarge, msg: "exceeded max allowed range", details: map[string]interface{}{"requestRange": int64(20000), "maxAllowedRange": int64(10000)}},
		{name: "GetLogsExceededMaxAllowedAddresses", err: NewErrGetLogsExceededMaxAllowedAddresses(30, 10), code: ErrCodeGetLogsExceededMaxAllowedAddresses, status: http.StatusRequestEntityTooLarge, msg: "exceeded max allowed addresses", details: map[string]interface{}{"requestAddresses": int64(30), "maxAllowedAddresses": int64(10)}},
		{name: "GetLogsExceededMaxAllowedTopics", err: NewErrGetLogsExceededMaxAllowedTopics(5, 4), code: ErrCodeGetLogsExceededMaxAllowedTopics, status: http.StatusRequestEntityTooLarge, msg: "exceeded max allowed topics", details: map[string]interface{}{"requestTopics": int64(5), "maxAllowedTopics": int64(4)}},
		{name: "EndpointContentValidation", err: NewErrEndpointContentValidation(covErrorsSentinel, up), code: ErrCodeEndpointContentValidation, status: http.StatusBadGateway, msg: "failed validation content check", details: map[string]interface{}{"upstreamId": "up-1"}, wrapsSentinel: true},
		{name: "EndpointNonceException", err: NewErrEndpointNonceException(covErrorsSentinel, NonceExceptionReasonNonceTooLow), code: ErrCodeEndpointNonceException, status: http.StatusOK, msg: "transaction with this nonce already exists", details: map[string]interface{}{"nonceExceptionReason": "nonce_too_low", "retryableTowardNetwork": false}, wrapsSentinel: true},
		{name: "SubscriptionNotFound", err: NewErrSubscriptionNotFound("0xabc"), code: ErrCodeSubscriptionNotFound, status: http.StatusNotFound, msg: "subscription not found", details: map[string]interface{}{"subscriptionId": "0xabc"}},
		{name: "NoWsUpstreamAvailable", err: NewErrNoWsUpstreamAvailable("evm:1"), code: ErrCodeNoWsUpstreamAvailable, status: http.StatusBadRequest, msg: "none configured for network evm:1", details: map[string]interface{}{"networkId": "evm:1"}},
		{name: "SubscriptionLimitExceeded", err: NewErrSubscriptionLimitExceeded(5), code: ErrCodeSubscriptionLimitExceeded, status: http.StatusTooManyRequests, msg: "per connection exceeded", details: map[string]interface{}{"maxPerConnection": 5}},
		{name: "InstanceSubscriptionLimitExceeded", err: NewErrInstanceSubscriptionLimitExceeded(50), code: ErrCodeSubscriptionLimitExceeded, status: http.StatusTooManyRequests, msg: "per instance exceeded", details: map[string]interface{}{"maxPerInstance": 50}},
	}
}

func TestCovErrors_Constructors_CodeStatusDetailsAndJSON(t *testing.T) {
	for _, tc := range covErrorsCtorCases() {
		t.Run(tc.name, func(t *testing.T) {
			var se StandardError
			require.True(t, errors.As(tc.err, &se), "constructor must return a StandardError")
			base := se.Base()
			assert.Equal(t, tc.code, base.Code)
			assert.True(t, HasErrorCode(tc.err, tc.code))
			assert.True(t, HasErrorCode(tc.err, "ErrSomethingElse", tc.code), "any matching code in the list matches")
			assert.False(t, HasErrorCode(tc.err, "ErrSomethingElse"))
			assert.Contains(t, base.Message, tc.msg)
			assert.True(t, strings.HasPrefix(tc.err.Error(), string(tc.code)+": "), tc.err.Error())

			sc, hasStatus := tc.err.(interface{ ErrorStatusCode() int })
			if tc.status == 0 {
				assert.False(t, hasStatus, "type unexpectedly maps to an HTTP status")
			} else {
				require.True(t, hasStatus, "type must map to an HTTP status")
				assert.Equal(t, tc.status, sc.ErrorStatusCode())
			}

			for k, v := range tc.details {
				assert.Equal(t, v, base.Details[k], "detail %s", k)
			}

			if tc.wrapsSentinel {
				assert.True(t, errors.Is(tc.err, covErrorsSentinel), "cause must be reachable via errors.Is")
				assert.Contains(t, tc.err.Error(), "caused by: "+covErrorsSentinel.Error())
			} else {
				assert.False(t, errors.Is(tc.err, covErrorsSentinel))
			}

			raw, err := SonicCfg.Marshal(tc.err)
			require.NoError(t, err)
			var m map[string]interface{}
			require.NoError(t, SonicCfg.Unmarshal(raw, &m), string(raw))
			assert.Equal(t, string(tc.code), m["code"])
			assert.Equal(t, base.Message, m["message"])
			if len(tc.details) > 0 {
				md, ok := m["details"].(map[string]interface{})
				require.True(t, ok, string(raw))
				for k := range tc.details {
					assert.Contains(t, md, k)
				}
			}
		})
	}
}

func TestCovErrors_ClientSideException_StatusDependsOnCause(t *testing.T) {
	cases := []struct {
		name   string
		cause  error
		status int
	}{
		{"NoCause", nil, http.StatusBadRequest},
		{"PlainCause", covErrorsSentinel, http.StatusBadRequest},
		{"EvmReverted", NewErrJsonRpcExceptionInternal(3, JsonRpcErrorEvmReverted, "execution reverted", nil, nil), http.StatusOK},
		{"CallException", NewErrJsonRpcExceptionInternal(-32000, JsonRpcErrorCallException, "call exception", nil, nil), http.StatusOK},
		{"TransactionRejected", NewErrJsonRpcExceptionInternal(-32003, JsonRpcErrorTransactionRejected, "rejected", nil, nil), http.StatusOK},
		{"InvalidArgument", NewErrJsonRpcExceptionInternal(-32602, JsonRpcErrorInvalidArgument, "bad arg", nil, nil), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := NewErrEndpointClientSideException(tc.cause)
			assert.True(t, HasErrorCode(err, ErrCodeEndpointClientSideException))
			assert.Equal(t, tc.status, err.(*ErrEndpointClientSideException).ErrorStatusCode())
			assert.True(t, IsRetryableTowardNetwork(err), "retryable toward network by default")
			assert.False(t, IsRetryableTowardNetwork(err.WithRetryableTowardNetwork(false)))
		})
	}
}

func TestCovErrors_JsonRpcRequestUnmarshal_Branches(t *testing.T) {
	t.Run("BaseErrorCauseIsKept", func(t *testing.T) {
		cause := &BaseError{Code: "ErrInner", Message: "inner"}
		err := NewErrJsonRpcRequestUnmarshal(cause, []byte(`{bad`))
		be := err.(*ErrJsonRpcRequestUnmarshal)
		assert.Equal(t, "failed to unmarshal json-rpc request", be.Message)
		assert.Same(t, cause, be.Cause)
		assert.Equal(t, "{bad", be.Details["body"])
		assert.Equal(t, 400, be.ErrorStatusCode())
		assert.Equal(t, "ErrJsonRpcRequestUnmarshal <- ErrInner", be.CodeChain())
	})
	t.Run("PlainCauseBecomesMessage", func(t *testing.T) {
		err := NewErrJsonRpcRequestUnmarshal(errors.New("unexpected EOF"), []byte(`{`))
		be := err.(*ErrJsonRpcRequestUnmarshal)
		assert.Equal(t, "unexpected EOF", be.Message)
		assert.Nil(t, be.Cause)
		assert.False(t, IsRetryableTowardNetwork(err))
	})
	t.Run("NilCause", func(t *testing.T) {
		err := NewErrJsonRpcRequestUnmarshal(nil, nil)
		be := err.(*ErrJsonRpcRequestUnmarshal)
		assert.Equal(t, "failed to unmarshal json-rpc request", be.Message)
		assert.Nil(t, be.Cause)
		assert.Equal(t, "", be.Details["body"])
		assert.True(t, IsClientError(err))
	})
}

func TestCovErrors_JsonRpcRequestPreparation_RetryableDefault(t *testing.T) {
	err := NewErrJsonRpcRequestPreparation(covErrorsSentinel, nil)
	assert.Equal(t, false, err.(*ErrJsonRpcRequestPreparation).Details["retryableTowardNetwork"])
	assert.False(t, IsRetryableTowardNetwork(err))
	assert.True(t, errors.Is(err, covErrorsSentinel))

	kept := NewErrJsonRpcRequestPreparation(nil, map[string]interface{}{"retryableTowardNetwork": true, "x": 1})
	assert.Equal(t, true, kept.(*ErrJsonRpcRequestPreparation).Details["retryableTowardNetwork"])
	assert.Equal(t, 1, kept.(*ErrJsonRpcRequestPreparation).Details["x"])
	assert.True(t, IsRetryableTowardNetwork(kept))
}

func TestCovErrors_EndpointTransportFailure_RedactsUrl(t *testing.T) {
	u, err := url.Parse("https://rpc.example.com/secret-key")
	require.NoError(t, err)

	plain := NewErrEndpointTransportFailure(u, errors.New(`Post "https://rpc.example.com/secret-key": dial tcp: connection refused`))
	tf := plain.(*ErrEndpointTransportFailure)
	assert.Equal(t, "rpc.example.com", tf.Details["host"])
	assert.NotContains(t, tf.Cause.Error(), "secret-key")
	assert.Contains(t, tf.Cause.Error(), "dial tcp: connection refused")

	std := NewErrEndpointRequestTimeout(time.Second, nil)
	wrapped := NewErrEndpointTransportFailure(u, std)
	assert.Same(t, std, wrapped.(*ErrEndpointTransportFailure).Cause, "standard errors are kept as-is")
	assert.True(t, HasErrorCode(wrapped, ErrCodeEndpointTransportFailure, ErrCodeEndpointRequestTimeout))

	noCause := NewErrEndpointTransportFailure(u, nil)
	assert.Nil(t, noCause.(*ErrEndpointTransportFailure).Cause)
}

func TestCovErrors_EndpointMissingData_UpstreamDetails(t *testing.T) {
	up := NewFakeUpstream("evm-up", WithEvmStatePoller(NewFakeEvmStatePoller(100, 90)))
	up.Config().Evm = &EvmUpstreamConfig{MaxAvailableRecentBlocks: 128}

	err := NewErrEndpointMissingData(nil, up)
	md := err.(*ErrEndpointMissingData)
	assert.Equal(t, "evm-up", md.Details["upstreamId"])
	assert.Equal(t, int64(100), md.Details["latestBlock"])
	assert.Equal(t, int64(90), md.Details["finalizedBlock"])
	assert.Equal(t, int64(128), md.Details["maxAvailableRecentBlocks"])
	assert.Same(t, up, md.Upstream())

	plainUp := NewFakeUpstream("plain-up")
	md2 := NewErrEndpointMissingData(nil, plainUp).(*ErrEndpointMissingData)
	assert.Equal(t, "plain-up", md2.Details["upstreamId"])
	assert.NotContains(t, md2.Details, "latestBlock")
	assert.NotContains(t, md2.Details, "maxAvailableRecentBlocks")
}

func TestCovErrors_EndpointContentValidation_NilUpstream(t *testing.T) {
	err := NewErrEndpointContentValidation(nil, nil).(*ErrEndpointContentValidation)
	assert.NotContains(t, err.Details, "upstreamId")
	assert.Nil(t, err.Upstream())
}

func TestCovErrors_AuthUnauthorized_Reason(t *testing.T) {
	assert.Equal(t, "token expired", AuthUnauthorizedReasonTokenExpired.Message())
	assert.Equal(t, "invalid signature", AuthUnauthorizedReasonInvalidSignature.Message())
	assert.Equal(t, "custom_reason", AuthUnauthorizedReason("custom_reason").Message())

	var nilErr *ErrAuthUnauthorized
	assert.Equal(t, AuthUnauthorizedReason(""), nilErr.Reason())
	assert.Equal(t, AuthUnauthorizedReason(""), (&ErrAuthUnauthorized{}).Reason())

	fromStr := NewErrAuthUnauthorizedReason("jwt", AuthUnauthorizedReasonTokenExpired).(*ErrAuthUnauthorized)
	assert.Equal(t, AuthUnauthorizedReasonTokenExpired, fromStr.Reason())
	assert.Equal(t, "token expired", fromStr.Message)

	typed := &ErrAuthUnauthorized{BaseError{Details: map[string]interface{}{"reason": AuthUnauthorizedReasonInvalidSignature}}}
	assert.Equal(t, AuthUnauthorizedReasonInvalidSignature, typed.Reason())

	wrongType := &ErrAuthUnauthorized{BaseError{Details: map[string]interface{}{"reason": 7}}}
	assert.Equal(t, AuthUnauthorizedReason(""), wrongType.Reason())
}

func TestCovErrors_UpstreamRequest_Accessors(t *testing.T) {
	up := NewFakeUpstream("up-a")
	err := NewErrUpstreamRequest(covErrorsSentinel, up, "evm:1", "eth_call", 1500*time.Millisecond, 3, 2, 1)
	ur := err.(*ErrUpstreamRequest)
	assert.Equal(t, "up-a", ur.UpstreamId())
	assert.Equal(t, 3, ur.Attempts())
	assert.Equal(t, 2, ur.Retries())
	assert.Equal(t, 1, ur.Hedges())
	assert.False(t, ur.FromCache())
	assert.False(t, ur.IsObjectNull())
	assert.Same(t, up, ur.Upstream())
	assert.Equal(t, int64(1500), ur.Details["durationMs"])
	assert.Equal(t, "evm:1", ur.Details["networkId"])
	assert.Equal(t, "eth_call", ur.Details["method"])
	assert.True(t, errors.Is(err, covErrorsSentinel))

	noUp := NewErrUpstreamRequest(nil, nil, "evm:1", "eth_call", 0, 0, 0, 0).(*ErrUpstreamRequest)
	assert.NotContains(t, noUp.Details, "upstreamId")
	assert.Equal(t, "", noUp.UpstreamId())

	emptyId := NewErrUpstreamRequest(nil, NewFakeUpstream(""), "evm:1", "eth_call", 0, 0, 0, 0).(*ErrUpstreamRequest)
	assert.NotContains(t, emptyId.Details, "upstreamId")

	bare := &ErrUpstreamRequest{}
	assert.True(t, bare.IsObjectNull())
	assert.Equal(t, "", bare.UpstreamId())
	assert.Equal(t, 0, bare.Attempts())
	assert.Equal(t, 0, bare.Retries())
	assert.Equal(t, 0, bare.Hedges())

	var nilUr *ErrUpstreamRequest
	assert.True(t, nilUr.IsObjectNull())

	wrongTypes := &ErrUpstreamRequest{BaseError: BaseError{Code: ErrCodeUpstreamRequest, Details: map[string]interface{}{
		"upstreamId": 5, "attempts": "3", "retries": 2.0, "hedges": int64(1),
	}}}
	assert.Equal(t, "", wrongTypes.UpstreamId())
	assert.Equal(t, 0, wrongTypes.Attempts())
	assert.Equal(t, 0, wrongTypes.Retries())
	assert.Equal(t, 0, wrongTypes.Hedges())
}

func TestCovErrors_FailsafeErrors_MessagesAndDeepest(t *testing.T) {
	start := time.Now().Add(-2 * time.Second)

	type ctor func(Scope, error, *time.Time) error
	cases := []struct {
		name      string
		ctor      ctor
		code      ErrorCode
		withStart string
		noStart   string
		hasDurMs  bool
	}{
		{"Timeout", NewErrFailsafeTimeoutExceeded, ErrCodeFailsafeTimeoutExceeded, "failsafe timeout policy exceeded on network-level after ", "failsafe timeout policy exceeded on network-level", false},
		{"Retry", NewErrFailsafeRetryExceeded, ErrCodeFailsafeRetryExceeded, "gave up retrying on network-level after ", "gave up retrying on network-level", true},
		{"CircuitBreaker", NewErrFailsafeCircuitBreakerOpen, ErrCodeFailsafeCircuitBreakerOpen, "failsafe circuit breaker open on network-level after ", "failsafe circuit breaker open on network-level", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			timed := tc.ctor(ScopeNetwork, nil, &start)
			var se StandardError
			require.True(t, errors.As(timed, &se))
			assert.True(t, strings.HasPrefix(se.Base().Message, tc.withStart), se.Base().Message)
			if tc.hasDurMs {
				assert.GreaterOrEqual(t, se.Base().Details["durationMs"].(int64), int64(2000))
			}
			assert.Equal(t, se.Base().Message, se.DeepestMessage(), "no cause: deepest message is own message")

			untimed := tc.ctor(ScopeNetwork, nil, nil)
			require.True(t, errors.As(untimed, &se))
			assert.Equal(t, tc.noStart, se.Base().Message)
			assert.Nil(t, se.Base().Details)

			stdCause := NewErrEndpointCapacityExceeded(NewErrJsonRpcExceptionInternal(0, 0, "slow down", nil, nil))
			withStd := tc.ctor(ScopeNetwork, stdCause, nil)
			require.True(t, errors.As(withStd, &se))
			assert.Equal(t, tc.noStart+": slow down", se.DeepestMessage())
			assert.True(t, HasErrorCode(withStd, tc.code, ErrCodeEndpointCapacityExceeded))

			withPlain := tc.ctor(ScopeNetwork, errors.New("boom"), nil)
			require.True(t, errors.As(withPlain, &se))
			assert.Equal(t, tc.noStart+": boom", se.DeepestMessage())
		})
	}
	assert.Equal(t, http.StatusGatewayTimeout, NewErrFailsafeTimeoutExceeded(ScopeUpstream, nil, nil).(*ErrFailsafeTimeoutExceeded).ErrorStatusCode())
}

func TestCovErrors_Classification(t *testing.T) {
	up := NewFakeUpstream("up-1")
	cases := []struct {
		name              string
		err               error
		client            bool
		capacity          bool
		retryableUpstream bool
		retryableNetwork  bool
		severity          Severity
	}{
		{"Nil", nil, false, false, false, true, SeverityInfo},
		{"ClientSide", NewErrEndpointClientSideException(nil), true, false, true, true, SeverityInfo},
		{"InvalidRequest", NewErrInvalidRequest(nil), true, false, false, false, SeverityInfo},
		{"GetLogsRange", NewErrGetLogsExceededMaxAllowedRange(2, 1), true, false, true, true, SeverityInfo},
		{"GetLogsAddresses", NewErrGetLogsExceededMaxAllowedAddresses(2, 1), true, false, true, true, SeverityInfo},
		{"GetLogsTopics", NewErrGetLogsExceededMaxAllowedTopics(2, 1), true, false, true, true, SeverityInfo},
		{"GetLogsResponseSize", NewErrGetLogsExceededMaxAllowedResponseSize(2, 1), true, false, true, true, SeverityInfo},
		{"SubscriptionNotFound", NewErrSubscriptionNotFound("s"), true, false, true, true, SeverityInfo},
		{"SubscriptionLimit", NewErrSubscriptionLimitExceeded(1), true, false, true, true, SeverityInfo},
		{"ExecutionException", NewErrEndpointExecutionException(nil), false, false, false, false, SeverityInfo},
		{"BlockUnavailable", NewErrUpstreamBlockUnavailable("u", 1, 0, 0), false, false, true, true, SeverityWarning},
		{"MissingData", NewErrEndpointMissingData(nil, up), false, false, true, true, SeverityWarning},
		{"NetworkNotSupported", NewErrNetworkNotSupported("p", "n"), false, false, true, true, SeverityWarning},
		{"CircuitBreakerOpen", NewErrFailsafeCircuitBreakerOpen(ScopeUpstream, nil, nil), false, false, false, true, SeverityWarning},
		{"RequestSkipped", NewErrUpstreamRequestSkipped(nil, "u"), false, false, false, true, SeverityWarning},
		{"MethodIgnored", NewErrUpstreamMethodIgnored("m", "u"), false, false, false, true, SeverityWarning},
		{"Unsupported", NewErrEndpointUnsupported(nil), false, false, false, true, SeverityWarning},
		{"NotImplemented", NewErrNotImplemented("x"), false, false, false, true, SeverityWarning},
		{"Billing", NewErrEndpointBillingIssue(nil), false, false, false, true, SeverityWarning},
		{"Unauthorized", NewErrEndpointUnauthorized(nil), false, false, false, true, SeverityWarning},
		{"TooLarge", NewErrEndpointRequestTooLarge(nil, EvmBlockRangeTooLarge), false, false, false, true, SeverityWarning},
		{"ContentValidation", NewErrEndpointContentValidation(nil, up), false, false, false, true, SeverityWarning},
		{"ProjectRateLimit", NewErrProjectRateLimitRuleExceeded("p", "b", "r"), false, true, false, true, SeverityWarning},
		{"NetworkRateLimit", NewErrNetworkRateLimitRuleExceeded("p", "n", "b", "r"), false, true, false, true, SeverityWarning},
		{"UpstreamRateLimit", NewErrUpstreamRateLimitRuleExceeded("u", "b", "r"), false, true, false, true, SeverityWarning},
		{"AuthRateLimit", NewErrAuthRateLimitRuleExceeded("p", "s", "b", "r", "u", "ip"), false, true, false, true, SeverityWarning},
		{"EndpointCapacity", NewErrEndpointCapacityExceeded(nil), false, true, false, true, SeverityWarning},
		{"RequestCanceled", NewErrEndpointRequestCanceled(nil), false, false, true, true, SeverityWarning},
		{"ContextCanceled", covErrorsWrap(context.Canceled), false, false, true, true, SeverityWarning},
		{"NonceException", NewErrEndpointNonceException(nil, NonceExceptionReasonAlreadyKnown), false, false, true, false, SeverityCritical},
		{"ServerSide", NewErrEndpointServerSideException(nil, nil, 0), false, false, true, true, SeverityCritical},
		{"Timeout", NewErrEndpointRequestTimeout(time.Second, nil), false, false, true, true, SeverityCritical},
		{"Plain", errors.New("plain"), false, false, true, true, SeverityCritical},
		{"UpstreamRequestWrapsCapacity", NewErrUpstreamRequest(NewErrEndpointCapacityExceeded(nil), up, "evm:1", "eth_call", 0, 1, 0, 0), false, true, false, true, SeverityWarning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.client, IsClientError(tc.err), "IsClientError")
			assert.Equal(t, tc.capacity, IsCapacityIssue(tc.err), "IsCapacityIssue")
			if tc.err != nil {
				assert.Equal(t, tc.retryableUpstream, IsRetryableTowardsUpstream(tc.err), "IsRetryableTowardsUpstream")
			}
			assert.Equal(t, tc.retryableNetwork, IsRetryableTowardNetwork(tc.err), "IsRetryableTowardNetwork")
			assert.Equal(t, tc.severity, ClassifySeverity(tc.err), "ClassifySeverity")
		})
	}
}

func covErrorsWrap(err error) error { return fmt.Errorf("covErrors wrap: %w", err) }
