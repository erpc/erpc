package evm

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeBlockParam(t *testing.T) {
	cases := []struct {
		name    string
		param   interface{}
		want    string
		wantErr bool
	}{
		{
			name:  "nil",
			param: nil,
			want:  "latest",
		},
		{
			name:  "hex number",
			param: "0x10",
			want:  "16",
		},
		{
			name:  "tag",
			param: "latest",
			want:  "latest",
		},
		{
			name:  "block hash",
			param: "0x" + strings.Repeat("ab", 32),
			want:  "0x" + strings.Repeat("ab", 32),
		},
		{
			name:  "block hash object",
			param: map[string]interface{}{"blockHash": "0x" + strings.Repeat("cd", 32)},
			want:  "0x" + strings.Repeat("cd", 32),
		},
		{
			name:  "block number object",
			param: map[string]interface{}{"blockNumber": "0x2"},
			want:  "2",
		},
		{
			name:  "block tag object",
			param: map[string]interface{}{"blockTag": "pending"},
			want:  "pending",
		},
		{
			name:    "empty string",
			param:   "",
			wantErr: true,
		},
		{
			name:    "invalid type",
			param:   []int{1},
			wantErr: true,
		},
		{
			name:    "invalid hex",
			param:   "0xzz",
			wantErr: true,
		},
		{
			name:    "invalid block hash",
			param:   map[string]interface{}{"blockHash": "0x" + strings.Repeat("ab", 33)},
			wantErr: true,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeBlockParam(tt.param)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBuildMulticall3Request_Success(t *testing.T) {
	call1 := map[string]interface{}{
		"to":   hexAddr(1),
		"data": hexData(1),
	}
	call2 := map[string]interface{}{
		"to":    hexAddr(2),
		"input": hexData(32),
	}

	req1 := newEthCallRequest(t, 1, call1, "latest")
	req2 := newEthCallRequest(t, "req-2", call2, "latest")
	req1.SetDirectives(&common.RequestDirectives{SkipCacheRead: true})
	req1.SetUser(&common.User{Id: "user-1"})

	mcReq, calls, err := BuildMulticall3Request([]*common.NormalizedRequest{req1, req2}, nil)
	require.NoError(t, err)
	require.Len(t, calls, 2)
	require.NotNil(t, mcReq)

	mcID, ok := mcReq.ID().(string)
	require.True(t, ok)
	assert.True(t, strings.HasPrefix(mcID, "multicall3-"))

	require.NotNil(t, mcReq.User())
	assert.Equal(t, "user-1", mcReq.User().Id)
	require.NotNil(t, mcReq.Directives())
	assert.True(t, mcReq.Directives().SkipCacheRead)

	jrq, err := mcReq.JsonRpcRequest()
	require.NoError(t, err)
	require.NotNil(t, jrq)
	assert.Equal(t, "eth_call", jrq.Method)
	require.Len(t, jrq.Params, 2)

	callObj, ok := jrq.Params[0].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, multicall3Address, callObj["to"])

	encodedCalls, err := encodeAggregate3Calls(calls)
	require.NoError(t, err)
	dataHex, ok := callObj["data"].(string)
	require.True(t, ok)
	assert.Equal(t, "0x"+fmt.Sprintf("%x", encodedCalls), dataHex)
	assert.Equal(t, "latest", jrq.Params[1])

	assert.Equal(t, 20, len(calls[0].Target))
	assert.Equal(t, 1, len(calls[0].CallData))
	assert.Equal(t, 32, len(calls[1].CallData))
}

func TestBuildMulticall3Request_Errors(t *testing.T) {
	validCall := map[string]interface{}{
		"to":   hexAddr(3),
		"data": "0x",
	}

	cases := []struct {
		name           string
		requests       []*common.NormalizedRequest
		eligibleErr    bool
		unexpectedWrap bool
	}{
		{
			name:        "no requests",
			requests:    []*common.NormalizedRequest{},
			eligibleErr: true,
		},
		{
			name:        "nil request",
			requests:    []*common.NormalizedRequest{nil},
			eligibleErr: true,
		},
		{
			name:           "invalid json",
			requests:       []*common.NormalizedRequest{common.NewNormalizedRequest([]byte("{"))},
			unexpectedWrap: true,
		},
		{
			name:        "wrong method",
			requests:    []*common.NormalizedRequest{newJsonRpcRequest(t, "eth_getBalance", []interface{}{hexAddr(1)}, 1)},
			eligibleErr: true,
		},
		{
			name:        "missing params",
			requests:    []*common.NormalizedRequest{newJsonRpcRequest(t, "eth_call", []interface{}{}, 1)},
			eligibleErr: true,
		},
		{
			name:        "too many params",
			requests:    []*common.NormalizedRequest{newJsonRpcRequest(t, "eth_call", []interface{}{validCall, "latest", "extra"}, 1)},
			eligibleErr: true,
		},
		{
			name:        "call obj not map",
			requests:    []*common.NormalizedRequest{newJsonRpcRequest(t, "eth_call", []interface{}{123}, 1)},
			eligibleErr: true,
		},
		{
			name:        "missing to",
			requests:    []*common.NormalizedRequest{newJsonRpcRequest(t, "eth_call", []interface{}{map[string]interface{}{"data": "0x"}}, 1)},
			eligibleErr: true,
		},
		{
			name:        "data not string",
			requests:    []*common.NormalizedRequest{newJsonRpcRequest(t, "eth_call", []interface{}{map[string]interface{}{"to": hexAddr(1), "data": 1}}, 1)},
			eligibleErr: true,
		},
		{
			name:        "input not string",
			requests:    []*common.NormalizedRequest{newJsonRpcRequest(t, "eth_call", []interface{}{map[string]interface{}{"to": hexAddr(1), "input": 1}}, 1)},
			eligibleErr: true,
		},
		{
			name:        "extra key",
			requests:    []*common.NormalizedRequest{newJsonRpcRequest(t, "eth_call", []interface{}{map[string]interface{}{"to": hexAddr(1), "data": "0x", "value": "0x1"}}, 1)},
			eligibleErr: true,
		},
		{
			name:        "invalid to length",
			requests:    []*common.NormalizedRequest{newJsonRpcRequest(t, "eth_call", []interface{}{map[string]interface{}{"to": "0x1234", "data": "0x"}}, 1)},
			eligibleErr: true,
		},
		{
			name:        "invalid data hex",
			requests:    []*common.NormalizedRequest{newJsonRpcRequest(t, "eth_call", []interface{}{map[string]interface{}{"to": hexAddr(1), "data": "0xzz"}}, 1)},
			eligibleErr: true,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := BuildMulticall3Request(tt.requests, "latest")
			require.Error(t, err)
			if tt.eligibleErr {
				assert.ErrorIs(t, err, ErrMulticall3BatchNotEligible)
			}
			if tt.unexpectedWrap {
				assert.False(t, errors.Is(err, ErrMulticall3BatchNotEligible))
			}
		})
	}
}

func TestDecodeMulticall3Aggregate3Result(t *testing.T) {
	results := []Multicall3Result{
		{Success: true, ReturnData: []byte{0x01, 0x02}},
		{Success: false, ReturnData: nil},
	}

	encoded := encodeAggregate3Results(results)
	decoded, err := DecodeMulticall3Aggregate3Result(encoded)
	require.NoError(t, err)
	assert.Equal(t, results, decoded)

	emptyEncoded := encodeAggregate3Results(nil)
	emptyDecoded, err := DecodeMulticall3Aggregate3Result(emptyEncoded)
	require.NoError(t, err)
	assert.Empty(t, emptyDecoded)
}

func TestDecodeMulticall3Aggregate3Result_Errors(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{
			name: "too short",
			data: []byte{0x01},
		},
		{
			name: "offset out of bounds",
			data: encodeUint64(64),
		},
		{
			name: "offsets out of bounds",
			data: append(encodeUint64(32), encodeUint64(2)...),
		},
		{
			name: "element out of bounds",
			data: buildAggregate3ResultWithOffset(96, nil, nil),
		},
		{
			name: "bytes offset out of bounds",
			data: buildAggregate3ResultWithElement(64, encodeBool(true), encodeUint64(256)),
		},
		{
			name: "bytes length out of bounds",
			data: buildAggregate3ResultBytesLengthOutOfBounds(),
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeMulticall3Aggregate3Result(tt.data)
			require.Error(t, err)
		})
	}

	_, err := readUint256([]byte{0x01})
	require.Error(t, err)

	overflow := make([]byte, 32)
	overflow[0] = 1
	_, err = readUint256(overflow)
	require.Error(t, err)

	_, err = readBool([]byte{0x01})
	require.Error(t, err)
}

func TestShouldFallbackMulticall3(t *testing.T) {
	assert.False(t, ShouldFallbackMulticall3(nil))
	assert.True(t, ShouldFallbackMulticall3(common.NewErrEndpointExecutionException(errors.New("boom"))))
	assert.True(t, ShouldFallbackMulticall3(common.NewErrEndpointUnsupported(errors.New("boom"))))
	assert.False(t, ShouldFallbackMulticall3(errors.New("nope")))
}

func encodeAggregate3Results(results []Multicall3Result) []byte {
	headSize := 32 + 32*len(results)
	offsets := make([]uint64, len(results))
	elems := make([][]byte, len(results))
	cur := uint64(headSize)

	for i, res := range results {
		elems[i] = encodeAggregate3ResultElement(res)
		offsets[i] = cur
		cur += uint64(len(elems[i]))
	}

	array := make([]byte, 0, int(cur))
	array = append(array, encodeUint64(uint64(len(results)))...)
	for _, off := range offsets {
		array = append(array, encodeUint64(off)...)
	}
	for _, elem := range elems {
		array = append(array, elem...)
	}

	out := make([]byte, 0, 32+len(array))
	out = append(out, encodeUint64(32)...)
	out = append(out, array...)
	return out
}

func encodeAggregate3ResultElement(result Multicall3Result) []byte {
	head := make([]byte, 0, 64)
	head = append(head, encodeBool(result.Success)...)
	head = append(head, encodeUint64(64)...)
	tail := encodeBytes(result.ReturnData)
	return append(head, tail...)
}

func buildAggregate3ResultWithOffset(offset uint64, count []byte, elemOffset []byte) []byte {
	data := make([]byte, 96)
	copy(data, encodeUint64(32))
	if count != nil {
		copy(data[32:], count)
	} else {
		copy(data[32:], encodeUint64(1))
	}
	if elemOffset != nil {
		copy(data[64:], elemOffset)
	} else {
		copy(data[64:], encodeUint64(offset))
	}
	return data
}

func buildAggregate3ResultWithElement(elemOffset uint64, head ...[]byte) []byte {
	data := make([]byte, 160)
	copy(data, encodeUint64(32))
	copy(data[32:], encodeUint64(1))
	copy(data[64:], encodeUint64(elemOffset))
	copy(data[96:], head[0])
	copy(data[128:], head[1])
	return data
}

func buildAggregate3ResultBytesLengthOutOfBounds() []byte {
	data := make([]byte, 192)
	copy(data, encodeUint64(32))
	copy(data[32:], encodeUint64(1))
	copy(data[64:], encodeUint64(64))
	copy(data[96:], encodeBool(true))
	copy(data[128:], encodeUint64(64))
	copy(data[160:], encodeUint64(128))
	return data
}

func hexAddr(n int) string {
	return fmt.Sprintf("0x%040x", n)
}

func hexData(size int) string {
	return "0x" + strings.Repeat("11", size)
}

func newEthCallRequest(t *testing.T, id interface{}, callObj map[string]interface{}, blockParam interface{}) *common.NormalizedRequest {
	t.Helper()
	params := []interface{}{callObj}
	if blockParam != nil {
		params = append(params, blockParam)
	}
	jr := common.NewJsonRpcRequest("eth_call", params)
	if id != nil {
		require.NoError(t, jr.SetID(id))
	}
	return common.NewNormalizedRequestFromJsonRpcRequest(jr)
}

func newJsonRpcRequest(t *testing.T, method string, params []interface{}, id interface{}) *common.NormalizedRequest {
	t.Helper()
	jr := common.NewJsonRpcRequest(method, params)
	require.NoError(t, jr.SetID(id))
	return common.NewNormalizedRequestFromJsonRpcRequest(jr)
}
