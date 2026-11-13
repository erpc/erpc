package erpc

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

// Two upstreams with skewed slots: "lag" trails "lead" on both axes. The
// served tip is the trailing one under the default (majority of 2 = both), and
// the leading one at svm.servedTip.quorum 1.
func TestSvm_ServedTipQuorum(t *testing.T) {
	const (
		lagHost  = "svm-quorum-lag.localhost"
		leadHost = "svm-quorum-lead.localhost"
	)

	cases := []struct {
		name          string
		servedTip     *common.SvmServedTipConfig
		wantLatest    int64
		wantFinalized int64
	}{
		{"unset is majority", nil, 1150, 1000},
		{"empty block is majority", &common.SvmServedTipConfig{}, 1150, 1000},
		{"majority", &common.SvmServedTipConfig{Quorum: common.ServedTipQuorumMajority}, 1150, 1000},
		{"quorum 2", &common.SvmServedTipConfig{Quorum: 2}, 1150, 1000},
		{"quorum 1 serves the highest", &common.SvmServedTipConfig{Quorum: 1}, 1200, 1100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForSvmStatePollerWithShred(lagHost, 1150, 1000, 1200)
			util.SetupMocksForSvmStatePollerWithShred(leadHost, 1200, 1100, 1200)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			net, _ := setupTestSvmNetworkWithConfig(t, ctx, &common.SvmNetworkConfig{
				Cluster:   "mainnet-beta",
				ServedTip: tc.servedTip,
			}, []*common.UpstreamConfig{
				svmUpstreamConfig("lag", lagHost),
				svmUpstreamConfig("lead", leadHost),
			})

			// The pool max is the same whatever the quorum: the guard's bound. Wait
			// for both pollers' first samples (slow under -race).
			require.Eventually(t, func() bool {
				return net.SvmHighestFinalizedSlotMax(ctx) == 1100 && net.SvmHighestLatestSlot(ctx) > 0 &&
					len(net.gatherSvmTipInputs(ctx, true)) == 2
			}, 5*time.Second, 20*time.Millisecond)
			require.Equal(t, tc.wantLatest, net.SvmHighestLatestSlot(ctx))
			require.Equal(t, tc.wantFinalized, net.SvmHighestFinalizedSlot(ctx))

			// getSlot(finalized) advertises min(finalized tip, indexed tip), whichever
			// upstream answered.
			resp, err := svmProjectForward(ctx, net, common.NewNormalizedRequest(
				[]byte(`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"finalized"}]}`)))
			require.NoError(t, err)
			jrr, err := resp.JsonRpcResponse()
			require.NoError(t, err)
			require.Nil(t, jrr.Error)
			got, err := strconv.ParseInt(string(jrr.GetResultBytes()), 10, 64)
			require.NoError(t, err)
			require.Equal(t, tc.wantFinalized, got)
		})
	}
}
