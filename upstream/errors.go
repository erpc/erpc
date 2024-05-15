package upstream

import (
	"github.com/flair-sdk/erpc/common"
)

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
