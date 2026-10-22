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
	// The composite runs extractors in sorted order (evm before svm); without this
	// guard the EVM extractor would claim SVM JSON-RPC codes first and drop the SVM
	// taxonomy (incl. the sendTransaction non-retryable-toward-network guard).
	if upstream != nil && upstream.Config() != nil && upstream.Config().Type == common.UpstreamTypeSvm {
		return nil
	}
	return ExtractJsonRpcError(resp, nr, jr, upstream)
}
