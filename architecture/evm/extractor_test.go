package evm

import (
	"net/http"
	"testing"

	"github.com/erpc/erpc/common"
)

// The composite error extractor runs architectures in sorted order (evm before
// svm). The EVM extractor must no-op on SVM upstreams; otherwise it claims
// standard JSON-RPC codes (e.g. -32602) first and drops the SVM taxonomy,
// including the sendTransaction non-retryable-toward-network guard.
func TestExtract_SvmUpstream_IsNoOp(t *testing.T) {
	t.Parallel()
	e := NewJsonRpcErrorExtractor()
	r := &http.Response{StatusCode: 200, Header: http.Header{}}
	jr := common.MustNewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(-32602, "Invalid params", ""))
	up := &queryTestConfigUpstream{cfg: &common.UpstreamConfig{Id: "svm-up", Type: common.UpstreamTypeSvm}}
	if got := e.Extract(r, nil, jr, up); got != nil {
		t.Fatalf("EVM extractor must no-op for SVM upstream, got %T %v", got, got)
	}
}

// Sanity guard: the EVM extractor still classifies for EVM upstreams (and the
// classification stays retryable-toward-network, the EVM default for -32602).
func TestExtract_EvmUpstream_StillClassifies(t *testing.T) {
	t.Parallel()
	e := NewJsonRpcErrorExtractor()
	r := &http.Response{StatusCode: 200, Header: http.Header{}}
	jr := common.MustNewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(-32602, "Invalid params", ""))
	up := &queryTestConfigUpstream{cfg: &common.UpstreamConfig{Id: "evm-up", Type: common.UpstreamTypeEvm}}
	if got := e.Extract(r, nil, jr, up); got == nil {
		t.Fatal("EVM extractor must classify -32602 for EVM upstream, got nil")
	}
}
