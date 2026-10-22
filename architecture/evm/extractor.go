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
	// The composite runs extractors in sorted order (evm before jsonrpc/svm);
	// without this guard the EVM extractor would claim SVM JSON-RPC codes first
	// and drop the SVM taxonomy (incl. the sendTransaction non-retryable-toward-
	// network guard), and would classify generic jsonrpc errors by EVM rules.
	if upstream != nil && upstream.Config() != nil {
		switch upstream.Config().Type {
		case common.UpstreamTypeSvm, common.UpstreamTypeJsonRpc:
			return nil
		}
	}
	return ExtractJsonRpcError(resp, nr, jr, upstream)
}
