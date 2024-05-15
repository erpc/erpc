package upstream

import (
	"github.com/flair-sdk/erpc/common"
)

// Upstreams
type ErrNetworkNotFound struct{ common.BaseError }

var NewErrNetworkNotFound = func(network string) error {
	return &ErrNetworkNotFound{
		common.BaseError{
			Code:    "ErrNetworkNotFound",
			Message: "network not found",
			Details: map[string]interface{}{
				"network": network,
			},
		},
	}
}

func (e *ErrNetworkNotFound) ErrorStatusCode() int { return 404 }

type ErrNoUpstreamsDefined struct{ common.BaseError }

var NewErrNoUpstreamsDefined = func(project string, network string) error {
	return &ErrNoUpstreamsDefined{
		common.BaseError{
			Code:    "ErrNoUpstreamsDefined",
			Message: "no upstreams defined for project/network",
			Details: map[string]interface{}{
				"project": project,
				"network": network,
			},
		},
	}
}

func (e *ErrNoUpstreamsDefined) ErrorStatusCode() int { return 404 }

//
// Clients
//

type ErrJsonRpcRequestUnmarshal struct {
	common.BaseError
}

var NewErrJsonRpcRequestUnmarshal = func(cause error) error {
	return &ErrJsonRpcRequestUnmarshal{
		common.BaseError{
			Code:    "ErrJsonRpcRequestUnmarshal",
			Message: "failed to unmarshal json-rpc request",
			Cause:   cause,
		},
	}
}

func (e *ErrJsonRpcRequestUnmarshal) ErrorStatusCode() int { return 400 }

type ErrJsonRpcRequestUnresolvableMethod struct {
	common.BaseError
}

var NewErrJsonRpcRequestUnresolvableMethod = func(rpcRequest interface{}) error {
	return &ErrJsonRpcRequestUnresolvableMethod{
		common.BaseError{
			Code:    "ErrJsonRpcRequestUnresolvableMethod",
			Message: "could not resolve method in json-rpc request",
			Details: map[string]interface{}{
				"request": rpcRequest,
			},
		},
	}
}

type ErrJsonRpcRequestPreparation struct {
	common.BaseError
}

var NewErrJsonRpcRequestPreparation = func(cause error, details map[string]interface{}) error {
	return &ErrJsonRpcRequestPreparation{
		common.BaseError{
			Code:    "ErrJsonRpcRequestPreparation",
			Message: "failed to prepare json-rpc request",
			Cause:   cause,
			Details: details,
		},
	}
}
