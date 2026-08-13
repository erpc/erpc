package common

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// NormalizedRequest.Validate is the edge gate: every inbound request (HTTP
// handler and the shared gRPC/query RequestProcessor) passes through it before
// auth, rate limiting, network bootstrap or any metric increment. These cases
// pin what it accepts and what it rejects.
func TestNormalizedRequest_Validate(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string // substring; empty means the request must be accepted
	}{
		{
			name: "canonical request",
			body: `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x0"},"latest"]}`,
		},
		{
			name: "params omitted entirely",
			body: `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber"}`,
		},
		{
			name: "params null",
			body: `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":null}`,
		},
		{
			name: "empty params array",
			body: `{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`,
		},
		{
			name: "openrpc discovery method keeps its dot",
			body: `{"jsonrpc":"2.0","id":1,"method":"rpc.discover"}`,
		},
		{
			name: "unknown-but-well-formed method still passes (open set)",
			body: `{"jsonrpc":"2.0","id":1,"method":"somechain_brandNewMethod","params":[]}`,
		},

		// Method rejections.
		{
			name:    "sql injection payload appended to method",
			body:    `{"jsonrpc":"2.0","id":1,"method":"eth_call0QIdoFZC') OR 157=(SELECT 157 FROM PG_SLEEP(15))--","params":[]}`,
			wantErr: "method must be 1-128 characters",
		},
		{
			name:    "sleep probe with embedded quotes",
			body:    `{"jsonrpc":"2.0","id":1,"method":"eth_call0\"XOR(if(now()=sysdate(),sleep(15),0))XOR\"Z","params":[]}`,
			wantErr: "method must be 1-128 characters",
		},
		{
			name:    "script tag payload",
			body:    `{"jsonrpc":"2.0","id":1,"method":"<script>alert(1)</script>","params":[]}`,
			wantErr: "method must be 1-128 characters",
		},
		{
			name:    "method longer than the ceiling",
			body:    fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":[]}`, strings.Repeat("a", MaxMethodNameLength+1)),
			wantErr: "method must be 1-128 characters",
		},
		{
			name:    "non-string method is not coerced",
			body:    `{"jsonrpc":"2.0","id":1,"method":123,"params":[]}`,
			wantErr: "method must be a string",
		},
		{
			name:    "boolean method is not coerced",
			body:    `{"jsonrpc":"2.0","id":1,"method":true,"params":[]}`,
			wantErr: "method must be a string",
		},
		{
			name:    "null method reads as missing",
			body:    `{"jsonrpc":"2.0","id":1,"method":null,"params":[]}`,
			wantErr: "method is required",
		},
		{
			name:    "empty method",
			body:    `{"jsonrpc":"2.0","id":1,"method":"","params":[]}`,
			wantErr: "method is required",
		},
		{
			name:    "method absent",
			body:    `{"jsonrpc":"2.0","id":1,"params":[]}`,
			wantErr: "ErrJsonRpcRequestUnmarshal",
		},

		// Params rejections — JSON-RPC 2.0 §4.2 structured value; eRPC's
		// JsonRpcRequest.Params is []interface{}, so only an array works.
		{
			name:    "object params",
			body:    `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":{"to":"0x0"}}`,
			wantErr: "params must be a json array",
		},
		{
			name:    "string params",
			body:    `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":"latest"}`,
			wantErr: "params must be a json array",
		},
		{
			name:    "number params",
			body:    `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":123}`,
			wantErr: "params must be a json array",
		},
		{
			name:    "boolean params",
			body:    `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":true}`,
			wantErr: "params must be a json array",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := NewNormalizedRequest([]byte(tc.body)).Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
			require.True(t, HasErrorCode(err, ErrCodeInvalidRequest),
				"rejection must classify as ErrInvalidRequest (HTTP 400), got: %v", err)
		})
	}
}

// A hostile method name must not be echoed back at full length into the error,
// the log line and the response body.
func TestNormalizedRequest_Validate_TruncatesEchoedMethod(t *testing.T) {
	method := strings.Repeat("!", 4096)
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":[]}`, method)

	err := NewNormalizedRequest([]byte(body)).Validate()
	require.Error(t, err)
	require.Less(t, len(err.Error()), 512, "error message must stay bounded: %d bytes", len(err.Error()))
}

// Validate must leave the raw body intact: the HTTP handler falls back to
// reading `networkId` out of it when the URL carries no architecture/chain.
func TestNormalizedRequest_Validate_PreservesBody(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[],"networkId":"evm:42161"}`
	nq := NewNormalizedRequest([]byte(body))
	require.NoError(t, nq.Validate())
	require.Equal(t, body, string(nq.Body()))
}

// Requests eRPC builds itself (composite sub-requests, integrity aux fetches,
// state pollers) never carry a raw body — Validate must accept them.
func TestNormalizedRequest_Validate_ProgrammaticRequest(t *testing.T) {
	nq := NewNormalizedRequestFromJsonRpcRequest(NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"latest", false}))
	require.NoError(t, nq.Validate())
}

// Method() MUST return bytes it owns. The HTTP handler hands NewNormalizedRequest
// a slice of a POOLED read buffer (util.ReadAll + a deferred cleanup that returns
// it to the pool), while the resolved method outlives the handler as a sync.Map
// key and a Prometheus label. A zero-copy view into that buffer mutates when the
// pool recycles it — which surfaces as `HashTrieMap: ran out of hash bits`
// panics deep in the consensus executor, not as anything resembling a parse bug.
func TestNormalizedRequest_Method_OwnsItsBytes(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":[]}`)
	nq := NewNormalizedRequest(body)

	method, err := nq.Method()
	require.NoError(t, err)
	require.Equal(t, "eth_getBalance", method)

	// Simulate the pooled buffer being recycled under us.
	for i := range body {
		body[i] = 'X'
	}

	require.Equal(t, "eth_getBalance", method, "returned method aliased the request buffer")
	cached, err := nq.Method()
	require.NoError(t, err)
	require.Equal(t, "eth_getBalance", cached, "cached method aliased the request buffer")
}

// Cost of the edge gate, M5, sonic v1.15.2 (before → after adding the method
// shape check and the params shape check):
//
//	small         265 ns / 506 B / 3 allocs  →   343 ns / 507 B / 3 allocs
//	params-first  255 ns / 508 B / 3 allocs  →   298 ns / 508 B / 3 allocs
//	large-raw-tx  221 ns / 515 B / 3 allocs  →  5175 ns / 506 B / 3 allocs
//
// Allocation profile is unchanged: the two lookups no longer copy the located
// value, and the one allocation that remains is the owned copy of the method
// (bounded by maxMethodRawLength) that the pooled-buffer invariant requires.
// The 128 KiB outlier is the structural skip over `params` — unavoidable in
// any key lookup, already at memory bandwidth, and ~12% of the full unmarshal
// (42 µs) the same request pays a few frames later regardless.
func BenchmarkNormalizedRequest_Validate(b *testing.B) {
	bodies := map[string]string{
		"small":        `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x742d35Cc6634C0532925a3b844Bc454e4438f44e","latest"]}`,
		"params-first": `{"params":["0x742d35Cc6634C0532925a3b844Bc454e4438f44e","latest"],"jsonrpc":"2.0","id":1,"method":"eth_getBalance"}`,
		"large-raw-tx": fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["0x%s"]}`, strings.Repeat("ab", 64*1024)),
	}
	for name, body := range bodies {
		raw := []byte(body)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if err := NewNormalizedRequest(raw).Validate(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	b.Run("rejected-method", func(b *testing.B) {
		raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call0QIdoFZC') OR 157=(SELECT 157 FROM PG_SLEEP(15))--","params":[]}`)
		b.ReportAllocs()
		for b.Loop() {
			if err := NewNormalizedRequest(raw).Validate(); err == nil {
				b.Fatal("expected rejection")
			}
		}
	})
}
