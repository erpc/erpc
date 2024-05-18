package common

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/flair-sdk/erpc/config"
)

func IsNull(err interface{}) bool {
	if err == nil || err == "" {
		return true
	}

	baseErr, ok := err.(*BaseError)
	if !ok {
		return true
	}

	return baseErr == nil || baseErr.Code == ""
}

//
// Base Types
//

type BaseError struct {
	Code    string                 `json:"code"`
	Message string                 `json:"message"`
	Cause   error                  `json:"cause"`
	Details map[string]interface{} `json:"details"`
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

	return e.Code
}

func (e BaseError) MarshalJSON() ([]byte, error) {
	type Alias BaseError
	cause := e.Cause

	if baseErr, ok := cause.(*BaseError); ok {
		return json.Marshal(&struct {
			Alias
			Cause BaseError `json:"cause"`
		}{
			Alias: (Alias)(e),
			Cause: *baseErr,
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

type ErrorWithStatusCode interface {
	ErrorStatusCode() int
}

type ErrorWithBody interface {
	ErrorResponseBody() interface{}
}

type RetryableError interface {
	RetryAfter() int
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

var NewErrUpstreamRequest = func(cause error, upstreamId string) error {
	return &ErrUpstreamRequest{
		BaseError{
			Code:    "ErrUpstreamRequest",
			Message: "failed to make request to upstream",
			Cause:   cause,
			Details: map[string]interface{}{
				"upstreamId": upstreamId,
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
			Cause:  errors.Join(ers...),
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

var NewErrJsonRpcRequestUnmarshal = func(cause error) error {
	return &ErrJsonRpcRequestUnmarshal{
		BaseError{
			Code:    "ErrJsonRpcRequestUnmarshal",
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

var NewErrProjectRateLimitRuleExceeded = func(project string, bucket string, rule *config.RateLimitRuleConfig) error {
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

var NewErrNetworkRateLimitRuleExceeded = func(project string, network string, bucket string, rule *config.RateLimitRuleConfig) error {
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

var NewErrUpstreamRateLimitRuleExceeded = func(upstream string, bucket string, rule *config.RateLimitRuleConfig) error {
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
