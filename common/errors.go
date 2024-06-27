package common

import (
	"encoding/json"
	"errors"
	"fmt"
)

func IsNull(err interface{}) bool {
	if err == nil || err == "" {
		return true
	}

	var be *BaseError
	if errors.As(err.(error), &be) {
		return be.Code == ""
	}

	return err == nil
}

//
// Base Types
//

type ErrorCode string

type BaseError struct {
	Code    ErrorCode              `json:"code"`
	Message string                 `json:"message,omitempty"`
	Cause   error                  `json:"cause,omitempty"`
	Details map[string]interface{} `json:"details,omitempty"`
}

type ErrorWithHasCode interface {
	HasCode(code ErrorCode) bool
}

func (e *BaseError) Unwrap() error {
	return e.Cause
}

func (e *BaseError) Error() string {
	var detailsStr string

	if e.Details != nil && len(e.Details) > 0 {
		s, er := json.Marshal(e.Details)
		if er == nil {
			detailsStr = fmt.Sprintf("(%s)", s)
		} else {
			detailsStr = fmt.Sprintf("(%v)", e.Details)
		}
	}

	if e.Cause == nil {
		if detailsStr == "" {
			return fmt.Sprintf("%s: %s", e.Code, e.Message)
		}
		return fmt.Sprintf("%s: %s -> %s", e.Code, e.Message, detailsStr)
	} else {
		if detailsStr == "" {
			return fmt.Sprintf("%s: %s \ncaused by: %v", e.Code, e.Message, e.Cause.Error())
		}
		return fmt.Sprintf("%s: %s -> %s \ncaused by: %v", e.Code, e.Message, detailsStr, e.Cause.Error())
	}
}

func (e *BaseError) CodeChain() string {
	if e.Cause != nil {
		if be, ok := e.Cause.(*BaseError); ok {
			return fmt.Sprintf("%s <- %s", e.Code, be.CodeChain())
		}
	}

	return string(e.Code)
}

func (e BaseError) MarshalJSON() ([]byte, error) {
	type Alias BaseError
	cause := e.Cause
	if cc, ok := cause.(ErrorWithHasCode); ok {
		return json.Marshal(&struct {
			Alias
			Cause interface{} `json:"cause"`
		}{
			Alias: (Alias)(e),
			Cause: cc,
		})
	} else if cause != nil {
		return json.Marshal(&struct {
			Alias
			Cause BaseError `json:"cause"`
		}{
			Alias: (Alias)(e),
			Cause: BaseError{
				Code:    "ErrGeneric",
				Message: cause.Error(),
			},
		})
	}

	return json.Marshal(&struct {
		Alias
		Cause interface{} `json:"cause"`
	}{
		Alias: (Alias)(e),
		Cause: nil,
	})
}

func (e *BaseError) Is(err error) bool {
	var is bool

	if be, ok := err.(*BaseError); ok {
		is = e.Code == be.Code
	}

	if !is && e.Cause != nil {
		is = errors.Is(e.Cause, err)
	}

	return is
}

func (e *BaseError) HasCode(code ErrorCode) bool {
	if e.Code == code {
		return true
	}

	if e.Cause != nil {
		if be, ok := e.Cause.(ErrorWithHasCode); ok {
			return be.HasCode(code)
		}
	}

	return false
}

type ErrorWithStatusCode interface {
	ErrorStatusCode() int
}

type ErrorWithBody interface {
	ErrorResponseBody() interface{}
}

//
// Projects
//

type ErrProjectNotFound struct{ BaseError }

var NewErrProjectNotFound = func(projectId string) error {
	return &ErrProjectNotFound{
		BaseError{
			Code:    "ErrProjectNotFound",
			Message: "project not configured in the config found",
			Details: map[string]interface{}{
				"projectId": projectId,
			},
		},
	}
}

type ErrProjectAlreadyExists struct{ BaseError }

var NewErrProjectAlreadyExists = func(projectId string) error {
	return &ErrProjectAlreadyExists{
		BaseError{
			Code:    "ErrProjectAlreadyExists",
			Message: "project already exists",
			Details: map[string]interface{}{
				"projectId": projectId,
			},
		},
	}
}

//
// Networks
//

type ErrNetworkNotFound struct{ BaseError }

var NewErrNetworkNotFound = func(networkId string) error {
	return &ErrNetworkNotFound{
		BaseError{
			Code:    "ErrNetworkNotFound",
			Message: "network not configured in the config",
			Details: map[string]interface{}{
				"networkId": networkId,
			},
		},
	}
}

type ErrUnknownNetworkID struct{ BaseError }

var NewErrUnknownNetworkID = func(arch NetworkArchitecture) error {
	return &ErrUnknownNetworkID{
		BaseError{
			Code:    "ErrUnknownNetworkID",
			Message: "could not resolve network ID not from config nor from the upstreams",
			Details: map[string]interface{}{
				"architecture": arch,
			},
		},
	}
}

type ErrUnknownNetworkArchitecture struct{ BaseError }

var NewErrUnknownNetworkArchitecture = func(arch NetworkArchitecture) error {
	return &ErrUnknownNetworkArchitecture{
		BaseError{
			Code:    "ErrUnknownNetworkArchitecture",
			Message: "unknown network architecture",
			Details: map[string]interface{}{
				"architecture": arch,
			},
		},
	}
}

type ErrInvalidEvmChainId struct{ BaseError }

var NewErrInvalidEvmChainId = func(chainId any) error {
	return &ErrInvalidEvmChainId{
		BaseError{
			Code:    "ErrInvalidEvmChainId",
			Message: "invalid EVM chain ID, it must be a number",
			Details: map[string]interface{}{
				"chainId": fmt.Sprintf("%+v", chainId),
			},
		},
	}
}

//
// Upstreams
//

type ErrUpstreamClientInitialization struct{ BaseError }

var NewErrUpstreamClientInitialization = func(cause error, upstreamId string) error {
	return &ErrUpstreamRequest{
		BaseError{
			Code:    "ErrUpstreamClientInitialization",
			Message: "could not initialize upstream client",
			Cause:   cause,
			Details: map[string]interface{}{
				"upstreamId": upstreamId,
			},
		},
	}
}

type ErrUpstreamRequest struct{ BaseError }

var NewErrUpstreamRequest = func(cause error, upstreamId string, req interface{}) error {
	// reqStr, err := json.Marshal(req)
	// if err != nil {
	// 	reqStr = []byte(fmt.Sprintf("%+v", req))
	// }
	return &ErrUpstreamRequest{
		BaseError{
			Code:    "ErrUpstreamRequest",
			Message: "failed to make request to upstream",
			Cause:   cause,
			Details: map[string]interface{}{
				"upstreamId": upstreamId,
				// "request":    reqStr,
			},
		},
	}
}

type ErrUpstreamMalformedResponse struct{ BaseError }

var NewErrUpstreamMalformedResponse = func(cause error, upstreamId string) error {
	return &ErrUpstreamMalformedResponse{
		BaseError{
			Code:    "ErrUpstreamMalformedResponse",
			Message: "malformed response from upstream",
			Cause:   cause,
			Details: map[string]interface{}{
				"upstreamId": upstreamId,
			},
		},
	}
}

type ErrUpstreamsExhausted struct{ BaseError }

var NewErrUpstreamsExhausted = func(ers []error) error {
	return &ErrUpstreamsExhausted{
		BaseError{
			Code:    "ErrUpstreamsExhausted",
			Message: "all available upstreams have been exhausted",
			Cause:   errors.Join(ers...),
			// Details: map[string]interface{}{
			// 	"errors": errors,
			// },
		},
	}
}

func (e *ErrUpstreamsExhausted) ErrorStatusCode() int {
	return 503
}

type ErrNoUpstreamsDefined struct{ BaseError }

var NewErrNoUpstreamsDefined = func(project string) error {
	return &ErrNoUpstreamsDefined{
		BaseError{
			Code:    "ErrNoUpstreamsDefined",
			Message: "no upstreams defined for project",
			Details: map[string]interface{}{
				"project": project,
			},
		},
	}
}

type ErrNoUpstreamsFound struct{ BaseError }

var NewErrNoUpstreamsFound = func(project string, network string) error {
	return &ErrNoUpstreamsFound{
		BaseError{
			Code:    "ErrNoUpstreamsFound",
			Message: "no upstreams found for network",
			Details: map[string]interface{}{
				"project": project,
				"network": network,
			},
		},
	}
}

func (e *ErrNoUpstreamsDefined) ErrorStatusCode() int { return 404 }

type ErrUpstreamNetworkNotDetected struct{ BaseError }

var NewErrUpstreamNetworkNotDetected = func(projectId string, upstreamId string) error {
	return &ErrUpstreamNetworkNotDetected{
		BaseError{
			Code:    "ErrUpstreamNetworkNotDetected",
			Message: "network not detected for upstream either from config nor by calling the endpoint",
			Details: map[string]interface{}{
				"projectId":  projectId,
				"upstreamId": upstreamId,
			},
		},
	}
}

type ErrUpstreamInitialization struct{ BaseError }

var NewErrUpstreamInitialization = func(cause error, upstreamId string) error {
	return &ErrUpstreamInitialization{
		BaseError{
			Code:    "ErrUpstreamInitialization",
			Message: "failed to initialize upstream",
			Cause:   cause,
			Details: map[string]interface{}{
				"upstreamId": upstreamId,
			},
		},
	}
}

type ErrResponseWriteLock struct{ BaseError }

var NewErrResponseWriteLock = func(writerId string) error {
	return &ErrResponseWriteLock{
		BaseError{
			Code:    "ErrResponseWriteLock",
			Message: "failed to acquire write lock, potentially being writen by another upstream/writer",
			Details: map[string]interface{}{
				"writerId": writerId,
			},
		},
	}
}

//
// Health Checks
//

type ErrHealthCheckGroupNotFound struct{ BaseError }

var NewErrHealthCheckGroupNotFound = func(healthCheckGroupId string) error {
	return &ErrHealthCheckGroupNotFound{
		BaseError{
			Code:    "ErrHealthCheckGroupNotFound",
			Message: "health check group not found",
			Details: map[string]interface{}{
				"healthCheckGroupId": healthCheckGroupId,
			},
		},
	}
}

type ErrInvalidHealthCheckConfig struct{ BaseError }

var NewErrInvalidHealthCheckConfig = func(cause error, healthCheckGroupId string) error {
	return &ErrInvalidHealthCheckConfig{
		BaseError{
			Code:    "ErrInvalidHealthCheckConfig",
			Message: "invalid health check config",
			Cause:   cause,
			Details: map[string]interface{}{
				"healthCheckGroupId": healthCheckGroupId,
			},
		},
	}
}

//
// Clients
//

type ErrJsonRpcRequestUnmarshal struct {
	BaseError
}

const ErrCodeJsonRpcRequestUnmarshal = "ErrJsonRpcRequestUnmarshal"

var NewErrJsonRpcRequestUnmarshal = func(cause error) error {
	return &ErrJsonRpcRequestUnmarshal{
		BaseError{
			Code:    ErrCodeJsonRpcRequestUnmarshal,
			Message: "failed to unmarshal json-rpc request",
			Cause:   cause,
		},
	}
}

func (e *ErrJsonRpcRequestUnmarshal) ErrorStatusCode() int { return 400 }

type ErrJsonRpcRequestUnresolvableMethod struct {
	BaseError
}

var NewErrJsonRpcRequestUnresolvableMethod = func(rpcRequest interface{}) error {
	return &ErrJsonRpcRequestUnresolvableMethod{
		BaseError{
			Code:    "ErrJsonRpcRequestUnresolvableMethod",
			Message: "could not resolve method in json-rpc request",
			Details: map[string]interface{}{
				"request": rpcRequest,
			},
		},
	}
}

type ErrJsonRpcRequestPreparation struct {
	BaseError
}

var NewErrJsonRpcRequestPreparation = func(cause error, details map[string]interface{}) error {
	return &ErrJsonRpcRequestPreparation{
		BaseError{
			Code:    "ErrJsonRpcRequestPreparation",
			Message: "failed to prepare json-rpc request",
			Cause:   cause,
			Details: details,
		},
	}
}

//
// Failsafe
//

type ErrFailsafeConfiguration struct{ BaseError }

var NewErrFailsafeConfiguration = func(cause error, details map[string]interface{}) error {
	return &ErrFailsafeConfiguration{
		BaseError{
			Code:    "ErrFailsafeConfiguration",
			Message: "failed to configure failsafe policy",
			Cause:   cause,
			Details: details,
		},
	}
}

type ErrFailsafeTimeoutExceeded struct{ BaseError }

var NewErrFailsafeTimeoutExceeded = func(cause error) error {
	return &ErrFailsafeTimeoutExceeded{
		BaseError{
			Code:    "ErrFailsafeTimeoutExceeded",
			Message: "failsafe timeout policy exceeded",
			Cause:   cause,
		},
	}
}

func (e *ErrFailsafeTimeoutExceeded) ErrorStatusCode() int {
	return 504
}

type ErrFailsafeRetryExceeded struct{ BaseError }

var NewErrFailsafeRetryExceeded = func(cause error, lastResult interface{}) error {
	return &ErrFailsafeRetryExceeded{
		BaseError{
			Code:    "ErrFailsafeRetryExceeded",
			Message: "failsafe retry policy exceeded",
			Cause:   cause,
			Details: map[string]interface{}{
				"lastResult": lastResult,
			},
		},
	}
}

func (e *ErrFailsafeRetryExceeded) ErrorStatusCode() int {
	return 503
}

type ErrFailsafeCircuitBreakerOpen struct{ BaseError }

var NewErrFailsafeCircuitBreakerOpen = func(cause error) error {
	return &ErrFailsafeCircuitBreakerOpen{
		BaseError{
			Code:    "ErrFailsafeCircuitBreakerOpen",
			Message: "circuit breaker is open due to high error rate",
			Cause:   cause,
		},
	}
}

type ErrFailsafeUnexpected struct{ BaseError }

var NewErrFailsafeUnexpected = func(cause error) error {
	return &ErrFailsafeUnexpected{
		BaseError{
			Code:    "ErrFailsafeUnexpected",
			Message: "unexpected failsafe error type encountered",
			Cause:   cause,
		},
	}
}

//
// Rate Limiters
//

type ErrRateLimitBucketNotFound struct{ BaseError }

var NewErrRateLimitBucketNotFound = func(bucketId string) error {
	return &ErrRateLimitBucketNotFound{
		BaseError{
			Code:    "ErrRateLimitBucketNotFound",
			Message: "rate limit bucket not found",
			Details: map[string]interface{}{
				"bucketId": bucketId,
			},
		},
	}
}

type ErrRateLimitRuleNotFound struct{ BaseError }

var NewErrRateLimitRuleNotFound = func(bucketId, method string) error {
	return &ErrRateLimitRuleNotFound{
		BaseError{
			Code:    "ErrRateLimitRuleNotFound",
			Message: "rate limit rule not found",
			Details: map[string]interface{}{
				"bucketId": bucketId,
				"method":   method,
			},
		},
	}
}

type ErrRateLimitInvalidConfig struct{ BaseError }

var NewErrRateLimitInvalidConfig = func(cause error) error {
	return &ErrRateLimitInvalidConfig{
		BaseError{
			Code:    "ErrRateLimitInvalidConfig",
			Message: "invalid rate limit config",
			Cause:   cause,
		},
	}
}

type ErrProjectRateLimitRuleExceeded struct{ BaseError }

var NewErrProjectRateLimitRuleExceeded = func(project string, bucket string, rule string) error {
	return &ErrProjectRateLimitRuleExceeded{
		BaseError{
			Code:    "ErrProjectRateLimitRuleExceeded",
			Message: "project-level rate limit rule exceeded",
			Details: map[string]interface{}{
				"project": project,
				"bucket":  bucket,
				"rule":    rule,
			},
		},
	}
}

func (e *ErrProjectRateLimitRuleExceeded) ErrorStatusCode() int {
	return 429
}

type ErrNetworkRateLimitRuleExceeded struct{ BaseError }

var NewErrNetworkRateLimitRuleExceeded = func(project string, network string, bucket string, rule string) error {
	return &ErrNetworkRateLimitRuleExceeded{
		BaseError{
			Code:    "ErrNetworkRateLimitRuleExceeded",
			Message: "network-level rate limit rule exceeded",
			Details: map[string]interface{}{
				"project": project,
				"network": network,
				"bucket":  bucket,
				"rule":    rule,
			},
		},
	}
}

func (e *ErrNetworkRateLimitRuleExceeded) ErrorStatusCode() int {
	return 429
}

type ErrUpstreamRateLimitRuleExceeded struct{ BaseError }

var NewErrUpstreamRateLimitRuleExceeded = func(upstream string, bucket string, rule string) error {
	return &ErrUpstreamRateLimitRuleExceeded{
		BaseError{
			Code:    "ErrUpstreamRateLimitRuleExceeded",
			Message: "upstream-level rate limit rule exceeded",
			Details: map[string]interface{}{
				"upstream": upstream,
				"bucket":   bucket,
				"rule":     rule,
			},
		},
	}
}

func (e *ErrUpstreamRateLimitRuleExceeded) ErrorStatusCode() int {
	return 429
}

//
// Endpoint (3rd party providers, RPC nodes)
// Main purpose of these error types is internal eRPC error handling (retries, etc)
//

type ErrEndpointUnauthorized struct{ BaseError }

const ErrCodeEndpointUnauthorized = "ErrEndpointUnauthorized"

var NewErrEndpointUnauthorized = func(cause error) error {
	return &ErrEndpointUnauthorized{
		BaseError{
			Code:    ErrCodeEndpointUnauthorized,
			Message: "remote endpoint responded with Unauthorized",
			Cause:   cause,
		},
	}
}

func (e *ErrEndpointUnauthorized) ErrorStatusCode() int {
	return 401
}

type ErrEndpointUnsupported struct{ BaseError }

const ErrCodeEndpointUnsupported = "ErrEndpointUnsupported"

var NewErrEndpointUnsupported = func(cause error) error {
	return &ErrEndpointUnsupported{
		BaseError{
			Code:    ErrCodeEndpointUnsupported,
			Message: "remote endpoint does not support requested method",
			Cause:   cause,
		},
	}
}

func (e *ErrEndpointUnsupported) ErrorStatusCode() int {
	return 415
}

type ErrEndpointClientSideException struct{ BaseError }

const ErrCodeEndpointClientSideException = "ErrEndpointClientSideException"

var NewErrEndpointClientSideException = func(cause error) error {
	return &ErrEndpointClientSideException{
		BaseError{
			Code:    ErrCodeEndpointClientSideException,
			Message: "client-side error when sending request to remote endpoint",
			Cause:   cause,
		},
	}
}

func (e *ErrEndpointClientSideException) ErrorStatusCode() int {
	return 400
}

type ErrEndpointServerSideException struct{ BaseError }

const ErrCodeEndpointServerSideException = "ErrEndpointServerSideException"

var NewErrEndpointServerSideException = func(cause error) error {
	return &ErrEndpointServerSideException{
		BaseError{
			Code:    ErrCodeEndpointServerSideException,
			Message: "an internal error on remote endpoint",
			Cause:   cause,
		},
	}
}

func (e *ErrEndpointServerSideException) ErrorStatusCode() int {
	return 500
}

type ErrEndpointCapacityExceeded struct{ BaseError }

const ErrCodeEndpointCapacityExceeded = "ErrEndpointCapacityExceeded"

var NewErrEndpointCapacityExceeded = func(cause error) error {
	return &ErrEndpointCapacityExceeded{
		BaseError{
			Code:    ErrCodeEndpointCapacityExceeded,
			Message: "remote endpoint capacity exceeded",
			Cause:   cause,
		},
	}
}

func (e *ErrEndpointCapacityExceeded) ErrorStatusCode() int {
	return 429
}

type ErrEndpointBillingIssue struct{ BaseError }

const ErrCodeEndpointBillingIssue = "ErrEndpointBillingIssue"

var NewErrEndpointBillingIssue = func(cause error) error {
	return &ErrEndpointBillingIssue{
		BaseError{
			Code:    ErrCodeEndpointBillingIssue,
			Message: "remote endpoint billing issue",
			Cause:   cause,
		},
	}
}

func (e *ErrEndpointBillingIssue) ErrorStatusCode() int {
	return 402
}

type ErrEndpointNodeTimeout struct{ BaseError }

const ErrCodeEndpointNodeTimeout = "ErrEndpointNodeTimeout"

var NewErrEndpointNodeTimeout = func(cause error) error {
	return &ErrEndpointNodeTimeout{
		BaseError{
			Code:    ErrCodeEndpointNodeTimeout,
			Message: "node timeout to execute the request, maybe increase the method timeout",
			Cause:   cause,
		},
	}
}

type ErrEndpointNotSyncedYet struct{ BaseError }

const ErrCodeEndpointNotSyncedYet = "ErrEndpointNotSyncedYet"

var NewErrEndpointNotSyncedYet = func(cause error) error {
	return &ErrEndpointNotSyncedYet{
		BaseError{
			Code:    ErrCodeEndpointNotSyncedYet,
			Message: "remote endpoint not synced yet for this specific data or block",
			Cause:   cause,
		},
	}
}

type ErrEndpointEvmLargeRange struct{ BaseError }

const ErrCodeEndpointEvmLargeRange = "ErrEndpointEvmLargeRange"

var NewErrEndpointEvmLargeRange = func(cause error) error {
	return &ErrEndpointEvmLargeRange{
		BaseError{
			Code:    ErrCodeEndpointEvmLargeRange,
			Message: "evm remote endpoint complained about large logs range",
			Cause:   cause,
		},
	}
}

//
// JSON-RPC
//

//
// These error numbers are necessary to represent errors in json-rpc spec (i.e. numeric codes)
// eRPC will do a best-effort to normalize these errors across 3rd-party providers
// The main purpose of these error numbers is for client usage i.e. internal error handling of erpc must rely on error types above
//

type JsonRpcErrorNumber int

const (
	JsonRpcErrorUnknown JsonRpcErrorNumber = -99999

	// Standard JSON-RPC codes
	JsonRpcErrorClientSideException  JsonRpcErrorNumber = -32600
	JsonRpcErrorUnsupportedException JsonRpcErrorNumber = -32601
	JsonRpcErrorInvalidArgument      JsonRpcErrorNumber = -32602
	JsonRpcErrorServerSideException  JsonRpcErrorNumber = -32603
	JsonRpcErrorParseException       JsonRpcErrorNumber = -32700

	// Normalized blockchain-specific codes by eRPC
	JsonRpcErrorCapacityExceeded  JsonRpcErrorNumber = -32005
	JsonRpcErrorEvmLogsLargeRange JsonRpcErrorNumber = -32012
	JsonRpcErrorEvmReverted       JsonRpcErrorNumber = -32013
	JsonRpcErrorNotSyncedYet      JsonRpcErrorNumber = -32014
	JsonRpcErrorNodeTimeout       JsonRpcErrorNumber = -32015
	JsonRpcErrorUnauthorized      JsonRpcErrorNumber = -32016
)

var jsonRpcErrorNumberToName = map[JsonRpcErrorNumber]string{
	JsonRpcErrorUnknown:              "JsonRpcErrorUnknown",
	JsonRpcErrorClientSideException:  "JsonRpcErrorClientSideException",
	JsonRpcErrorUnsupportedException: "JsonRpcErrorUnsupportedException",
	JsonRpcErrorInvalidArgument:      "JsonRpcErrorInvalidArgument",
	JsonRpcErrorServerSideException:  "JsonRpcErrorServerSideException",
	JsonRpcErrorParseException:       "JsonRpcErrorParseException",
	JsonRpcErrorCapacityExceeded:     "JsonRpcErrorCapacityExceeded",
	JsonRpcErrorEvmLogsLargeRange:    "JsonRpcErrorEvmLogsLargeRange",
	JsonRpcErrorEvmReverted:          "JsonRpcErrorEvmReverted",
	JsonRpcErrorNotSyncedYet:         "JsonRpcErrorNotSyncedYet",
	JsonRpcErrorNodeTimeout:          "JsonRpcErrorNodeTimeout",
}

func GetJsonRpcErrorName(code JsonRpcErrorNumber) string {
	if name, exists := jsonRpcErrorNumberToName[code]; exists {
		return name
	}

	return "UnknownErrorCode"
}

type ErrJsonRpcException struct{ BaseError }

func (e *ErrJsonRpcException) ErrorStatusCode() int {
	if e.Cause != nil {
		if er, ok := e.Cause.(ErrorWithStatusCode); ok {
			return er.ErrorStatusCode()
		}
	}
	return 400
}

func (e *ErrJsonRpcException) NormalizedCode() JsonRpcErrorNumber {
	if code, ok := e.Details["normalizedCode"]; ok {
		return code.(JsonRpcErrorNumber)
	}
	return 0
}

func (e *ErrJsonRpcException) OriginalCode() int {
	if code, ok := e.Details["originalCode"]; ok {
		return code.(int)
	}
	return 0
}

const ErrCodeJsonRpcException = "ErrJsonRpcException"

var NewErrJsonRpcException = func(originalCode int, normalizedCode JsonRpcErrorNumber, message string, cause error) *ErrJsonRpcException {
	var dt map[string]interface{} = make(map[string]interface{})
	if originalCode != 0 {
		dt["originalCode"] = originalCode
	}
	if normalizedCode != 0 {
		dt["normalizedCode"] = normalizedCode
	}

	var msg string
	if normalizedCode != 0 {
		msg = fmt.Sprintf("%s: %s", GetJsonRpcErrorName(normalizedCode), message)
	} else {
		msg = message
	}

	return &ErrJsonRpcException{
		BaseError{
			Code:    ErrCodeJsonRpcException,
			Message: msg,
			Details: dt,
			Cause:   cause,
		},
	}
}

//
// Store
//

type ErrInvalidConnectorDriver struct{ BaseError }

const ErrCodeInvalidConnectorDriver = "ErrInvalidConnectorDriver"

var NewErrInvalidConnectorDriver = func(driver string) error {
	return &ErrInvalidConnectorDriver{
		BaseError{
			Code:    ErrCodeInvalidConnectorDriver,
			Message: "invalid store driver",
			Details: map[string]interface{}{
				"driver": driver,
			},
		},
	}
}

type ErrRecordNotFound struct{ BaseError }

const ErrCodeRecordNotFound = "ErrRecordNotFound"

var NewErrRecordNotFound = func(key string, driver string) error {
	return &ErrRecordNotFound{
		BaseError{
			Code:    ErrCodeRecordNotFound,
			Message: "record not found",
			Details: map[string]interface{}{
				"key":    key,
				"driver": driver,
			},
		},
	}
}

func HasCode(err error, code ErrorCode) bool {
	if be, ok := err.(ErrorWithHasCode); ok {
		return be.HasCode(code)
	}

	if be, ok := err.(*BaseError); ok {
		return be.Code == code
	}

	return false
}
