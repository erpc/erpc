package erpc

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func costTestServer(cfg *common.ServerConfig) *HttpServer {
	return &HttpServer{serverCfg: cfg}
}

// routedResponse builds a NormalizedResponse whose request carries the given
// upstream attempts in both the attempt log and the per-scope counters, the
// way a real forward records them (CreditUnits included — the upstream
// stamps them at attempt time, see upstream.creditUnitsFor).
func routedResponse(t *testing.T, method string, attempts ...common.UpstreamAttempt) *common.NormalizedResponse {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":[]}`,
	))
	st := req.ExecState()
	for _, a := range attempts {
		st.RecordUpstreamAttempt(a)
		st.UpstreamAttempts.Add(1)
	}
	jrr, err := common.NewJsonRpcResponse(1, "0x1", nil)
	require.NoError(t, err)
	return common.NewNormalizedResponse().WithJsonRpcResponse(jrr).WithRequest(req)
}

func TestCostHeaders_SingleWithVendorAttempts(t *testing.T) {
	enabled := true
	s := costTestServer(&common.ServerConfig{CostHeaders: &enabled})

	// Primary timed out on alchemy, sweep succeeded on quicknode: the
	// customer gets ONE billable call, the cost covers BOTH attempts.
	resp := routedResponse(t, "eth_call",
		common.UpstreamAttempt{UpstreamId: "alchemy-growth", VendorName: "alchemy", Outcome: common.UpstreamOutcomeTimeout, CreditUnits: 26},
		common.UpstreamAttempt{UpstreamId: "quicknode-pro", VendorName: "quicknode", Outcome: common.UpstreamOutcomeSuccess, Won: true, CreditUnits: 20},
	)

	w := httptest.NewRecorder()
	s.writeCostHeaders(context.Background(), w, []interface{}{resp})

	h := w.Header()
	assert.Equal(t, "1", h.Get("X-ERPC-Calls"))
	assert.Equal(t, "1", h.Get("X-ERPC-Billable"))
	assert.Equal(t, "eth_call", h.Get("X-ERPC-Methods"))
	assert.Equal(t, "alchemy:eth_call=26;quicknode:eth_call=20", h.Get("X-ERPC-Credits"))
	assert.Equal(t, common.ErpcVersion, h.Get("X-ERPC-Credits-Version"))
}

func TestCostHeaders_BatchMixedOutcomes(t *testing.T) {
	enabled := true
	s := costTestServer(&common.ServerConfig{CostHeaders: &enabled})

	success := routedResponse(t, "eth_call",
		common.UpstreamAttempt{UpstreamId: "alchemy-growth", VendorName: "alchemy", Outcome: common.UpstreamOutcomeSuccess, Won: true, CreditUnits: 26})

	// An execution revert is real work the node performed → billable, and
	// its attempt still accrued credits.
	revertReq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":2,"method":"eth_call","params":[]}`))
	revertReq.ExecState().RecordUpstreamAttempt(
		common.UpstreamAttempt{UpstreamId: "alchemy-growth", VendorName: "alchemy", Outcome: common.UpstreamOutcomeExecRevert, Won: true, CreditUnits: 26})
	revert := &HttpJsonRpcErrorResponse{
		Cause:   common.NewErrEndpointExecutionException(errors.New("execution reverted")),
		Request: revertReq,
	}

	// A protocol-level failure that never did chain work → not billable.
	protoErr := &HttpJsonRpcErrorResponse{
		Cause:   errors.New("method not found"),
		Request: common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":3,"method":"eth_bogus","params":[]}`)),
	}

	w := httptest.NewRecorder()
	s.writeCostHeaders(context.Background(), w, []interface{}{success, revert, protoErr})

	h := w.Header()
	assert.Equal(t, "3", h.Get("X-ERPC-Calls"))
	assert.Equal(t, "2", h.Get("X-ERPC-Billable"))
	assert.Equal(t, "eth_bogus,eth_call", h.Get("X-ERPC-Methods"))
	assert.Equal(t, "alchemy:eth_call=52", h.Get("X-ERPC-Credits"))
}

func TestCostHeaders_OffByDefault(t *testing.T) {
	s := costTestServer(&common.ServerConfig{})
	w := httptest.NewRecorder()
	s.writeCostHeaders(context.Background(), w, []interface{}{routedResponse(t, "eth_call")})
	assert.Empty(t, w.Header().Get("X-ERPC-Calls"), "cost headers are opt-in")
	assert.Empty(t, w.Header().Get("X-ERPC-Credits"))
}

func TestBatchExecHeaders_AggregateCountersAndPartialCache(t *testing.T) {
	s := costTestServer(&common.ServerConfig{}) // executionHeaders defaults to "all"

	cached := routedResponse(t, "eth_blockNumber").SetFromCache(true)
	cached.Request().ExecState().CacheAttempts.Add(1)

	fetched := routedResponse(t, "eth_call",
		common.UpstreamAttempt{UpstreamId: "alchemy-growth", VendorName: "alchemy", Outcome: common.UpstreamOutcomeSuccess, Won: true})

	w := httptest.NewRecorder()
	s.writeBatchExecHeaders(context.Background(), w, []interface{}{cached, fetched})

	h := w.Header()
	assert.Equal(t, "2", h.Get("X-ERPC-Attempts"), "1 upstream + 1 cache attempt")
	assert.Equal(t, "1", h.Get("X-ERPC-Upstream-Attempts"))
	assert.Equal(t, "1", h.Get("X-ERPC-Cache-Attempts"))
	assert.Equal(t, "PARTIAL:1", h.Get("X-ERPC-Cache"))
	assert.Contains(t, h.Get("X-ERPC-Upstreams"), "alchemy-growth=")
	assert.Empty(t, h.Get("X-ERPC-Upstream"), "single-winner header has no batch meaning")
	assert.Empty(t, h.Get("X-ERPC-Upstreams-Truncated"))
}
