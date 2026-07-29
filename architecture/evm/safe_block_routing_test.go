package evm

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplySafeBlockSource(t *testing.T) {
	const source = "tier:source"
	tests := []struct {
		name   string
		source string
		method string
		params string
		routed bool
	}{
		{name: "unconfigured safe is unchanged", method: "eth_getBalance", params: `["0xabc","safe"]`},
		{name: "scalar safe routes", source: source, method: "eth_getBalance", params: `["0xabc","safe"]`, routed: true},
		{name: "latest is unchanged", source: source, method: "eth_getBalance", params: `["0xabc","latest"]`},
		{name: "finalized is unchanged", source: source, method: "eth_getBalance", params: `["0xabc","finalized"]`},
		{name: "numeric is unchanged", source: source, method: "eth_getBalance", params: `["0xabc","0x1"]`},
		{name: "pending is unchanged", source: source, method: "eth_getBalance", params: `["0xabc","pending"]`},
		{name: "safe upper bound routes with numeric sibling", source: source, method: "eth_getLogs", params: `[{"fromBlock":"0x1","toBlock":"safe"}]`, routed: true},
		{name: "EIP-1898 blockNumber routes", source: source, method: "eth_call", params: `[{},{"blockNumber":"safe"}]`, routed: true},
		{name: "access list block parameter routes", source: source, method: "eth_createAccessList", params: `[{"to":"0xabc"},"safe"]`, routed: true},
		{name: "new filter range routes", source: source, method: "eth_newFilter", params: `[{"fromBlock":"safe","toBlock":"latest"}]`, routed: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			network := &testNetwork{cfg: &common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{SafeBlockSource: tt.source},
			}}
			req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
				`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, tt.method, tt.params,
			)))
			req.SetDirectives(&common.RequestDirectives{
				UseUpstream:       "client-choice",
				SkipCacheRead:     "false",
				SkipInterpolation: true,
			})

			err := ApplySafeBlockSource(context.Background(), network, req)
			require.NoError(t, err)

			directives := req.Directives()
			require.NotNil(t, directives)
			if tt.routed {
				assert.Equal(t, source, directives.UseUpstream, "operator source must override the client selector")
				assert.Equal(t, "true", directives.SkipCacheRead)
			} else {
				assert.Equal(t, "client-choice", directives.UseUpstream)
				assert.Equal(t, "false", directives.SkipCacheRead)
			}
			assert.True(t, directives.SkipInterpolation, "unrelated client directives must be preserved")

			jrq, err := req.JsonRpcRequest(context.Background())
			require.NoError(t, err)
			gotParams, err := json.Marshal(jrq.Params)
			require.NoError(t, err)
			assert.JSONEq(t, tt.params, string(gotParams), "routing must not rewrite block parameters")
		})
	}
}
