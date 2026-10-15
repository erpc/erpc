package common

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizedRequest_Validate_MethodShape(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "known method",
			body: `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x0"},"latest"]}`,
		},
		{
			name: "openrpc discovery method keeps its dot",
			body: `{"jsonrpc":"2.0","id":1,"method":"rpc.discover"}`,
		},
		{
			name: "unknown but well-shaped method passes",
			body: `{"jsonrpc":"2.0","id":1,"method":"somechain_brandNewMethod","params":[]}`,
		},
		{
			name:    "sql injection payload appended to method",
			body:    `{"jsonrpc":"2.0","id":1,"method":"eth_call0QIdoFZC') OR 157=(SELECT 157 FROM PG_SLEEP(15))--","params":[]}`,
			wantErr: "method must be 1-128 characters",
		},
		{
			name:    "escaped quote in method",
			body:    `{"jsonrpc":"2.0","id":1,"method":"eth_call0\"XOR(if(now()=sysdate(),sleep(15),0))XOR\"Z","params":[]}`,
			wantErr: "method must be 1-128 characters",
		},
		{
			name:    "method longer than the ceiling",
			body:    fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":[]}`, strings.Repeat("a", MaxMethodNameLength+1)),
			wantErr: "method must be 1-128 characters",
		},
		{
			name:    "object params",
			body:    `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":{"to":"0x0"}}`,
			wantErr: "params",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := NewNormalizedRequest([]byte(tc.body)).Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
			require.True(t, HasErrorCode(err, ErrCodeInvalidRequest), "want ErrInvalidRequest, got: %v", err)
		})
	}
}

func TestNormalizedRequest_Validate_TruncatesEchoedMethod(t *testing.T) {
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":[]}`, strings.Repeat("!", 4096))

	err := NewNormalizedRequest([]byte(body)).Validate()
	require.ErrorContains(t, err, `got: "`+strings.Repeat("!", 64)+`..."`)
	require.Less(t, len(err.Error()), 512)
}

func TestNormalizedRequest_Validate_ProgrammaticRequest(t *testing.T) {
	nq := NewNormalizedRequestFromJsonRpcRequest(NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"latest", false}))
	require.NoError(t, nq.Validate())
}
