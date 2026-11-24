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

// gock runs every mock's filter on every request, so each filter checks its
// own host and method before it counts.
func countCallsTo(host, method string, calls *atomic.Int32) func(*http.Request) bool {
	return func(r *http.Request) bool {
		if r.URL.Host != host || !strings.Contains(util.SafeReadBody(r), `"`+method+`"`) {
			return false
		}
		calls.Add(1)
		return true
	}
}

func newRefusalTestNetwork(t *testing.T, ctx context.Context) *Network {
	return setupSendRawTxTestNetworkWithRetry(t, ctx, &common.RetryPolicyConfig{
		MaxAttempts: 6,
		Delay:       common.Duration(10 * time.Millisecond),
	})
}

// A node that refuses a call for asking more gas than its own cap is not
// asked again, but the call still fails over: the cap is per-provider
// configuration, so another upstream may allow the same gas. Monad says
// "user-specified gas exceeds provider limit" on eth_call; eth_simulateV1
// gives the spec code -38026 ("client adjustable limit").
func TestNetwork_ProviderGasCap(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		request string
		refusal map[string]interface{}
	}{
		{
			name:    "EthCall",
			method:  "eth_call",
			request: `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000001","data":"0x","gas":"0x8f0d1801"},"latest"]}`,
			refusal: map[string]interface{}{"code": -32000, "message": "user-specified gas exceeds provider limit"},
		},
		{
			name:    "SimulateV1",
			method:  "eth_simulateV1",
			request: `{"jsonrpc":"2.0","id":1,"method":"eth_simulateV1","params":[{"blockStateCalls":[{"calls":[{"to":"0x0000000000000000000000000000000000000001","gas":"0x3b9aca00"}]}]},"latest"]}`,
			refusal: map[string]interface{}{"code": -38026, "message": "Client adjustable limit reached"},
		},
	}

	for _, tc := range cases {
		refusalReply := map[string]interface{}{"jsonrpc": "2.0", "id": 1, "error": tc.refusal}

		t.Run(tc.name+"/FailsOverToAnUpstreamWithAHigherCap", func(t *testing.T) {
			util.ResetGock()
			t.Cleanup(util.ResetGock)
			util.SetupMocksForEvmStatePoller()

			var rpc1Calls, rpc2Calls atomic.Int32
			gock.New("http://rpc1.localhost").Post("").Persist().
				Filter(countCallsTo("rpc1.localhost", tc.method, &rpc1Calls)).
				Reply(200).JSON(refusalReply)
			gock.New("http://rpc2.localhost").Post("").Persist().
				Filter(countCallsTo("rpc2.localhost", tc.method, &rpc2Calls)).
				Reply(200).JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x01"})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			resp, err := newRefusalTestNetwork(t, ctx).Forward(ctx, common.NewNormalizedRequest([]byte(tc.request)))
			require.NoError(t, err)
			jrr, err := resp.JsonRpcResponse()
			require.NoError(t, err)
			assert.Equal(t, `"0x01"`, string(jrr.GetResultBytes()))
			assert.LessOrEqual(t, rpc1Calls.Load(), int32(1), "the refusing upstream must not be asked again")
			assert.Equal(t, int32(1), rpc2Calls.Load())
		})

		t.Run(tc.name+"/AsksEachUpstreamOnceWhenAllRefuse", func(t *testing.T) {
			util.ResetGock()
			t.Cleanup(util.ResetGock)
			util.SetupMocksForEvmStatePoller()

			var rpc1Calls, rpc2Calls atomic.Int32
			gock.New("http://rpc1.localhost").Post("").Persist().
				Filter(countCallsTo("rpc1.localhost", tc.method, &rpc1Calls)).
				Reply(200).JSON(refusalReply)
			gock.New("http://rpc2.localhost").Post("").Persist().
				Filter(countCallsTo("rpc2.localhost", tc.method, &rpc2Calls)).
				Reply(200).JSON(refusalReply)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			_, err := newRefusalTestNetwork(t, ctx).Forward(ctx, common.NewNormalizedRequest([]byte(tc.request)))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.refusal["message"], "the caller must see the node's reason")
			assert.Equal(t, http.StatusOK, determineResponseStatusCode(err), "a node's refusal, not a rate limit")
			assert.Equal(t, int32(1), rpc1Calls.Load(), "rpc1 is asked once")
			assert.Equal(t, int32(1), rpc2Calls.Load(), "rpc2 is asked once")
		})
	}
}

// An eth_simulateV1 refusal that every node gives for the same request is not
// retried: the caller gets the node's reason after one attempt, with HTTP 200
// as a node sends it. -38015 means a call asks more gas than the simulated
// block allows; reth and geth word it differently, and reth's wording ("limit
// exceeded") once read as a rate limit.
func TestNetwork_SimulateV1_SharedRefusalsAreNotRetried(t *testing.T) {
	const oneCall = `{"jsonrpc":"2.0","id":1,"method":"eth_simulateV1","params":[{"blockStateCalls":[{"calls":[{"to":"0x0000000000000000000000000000000000000001","gas":"0x11e1a300"}]}]},"latest"]}`
	cases := []struct {
		name    string
		request string
		refusal map[string]interface{}
	}{
		{
			name:    "EmptyInput",
			request: `{"jsonrpc":"2.0","id":1,"method":"eth_simulateV1","params":[{"blockStateCalls":[]},"latest"]}`,
			refusal: map[string]interface{}{"code": -32000, "message": "empty input"},
		},
		{
			name:    "BlockGasLimitReth",
			request: oneCall,
			refusal: map[string]interface{}{"code": -38015, "message": "Block gas limit exceeded by the block's transactions"},
		},
		{
			name:    "BlockGasLimitGeth",
			request: oneCall,
			refusal: map[string]interface{}{"code": -38015, "message": "block gas limit reached: remaining: 60000000, required: 300000000"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			util.ResetGock()
			t.Cleanup(util.ResetGock)
			util.SetupMocksForEvmStatePoller()

			var calls atomic.Int32
			for _, host := range []string{"rpc1.localhost", "rpc2.localhost"} {
				gock.New("http://" + host).Post("").Persist().
					Filter(countCallsTo(host, "eth_simulateV1", &calls)).
					Reply(200).
					JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "error": tc.refusal})
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			_, err := newRefusalTestNetwork(t, ctx).Forward(ctx, common.NewNormalizedRequest([]byte(tc.request)))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.refusal["message"], "the caller must see the node's reason")
			assert.Equal(t, http.StatusOK, determineResponseStatusCode(err), "a node's refusal, not a rate limit")
			assert.Equal(t, int32(1), calls.Load(), "one attempt, then the call fails")
		})
	}
}
