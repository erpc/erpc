package evm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// covLogsWriteFailer fails the failAt-th Write call (1-based) and records
// every byte written before it.
type covLogsWriteFailer struct {
	failAt int
	calls  int
	buf    bytes.Buffer
}

var covLogsErrWrite = errors.New("client connection reset")

func (w *covLogsWriteFailer) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == w.failAt {
		return 0, covLogsErrWrite
	}
	return w.buf.Write(p)
}

// covLogsBrokenResult is a result writer whose WriteTo and Size both fail,
// standing in for a sub-response whose body can no longer be read.
type covLogsBrokenResult struct{ err error }

func (b *covLogsBrokenResult) WriteTo(io.Writer, bool) (int64, error) { return 0, b.err }
func (b *covLogsBrokenResult) IsResultEmptyish() bool                 { return false }
func (b *covLogsBrokenResult) Size(...context.Context) (int, error)   { return 0, b.err }

func covLogsBrokenResponse(err error) *common.JsonRpcResponse {
	jrr := &common.JsonRpcResponse{}
	jrr.SetResultWriter(&covLogsBrokenResult{err: err})
	return jrr
}

func covLogsResult(body string) *common.NormalizedResponse {
	return common.NewNormalizedResponse().WithJsonRpcResponse(
		common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(body), nil),
	)
}

func covLogsAddr(c string) string  { return "0x" + strings.Repeat(c, 40) }
func covLogsTopic(c string) string { return "0x" + strings.Repeat(c, 64) }

func TestCovLogs_ResolveBlockTagForGetLogs(t *testing.T) {
	ctx := context.Background()
	n := &queryTestNetwork{latest: 100, finalized: 90}
	cases := []struct {
		name    string
		net     common.Network
		in      string
		wantHex string
		wantNum int64
	}{
		{"empty", n, "", "", 0},
		{"hex", n, "0x10", "0x10", 16},
		{"malformed hex", n, "0xzz", "", 0},
		{"latest", n, "latest", "0x64", 100},
		{"finalized", n, "finalized", "0x5a", 90},
		{"safe is never resolved", n, "safe", "", 0},
		{"latest without state", &queryTestNetwork{}, "latest", "", 0},
		{"no network", nil, "latest", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hx, bn := resolveBlockTagForGetLogs(ctx, tc.net, tc.in)
			assert.Equal(t, tc.wantHex, hx)
			assert.Equal(t, tc.wantNum, bn)
		})
	}
}

func TestCovLogs_GetLogsConcreteRangeSize_Rejects(t *testing.T) {
	ctx := context.Background()
	size, ok := getLogsConcreteRangeSize(ctx, nil)
	assert.False(t, ok)
	assert.Zero(t, size)

	cases := []struct {
		name   string
		params []interface{}
	}{
		{"no params", []interface{}{}},
		{"filter is not an object", []interface{}{"0x1"}},
		{"malformed fromBlock", []interface{}{map[string]interface{}{"fromBlock": "0xzz", "toBlock": "0x2"}}},
		{"malformed toBlock", []interface{}{map[string]interface{}{"fromBlock": "0x1", "toBlock": "0xzz"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			size, ok := getLogsConcreteRangeSize(ctx, common.NewJsonRpcRequest("eth_getLogs", tc.params))
			assert.False(t, ok)
			assert.Zero(t, size)
		})
	}
	size, ok = getLogsConcreteRangeSize(ctx, common.NewJsonRpcRequest("eth_getLogs", []interface{}{
		map[string]interface{}{"fromBlock": "0x1", "toBlock": "0xa"},
	}))
	assert.True(t, ok)
	assert.Equal(t, float64(10), size)
}

func TestCovLogs_CheckGetLogsFilterValues(t *testing.T) {
	cases := []struct {
		name    string
		filter  map[string]interface{}
		wantErr string
	}{
		{"uppercase 0X prefix is accepted", map[string]interface{}{"address": "0X" + strings.Repeat("A", 40)}, ""},
		{"null topic positions are wildcards", map[string]interface{}{"topics": []interface{}{nil, covLogsTopic("b")}}, ""},
		{"topics given as a string", map[string]interface{}{"topics": covLogsTopic("b")}, "topics: expected an array"},
		{"address array holding a number", map[string]interface{}{"address": []interface{}{covLogsAddr("a"), 7.0}}, "address[1]: expected a hex string"},
		{"address given as a number", map[string]interface{}{"address": 7.0}, "address: expected a hex string or an array"},
		{"address without prefix", map[string]interface{}{"address": strings.Repeat("a", 40)}, "address: expected 20 bytes"},
		{"address with non-hex digit", map[string]interface{}{"address": "0x" + strings.Repeat("g", 40)}, "address: expected 20 bytes"},
		{"topic OR-list with short entry", map[string]interface{}{"topics": []interface{}{[]interface{}{covLogsTopic("c"), "0x01"}}}, "topics[0][1]: expected 32 bytes"},
		{"five topic positions", map[string]interface{}{"topics": []interface{}{nil, nil, nil, nil, nil}}, "at most 4 positions, got 5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkGetLogsFilterValues(tc.filter)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.True(t, common.HasErrorCode(err, common.ErrCodeInvalidRequest))
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestCovLogs_CheckGetLogsHardLimits_NoEvmConfig(t *testing.T) {
	ctx := context.Background()
	for name, cfg := range map[string]*common.NetworkConfig{"nil config": nil, "nil evm": {}} {
		t.Run(name, func(t *testing.T) {
			n := &queryTestNetwork{cfg: cfg}
			from, to, resolved, err := checkGetLogsHardLimits(ctx, n, "0x1", "0x186a0", 1000, 1000)
			require.NoError(t, err, "without evm config no hard limit applies")
			assert.True(t, resolved)
			assert.Equal(t, int64(1), from)
			assert.Equal(t, int64(100000), to)
		})
	}
}

func TestCovLogs_ProjectPreForward_PassThrough(t *testing.T) {
	ctx := context.Background()
	n := &queryTestNetwork{cfg: &common.NetworkConfig{Evm: &common.EvmNetworkConfig{GetLogsMaxAllowedRange: 1}}}

	handled, resp, err := projectPreForward_eth_getLogs(ctx, nil, createTestRequest(map[string]interface{}{}))
	assert.False(t, handled)
	assert.Nil(t, resp)
	assert.NoError(t, err)
	handled, _, err = projectPreForward_eth_getLogs(ctx, n, nil)
	assert.False(t, handled)
	assert.NoError(t, err)

	cases := map[string]*common.NormalizedRequest{
		"unparseable body": common.NewNormalizedRequest([]byte(`{not json`)),
		"no params":        common.NewNormalizedRequestFromJsonRpcRequest(common.NewJsonRpcRequest("eth_getLogs", []interface{}{})),
		"positional param": common.NewNormalizedRequestFromJsonRpcRequest(common.NewJsonRpcRequest("eth_getLogs", []interface{}{"0x1"})),
	}
	for name, rq := range cases {
		t.Run(name, func(t *testing.T) {
			handled, resp, err := projectPreForward_eth_getLogs(ctx, n, rq)
			assert.False(t, handled)
			assert.Nil(t, resp)
			assert.NoError(t, err)
		})
	}
}

func TestCovLogs_NetworkPreForward_Guards(t *testing.T) {
	ctx := context.Background()

	handled, _, err := networkPreForward_eth_getLogs(ctx, nil, nil, createTestRequest(map[string]interface{}{}))
	assert.False(t, handled)
	assert.NoError(t, err)

	t.Run("unparseable body is refused", func(t *testing.T) {
		handled, resp, err := networkPreForward_eth_getLogs(ctx, &queryTestNetwork{}, nil, common.NewNormalizedRequest([]byte(`{not json`)))
		assert.True(t, handled)
		assert.Nil(t, resp)
		require.Error(t, err)
	})

	t.Run("no evm config passes a resolved range through", func(t *testing.T) {
		n := &queryTestNetwork{forwardFn: func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			t.Fatal("must not forward")
			return nil, nil
		}}
		handled, resp, err := networkPreForward_eth_getLogs(ctx, n, nil, createTestRequest(map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x1000"}))
		assert.False(t, handled)
		assert.Nil(t, resp)
		assert.NoError(t, err)
	})

	t.Run("split threshold above max range is capped and does not split an in-cap range", func(t *testing.T) {
		n := &queryTestNetwork{
			cfg: &common.NetworkConfig{Evm: &common.EvmNetworkConfig{GetLogsMaxAllowedRange: 10}},
			forwardFn: func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
				t.Fatal("must not forward")
				return nil, nil
			},
		}
		ups := []common.Upstream{
			nil,
			&queryTestConfigUpstream{cfg: &common.UpstreamConfig{}},
			&queryTestConfigUpstream{cfg: &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{GetLogsAutoSplittingRangeThreshold: 100}}},
		}
		handled, resp, err := networkPreForward_eth_getLogs(ctx, n, ups, createTestRequest(map[string]interface{}{"fromBlock": "0x1", "toBlock": "0xa"}))
		assert.False(t, handled)
		assert.Nil(t, resp)
		assert.NoError(t, err)
	})
}

func TestCovLogs_NetworkPreForward_ProactiveSplitSkipsUnconfiguredUpstreams(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []string
	n := &queryTestNetwork{
		cfg: &common.NetworkConfig{Evm: &common.EvmNetworkConfig{}},
		forwardFn: func(_ context.Context, r *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			jrq, _ := r.JsonRpcRequest()
			f := jrq.Params[0].(map[string]interface{})
			mu.Lock()
			seen = append(seen, f["fromBlock"].(string)+"-"+f["toBlock"].(string)+"/"+r.Directives().SkipCacheRead)
			mu.Unlock()
			return covLogsResult(`["` + f["fromBlock"].(string) + `"]`), nil
		},
	}
	ups := []common.Upstream{
		nil,
		&queryTestConfigUpstream{cfg: &common.UpstreamConfig{}},
		&queryTestConfigUpstream{cfg: &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{GetLogsAutoSplittingRangeThreshold: 2}}},
	}
	rq := createTestRequest(map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x4"})
	rq.SetDirectives(&common.RequestDirectives{SkipCacheRead: "true"})

	handled, resp, err := networkPreForward_eth_getLogs(ctx, n, ups, rq)
	require.NoError(t, err)
	require.True(t, handled)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	var buf bytes.Buffer
	_, err = jrr.WriteResultTo(&buf, false)
	require.NoError(t, err)
	assert.JSONEq(t, `["0x1","0x3"]`, buf.String())
	assert.ElementsMatch(t, []string{"0x1-0x2/true", "0x3-0x4/true"}, seen)
	assert.Equal(t, common.CompositeTypeLogsSplitProactive, rq.CompositeType())
}

func TestCovLogs_UpstreamPreForward_Guards(t *testing.T) {
	ctx := context.Background()
	enforce := &common.NetworkConfig{Evm: &common.EvmNetworkConfig{Integrity: &common.EvmIntegrityConfig{EnforceGetLogsBlockRange: util.BoolPtr(true)}}}
	n := &queryTestNetwork{cfg: enforce, latest: 100}
	u := common.NewFakeUpstream("rpc1")

	t.Run("non-evm upstream is passed through", func(t *testing.T) {
		handled, resp, err := upstreamPreForward_eth_getLogs(ctx, n, &queryTestConfigUpstream{}, createTestRequest(map[string]interface{}{"fromBlock": "0x5", "toBlock": "0x1"}))
		assert.False(t, handled)
		assert.Nil(t, resp)
		assert.NoError(t, err)
	})

	t.Run("unparseable body is refused", func(t *testing.T) {
		handled, _, err := upstreamPreForward_eth_getLogs(ctx, n, u, common.NewNormalizedRequest([]byte(`{not json`)))
		assert.True(t, handled)
		require.Error(t, err)
	})

	cases := map[string]*common.NormalizedRequest{
		"no params":           common.NewNormalizedRequestFromJsonRpcRequest(common.NewJsonRpcRequest("eth_getLogs", []interface{}{})),
		"positional param":    common.NewNormalizedRequestFromJsonRpcRequest(common.NewJsonRpcRequest("eth_getLogs", []interface{}{"0x1"})),
		"unresolvable safe":   createTestRequest(map[string]interface{}{"fromBlock": "safe", "toBlock": "0x1"}),
		"unresolvable bounds": createTestRequest(map[string]interface{}{"fromBlock": "0x1", "toBlock": "pending"}),
	}
	for name, rq := range cases {
		t.Run(name, func(t *testing.T) {
			handled, resp, err := upstreamPreForward_eth_getLogs(ctx, n, u, rq)
			assert.False(t, handled)
			assert.Nil(t, resp)
			assert.NoError(t, err)
		})
	}

	t.Run("inverted range after resolving latest", func(t *testing.T) {
		handled, _, err := upstreamPreForward_eth_getLogs(ctx, n, u, createTestRequest(map[string]interface{}{"fromBlock": "0xc8", "toBlock": "latest"}))
		assert.True(t, handled)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeInvalidRequest))
		assert.Contains(t, err.Error(), "fromBlock (200) must be less than or equal to toBlock (100)")
	})
}

func TestCovLogs_NetworkPostForward_SplitOnErrorFallbacks(t *testing.T) {
	ctx := context.Background()
	tooLarge := common.NewErrEndpointRequestTooLarge(errors.New("query returned more than 10000 results"), common.EvmBlockRangeTooLarge)
	cfg := &common.NetworkConfig{Evm: &common.EvmNetworkConfig{GetLogsSplitOnError: util.BoolPtr(true)}}

	t.Run("unsplittable single-block request keeps the upstream error", func(t *testing.T) {
		n := &queryTestNetwork{cfg: cfg, forwardFn: func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			t.Fatal("must not forward")
			return nil, nil
		}}
		orig := common.NewNormalizedResponse()
		rs, err := networkPostForward_eth_getLogs(ctx, n, createTestRequest(map[string]interface{}{"fromBlock": "0x7", "toBlock": "0x7", "address": covLogsAddr("a")}), orig, tooLarge)
		assert.Same(t, orig, rs)
		assert.Same(t, tooLarge, err)
	})

	t.Run("failed sub-request keeps the original error and response", func(t *testing.T) {
		var mu sync.Mutex
		var skips []string
		n := &queryTestNetwork{cfg: cfg, forwardFn: func(_ context.Context, r *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			mu.Lock()
			skips = append(skips, r.Directives().SkipCacheRead)
			mu.Unlock()
			return nil, errors.New("upstream timeout")
		}}
		rq := createTestRequest(map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x2"})
		rq.SetDirectives(&common.RequestDirectives{SkipCacheRead: "conn-a"})
		orig := common.NewNormalizedResponse()
		rs, err := networkPostForward_eth_getLogs(ctx, n, rq, orig, tooLarge)
		assert.Same(t, orig, rs)
		assert.Same(t, tooLarge, err)
		assert.Equal(t, []string{"conn-a", "conn-a"}, skips, "sub-requests inherit the parent's skip-cache-read directive")
	})

	t.Run("derived sub-requests never split again", func(t *testing.T) {
		n := &queryTestNetwork{cfg: cfg}
		rq := createTestRequest(map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x2"})
		rq.SetParentRequestId("parent")
		rs, err := networkPostForward_eth_getLogs(ctx, n, rq, nil, tooLarge)
		assert.Nil(t, rs)
		assert.Same(t, tooLarge, err)
	})
}

func TestCovLogs_SplitEthGetLogsRequest_Invalid(t *testing.T) {
	cases := []struct {
		name    string
		rq      *common.NormalizedRequest
		wantErr string
	}{
		{"unparseable body", common.NewNormalizedRequest([]byte(`{not json`)), ""},
		{"no params", common.NewNormalizedRequestFromJsonRpcRequest(common.NewJsonRpcRequest("eth_getLogs", []interface{}{})), "invalid params length"},
		{"positional param", common.NewNormalizedRequestFromJsonRpcRequest(common.NewJsonRpcRequest("eth_getLogs", []interface{}{"0x1"})), "invalid filter format"},
		{"malformed fromBlock", createTestRequest(map[string]interface{}{"fromBlock": "0xzz", "toBlock": "0x2"}), "invalid syntax"},
		{"tag toBlock", createTestRequest(map[string]interface{}{"fromBlock": "0x1", "toBlock": "latest"}), "invalid toBlock"},
		{"malformed toBlock", createTestRequest(map[string]interface{}{"fromBlock": "0x1", "toBlock": "0xzz"}), "invalid syntax"},
		{"single topic0 cannot split", createTestRequest(map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x1", "topics": []interface{}{[]interface{}{covLogsTopic("a")}}}), "cannot be split further"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subs, err := splitEthGetLogsRequest(tc.rq)
			require.Error(t, err)
			assert.Nil(t, subs)
			if tc.wantErr != "" {
				assert.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

func TestCovLogs_ExecuteSubRequests_FailureModes(t *testing.T) {
	ctx := context.Background()
	subs := []ethGetLogsSubRequest{{fromBlock: 1, toBlock: 1}, {fromBlock: 2, toBlock: 2}}
	newReq := func() *common.NormalizedRequest {
		return common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x2"}],"id":1}`))
	}
	brokenErr := errors.New("sub-response body already released")
	cases := []struct {
		name    string
		forward func(r *common.NormalizedRequest) (*common.NormalizedResponse, error)
		wantErr string
	}{
		{
			name:    "nil response without error",
			forward: func(*common.NormalizedRequest) (*common.NormalizedResponse, error) { return nil, nil },
			wantErr: "unexpected empty json-rpc response",
		},
		{
			name: "json-rpc error in sub-response",
			forward: func(*common.NormalizedRequest) (*common.NormalizedResponse, error) {
				return common.NewNormalizedResponse().WithJsonRpcResponse(common.MustNewJsonRpcResponseFromBytes(
					[]byte(`1`), nil, []byte(`{"code":-32005,"message":"query returned more than 10000 results"}`),
				)), nil
			},
			wantErr: "query returned more than 10000 results",
		},
		{
			name: "unreadable sub-response body",
			forward: func(*common.NormalizedRequest) (*common.NormalizedResponse, error) {
				return common.NewNormalizedResponse().WithJsonRpcResponse(covLogsBrokenResponse(brokenErr)), nil
			},
			wantErr: brokenErr.Error(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := &queryTestNetwork{
				cfg: &common.NetworkConfig{Evm: &common.EvmNetworkConfig{GetLogsSplitConcurrency: 1}},
				forwardFn: func(_ context.Context, r *common.NormalizedRequest) (*common.NormalizedResponse, error) {
					return tc.forward(r)
				},
			}
			merged, fromCache, err := executeGetLogsSubRequests(ctx, n, newReq(), subs, "")
			require.Error(t, err)
			assert.Nil(t, merged)
			assert.False(t, fromCache)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}

	t.Run("unmarshalable parent id fails the merge", func(t *testing.T) {
		n := &queryTestNetwork{forwardFn: func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return covLogsResult(`["log"]`), nil
		}}
		jrq := common.NewJsonRpcRequest("eth_getLogs", []interface{}{map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x2"}})
		jrq.ID = make(chan int)
		merged, _, err := executeGetLogsSubRequests(ctx, n, common.NewNormalizedRequestFromJsonRpcRequest(jrq), subs, "")
		require.Error(t, err)
		assert.Nil(t, merged)
	})
}

func TestCovLogs_MultiResponseWriter_WriteErrors(t *testing.T) {
	newWriter := func() *GetLogsMultiResponseWriter {
		return NewGetLogsMultiResponseWriter([]*common.JsonRpcResponse{
			common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`["a"]`), nil),
			common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`["b"]`), nil),
		})
	}
	// Write order: "[", `"a"`, ",", `"b"`, "]".
	for failAt, wantPrefix := range map[int]string{1: ``, 2: `[`, 3: `["a"`, 4: `["a",`, 5: `["a","b"`} {
		w := &covLogsWriteFailer{failAt: failAt}
		n, err := newWriter().WriteTo(w, false)
		assert.ErrorIs(t, err, covLogsErrWrite, "write %d", failAt)
		assert.Equal(t, wantPrefix, w.buf.String(), "write %d", failAt)
		assert.Equal(t, int64(len(wantPrefix)), n, "write %d", failAt)
	}

	var ok bytes.Buffer
	n, err := newWriter().WriteTo(&ok, false)
	require.NoError(t, err)
	assert.Equal(t, `["a","b"]`, ok.String())
	assert.Equal(t, int64(9), n)
}

func TestCovLogs_MultiResponseWriter_EmptyishAndSize(t *testing.T) {
	assert.True(t, NewGetLogsMultiResponseWriter(nil).IsResultEmptyish())
	assert.True(t, NewGetLogsMultiResponseWriter([]*common.JsonRpcResponse{
		nil,
		common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`[]`), nil),
	}).IsResultEmptyish())
	assert.False(t, NewGetLogsMultiResponseWriter([]*common.JsonRpcResponse{
		nil,
		common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`["x"]`), nil),
	}).IsResultEmptyish())

	sizeErr := errors.New("size unavailable")
	size, err := NewGetLogsMultiResponseWriter([]*common.JsonRpcResponse{
		common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`["x"]`), nil),
		covLogsBrokenResponse(sizeErr),
	}).Size()
	assert.ErrorIs(t, err, sizeErr)
	assert.Zero(t, size)
}
