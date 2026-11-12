package erpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
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

// A null eth_getTransactionReceipt from every vendor on a settled Head ends
// the hedge race: the Head stood a hedge delay, and the next block can come
// no sooner than half a block, nor sooner than the view's shortest recent
// step, after it; so the first sweep's null is the answer, at once, one
// call per vendor. Late in the slot the Head is not settled and the hedge
// finds the receipt a block brought; a vendor still in flight, or one that
// failed transiently, keeps the race and its receipt is the answer; a
// receipt above Latest already seen (a preconfirmation) keeps the hedge;
// and a 2 s chain never settles at a 500 ms hedge (spec §6.4 settled
// head). Full HTTP server and the real chain tracker on running simulated
// chains.
func TestHedge_NullReceiptEndsRaceOnSettledHead(t *testing.T) {
	const (
		// 4 s blocks that every vendor serves 3 s after mint: the view's
		// Head stands from 1 s before a block's mint to 3 s after it, and
		// is settled for 300 ms from 0.3 s to 1.7 s into that time.
		bt         = 4 * time.Second
		headDelay  = 3 * time.Second
		hedgeDelay = 300 * time.Millisecond
	)
	node := newSettledNode(t, bt, hedgeDelay, map[string]chainsim.Faults{
		"http-a": {HeadDelay: headDelay}, "http-b": {HeadDelay: headDelay}, "http-c": {HeadDelay: headDelay}, "http-p": {HeadDelay: headDelay},
	})
	chain, vendors, nw := node.chain, node.vendors, node.nw
	receipt, receiptCalls := node.receipt, node.receiptCalls
	vendor := func(id string) *chainsim.Vendor {
		return vendors[slices.IndexFunc(vendors, func(v *chainsim.Vendor) bool { return v.ID == id })]
	}
	step, steps := nw.chainTracker.MinStep()
	t.Logf("view step S %v over %d steps; block time %v", step, steps, nw.EvmBlockTime())
	settled := func(d time.Duration) func() bool {
		return func() bool { return nw.EvmLaneHead("", "").Settled(time.Now(), d) }
	}
	oldReceipt := func() *chainsim.Block {
		for n := chain.Tip().Number - 10; ; n-- {
			if c := chain.CanonAt(n); c != nil && c.TxCount() > 0 {
				return c
			}
		}
	}
	// pruneBut prunes every vendor but holder, so only it holds an old
	// block's receipt.
	pruneBut := func(t *testing.T, holder *chainsim.Vendor) {
		for _, v := range vendors {
			if v != holder {
				v.Update(func(f *chainsim.Faults) { f.Pruned = 5 })
			}
		}
		t.Cleanup(func() {
			for _, v := range vendors {
				v.Update(func(f *chainsim.Faults) { f.Pruned, f.SlowTxReceipts, f.TxReceiptsDown = 0, 0, false })
			}
		})
	}
	unknown := "0x" + fmt.Sprintf("%064x", 0xdead)

	t.Run("settled head answers a null receipt from the first sweep", func(t *testing.T) {
		require.Eventually(t, settled(hedgeDelay+300*time.Millisecond), 2*bt, 10*time.Millisecond, "the head settles")
		before := receiptCalls()
		res, took := receipt(unknown)
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
		// Asked 150 ms before the vendors serve the block, late in the
		// Head's slot: the first sweep gets null everywhere, the hedge fires
		// 150 ms after they serve it.
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

	t.Run("an upstream in flight keeps the race on a settled head", func(t *testing.T) {
		// An old block's receipt: every vendor but http-c pruned it and
		// answers null at once; http-c holds it and answers 300 ms after the
		// hedge fires. Leg 1's sweep reserves http-c whatever the order, so
		// the hedge leg sweeps only the pruned and ends null while http-c is
		// in flight; the Head must stay settled until then.
		b, holder := oldReceipt(), vendor("http-c")
		pruneBut(t, holder)
		holder.Update(func(f *chainsim.Faults) { f.SlowTxReceipts = hedgeDelay + 300*time.Millisecond })
		require.Eventually(t, settled(2*hedgeDelay+300*time.Millisecond), 2*bt, 10*time.Millisecond, "the head settles")
		res, took := receipt(b.TxHash(0).Hex())
		var got struct {
			TransactionHash string `json:"transactionHash"`
		}
		require.NoError(t, json.Unmarshal(res, &got), "result: %s", res)
		assert.Equal(t, b.TxHash(0).Hex(), got.TransactionHash, "result: %s", res)
		assert.GreaterOrEqual(t, took, hedgeDelay+300*time.Millisecond, "http-c answered it, after its delay")
	})

	t.Run("an upstream that failed transiently keeps the race on a settled head", func(t *testing.T) {
		// An old block's receipt: every vendor but http-c pruned it; http-c
		// holds it, answers -32603 in the first sweep and recovers 100 ms
		// in, before the hedge fires and asks it again.
		b, holder := oldReceipt(), vendor("http-c")
		pruneBut(t, holder)
		holder.Update(func(f *chainsim.Faults) { f.TxReceiptsDown = true })
		require.Eventually(t, settled(hedgeDelay+300*time.Millisecond), 2*bt, 10*time.Millisecond, "the head settles")
		heal := time.AfterFunc(100*time.Millisecond, func() {
			holder.Update(func(f *chainsim.Faults) { f.TxReceiptsDown = false })
		})
		defer heal.Stop()
		before := holder.MethodCalls("eth_getTransactionReceipt")
		res, _ := receipt(b.TxHash(0).Hex())
		var got struct {
			TransactionHash string `json:"transactionHash"`
		}
		require.NoError(t, json.Unmarshal(res, &got), "result: %s", res)
		assert.Equal(t, b.TxHash(0).Hex(), got.TransactionHash, "result: %s", res)
		assert.GreaterOrEqual(t, holder.MethodCalls("eth_getTransactionReceipt")-before, int64(2), "the hedge asked http-c again")
	})

	t.Run("a receipt seen above Latest keeps the hedge", func(t *testing.T) {
		// http-p answers each block's receipts at its mint, as a flashblocks
		// node answers a preconfirmed receipt, 3 s before any vendor serves
		// the block: at mint the Head is the block before, settled by time.
		p := vendor("http-p")
		p.Update(func(f *chainsim.Faults) { f.ReceiptsLead = headDelay })
		t.Cleanup(func() { p.Update(func(f *chainsim.Faults) { f.ReceiptsLead = 0 }) })
		chain.Plan(chainsim.KindQuiet)
		chain.WaitTicks(1)
		b := chain.Tip()
		require.Positive(t, b.TxCount(), "the block holds a transaction")
		res, _ := receipt(b.TxHash(0).Hex())
		require.Contains(t, string(res), b.TxHash(0).Hex(), "http-p answers block %d's receipt at its mint", b.Number)
		h := nw.EvmLaneHead("", "")
		require.Less(t, h.Latest, int64(b.Number), "block %d is above Latest", b.Number)
		require.True(t, h.Settled(time.Now(), hedgeDelay+100*time.Millisecond), "the Head is settled by time alone")
		before := receiptCalls()
		res, took := receipt(unknown)
		after := receiptCalls()
		assert.Equal(t, "null", string(res))
		assert.GreaterOrEqual(t, took, hedgeDelay, "the hedge fired")
		for i := range vendors {
			assert.EqualValues(t, 2, after[i]-before[i], "vendor %s is asked by both legs", vendors[i].ID)
		}
	})

	t.Run("a 2 s chain never settles at a 500 ms hedge", func(t *testing.T) {
		const fastDelay = 500 * time.Millisecond
		fast := newSettledNode(t, 2*time.Second, fastDelay, map[string]chainsim.Faults{
			"http-a": {HeadDelay: 500 * time.Millisecond}, "http-b": {HeadDelay: 500 * time.Millisecond}, "http-c": {HeadDelay: 500 * time.Millisecond},
		})
		step, steps := fast.nw.chainTracker.MinStep()
		t.Logf("view step S %v over %d steps; block time %v", step, steps, fast.nw.EvmBlockTime())
		// Mid-slot: the Head stood 1 s.
		require.Eventually(t, func() bool {
			h := fast.nw.EvmLaneHead("", "")
			return !h.LatestAt.IsZero() && time.Since(h.LatestAt) >= time.Second
		}, 4*time.Second, 10*time.Millisecond, "mid-slot")
		before := fast.receiptCalls()
		res, took := fast.receipt(unknown)
		after := fast.receiptCalls()
		assert.Equal(t, "null", string(res))
		assert.GreaterOrEqual(t, took, fastDelay, "the hedge fired")
		for i := range fast.vendors {
			assert.EqualValues(t, 2, after[i]-before[i], "vendor %s is asked by both legs", fast.vendors[i].ID)
		}
	})
}

// settledNode is one eRPC instance on a running simulated chain whose HTTP
// vendors have the given faults, a hedge of delay on receipts, and reads
// of latest that keep its block clock running; its clock's fit is warm.
type settledNode struct {
	t       *testing.T
	chain   *chainsim.Chain
	vendors []*chainsim.Vendor
	nw      *Network
	addr    string
}

func newSettledNode(t *testing.T, bt, hedgeDelay time.Duration, faults map[string]chainsim.Faults) *settledNode {
	chain := chainsim.NewChain(chainsim.Config{ChainID: 123, BlockTime: bt, Seed: 13, History: 20})
	t.Cleanup(chain.Close)
	ids := slices.Sorted(maps.Keys(faults))
	n := &settledNode{t: t, chain: chain}
	var ups []*common.UpstreamConfig
	for _, id := range ids {
		v, err := chain.AddVendor(id, "vendor-"+id, false)
		require.NoError(t, err)
		v.SetFaults(faults[id])
		n.vendors = append(n.vendors, v)
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
	n.addr = addr
	t.Cleanup(func() {
		_ = srv.Shutdown(&zerolog.Logger{})
		cancel()
		if n.nw != nil {
			n.nw.wait()
		}
	})
	require.Eventually(t, func() bool {
		nw, err := srv.erpc.GetNetwork(context.Background(), "test_ws", "evm:123")
		if err != nil || nw == nil || nw.chainTracker == nil {
			return false
		}
		n.nw = nw
		return true
	}, 15*time.Second, 20*time.Millisecond, "network initializes")

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
	// The view's step record and the block time warm over regular blocks.
	require.Eventually(t, func() bool {
		_, steps := n.nw.chainTracker.MinStep()
		return steps >= 9 && n.nw.EvmBlockTime() > 0
	}, 16*bt, 50*time.Millisecond, "nine view steps")
	return n
}

// receipt asks eth_getTransactionReceipt of tx over HTTP: its result and
// how long the answer took.
func (n *settledNode) receipt(tx string) (json.RawMessage, time.Duration) {
	began := time.Now()
	resp, err := http.Post(fmt.Sprintf("http://%s/test_ws/evm/123", n.addr), "application/json",
		bytes.NewReader([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":[%q]}`, tx))))
	require.NoError(n.t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(n.t, err)
	took := time.Since(began)
	require.Equal(n.t, http.StatusOK, resp.StatusCode, "body: %s", raw)
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	require.NoError(n.t, json.Unmarshal(raw, &r), "body: %s", raw)
	require.Empty(n.t, r.Error, "body: %s", raw)
	return r.Result, took
}

// receiptCalls are the eth_getTransactionReceipt calls each vendor got.
func (n *settledNode) receiptCalls() []int64 {
	out := make([]int64, len(n.vendors))
	for i, v := range n.vendors {
		out[i] = v.MethodCalls("eth_getTransactionReceipt")
	}
	return out
}
