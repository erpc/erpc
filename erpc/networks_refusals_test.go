package erpc

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A node that refuses an eth_call for asking more gas than its own cap
// ("user-specified gas exceeds provider limit") is not asked again, but the
// call still fails over: the cap is per-provider configuration, so another
// upstream may allow the same gas.
func TestNetwork_EthCall_ProviderGasCap(t *testing.T) {
	const gasCapError = "user-specified gas exceeds provider limit"
	requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000001","data":"0x","gas":"0x8f0d1801"},"latest"]}`)

	// gock runs every mock's filter on every request, so each filter checks its
	// own host before it counts.
	ethCallTo := func(host string, calls *atomic.Int32) func(*http.Request) bool {
		return func(r *http.Request) bool {
			if r.URL.Host != host || !strings.Contains(util.SafeReadBody(r), `"eth_call"`) {
				return false
			}
			calls.Add(1)
			return true
		}
	}
	gasCapReply := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"error":   map[string]interface{}{"code": -32000, "message": gasCapError},
	}
	newNetwork := func(t *testing.T, ctx context.Context) *Network {
		return setupSendRawTxTestNetworkWithRetry(t, ctx, &common.RetryPolicyConfig{
			MaxAttempts: 6,
			Delay:       common.Duration(10 * time.Millisecond),
		})
	}

	t.Run("FailsOverToAnUpstreamWithAHigherCap", func(t *testing.T) {
		util.ResetGock()
		t.Cleanup(util.ResetGock)
		util.SetupMocksForEvmStatePoller()

		var rpc1Calls, rpc2Calls atomic.Int32
		gock.New("http://rpc1.localhost").Post("").Persist().
			Filter(ethCallTo("rpc1.localhost", &rpc1Calls)).
			Reply(200).JSON(gasCapReply)
		gock.New("http://rpc2.localhost").Post("").Persist().
			Filter(ethCallTo("rpc2.localhost", &rpc2Calls)).
			Reply(200).JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x01"})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		resp, err := newNetwork(t, ctx).Forward(ctx, common.NewNormalizedRequest(requestBytes))
		require.NoError(t, err)
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, `"0x01"`, string(jrr.GetResultBytes()))
		assert.LessOrEqual(t, rpc1Calls.Load(), int32(1), "the refusing upstream must not be asked again")
		assert.Equal(t, int32(1), rpc2Calls.Load())
	})

	t.Run("AsksEachUpstreamOnceWhenAllRefuse", func(t *testing.T) {
		util.ResetGock()
		t.Cleanup(util.ResetGock)
		util.SetupMocksForEvmStatePoller()

		var rpc1Calls, rpc2Calls atomic.Int32
		gock.New("http://rpc1.localhost").Post("").Persist().
			Filter(ethCallTo("rpc1.localhost", &rpc1Calls)).
			Reply(200).JSON(gasCapReply)
		gock.New("http://rpc2.localhost").Post("").Persist().
			Filter(ethCallTo("rpc2.localhost", &rpc2Calls)).
			Reply(200).JSON(gasCapReply)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		_, err := newNetwork(t, ctx).Forward(ctx, common.NewNormalizedRequest(requestBytes))
		require.Error(t, err)
		assert.Contains(t, err.Error(), gasCapError, "the caller must see the node's reason")
		assert.Equal(t, int32(1), rpc1Calls.Load(), "rpc1 is asked once")
		assert.Equal(t, int32(1), rpc2Calls.Load(), "rpc2 is asked once")
	})
}

// An eth_simulateV1 with no block to simulate is refused with "empty input"
// by every node: the caller gets that reason after one attempt, not after the
// failsafe has asked every upstream again and again.
func TestNetwork_SimulateV1_EmptyInputIsNotRetried(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	util.SetupMocksForEvmStatePoller()

	var calls atomic.Int32
	for _, host := range []string{"rpc1.localhost", "rpc2.localhost"} {
		host := host
		gock.New("http://" + host).Post("").Persist().
			Filter(func(r *http.Request) bool {
				if r.URL.Host != host || !strings.Contains(util.SafeReadBody(r), `"eth_simulateV1"`) {
					return false
				}
				calls.Add(1)
				return true
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32000, "message": "empty input"},
			})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := setupSendRawTxTestNetworkWithRetry(t, ctx, &common.RetryPolicyConfig{
		MaxAttempts: 6,
		Delay:       common.Duration(10 * time.Millisecond),
	})

	_, err := network.Forward(ctx, common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_simulateV1","params":[{"blockStateCalls":[]},"latest"]}`)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty input", "the caller must see the node's reason")
	assert.Equal(t, int32(1), calls.Load(), "one attempt, then the call fails")
}
