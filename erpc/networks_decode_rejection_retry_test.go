package erpc

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// decodeRejectionNetwork builds two upstreams, each with an upstream retry of
// 3, under a network retry of 3. Each upstream answers eth_call with reply up
// to 9 times. It returns the mocks, so a test can count the calls they took.
// gock's ReplyFunc runs once at setup, so one-shot mocks count the calls.
func decodeRejectionNetwork(t *testing.T, ctx context.Context, reply map[string]interface{}) (*Network, []gock.Mock) {
	t.Helper()
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	util.SetupMocksForEvmStatePoller()

	isCall := func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_call") }
	var mocks []gock.Mock
	var upstreams []*common.UpstreamConfig
	for _, id := range []string{"rpc1", "rpc2"} {
		endpoint := "http://" + id + ".localhost"
		upstreams = append(upstreams, &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       id,
			Endpoint: endpoint,
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
			Failsafe: []*common.FailsafeConfig{{
				Retry: &common.RetryPolicyConfig{MaxAttempts: 3},
			}},
		})
		for i := 0; i < 9; i++ {
			res := gock.New(endpoint).Post("").Filter(isCall).Times(1).Reply(200).JSON(reply)
			mocks = append(mocks, res.Mock)
		}
	}
	network := setupTestNetworkWithCustomUpstreams(t, ctx, upstreams, nil,
		&common.RetryPolicyConfig{MaxAttempts: 3})
	network.PinUpstreamOrderForTest("rpc1", "rpc2")
	return network, mocks
}

func usedMocks(mocks []gock.Mock) int {
	n := 0
	for _, m := range mocks {
		if m.Done() {
			n++
		}
	}
	return n
}

func invalidParamsReply(message string, data interface{}) map[string]interface{} {
	e := map[string]interface{}{"code": -32602, "message": message}
	if data != nil {
		e["data"] = data
	}
	return map[string]interface{}{"jsonrpc": "2.0", "id": 1, "error": e}
}

// reth answers a request it cannot decode with -32602 and the serde_json
// error in "data". Every node decodes the same bytes the same way, so the
// first answer is the answer: no retry on the same upstream, no failover.
func TestNetwork_ADecodeRejectionIsAskedOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, mocks := decodeRejectionNetwork(t, ctx, invalidParamsReply("Invalid params",
		`invalid value: string "0x70a082310", expected a valid hex string at line 1 column 72`))

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000001","data":"0x70a082310"},"latest"]}`))
	_, err := network.Forward(ctx, req)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointClientSideException), "got %v", err)
	require.Equal(t, 1, usedMocks(mocks), "a request no node can decode must cost one upstream call")
}

// A generic -32602 does not say whether another node, or a later call, can
// succeed, so it keeps its retry and its failover.
func TestNetwork_AnyOtherInvalidParamsKeepsItsRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, mocks := decodeRejectionNetwork(t, ctx, invalidParamsReply("Invalid params", nil))

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000001","data":"0x"},"latest"]}`))
	_, err := network.Forward(ctx, req)
	require.Error(t, err)
	require.Greater(t, usedMocks(mocks), 1, "a generic -32602 must still reach another attempt")
}
