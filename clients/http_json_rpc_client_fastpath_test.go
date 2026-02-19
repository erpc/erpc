package clients

import (
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
)

func TestBuildJsonRpcBatchRequestBody(t *testing.T) {
	tests := []struct {
		name     string
		bodies   [][]byte
		expected string
	}{
		{
			name:     "empty",
			bodies:   nil,
			expected: "[]",
		},
		{
			name: "single",
			bodies: [][]byte{
				[]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`),
			},
			expected: `[{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}]`,
		},
		{
			name: "multiple",
			bodies: [][]byte{
				[]byte(`{"id":1}`),
				[]byte(`{"id":2}`),
				[]byte(`{"id":3}`),
			},
			expected: `[{"id":1},{"id":2},{"id":3}]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildJsonRpcBatchRequestBody(tt.bodies)
			if string(got) != tt.expected {
				t.Fatalf("unexpected batch body: expected %s got %s", tt.expected, string(got))
			}
		})
	}
}

func TestBuildJsonRpcBatchRequestBody_MixedRawAndModifiedRequests(t *testing.T) {
	rawReq1 := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`)
	rawReq2 := []byte(`{"jsonrpc":"2.0","id":2,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000001"}]}`)

	req1 := common.NewNormalizedRequest(rawReq1)
	if _, err := req1.JsonRpcRequest(); err != nil {
		t.Fatalf("failed to parse req1: %v", err)
	}
	req1Body, err := req1.ForwardBody()
	if err != nil {
		t.Fatalf("failed to get req1 body: %v", err)
	}
	if string(req1Body) != string(rawReq1) {
		t.Fatalf("expected req1 to keep raw body")
	}

	req2 := common.NewNormalizedRequest(rawReq2)
	jrq2, err := req2.JsonRpcRequest()
	if err != nil {
		t.Fatalf("failed to parse req2: %v", err)
	}
	if err := jrq2.AppendParam("latest"); err != nil {
		t.Fatalf("failed to mutate req2 params: %v", err)
	}
	req2Body, err := req2.ForwardBody()
	if err != nil {
		t.Fatalf("failed to get req2 body: %v", err)
	}
	if !strings.Contains(string(req2Body), `"latest"`) {
		t.Fatalf("expected req2 marshaled body to include latest block tag")
	}

	batchBody := buildJsonRpcBatchRequestBody([][]byte{req1Body, req2Body})
	if !strings.Contains(string(batchBody), string(rawReq1)) {
		t.Fatalf("expected batch body to include req1 raw body")
	}
	if !strings.Contains(string(batchBody), `"latest"`) {
		t.Fatalf("expected batch body to include req2 mutated payload")
	}

	var parsed []map[string]interface{}
	if err := common.SonicCfg.Unmarshal(batchBody, &parsed); err != nil {
		t.Fatalf("expected valid JSON array body, got error: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("expected 2 requests in batch body, got %d", len(parsed))
	}
}
