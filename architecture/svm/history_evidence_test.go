package svm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// CorroboratedHead trusts the served latest slot only at a majority quorum:
// at svm.servedTip.quorum 1 it is one upstream's report.
func TestSvmHistoryEvidence_CorroboratedHeadNeedsMajority(t *testing.T) {
	cases := []struct {
		name      string
		servedTip *common.SvmServedTipConfig
		want      int64
	}{
		{"unset is majority", nil, 500},
		{"empty block is majority", &common.SvmServedTipConfig{}, 500},
		{"majority", &common.SvmServedTipConfig{Quorum: common.ServedTipQuorumMajority}, 500},
		{"quorum 1", &common.SvmServedTipConfig{Quorum: 1}, 0},
		{"quorum 2", &common.SvmServedTipConfig{Quorum: 2}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			net := &fakeNetwork{
				cfg: &common.NetworkConfig{
					Architecture: common.ArchitectureSvm,
					Svm:          &common.SvmNetworkConfig{ServedTip: tc.servedTip},
				},
				latestSlot: 500,
			}
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[400]}`))
			req.SetNetwork(net)
			require.Equal(t, tc.want, svmHistoryEvidence{}.CorroboratedHead(context.Background(), req))
		})
	}

	t.Run("no svm block is majority", func(t *testing.T) {
		net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}, latestSlot: 500}
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[400]}`))
		req.SetNetwork(net)
		require.Equal(t, int64(500), svmHistoryEvidence{}.CorroboratedHead(context.Background(), req))
	})
}
