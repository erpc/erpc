package erpc

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failover.emptyTriesNextFallback: The primaries can't serve the old
// block (an upstream-wide lower bound), so eth_getLogs escalates; a
// fallback's [] then doesn't end the request while another fallback is left.
func TestNetwork_EmptyTriesNextFallback(t *testing.T) {
	const logs = `[{"address":"0xdead","topics":[],"data":"0x","blockNumber":"0x3d0","transactionHash":"0x01","logIndex":"0x0"}]`
	cases := []struct {
		name       string
		on         bool
		rpc3, rpc4 string
		want       string // "logs" or "[]"
		wantCalls  int64  // fallback eth_getLogs calls
		exactCalls bool
	}{
		{name: "OnFindsTheOneWithLogs", on: true, rpc3: "[]", rpc4: logs, want: "logs"},
		{name: "OnAllEmpty", on: true, rpc3: "[]", rpc4: "[]", want: "[]", wantCalls: 2, exactCalls: true},
		{name: "OffStopsAtTheFirst", on: false, rpc3: "[]", rpc4: "[]", want: "[]", wantCalls: 1, exactCalls: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer util.ResetGock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			calls := countCalls("eth_getLogs")
			defer gock.Observe(nil)
			network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
				primaryLatest:  "0x3e8",
				fallbackLatest: "0x3e8",
				enableFailover: true,
				mocks: func() {
					for _, h := range []string{"rpc1.localhost", "rpc2.localhost"} {
						mockCountedMethod(h, "eth_getLogs", "[]")
					}
					mockCountedMethod("rpc3.localhost", "eth_getLogs", tc.rpc3)
					mockCountedMethod("rpc4.localhost", "eth_getLogs", tc.rpc4)
				},
				configure: func(cfgs []*common.UpstreamConfig) {
					for _, c := range cfgs {
						if c.HasTag(common.TagTierFallback) {
							c.Routing = &common.UpstreamRoutingConfig{Probe: common.ProbeModeOff}
							continue
						}
						// the primaries keep only the last 10 blocks
						c.Evm.BlockAvailability = &common.EvmBlockAvailabilityConfig{
							Lower: &common.EvmAvailabilityBoundConfig{LatestBlockMinus: i64(10)},
						}
					}
				},
				network: func(cfg *common.NetworkConfig) {
					// on by default in production (SetDefaults); the fixture builds the config by hand
					cfg.Evm.Integrity = &common.EvmIntegrityConfig{EnforceGetLogsBlockRange: util.BoolPtr(true)}
					if tc.on {
						cfg.Failover.EmptyTriesNextFallback = util.BoolPtr(true)
					}
				},
			})

			req := common.NewNormalizedRequest([]byte(
				`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x3d0","toBlock":"0x3d0"}]}`))
			req.SetNetwork(network)
			resp, err := network.Forward(ctx, req)
			require.NoError(t, err)
			require.NotNil(t, resp)
			defer resp.Release()
			jrr, err := resp.JsonRpcResponse()
			require.NoError(t, err)
			result := jrr.GetResultString()

			if tc.want == "logs" {
				assert.Contains(t, result, `"address":"0xdead"`)
			} else {
				assert.Equal(t, "[]", result)
			}
			assert.EqualValues(t, 0, calls("rpc1.localhost", "eth_getLogs")+calls("rpc2.localhost", "eth_getLogs"),
				"the primaries are skipped (below their lower bound)")
			if tc.exactCalls {
				assert.EqualValues(t, tc.wantCalls, calls("rpc3.localhost", "eth_getLogs")+calls("rpc4.localhost", "eth_getLogs"))
			}
		})
	}
}
