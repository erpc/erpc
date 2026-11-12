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
// the hedge race: no block can reach an upstream before the hedge would
// fire, so the first sweep's null is the answer, at once, one call per
// vendor. Asked just before a block reaches the vendors, the Head is not
// settled and the hedge finds the receipt the block brought; a vendor
// still in flight, or one that failed transiently, when the hedge leg's
// null comes keeps the race, and its receipt is the answer; on a lane with
// a vendor that answers receipts before the block, the head never settles
// and the hedge finds its receipt (spec §6.4 settled head). Full HTTP
// server, the real chain tracker and block clock on a running simulated
// chain whose vendors serve each block 500 ms after its mint.
func TestHedge_NullReceiptEndsRaceOnSettledHead(t *testing.T) {
	const (
		bt         = 2 * time.Second
		headDelay  = 500 * time.Millisecond
		hedgeDelay = 300 * time.Millisecond
	)
	node := newSettledNode(t, bt, hedgeDelay, map[string]chainsim.Faults{
		"http-a": {HeadDelay: headDelay}, "http-b": {HeadDelay: headDelay}, "http-c": {HeadDelay: headDelay},
	})
	chain, vendors, nw := node.chain, node.vendors, node.nw
	receipt, receiptCalls := node.receipt, node.receiptCalls

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

	t.Run("an upstream in flight keeps the race on a settled head", func(t *testing.T) {
		// An old block's receipt: http-a and http-b pruned it and answer null
		// at once; http-c holds it and answers 300 ms after the hedge fires.
		var b *chainsim.Block
		for n := chain.Tip().Number - 10; b == nil; n-- {
			if c := chain.CanonAt(n); c != nil && c.TxCount() > 0 {
				b = c
			}
		}
		for _, v := range vendors[:2] {
			v.Update(func(f *chainsim.Faults) { f.Pruned = 5 })
		}
		vendors[2].Update(func(f *chainsim.Faults) { f.SlowTxReceipts = hedgeDelay + 300*time.Millisecond })
		t.Cleanup(func() {
			for _, v := range vendors {
				v.Update(func(f *chainsim.Faults) { f.Pruned, f.SlowTxReceipts = 0, 0 })
			}
		})
		// Leg 1's sweep reserves http-c whatever the order, so the hedge leg
		// sweeps only the pruned two and ends null while http-c is in flight.
		// The Head must stay settled until then: the hedge fires a hedge
		// delay in, and its null must find the next block more than one away.
		require.Eventually(t, func() bool {
			h, now := nw.EvmLaneHead("", ""), time.Now()
			return h.Settled(now, hedgeDelay+300*time.Millisecond) && h.NextAt.Sub(now) > 2*hedgeDelay+300*time.Millisecond
		}, 4*bt, 10*time.Millisecond, "the head settles early in the block")
		res, took := receipt(b.TxHash(0).Hex())
		var got struct {
			TransactionHash string `json:"transactionHash"`
		}
		require.NoError(t, json.Unmarshal(res, &got), "result: %s", res)
		assert.Equal(t, b.TxHash(0).Hex(), got.TransactionHash, "result: %s", res)
		assert.GreaterOrEqual(t, took, hedgeDelay+300*time.Millisecond, "http-c answered it, after its delay")
	})

	t.Run("an upstream that failed transiently keeps the race on a settled head", func(t *testing.T) {
		// An old block's receipt: http-a and http-b pruned it and answer null;
		// http-c holds it, answers -32603 in the first sweep and recovers
		// 100 ms in, before the hedge fires and asks it again.
		var b *chainsim.Block
		for n := chain.Tip().Number - 10; b == nil; n-- {
			if c := chain.CanonAt(n); c != nil && c.TxCount() > 0 {
				b = c
			}
		}
		for _, v := range vendors[:2] {
			v.Update(func(f *chainsim.Faults) { f.Pruned = 5 })
		}
		vendors[2].Update(func(f *chainsim.Faults) { f.TxReceiptsDown = true })
		t.Cleanup(func() {
			for _, v := range vendors {
				v.Update(func(f *chainsim.Faults) { f.Pruned, f.TxReceiptsDown = 0, false })
			}
		})
		require.Eventually(t, func() bool {
			return nw.EvmLaneHead("", "").Settled(time.Now(), hedgeDelay+300*time.Millisecond)
		}, 4*bt, 10*time.Millisecond, "the head settles mid-block")
		heal := time.AfterFunc(100*time.Millisecond, func() {
			vendors[2].Update(func(f *chainsim.Faults) { f.TxReceiptsDown = false })
		})
		defer heal.Stop()
		before := vendors[2].MethodCalls("eth_getTransactionReceipt")
		res, _ := receipt(b.TxHash(0).Hex())
		var got struct {
			TransactionHash string `json:"transactionHash"`
		}
		require.NoError(t, json.Unmarshal(res, &got), "result: %s", res)
		assert.Equal(t, b.TxHash(0).Hex(), got.TransactionHash, "result: %s", res)
		assert.GreaterOrEqual(t, vendors[2].MethodCalls("eth_getTransactionReceipt")-before, int64(2), "the hedge asked http-c again")
	})

	t.Run("a preconfirming upstream keeps the hedge", func(t *testing.T) {
		// Every vendor serves each block 500 ms after mint; http-p answers
		// its receipts at mint, as a flashblocks node answers a preconfirmed
		// receipt, so no head, poll or by-number fetch sights the block
		// sooner than the others serve it.
		const pDelay = 200 * time.Millisecond
		p := newSettledNode(t, bt, pDelay, map[string]chainsim.Faults{
			"http-a": {HeadDelay: headDelay}, "http-b": {HeadDelay: headDelay}, "http-c": {HeadDelay: headDelay},
			"http-p": {HeadDelay: headDelay, ReceiptsLead: headDelay},
		})
		pv := p.vendors[slices.IndexFunc(p.vendors, func(v *chainsim.Vendor) bool { return v.ID == "http-p" })]
		txOf := func() *chainsim.Block {
			p.chain.Plan(chainsim.KindQuiet)
			p.chain.WaitTicks(1)
			b := p.chain.Tip()
			require.Positive(t, b.TxCount(), "block %d holds a transaction", b.Number)
			return b
		}
		// Warm-up: each new block's receipt, asked at its mint, comes from
		// http-p above Latest: the clock's proof, and its lead.
		for range 4 {
			b := txOf()
			res, _ := p.receipt(b.TxHash(0).Hex())
			require.Contains(t, string(res), b.TxHash(0).Hex(), "http-p answers block %d's receipt at its mint", b.Number)
		}
		// The next block's receipts reach http-p 150 ms after mint: asked at
		// mint, the first sweep gets null everywhere on a Head that stood
		// 1.5 s, and a next block predicted about 450 ms away.
		pv.Update(func(f *chainsim.Faults) { f.ReceiptsLead = headDelay - 150*time.Millisecond })
		b := txOf()
		h, now := p.nw.EvmLaneHead("", ""), time.Now()
		require.True(t, now.Before(b.MintedAt.Add(100*time.Millisecond)), "asked at block %d's mint", b.Number)
		require.GreaterOrEqual(t, now.Sub(h.LatestAt), pDelay, "the view stood a hedge delay")
		t.Logf("NextAt %v after now", h.NextAt.Sub(now))
		res, _ := p.receipt(b.TxHash(0).Hex())
		var got struct {
			TransactionHash string `json:"transactionHash"`
		}
		require.NoError(t, json.Unmarshal(res, &got), "result: %s", res)
		assert.Equal(t, b.TxHash(0).Hex(), got.TransactionHash, "the hedge found http-p's receipt; result: %s", res)
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
	// The clock's fit warms over regular blocks.
	chain.WaitTicks(8)
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
