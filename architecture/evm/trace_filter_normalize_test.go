package evm

import (
	"context"
	"errors"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

func newMockEvmUpstream(id string) *mockEvmUpstream {
	m := &mockEvmUpstream{}
	m.On("Id").Return(id).Maybe()
	return m
}

func TestUpstreamPostForward_TraceFilter_NormalizesNullToEmptyArray(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		rawBody []byte
	}{
		{"trace_filter null", "trace_filter", []byte("null")},
		{"trace_filter empty string", "trace_filter", []byte(`""`)},
		{"trace_filter empty object", "trace_filter", []byte("{}")},
		{"arbtrace_filter null", "arbtrace_filter", []byte("null")},
	}

	network := &testNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureEvm}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := common.NewNormalizedRequest([]byte(
				`{"jsonrpc":"2.0","id":1,"method":"` + tc.method + `","params":[{"fromBlock":"0x1","toBlock":"0x1"}]}`,
			))
			jrr, err := common.NewJsonRpcResponseFromBytes([]byte(`1`), tc.rawBody, nil)
			assert.NoError(t, err)
			resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

			out, outErr := HandleUpstreamPostForward(
				context.Background(), network, newMockEvmUpstream("mock-up"), req, resp, nil, false,
			)
			assert.NoError(t, outErr)
			assert.NotNil(t, out)

			outJrr, err := out.JsonRpcResponse(context.Background())
			assert.NoError(t, err)
			assert.Equal(t, "[]", outJrr.GetResultString(),
				"expected emptyish result to be normalized to []")
		})
	}
}

func TestUpstreamPostForward_TraceFilter_PreservesNonEmptyResult(t *testing.T) {
	network := &testNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureEvm}}

	original := `[{"type":"call","subtraces":0,"traceAddress":[],"blockNumber":1}]`
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"trace_filter","params":[{"fromBlock":"0x1","toBlock":"0x1"}]}`,
	))
	jrr, err := common.NewJsonRpcResponseFromBytes([]byte(`1`), []byte(original), nil)
	assert.NoError(t, err)
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

	out, outErr := HandleUpstreamPostForward(
		context.Background(), network, newMockEvmUpstream("mock-up"), req, resp, nil, false,
	)
	assert.NoError(t, outErr)

	outJrr, err := out.JsonRpcResponse(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, original, outJrr.GetResultString(),
		"non-empty result must be passed through unchanged")
}

func TestUpstreamPostForward_TraceFilter_PassThroughOnError(t *testing.T) {
	network := &testNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureEvm}}

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"trace_filter","params":[{"fromBlock":"0x1","toBlock":"0x1"}]}`,
	))
	jrr, err := common.NewJsonRpcResponseFromBytes([]byte(`1`), []byte("null"), nil)
	assert.NoError(t, err)
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

	upstreamErr := errors.New("upstream transport failure")
	out, outErr := HandleUpstreamPostForward(
		context.Background(), network, newMockEvmUpstream("mock-up"), req, resp, upstreamErr, false,
	)
	assert.Same(t, upstreamErr, outErr, "hook must propagate the upstream error unchanged")
	assert.Same(t, resp, out, "response should be returned unchanged when an error is present")
}
