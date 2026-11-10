package erpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/test/chainsim"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// A null eth_getTransactionReceipt on a settled Head ends the hedge race:
// no block can reach an upstream before the hedge would fire, so the first
// sweep's null is the answer, at once, one call per vendor. Asked just
// before a block reaches the vendors, the Head is not settled and the
// hedge finds the receipt the block brought (spec §6.4 settled head). Full
// HTTP server, the real chain tracker and block clock on a running
// simulated chain whose three vendors serve each block 500 ms after its
// mint.
func TestHedge_NullReceiptEndsRaceOnSettledHead(t *testing.T) {
	const (
		bt         = 2 * time.Second
		headDelay  = 500 * time.Millisecond
		hedgeDelay = 300 * time.Millisecond
	)
	chain := chainsim.NewChain(chainsim.Config{ChainID: 123, BlockTime: bt, Seed: 13, History: 20})
	t.Cleanup(chain.Close)
	var vendors []*chainsim.Vendor
	var ups []*common.UpstreamConfig
	for _, id := range []string{"http-a", "http-b", "http-c"} {
		v, err := chain.AddVendor(id, "vendor-"+id, false)
		require.NoError(t, err)
		v.SetFaults(chainsim.Faults{HeadDelay: headDelay})
		vendors = append(vendors, v)
		ups = append(ups, sourceUpstream(v.ID, v.URL(), util.BoolPtr(true)))
	}
	cfg := sourcesConfig(func(nw *common.NetworkConfig) {
		nw.Failsafe = []*common.FailsafeConfig{{
			MatchMethod: "eth_getTransactionReceipt",
			Hedge:       &common.HedgePolicyConfig{Delay: common.NewStaticDuration(hedgeDelay), MaxCount: 1},
		}}
	}, ups...)
	cfg.Server.WaitBeforeShutdown = common.Duration(0).Ptr()
	srv, addr, cancel := startTestERPCServer(t, cfg)
	var nw *Network
	t.Cleanup(func() {
		_ = srv.Shutdown(&zerolog.Logger{})
		cancel()
		if nw != nil {
			nw.wait()
		}
	})
	require.Eventually(t, func() bool {
		n, err := srv.erpc.GetNetwork(context.Background(), "test_ws", "evm:123")
		if err != nil || n == nil || n.chainTracker == nil {
			return false
		}
		nw = n
		return true
	}, 15*time.Second, 20*time.Millisecond, "network initializes")

	post := func(body string) []byte {
		resp, err := http.Post(fmt.Sprintf("http://%s/test_ws/evm/123", addr), "application/json", bytes.NewReader([]byte(body)))
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
		return raw
	}
	receipt := func(tx string) (json.RawMessage, time.Duration) {
		began := time.Now()
		raw := post(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":[%q]}`, tx))
		took := time.Since(began)
		var r struct {
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		require.NoError(t, json.Unmarshal(raw, &r), "body: %s", raw)
		require.Empty(t, r.Error, "body: %s", raw)
		return r.Result, took
	}
	receiptCalls := func() []int64 {
		out := make([]int64, len(vendors))
		for i, v := range vendors {
			out[i] = v.MethodCalls("eth_getTransactionReceipt")
		}
		return out
	}

	// Reads of latest keep the block clock running (its demand, 2 W).
	reads, stopReads := sync.WaitGroup{}, make(chan struct{})
	reads.Add(1)
	go func() {
		defer reads.Done()
		for {
			select {
			case <-stopReads:
				return
			case <-time.After(200 * time.Millisecond):
			}
			resp, err := http.Post(fmt.Sprintf("http://%s/test_ws/evm/123", addr), "application/json",
				bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`)))
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}
	}()
	t.Cleanup(func() { close(stopReads); reads.Wait() })
	chain.Start()
	// The clock's fit warms over regular blocks.
	chain.WaitTicks(8)

	t.Run("settled head answers a null receipt from the first sweep", func(t *testing.T) {
		// Mid-block: the view stood a hedge delay ago and the next block is
		// more than a hedge delay away, with room for the request's trip.
		require.Eventually(t, func() bool {
			return nw.EvmLaneHead("", "").Settled(time.Now(), hedgeDelay+300*time.Millisecond)
		}, 4*bt, 10*time.Millisecond, "the head settles mid-block")
		before := receiptCalls()
		res, took := receipt("0x" + fmt.Sprintf("%064x", 0xdead))
		after := receiptCalls()
		assert.Equal(t, "null", string(res))
		assert.Less(t, took, hedgeDelay, "the null is answered before the hedge would fire")
		for i := range vendors {
			assert.EqualValues(t, 1, after[i]-before[i], "vendor %s is asked once", vendors[i].ID)
		}
	})

	t.Run("due block lets the hedge find the receipt", func(t *testing.T) {
		chain.Plan(chainsim.KindQuiet)
		chain.WaitTicks(1)
		b := chain.Tip()
		require.Positive(t, b.TxCount(), "the block holds a transaction")
		serves := vendors[0].ServesAt(b, "eth_getTransactionReceipt")
		// Asked 150 ms before the vendors serve the block: the first sweep
		// gets null everywhere, the hedge fires 150 ms after they do.
		time.Sleep(time.Until(serves.Add(-150 * time.Millisecond)))
		require.True(t, time.Now().Before(serves.Add(-100*time.Millisecond)), "asked before the vendors serve block %d", b.Number)
		before := receiptCalls()
		res, _ := receipt(b.TxHash(0).Hex())
		after := receiptCalls()
		var got struct {
			TransactionHash string `json:"transactionHash"`
			BlockNumber     string `json:"blockNumber"`
		}
		require.NoError(t, json.Unmarshal(res, &got), "result: %s", res)
		assert.Equal(t, b.TxHash(0).Hex(), got.TransactionHash, "result: %s", res)
		assert.Equal(t, fmt.Sprintf("0x%x", b.Number), got.BlockNumber, "result: %s", res)
		var calls int64
		for i := range vendors {
			calls += after[i] - before[i]
		}
		assert.Greater(t, calls, int64(len(vendors)), "the hedge asked again after the first sweep's nulls")
	})
}
