package erpc

import (
	"bytes"
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNormalizeResponse_IDByteFidelity pins byte-for-byte preservation of the
// client's request id in the response, even for ids that don't survive a
// float64→int64 round-trip:
//   - integers > 2^53 (e.g. nanosecond timestamps used by some indexers)
//   - fractional ids (uncommon but legal per JSON-RPC spec)
//
// Prior to the fix, JsonRpcRequest.UnmarshalJSON parsed the id via
// `interface{}` (Go decodes JSON numbers as float64) and cast to int64,
// silently truncating both. The response then echoed the truncated value.
func TestNormalizeResponse_IDByteFidelity(t *testing.T) {
	ctx := context.Background()
	network := &Network{cfg: &common.NetworkConfig{Architecture: common.ArchitectureEvm}}

	cases := []struct {
		name      string
		requestID string // raw bytes as they appear in the request body
		wantID    string // raw bytes that must appear in the response output
	}{
		{
			name:      "small_int_unchanged",
			requestID: `1`,
			wantID:    `1`,
		},
		{
			name:      "zero_unchanged",
			requestID: `0`,
			wantID:    `0`,
		},
		{
			name:      "string_id_unchanged",
			requestID: `"abc-123"`,
			wantID:    `"abc-123"`,
		},
		{
			name:      "large_int_above_2_53_preserved",
			requestID: `9007199254740993`, // 2^53 + 1, smallest int that loses precision in float64
			wantID:    `9007199254740993`,
		},
		{
			name:      "fractional_id_preserved",
			requestID: `3.14`,
			wantID:    `3.14`,
		},
		{
			name:      "very_large_int_preserved",
			requestID: `18446744073709551614`, // near uint64 max
			wantID:    `18446744073709551614`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":` + tc.requestID + `}`)
			req := common.NewNormalizedRequest(body)

			// Upstream echoed back a different id (simulating a multiplexing proxy).
			jrr := common.MustNewJsonRpcResponseFromBytes(
				[]byte(`42`),
				[]byte(`"0x1"`),
				nil,
			)
			resp := common.NewNormalizedResponse().WithJsonRpcResponse(jrr)

			require.NoError(t, network.normalizeResponse(ctx, req, resp))

			out, err := resp.JsonRpcResponse(ctx)
			require.NoError(t, err)

			var buf bytes.Buffer
			_, err = out.WriteTo(&buf)
			require.NoError(t, err)

			// Wire output must contain the original id verbatim — no truncation,
			// no canonicalization.
			assert.Contains(t, buf.String(), `"id":`+tc.wantID,
				"response wire output must preserve the request id byte-for-byte; got %q", buf.String())
		})
	}
}

// TestJsonRpcRequest_IDRawBytes verifies the verbatim id bytes are captured
// during UnmarshalJSON for each id shape, and that programmatically-built
// requests (no UnmarshalJSON) return nil.
func TestJsonRpcRequest_IDRawBytes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // empty string means: expect nil (no idRaw)
	}{
		{name: "int", body: `{"jsonrpc":"2.0","method":"x","id":1}`, want: `1`},
		{name: "string", body: `{"jsonrpc":"2.0","method":"x","id":"a"}`, want: `"a"`},
		{name: "large_int", body: `{"jsonrpc":"2.0","method":"x","id":9007199254740993}`, want: `9007199254740993`},
		{name: "fractional", body: `{"jsonrpc":"2.0","method":"x","id":3.14}`, want: `3.14`},
		{name: "null_id_no_raw", body: `{"jsonrpc":"2.0","method":"x","id":null}`, want: ``},
		{name: "missing_id_no_raw", body: `{"jsonrpc":"2.0","method":"x"}`, want: ``},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &common.JsonRpcRequest{}
			require.NoError(t, req.UnmarshalJSON([]byte(tc.body)))
			got := req.IDRawBytes()
			if tc.want == "" {
				assert.Nil(t, got, "expected no idRaw for case %q", tc.name)
			} else {
				assert.Equal(t, tc.want, string(got))
			}
		})
	}

	t.Run("programmatic_request_no_raw", func(t *testing.T) {
		req := common.NewJsonRpcRequest("eth_chainId", nil)
		assert.Nil(t, req.IDRawBytes(), "programmatically-built requests should have no idRaw")
	})
}

// TestJsonRpcRequest_Clone_PropagatesIDRaw pins the contract that Clone()
// carries the verbatim id bytes forward. Without this, a cloned request
// would silently re-introduce the precision-loss bug for any flow that
// clones the request before response normalization.
func TestJsonRpcRequest_Clone_PropagatesIDRaw(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","method":"x","id":9007199254740993}`)
	req := &common.JsonRpcRequest{}
	require.NoError(t, req.UnmarshalJSON(body))
	require.Equal(t, "9007199254740993", string(req.IDRawBytes()))

	clone := req.Clone()
	assert.Equal(t, "9007199254740993", string(clone.IDRawBytes()),
		"Clone must propagate idRaw so cloned requests still round-trip the id byte-for-byte")
}

// TestJsonRpcRequest_SetID_ClearsStaleIDRaw pins that SetID makes the typed
// id authoritative — any captured wire bytes from UnmarshalJSON must be
// dropped, otherwise normalizeResponse (which prefers IDRawBytes) would
// echo the OLD wire id back to the client instead of the newly-set one.
func TestJsonRpcRequest_SetID_ClearsStaleIDRaw(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","method":"x","id":1}`)
	req := &common.JsonRpcRequest{}
	require.NoError(t, req.UnmarshalJSON(body))
	require.Equal(t, "1", string(req.IDRawBytes()), "precondition: idRaw is captured from wire")

	require.NoError(t, req.SetID(int64(42)))
	assert.Nil(t, req.IDRawBytes(),
		"SetID must clear idRaw so the new typed id (not the stale wire bytes) wins on the response")
}
