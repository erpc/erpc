package evm

import (
	"net/http"

	"github.com/erpc/erpc/common"
)

// JsonRpcErrorExtractor implements common.JsonRpcErrorExtractor for EVM
// by delegating to ExtractJsonRpcError.
type JsonRpcErrorExtractor struct{}

func NewJsonRpcErrorExtractor() common.JsonRpcErrorExtractor { return &JsonRpcErrorExtractor{} }

func (e *JsonRpcErrorExtractor) Extract(resp *http.Response, nr *common.NormalizedResponse, jr *common.JsonRpcResponse, upstream common.Upstream) error {
	return ExtractJsonRpcError(resp, nr, jr, upstream)
}
