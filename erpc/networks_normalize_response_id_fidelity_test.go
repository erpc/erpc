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
// client's request id in the response for every id that eRPC accepts, notably
// integers above 2^53 that don't survive a float64 round-trip (e.g. nanosecond
// timestamps used by some indexers).
//
// Sonic decodes JSON numbers as float64, so JsonRpcRequest.UnmarshalJSON used
// to cast the id to int64 and silently truncate it. It now recovers the exact
// integer from the verbatim id bytes; ids that cannot be represented losslessly
// as int64 (fractional or out-of-range) are rejected before they can be
// corrupted — see TestNormalizeResponse_IDLossyRejected.
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
			name:      "nanosecond_timestamp_preserved",
			requestID: `1755648000000000123`, // 19-digit int, > 2^53, still within int64
			wantID:    `1755648000000000123`,
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

// TestNormalizeResponse_IDLossyRejected pins that a numeric request id which
// cannot be represented losslessly as int64 is rejected at parse time rather
// than silently truncated/clamped into a different internal id (issue #869).
// eRPC represents numeric request ids as int64 internally and echoes them to
// upstreams, so a lossy id would corrupt the upstream-facing request and could
// collapse distinct client ids onto the same internal id.
func TestNormalizeResponse_IDLossyRejected(t *testing.T) {
	ctx := context.Background()
	network := &Network{cfg: &common.NetworkConfig{Architecture: common.ArchitectureEvm}}

	lossy := []string{
		`3.14`,                       // fractional
		`1e20`,                       // exponent, out of int64 range
		`9.3e18`,                     // exponent within magnitude but not an exact int
		`9223372036854775808`,        // MaxInt64 + 1
		`18446744073709551614`,       // near uint64 max
		`99999999999999999999999999`, // far beyond int64
	}

	for _, idJSON := range lossy {
		t.Run(idJSON, func(t *testing.T) {
			body := []byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":` + idJSON + `}`)
			req := common.NewNormalizedRequest(body)

			jrr := common.MustNewJsonRpcResponseFromBytes([]byte(`42`), []byte(`"0x1"`), nil)
			resp := common.NewNormalizedResponse().WithJsonRpcResponse(jrr)

			err := network.normalizeResponse(ctx, req, resp)
			require.Error(t, err, "lossy id %s must be rejected, not silently converted", idJSON)
			assert.Contains(t, err.Error(), "cannot be represented as a 64-bit integer")
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
		{name: "negative_int", body: `{"jsonrpc":"2.0","method":"x","id":-42}`, want: `-42`},
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
