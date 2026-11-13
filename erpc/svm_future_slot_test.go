package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// svmProdShapedNetworkYaml is the failsafe shape of a production Solana
// network: hedge maxCount 1 and retry maxAttempts 3 for every method, with the
// delays scaled down so a test round stays short. statePollerDebounce 2s makes
// the getBlock staleness margin 2 + 2s/400ms = 7 slots.
const svmProdShapedNetworkYaml = `
architecture: svm
svm:
  cluster: mainnet-beta
  statePollerDebounce: 2s
failsafe:
- matchMethod: '*'
  timeout:
    duration: 30s
  hedge:
    maxCount: 1
    delay:
      quantile: 0.95
      min: 50ms
      max: 1s
  retry:
    maxAttempts: 3
    delay: 50ms
    backoffMaxDelay: 1s
    backoffFactor: 2
`

// svmWireError is the JSON-RPC error object a client receives for err.
func svmWireError(t *testing.T, req *common.NormalizedRequest, err error) map[string]interface{} {
	t.Helper()
	raw, mErr := json.Marshal(buildErrorResponseBody(req, err, err, nil))
	require.NoError(t, mErr)
	var body struct {
		Error map[string]interface{} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &body))
	require.NotNilf(t, body.Error, "expected a json-rpc error body, got %s", raw)
	return body.Error
}

// TestSvm_ReadBeyondPoolTip_AnsweredLocally covers reads that name a slot far
// beyond the highest slot any upstream of the pool knows: getBlockTime's slot
// and a minContextSlot option. No upstream can answer them, so every attempt,
// failover, retry and hedge is a wasted round-trip ending in the same agave
// error. Up to 128 slots above the tracked tip (a read-your-writes slot from a
// fresher node, which the pool often reaches within the retry waits) and with a
// cold poller, they forward. The getBlock guard's staleness margin (7 slots
// here) is not the bound: tip+10 is past it and still forwards.
func TestSvm_ReadBeyondPoolTip_AnsweredLocally(t *testing.T) {
	const (
		latest    = int64(300000)
		finalized = int64(299968)
		// Unpinned getBalance evaluates at finalized: tip = min(finalized, indexed).
		finalizedTip = finalized
		// getBlockTime serves any frozen bank, so its tip is the indexed frontier.
		indexedTip = latest
		far        = int64(100000)
	)

	minContextSlotNotReached := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"error":{"code":-32016,"message":"Minimum context slot has not been reached","data":{"contextSlot":%d}}}`, finalized)
	blockNotAvailable := func(slot int64) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"error":{"code":-32004,"message":"Block not available for slot %d"}}`, slot)
	}

	for _, tc := range []struct {
		name     string
		method   string
		params   string
		shred    int64 // 0 = the pool has no indexed frontier (cold poller)
		reply    string
		local    bool
		wantCode float64
		wantMsg  string
		wantData map[string]interface{}
	}{
		{
			name:     "minContextSlot far beyond the finalized tip",
			method:   "getBalance",
			params:   fmt.Sprintf(`["11111111111111111111111111111111",{"minContextSlot":%d}]`, finalizedTip+far),
			shred:    latest,
			reply:    minContextSlotNotReached,
			local:    true,
			wantCode: -32016,
			wantMsg:  "Minimum context slot has not been reached",
			wantData: map[string]interface{}{"contextSlot": float64(finalizedTip)},
		},
		{
			name:     "getBlockTime far beyond the indexed tip",
			method:   "getBlockTime",
			params:   fmt.Sprintf(`[%d]`, indexedTip+far),
			shred:    latest,
			reply:    blockNotAvailable(indexedTip + far),
			local:    true,
			wantCode: -32004,
			wantMsg:  fmt.Sprintf("Block not available for slot %d", indexedTip+far),
		},
		{
			name:   "minContextSlot past the getBlock margin forwards",
			method: "getBalance",
			params: fmt.Sprintf(`["11111111111111111111111111111111",{"minContextSlot":%d}]`, finalizedTip+10),
			shred:  latest,
			reply:  fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":%d},"value":42}}`, finalizedTip+10),
		},
		{
			name:   "getBlockTime past the getBlock margin forwards",
			method: "getBlockTime",
			params: fmt.Sprintf(`[%d]`, indexedTip+10),
			shred:  latest,
			reply:  `{"jsonrpc":"2.0","id":1,"result":1700000000}`,
		},
		{
			name:   "minContextSlot at tip plus 128 forwards",
			method: "getBalance",
			params: fmt.Sprintf(`["11111111111111111111111111111111",{"minContextSlot":%d}]`, finalizedTip+128),
			shred:  latest,
			reply:  fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":%d},"value":42}}`, finalizedTip+128),
		},
		{
			name:   "getBlockTime at tip plus 128 forwards",
			method: "getBlockTime",
			params: fmt.Sprintf(`[%d]`, indexedTip+128),
			shred:  latest,
			reply:  `{"jsonrpc":"2.0","id":1,"result":1700000000}`,
		},
		{
			name:     "minContextSlot at tip plus 129 answered locally",
			method:   "getBalance",
			params:   fmt.Sprintf(`["11111111111111111111111111111111",{"minContextSlot":%d}]`, finalizedTip+129),
			shred:    latest,
			reply:    minContextSlotNotReached,
			local:    true,
			wantCode: -32016,
			wantMsg:  "Minimum context slot has not been reached",
			wantData: map[string]interface{}{"contextSlot": float64(finalizedTip)},
		},
		{
			name:   "minContextSlot with a cold poller forwards",
			method: "getBalance",
			params: fmt.Sprintf(`["11111111111111111111111111111111",{"minContextSlot":%d}]`, finalizedTip+far),
			reply:  minContextSlotNotReached,
		},
		{
			name:   "getBlockTime with a cold poller forwards",
			method: "getBlockTime",
			params: fmt.Sprintf(`[%d]`, indexedTip+far),
			reply:  blockNotAvailable(indexedTip + far),
		},
		{
			name:     "getBlock far beyond the indexed tip answers agave's -32004",
			method:   "getBlock",
			params:   fmt.Sprintf(`[%d,{"maxSupportedTransactionVersion":0,"transactionDetails":"none","rewards":false}]`, indexedTip+far),
			shred:    latest,
			reply:    blockNotAvailable(indexedTip + far),
			local:    true,
			wantCode: -32004,
			wantMsg:  fmt.Sprintf("Block not available for slot %d", indexedTip+far),
		},
		{
			name:   "getBlock at the tip forwards",
			method: "getBlock",
			params: fmt.Sprintf(`[%d,{"maxSupportedTransactionVersion":0}]`, finalizedTip),
			shred:  latest,
			reply:  `{"jsonrpc":"2.0","id":1,"result":{"blockhash":"x"}}`,
		},
		{
			name:     "getBlockTime at u64 max names the slot the client sent",
			method:   "getBlockTime",
			params:   `[18446744073709551615]`,
			shred:    latest,
			reply:    `{"jsonrpc":"2.0","id":1,"error":{"code":-32004,"message":"Block not available for slot 18446744073709551615"}}`,
			local:    true,
			wantCode: -32004,
			wantMsg:  "Block not available for slot 18446744073709551615",
		},
		// agave decodes slot and minContextSlot params as serde u64 and refuses
		// anything else with -32602 before the method runs; every upstream would.
		{
			name:     "minContextSlot as a string",
			method:   "getBalance",
			params:   `["11111111111111111111111111111111",{"minContextSlot":"999999999999"}]`,
			shred:    latest,
			reply:    minContextSlotNotReached,
			local:    true,
			wantCode: -32602,
			wantMsg:  `Invalid params: invalid type: string "999999999999", expected u64.`,
		},
		{
			name:     "minContextSlot as a float",
			method:   "getBalance",
			params:   `["11111111111111111111111111111111",{"minContextSlot":1.5}]`,
			shred:    latest,
			reply:    minContextSlotNotReached,
			local:    true,
			wantCode: -32602,
			wantMsg:  "Invalid params: invalid type: floating point `1.5`, expected u64.",
		},
		{
			name:     "minContextSlot in exponent form",
			method:   "getBalance",
			params:   `["11111111111111111111111111111111",{"minContextSlot":1e12}]`,
			shred:    latest,
			reply:    minContextSlotNotReached,
			local:    true,
			wantCode: -32602,
			wantMsg:  "Invalid params: invalid type: floating point `1000000000000.0`, expected u64.",
		},
		{
			name:     "minContextSlot above u64 max",
			method:   "getBalance",
			params:   `["11111111111111111111111111111111",{"minContextSlot":18446744073709551616}]`,
			shred:    latest,
			reply:    minContextSlotNotReached,
			local:    true,
			wantCode: -32602,
			wantMsg:  "Invalid params: invalid type: floating point `1.8446744073709552e+19`, expected u64.",
		},
		{
			name:   "minContextSlot null is absent and forwards",
			method: "getBalance",
			params: `["11111111111111111111111111111111",{"minContextSlot":null}]`,
			shred:  latest,
			reply:  fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":%d},"value":42}}`, finalizedTip),
		},
		{
			name:     "getBlockTime slot as a float",
			method:   "getBlockTime",
			params:   `[470000000.0]`,
			shred:    latest,
			reply:    blockNotAvailable(470000000),
			local:    true,
			wantCode: -32602,
			wantMsg:  "Invalid params: invalid type: floating point `470000000.0`, expected u64.",
		},
		{
			name:     "getBlockTime slot in exponent form",
			method:   "getBlockTime",
			params:   `[1e12]`,
			shred:    latest,
			reply:    blockNotAvailable(1000000000000),
			local:    true,
			wantCode: -32602,
			wantMsg:  "Invalid params: invalid type: floating point `1000000000000.0`, expected u64.",
		},
		{
			name:     "getBlockTime slot as a string",
			method:   "getBlockTime",
			params:   `["470000000"]`,
			shred:    latest,
			reply:    blockNotAvailable(470000000),
			local:    true,
			wantCode: -32602,
			wantMsg:  `Invalid params: invalid type: string "470000000", expected u64.`,
		},
		{
			name:     "getBlockTime negative slot",
			method:   "getBlockTime",
			params:   `[-1]`,
			shred:    latest,
			reply:    blockNotAvailable(0),
			local:    true,
			wantCode: -32602,
			wantMsg:  "Invalid params: invalid value: integer `-1`, expected u64.",
		},
		{
			name:     "getBlockTime slot above u64 max",
			method:   "getBlockTime",
			params:   `[18446744073709551616]`,
			shred:    latest,
			reply:    blockNotAvailable(0),
			local:    true,
			wantCode: -32602,
			wantMsg:  "Invalid params: invalid type: floating point `1.8446744073709552e+19`, expected u64.",
		},
		{
			name:     "getBlock slot as null",
			method:   "getBlock",
			params:   `[null]`,
			shred:    latest,
			reply:    blockNotAvailable(0),
			local:    true,
			wantCode: -32602,
			wantMsg:  "Invalid params: invalid type: null, expected u64.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			util.ResetGock()
			t.Cleanup(util.ResetGock)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var upstreams []*common.UpstreamConfig
			var calls []*atomic.Int64
			for i := 1; i <= 4; i++ {
				host := fmt.Sprintf("svm-beyondtip-rpc%d.localhost", i)
				util.SetupMocksForSvmStatePollerWithShred(host, latest, finalized, tc.shred)
				calls = append(calls, svmCountedMock(host, tc.method, 0, 200, tc.reply))
				upstreams = append(upstreams, svmUpstreamConfig(fmt.Sprintf("rpc%d", i), host))
			}
			netCfg := &common.NetworkConfig{}
			require.NoError(t, yaml.Unmarshal([]byte(svmProdShapedNetworkYaml), netCfg))
			require.NoError(t, netCfg.SetDefaults(upstreams, nil))
			net, _ := setupTestSvmNetworkFromConfig(t, ctx, netCfg, upstreams)
			require.Eventually(t, func() bool {
				return net.SvmHighestFinalizedSlotMax(ctx) == finalized && net.SvmHighestIndexedSlot(ctx) == tc.shred
			}, 5*time.Second, 20*time.Millisecond, "pollers never reported the mocked slots")

			req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
				`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, tc.method, tc.params)))
			start := time.Now()
			resp, err := svmProjectForward(ctx, net, req)
			elapsed := time.Since(start)
			if resp != nil {
				resp.Release()
			}

			var total int64
			for _, c := range calls {
				total += c.Load()
			}
			t.Logf("%s: %d upstream calls in %s", tc.method, total, elapsed)

			if !tc.local {
				require.Positive(t, total, "the read must reach an upstream, as before the guard")
				return
			}
			require.Zero(t, total, "no upstream can serve this slot; every call is wasted")
			require.Less(t, elapsed, 100*time.Millisecond, "a local answer must not wait out retry delays")
			require.Error(t, err)
			wire := svmWireError(t, req, err)
			require.Equal(t, tc.wantCode, wire["code"])
			require.Equal(t, tc.wantMsg, wire["message"])
			if tc.wantData != nil {
				require.Equal(t, tc.wantData, wire["data"])
			} else {
				require.Nil(t, wire["data"], "agave sends no data for this error")
			}
		})
	}
}
