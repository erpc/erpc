package proxy

import (
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
)

//
// Upstreams
//

type ErrUpstreamClientInitialization struct{ common.BaseError }

var NewErrUpstreamClientInitialization = func(cause error, upstreamId string) error {
	return &ErrUpstreamRequest{
		common.BaseError{
			Code:    "ErrUpstreamClientInitialization",
			Message: "could not initialize upstream client",
			Cause:   cause,
			Details: map[string]interface{}{
				"upstreamId": upstreamId,
			},
		},
	}
}

type ErrUpstreamRequest struct{ common.BaseError }

var NewErrUpstreamRequest = func(cause error, upstreamId string) error {
	return &ErrUpstreamRequest{
		common.BaseError{
			Code:    "ErrUpstreamRequest",
			Message: "failed to make request to upstream",
			Cause:   cause,
			Details: map[string]interface{}{
				"upstreamId": upstreamId,
			},
		},
	}
}

type ErrUpstreamMalformedResponse struct{ common.BaseError }

var NewErrUpstreamMalformedResponse = func(cause error, upstreamId string) error {
	return &ErrUpstreamMalformedResponse{
		common.BaseError{
			Code:    "ErrUpstreamMalformedResponse",
			Message: "malformed response from upstream",
			Cause:   cause,
			Details: map[string]interface{}{
				"upstreamId": upstreamId,
			},
		},
	}
}

type ErrUpstreamsExhausted struct{ common.BaseError }

var NewErrUpstreamsExhausted = func(errors map[string]error) error {
	return &ErrUpstreamsExhausted{
		common.BaseError{
			Code:    "ErrUpstreamsExhausted",
			Message: "all available upstreams have been exhausted",
			Details: map[string]interface{}{
				"errors": errors,
			},
		},
	}
}

func (e *ErrUpstreamsExhausted) ErrorStatusCode() int {
	return 503
}

//
// Rate Limiters
//

type ErrRateLimitBucketNotFound struct{ common.BaseError }

var NewErrRateLimitBucketNotFound = func(bucketId string) error {
	return &ErrRateLimitBucketNotFound{
		common.BaseError{
			Code:    "ErrRateLimitBucketNotFound",
			Message: "rate limit bucket not found",
			Details: map[string]interface{}{
				"bucketId": bucketId,
			},
		},
	}
}

type ErrRateLimitRuleNotFound struct{ common.BaseError }

var NewErrRateLimitRuleNotFound = func(bucketId, method string) error {
	return &ErrRateLimitRuleNotFound{
		common.BaseError{
			Code:    "ErrRateLimitRuleNotFound",
			Message: "rate limit rule not found",
			Details: map[string]interface{}{
				"bucketId": bucketId,
				"method":   method,
			},
		},
	}
}

type ErrUpstreamRateLimitRuleExceeded struct{ common.BaseError }

var NewErrUpstreamRateLimitRuleExceeded = func(upstream string, bucket string, rule *config.RateLimitRuleConfig) error {
	return &ErrUpstreamRateLimitRuleExceeded{
		common.BaseError{
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

type ErrNetworkRateLimitRuleExceeded struct{ common.BaseError }

var NewErrNetworkRateLimitRuleExceeded = func(network string, bucket string, rule *config.RateLimitRuleConfig) error {
	return &ErrNetworkRateLimitRuleExceeded{
		common.BaseError{
			Code:    "ErrNetworkRateLimitRuleExceeded",
			Message: "network-level rate limit rule exceeded",
			Details: map[string]interface{}{
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

type ErrRateLimitInvalidConfig struct{ common.BaseError }

var NewErrRateLimitInvalidConfig = func(cause error) error {
	return &ErrRateLimitInvalidConfig{
		common.BaseError{
			Code:    "ErrRateLimitInvalidConfig",
			Message: "invalid rate limit config",
			Cause:   cause,
		},
	}
}
