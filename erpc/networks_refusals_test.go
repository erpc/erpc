package erpc

import (
	"context"
	"fmt"
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
// upstream may allow the same gas. A node that lowers the gas to its cap
// instead, as geth does, is asked after the upstreams whose measured cap
// gives the gas, by a retry or a hedge alike.
func TestNetwork_EthCall_ProviderGasCap(t *testing.T) {
	const gasCapError = "user-specified gas exceeds provider limit"
	const callTo = `"to":"0x0000000000000000000000000000000000000001"`
	requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{` + callTo + `,"data":"0x","gas":"0x8f0d1801"},"latest"]}`)

	// gock runs every mock's filter on every request, so each filter checks its
	// own host. It counts only the call under test, not the gas cap probe's.
	ethCallTo := func(host string, calls *atomic.Int32) func(*http.Request) bool {
		return func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			if r.URL.Host != host || !strings.Contains(body, `"eth_call"`) {
				return false
			}
			if strings.Contains(body, callTo) {
				calls.Add(1)
			}
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

	t.Run("LowersGasToTheNetworkCapAndAsksAnUpstreamThatGivesIt", func(t *testing.T) {
		util.ResetGock()
		t.Cleanup(util.ResetGock)
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// rpc3 and rpc4 have the same head, so both hold latest.
		upstreamTo := func(id string) *common.UpstreamConfig {
			return &common.UpstreamConfig{Type: common.UpstreamTypeEvm, Id: id, Endpoint: "http://" + id + ".localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}}
		}
		network := setupSendRawTxNetwork(t, ctx, []*common.UpstreamConfig{upstreamTo("rpc3"), upstreamTo("rpc4")}, &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{ChainId: 123},
			Failsafe: []*common.FailsafeConfig{{
				Retry: &common.RetryPolicyConfig{MaxAttempts: 6, Delay: common.Duration(10 * time.Millisecond)},
				Hedge: &common.HedgePolicyConfig{Delay: common.NewStaticDuration(50 * time.Millisecond), MaxCount: 1},
			}},
		})

		// The upstream the policy orders first gets the low cap, so only the gas
		// order sends a call past it. The high cap answers after the hedge
		// fires, so only the hedge's wait keeps the low cap out of the race.
		// Its provider refuses gas above 1B, as a proxy before a node may.
		const refusedGas = `"gas":"0x8f0d1801"`
		ups := network.upstreamsRegistry.GetNetworkUpstreams(ctx, network.networkId)
		require.Len(t, ups, 2)
		low, high := ups[0].Id(), ups[1].Id()

		// The probe's code answers the gas left as one word, after a cost of 2:
		// the 10M run all of it, the 2^40 run the node's cap.
		answers := map[string]string{"rpc3": "0x03", "rpc4": "0x04"}
		calls := map[string]*atomic.Int32{low: {}, high: {}}
		lastCall := map[string]*atomic.Value{low: {}, high: {}}
		for id, gasCap := range map[string]int64{low: 50_000_000, high: 600_000_000} {
			host := id + ".localhost"
			for gas, left := range map[string]int64{`"gas":"0x989680"`: 10_000_000 - 2, `"gas":"0x10000000000"`: gasCap - 2} {
				gock.New("http://" + host).Post("").Persist().
					Filter(func(r *http.Request) bool {
						body := util.SafeReadBody(r)
						return r.URL.Host == host && strings.Contains(body, `"eth_call"`) && !strings.Contains(body, callTo) && strings.Contains(body, gas)
					}).
					Reply(200).JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": fmt.Sprintf("0x%064x", left)})
			}
			countCall := ethCallTo(host, calls[id])
			if id == high {
				gock.New("http://" + host).Post("").Persist().
					Filter(func(r *http.Request) bool {
						return strings.Contains(util.SafeReadBody(r), refusedGas) && countCall(r)
					}).
					Reply(200).Delay(200 * time.Millisecond).JSON(gasCapReply)
			}
			reply := gock.New("http://" + host).Post("").Persist().
				Filter(func(r *http.Request) bool {
					body := util.SafeReadBody(r)
					if id == high && strings.Contains(body, refusedGas) || !countCall(r) {
						return false
					}
					if strings.Contains(body, callTo) {
						lastCall[id].Store(body)
					}
					return true
				}).
				Reply(200)
			if id == high {
				reply.Delay(200 * time.Millisecond)
			}
			reply.JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": answers[id]})
		}

		// A read of the cap starts its probe.
		require.Eventually(t, func() bool {
			return ups[0].EvmStatePoller().CallGasCap() > 0 && ups[1].EvmStatePoller().CallGasCap() > 0
		}, 5*time.Second, 20*time.Millisecond)

		forward := func(gas string) string {
			body := strings.Replace(string(requestBytes), `"gas":"0x8f0d1801"`, gas, 1)
			resp, err := network.Forward(ctx, common.NewNormalizedRequest([]byte(body)))
			require.NoError(t, err)
			jrr, err := resp.JsonRpcResponse()
			require.NoError(t, err)
			return strings.Trim(string(jrr.GetResultBytes()), `"`)
		}
		// The low upstream's config fixes its cap at 40M, in place of the
		// 50M measured. Two upstreams give 40M, the default quorum, so the
		// network's cap is 40M: 100M gas is lowered to it and runs where the
		// policy orders it, as on any node.
		ups[0].Config().Evm.CallGasCap = 40_000_000
		assert.Equal(t, answers[low], forward(`"gas":"0x5f5e100"`))
		assert.Contains(t, lastCall[low].Load(), `"gas":"0x2625a00"`)

		// A quorum of 1 takes the largest cap: 100M gas runs whole, on the
		// one upstream that gives it.
		network.cfg.Evm.CallGasCapQuorum = 1
		assert.Equal(t, answers[high], forward(`"gas":"0x5f5e100"`))
		assert.Contains(t, lastCall[high].Load(), `"gas":"0x5f5e100"`)

		// A network cap above every upstream's promises what only some give.
		network.cfg.Evm.CallGasCap = 3_000_000_000
		// 30M gas: both caps give it, so the policy's order stands.
		assert.Equal(t, answers[low], forward(`"gas":"0x1c9c380"`))
		// 100M gas: only the 600M cap gives it.
		assert.Equal(t, answers[high], forward(`"gas":"0x5f5e100"`))
		// 2.4B gas: neither cap gives it, so the larger cap goes first. Its
		// refusal ends its attempt, so the 40M cap runs the call.
		assert.Equal(t, answers[low], forward(refusedGas))
		assert.Equal(t, int32(3), calls[high].Load(), "the 600M upstream runs the 100M call twice and goes first for 2.4B")
		assert.Equal(t, int32(3), calls[low].Load(), "the 40M upstream runs the lowered call, the 30M call and the refused 2.4B call")
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
