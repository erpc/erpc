package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
)

// A call-simulation request without a block parameter must reach upstreams
// pinned to "latest". Forwarded bare, each node applies its own default block,
// and one HyperEVM vendor answered bare eth_estimateGas from state 25+ minutes
// behind the head it served for eth_call.
func TestProjectPreForward_MissingBlockParamDefaultsToLatest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		body        string
		wantHandled bool
		wantParams  []interface{}
	}{
		{
			name:        "eth_call without block",
			body:        `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x1"}]}`,
			wantHandled: true,
			wantParams:  []interface{}{map[string]interface{}{"to": "0x1"}, "latest"},
		},
		{
			name:        "eth_estimateGas without block",
			body:        `{"jsonrpc":"2.0","id":1,"method":"eth_estimateGas","params":[{"to":"0x1"}]}`,
			wantHandled: true,
			wantParams:  []interface{}{map[string]interface{}{"to": "0x1"}, "latest"},
		},
		{
			name:        "eth_estimateGas with explicit block is left alone",
			body:        `{"jsonrpc":"2.0","id":1,"method":"eth_estimateGas","params":[{"to":"0x1"},"pending"]}`,
			wantHandled: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var forwarded []interface{}
			network := &queryTestNetwork{
				cfg: &common.NetworkConfig{Architecture: common.ArchitectureEvm},
				forwardFn: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
					jrq, err := req.JsonRpcRequest()
					if err != nil {
						return nil, err
					}
					forwarded = jrq.Params
					return common.NewNormalizedResponse().WithRequest(req), nil
				},
			}

			req := common.NewNormalizedRequest([]byte(tc.body))
			handled, _, err := HandleProjectPreForward(context.Background(), network, req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if handled != tc.wantHandled {
				t.Fatalf("handled: got %v, want %v", handled, tc.wantHandled)
			}
			if !tc.wantHandled {
				return
			}
			if len(forwarded) != len(tc.wantParams) || forwarded[1] != tc.wantParams[1] {
				t.Fatalf("forwarded params: got %v, want %v", forwarded, tc.wantParams)
			}
		})
	}
}
