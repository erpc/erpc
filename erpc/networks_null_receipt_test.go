package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const nullReceiptTxHash = "0xe39263b2b8ccf8f18f344e860bb8eb62c68f6ee0de8c877131bc469e31af837e"

// mockCountedMethod answers one method on one host with a fixed JSON-RPC result.
// Calls are counted by countCalls (a Filter can run more than once per request).
func mockCountedMethod(host, method, resultJSON string) {
	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), `"`+method+`"`)
		}).
		Reply(200).
		JSON([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":%s}`, resultJSON)))
}

// countCalls counts the requests gock served, per "host method".
func countCalls(methods ...string) func(host, method string) int64 {
	var mu sync.Mutex
	n := map[string]int64{}
	gock.Observe(func(r *http.Request, _ gock.Mock) {
		if r == nil || r.URL == nil {
			return
		}
		body := util.SafeReadBody(r)
		for _, m := range methods {
			if strings.Contains(body, `"`+m+`"`) {
				mu.Lock()
				n[r.URL.Host+" "+m]++
				mu.Unlock()
			}
		}
	})
	return func(host, method string) int64 {
		mu.Lock()
		defer mu.Unlock()
		return n[host+" "+method]
	}
}

// The primaries return a null receipt; whether the request reaches the
// fallbacks depends on what each primary says about the transaction itself.
func TestNetwork_NullReceiptOfMinedTx(t *testing.T) {
	const minedTx = `{"hash":"` + nullReceiptTxHash + `","blockNumber":"0x10","blockHash":"0xabc"}`
	const pendingTx = `{"hash":"` + nullReceiptTxHash + `","blockNumber":null,"blockHash":null}`
	const otherTx = `{"hash":"0x0000000000000000000000000000000000000000000000000000000000000001","blockNumber":"0x10"}`
	const fallbackReceipt = `{"transactionHash":"` + nullReceiptTxHash + `","blockNumber":"0x10","status":"0x1","logs":[]}`

	cases := []struct {
		name        string
		failover    bool
		primaryTx   string // the primaries' eth_getTransactionByHash result
		wantReceipt bool   // the client gets the fallback's receipt
		sideCalls   int64  // eth_getTransactionByHash calls to the primaries
	}{
		{name: "MinedEscalates", failover: true, primaryTx: minedTx, wantReceipt: true, sideCalls: 2},
		{name: "PendingKeepsNull", failover: true, primaryTx: pendingTx, sideCalls: 2},
		{name: "UnknownKeepsNull", failover: true, primaryTx: "null", sideCalls: 2},
		{name: "AnotherTxKeepsNull", failover: true, primaryTx: otherTx, sideCalls: 2},
		{name: "NoFailoverNoSideCall", failover: false, primaryTx: minedTx, sideCalls: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer util.ResetGock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			calls := countCalls("eth_getTransactionReceipt", "eth_getTransactionByHash")
			defer gock.Observe(nil)
			network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
				primaryLatest:  "0x3e8",
				fallbackLatest: "0x3e8",
				enableFailover: tc.failover,
				mocks: func() {
					for _, h := range []string{"rpc1.localhost", "rpc2.localhost"} {
						mockCountedMethod(h, "eth_getTransactionReceipt", "null")
						mockCountedMethod(h, "eth_getTransactionByHash", tc.primaryTx)
					}
					fallback := fallbackReceipt
					if !tc.failover {
						// without failover the fallbacks are ordinary upstreams
						fallback = "null"
					}
					for _, h := range []string{"rpc3.localhost", "rpc4.localhost"} {
						mockCountedMethod(h, "eth_getTransactionReceipt", fallback)
						mockCountedMethod(h, "eth_getTransactionByHash", "null")
					}
				},
				// as in production: fallbacks only through the escape, never probed
				configure: func(cfgs []*common.UpstreamConfig) {
					for _, c := range cfgs {
						if c.HasTag(common.TagTierFallback) {
							c.Routing = &common.UpstreamRoutingConfig{Probe: common.ProbeModeOff}
						}
					}
				},
			})

			req := common.NewNormalizedRequest([]byte(
				`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["` + nullReceiptTxHash + `"]}`))
			req.SetNetwork(network)
			resp, err := network.Forward(ctx, req)
			require.NoError(t, err, "a null receipt is an answer, never an error")
			require.NotNil(t, resp)
			defer resp.Release()
			jrr, err := resp.JsonRpcResponse()
			require.NoError(t, err)
			result := jrr.GetResultString()

			fallbackCalls := calls("rpc3.localhost", "eth_getTransactionReceipt") + calls("rpc4.localhost", "eth_getTransactionReceipt")
			sideCalls := calls("rpc1.localhost", "eth_getTransactionByHash") + calls("rpc2.localhost", "eth_getTransactionByHash")

			if tc.wantReceipt {
				assert.Contains(t, result, `"status":"0x1"`, "the fallback's receipt")
				assert.EqualValues(t, 1, fallbackCalls, "one fallback asked")
			} else {
				assert.Equal(t, "null", result)
				if tc.failover {
					assert.EqualValues(t, 0, fallbackCalls, "no fallback asked")
				}
			}
			assert.EqualValues(t, tc.sideCalls, sideCalls, "one eth_getTransactionByHash per primary that answered null")
		})
	}
}
