package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/require"
)

func TestNetwork_Forward_TimeoutClassificationDeterministic(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_traceTransaction")
		}).
		Persist().
		Reply(200).
		Delay(200 * time.Millisecond).
		JSON(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]any{"hash": "0xabc"},
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{
			{
				Timeout: &common.TimeoutPolicyConfig{
					Duration: common.Duration(40 * time.Millisecond),
				},
			},
		},
	}
	network := setupTestNetworkSimple(t, ctx, nil, networkCfg)

	for i := 0; i < 12; i++ {
		req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%d,"method":"eth_traceTransaction","params":["0x1273c18",false]}`,
			i+1,
		)))
		resp, err := network.Forward(ctx, req)
		if resp != nil {
			resp.Release()
		}

		require.Error(t, err)
		require.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeTimeoutExceeded), "iteration=%d err=%v", i, err)
		require.False(t, common.HasErrorCode(err, common.ErrCodeEndpointRequestTimeout), "iteration=%d err=%v", i, err)
		require.False(t, common.HasErrorCode(err, common.ErrCodeNetworkRequestTimeout), "iteration=%d err=%v", i, err)
	}
}
