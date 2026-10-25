package realworld

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/test/chainsim"
	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Bounds of the client-behaviour scenarios (§10, §12, §15; §19 D3–D7,
// D9).
const (
	// d3History: D3's chain starts this many blocks back, so its oldest
	// blocks are more than maxBackfill of chain time below the head: below
	// the window (§9). d3AboveBlocks: a cursor this far above the head,
	// which the head does not reach within d3Wait, the 60s stalled-Read
	// bound plus slack: an above-head cursor waits without end (§10.1;
	// §10.2: the wait counts toward 60s only while the head stands above
	// the cursor). d3Reply: an end or a header that answers at once comes
	// within it (harness).
	d3History     = int(maxBackfill/blockTime) + 300
	d3AboveBlocks = 400
	d3Wait        = grpcReadWait + reconnectSlack
	d3Reply       = 10 * time.Second

	// d4Subs logs subscriptions over d4PerConn per connection (the
	// default per-connection cap, §15.2) on one instance, d4Blocks long;
	// d4P99: delivery p99 within 2 bt (§19 D4). d4Settled: logs of a
	// block minted this long before the subscribers stop are all owed
	// (harness).
	d4Subs    = 1000
	d4PerConn = 100
	d4Blocks  = 50
	d4P99     = 2 * blockTime
	d4Settled = 2 * liveBound

	// d5Rate: subscribe+unsubscribe pairs per second on one instance, for
	// d5For (§19 D5); d5Conns: connections they spread over; d5Hold: a
	// subscription lives up to this long, so unsubscribes land during
	// sends (harness); d5Drain: erpc_taps reads 0 this soon after the
	// last unsubscribe answered (harness: a scrape and the gauge's update).
	d5Rate  = 1000
	d5For   = 20 * time.Second
	d5Conns = 10
	d5Hold  = 3 * blockTime
	d5Drain = reconnectSlack

	// d7Limit: D7's server.webSocket.maxSubscriptionsPerConnection
	// (§15.2: subscribe refused -32005 at the bound).
	d7Limit = 3

	// d9Silent: a pending-transaction source silent this long, while
	// blocks carry transactions, is resubscribed (§12; §15.1 mempool).
	d9Silent = 60 * time.Second
)

// streamEvent is one StreamBlocks message, or its end (h nil).
type streamEvent struct {
	h    *bdsevm.BlockHeader
	code codes.Code
	msg  string
}

// openStream opens StreamBlocks on pod with the cursor fields as given
// (nil, nil: none) and sends each header, then the end, on the returned
// channel, until ctx ends.
func (s *scenario) openStream(ctx context.Context, pod int, from *uint64, fromHash []byte) <-chan streamEvent {
	out := make(chan streamEvent, 1024)
	go func() {
		defer close(out)
		conn, err := grpc.NewClient(s.h.pods[pod].grpcAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			out <- streamEvent{code: codes.Unavailable, msg: err.Error()}
			return
		}
		defer conn.Close()
		sctx := metadata.NewOutgoingContext(ctx, metadata.Pairs("x-erpc-project", s.project, "x-erpc-chain-id", fmt.Sprint(s.chain.ID())))
		stream, err := bdsevm.NewStreamServiceClient(conn).StreamBlocks(sctx, &bdsevm.StreamBlocksRequest{FromBlock: from, FromHash: fromHash})
		for err == nil {
			var resp *bdsevm.StreamBlocksResponse
			if resp, err = stream.Recv(); err == nil {
				out <- streamEvent{h: resp.GetHeader()}
			}
		}
		if ctx.Err() == nil {
			st, _ := status.FromError(err)
			out <- streamEvent{code: st.Code(), msg: st.Message()}
		}
	}()
	return out
}

// next is the stream's next event within d, ok false when none came.
func next(ev <-chan streamEvent, d time.Duration) (streamEvent, bool) {
	select {
	case e, open := <-ev:
		return e, open
	case <-time.After(d):
		return streamEvent{}, false
	}
}

// D3: StreamBlocks cursors on both pods (§19 D3; §10.1; §10.3). A stream
// read on pod-1, stopped, then a reorg replaces its last header: resumed on
// pod-2 from it, the stream resends from the fork point + 1. A cursor of
// an orphan no vendor serves, and a canonical cursor below the window, end
// OUT_OF_RANGE. A cursor above the head waits: no header and no end while
// the head stays below it, past the 60s stalled-Read bound.
func d3() spec {
	return spec{name: "D3", bound: healthyDelivery, sim: chainsim.Config{History: d3History}, body: func(s *scenario) {
		s.blocks(25)
		rctx, cancel := context.WithCancel(s.ctx)
		ev := s.openStream(rctx, 0, nil, nil)
		var last *bdsevm.BlockHeader
		for range 5 {
			e, ok := next(ev, d3Reply)
			if !ok || e.h == nil {
				s.violate(s.name, "pod-1: StreamBlocks gave no header within %s (end %s %s)", d3Reply, e.code, e.msg)
				break
			}
			last = e.h
		}
		cancel()
		if last != nil {
			r := s.reorg(int(s.chain.Tip().Number-last.Number) + 2)
			fork := r.old[0].Number - 1
			time.Sleep(healthyDelivery)
			rctx, cancel := context.WithCancel(s.ctx)
			e, ok := next(s.openStream(rctx, 1, &last.Number, last.Hash), d3Reply)
			want := s.chain.CanonAt(fork + 1)
			switch {
			case !ok || e.h == nil:
				s.violate(s.name, "pod-2: resumed from %d %s, replaced by a reorg at fork point %d: no header within %s (end %s %s)", last.Number, short(gethcommon.BytesToHash(last.Hash)), fork, d3Reply, e.code, e.msg)
			case e.h.Number != want.Number || gethcommon.BytesToHash(e.h.Hash) != want.Hash:
				s.violate(s.name, "pod-2: resumed from %d, replaced by a reorg at fork point %d: first header %d %s, not the fork point + 1 %d %s", last.Number, fork, e.h.Number, short(gethcommon.BytesToHash(e.h.Hash)), want.Number, short(want.Hash))
			default:
				s.logf("pod-2 resent from the fork point + 1 (%d) for a cursor at %d", want.Number, last.Number)
			}
			cancel()
		}
		tip := s.chain.Tip()
		orphan := s.chain.Orphan(s.chain.CanonAt(tip.Number - 5))
		old := s.chain.CanonAt(tip.Number - uint64(d3History) + 10)
		if time.Duration(tip.Time-old.Time)*time.Second <= maxBackfill {
			s.violate("harness", "cursor %d is %ds below the head, inside maxBackfill %s", old.Number, tip.Time-old.Time, maxBackfill)
		}
		for pod := range s.h.pods {
			for _, c := range []struct {
				what string
				b    *chainsim.Block
			}{{"an orphan no vendor serves", orphan}, {"below the window", old}} {
				rctx, cancel := context.WithCancel(s.ctx)
				n := c.b.Number
				e, ok := next(s.openStream(rctx, pod, &n, c.b.Hash.Bytes()), d3Reply)
				if !ok || e.h != nil || e.code != codes.OutOfRange {
					got := fmt.Sprintf("end %s %s", e.code, e.msg)
					if e.h != nil {
						got = fmt.Sprintf("header %d", e.h.Number)
					} else if !ok {
						got = "nothing"
					}
					s.violate(s.name, "pod-%d: cursor %d %s, %s: %s within %s, not OUT_OF_RANGE", pod+1, n, short(c.b.Hash), c.what, got, d3Reply)
				}
				cancel()
			}
		}
		var wg sync.WaitGroup
		for pod := range s.h.pods {
			wg.Add(1)
			go func() {
				defer wg.Done()
				n := s.chain.Tip().Number + d3AboveBlocks
				var h hash
				for i := range h {
					h[i] = byte(rand.IntN(256))
				}
				rctx, cancel := context.WithCancel(s.ctx)
				defer cancel()
				if e, ok := next(s.openStream(rctx, pod, &n, h.Bytes()), d3Wait); ok {
					what := fmt.Sprintf("ended %s %s", e.code, e.msg)
					if e.h != nil {
						what = fmt.Sprintf("sent header %d", e.h.Number)
					}
					s.violate(s.name, "pod-%d: cursor %d above the head %s within %s, while the head stayed below it: it must wait", pod+1, n, what, d3Wait)
				}
			}()
		}
		wg.Wait()
	}}
}

// subscriber is one logs subscription of a pool connection.
type subscriber struct {
	filter   int
	ackAt    time.Time
	ackTip   uint64
	live     map[chainsim.LogKey]uint64
	failures int
}

// D4: one instance, d4Subs logs subscriptions spread over three filters
// and d4Subs/d4PerConn connections, for d4Blocks. Each block a filter
// wants is fetched once: one receipts call over every vendor; every
// subscription's logs equal the canonical logs; delivery p99 within 2 bt
// of the block's first header (§19 D4; §10.1 per-block work is shared).
func d4() spec {
	filters := []chainsim.Filter{chainsim.Filters[0], chainsim.Filters[1], {Address: &chainsim.Addrs[1]}}
	return spec{name: "D4", pods: 1, memory: true, bound: healthyDelivery, clients: func(*scenario) {}, body: func(s *scenario) {
		s.blocks(10)
		subs := make([]*subscriber, d4Subs)
		var lat []time.Duration
		var mu sync.Mutex
		ctx, cancel := context.WithCancel(s.ctx)
		var wg sync.WaitGroup
		opened := time.Now()
		for c := range d4Subs / d4PerConn {
			conn, _, err := websocket.DefaultDialer.DialContext(ctx, s.h.pods[0].wsURL(s.project, s.chain.ID()), nil)
			if err != nil {
				s.violate(s.name, "pod-1: ws dial: %v", err)
				continue
			}
			byID := map[string]*subscriber{}
			for i := range d4PerConn {
				k := c*d4PerConn + i
				subs[k] = &subscriber{filter: k % len(filters), live: map[chainsim.LogKey]uint64{}}
				if err := conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": k, "method": "eth_subscribe", "params": []interface{}{"logs", filters[k%len(filters)].Params()}}); err != nil {
					s.violate(s.name, "pod-1: subscribe write: %v", err)
				}
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				go func() { <-ctx.Done(); _ = conn.Close() }()
				for {
					var m struct {
						ID     *int            `json:"id"`
						Result json.RawMessage `json:"result"`
						Error  *struct {
							Code    int    `json:"code"`
							Message string `json:"message"`
						} `json:"error"`
						Params struct {
							Subscription string  `json:"subscription"`
							Result       logJSON `json:"result"`
						} `json:"params"`
					}
					if err := conn.ReadJSON(&m); err != nil {
						if ctx.Err() == nil {
							s.violate(s.name, "pod-1: pool connection %d ended: %v", c, err)
						}
						return
					}
					now := time.Now()
					if m.ID != nil {
						sub := subs[*m.ID]
						var id string
						if m.Error != nil || json.Unmarshal(m.Result, &id) != nil {
							s.violate(s.name, "pod-1: logs subscribe %d refused: %+v", *m.ID, m.Error)
							continue
						}
						mu.Lock()
						sub.ackAt, sub.ackTip = now, s.chain.Tip().Number
						byID[id] = sub
						mu.Unlock()
						continue
					}
					mu.Lock()
					sub := byID[m.Params.Subscription]
					l := m.Params.Result
					if sub == nil {
						mu.Unlock()
						s.violate(s.name, "pod-1: log for unknown subscription %s", m.Params.Subscription)
						continue
					}
					key := chainsim.LogKey{BlockHash: l.BlockHash, LogIndex: int(l.LogIndex)}
					if l.Removed {
						delete(sub.live, key)
					} else {
						sub.live[key] = uint64(l.BlockNumber)
						if b := s.chain.Block(l.BlockHash); b != nil && b.MintedAt.After(sub.ackAt) {
							lat = append(lat, now.Sub(s.waitFrom(0, b.Hash, b.MintedAt)))
						}
					}
					mu.Unlock()
				}
			}()
		}
		s.blocks(d4Blocks)
		stopped := time.Now()
		cancel()
		wg.Wait()
		var to uint64
		for _, b := range s.chain.MintedBetween(opened, stopped.Add(-d4Settled)) {
			to = b.Number
		}
		flagged := 0
		for k, sub := range subs {
			if sub == nil || sub.ackAt.IsZero() {
				s.violate(s.name, "pod-1: subscription %d never acknowledged", k)
				continue
			}
			want := s.chain.CanonicalLogs(filters[sub.filter], sub.ackTip+1, to)
			missing, extra := 0, 0
			for key := range want {
				if _, ok := sub.live[key]; !ok {
					missing++
				}
			}
			for key, n := range sub.live {
				if n > sub.ackTip && n <= to {
					if _, ok := want[key]; !ok {
						extra++
					}
				}
			}
			if missing+extra > 0 {
				if flagged++; flagged <= 5 {
					s.violate(s.name, "pod-1: subscription %d (filter %d) over %d..%d: %d logs missing, %d not canonical", k, sub.filter, sub.ackTip+1, to, missing, extra)
				}
			}
		}
		if flagged > 5 {
			s.violate(s.name, "pod-1: %d subscriptions in all differ from the canonical logs", flagged)
		}
		wanted := 0
		for _, b := range s.chain.MintedBetween(opened, stopped.Add(-d4Settled)) {
			matched := false
			for _, f := range filters {
				matched = matched || len(s.chain.CanonicalLogs(f, b.Number, b.Number)) > 0
			}
			if !matched {
				continue
			}
			wanted++
			calls := 0
			for _, v := range s.vendors {
				n, _ := v.ReceiptsOf(b.Hash)
				calls += n
			}
			if calls != 1 {
				s.violate(s.name, "block %d, wanted by %d subscriptions' filters: %d receipts calls over every vendor, not 1", b.Number, d4Subs, calls)
			}
		}
		slices.Sort(lat)
		s.logf("%d wanted blocks; %d log deliveries", wanted, len(lat))
		if len(lat) == 0 {
			s.violate(s.name, "pod-1: no log delivered to %d subscriptions", d4Subs)
		} else if p99 := lat[len(lat)*99/100]; p99 > d4P99 {
			s.violate(s.name, "pod-1: log delivery p99 %s over %d deliveries to %d subscriptions, above 2 bt (%s)", p99.Round(time.Millisecond), len(lat), d4Subs, d4P99)
		}
	}}
}

// wsMsg is one ws JSON-RPC message: an answer (id) or a notification.
type wsMsg struct {
	ID     *int            `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Params struct {
		Subscription string `json:"subscription"`
	} `json:"params"`
}

// wsCall sends one call on conn and reads until its answer, skipping
// notifications.
func wsCall(conn *websocket.Conn, id int, method string, params ...interface{}) (wsMsg, error) {
	if err := conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return wsMsg{}, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(d3Reply))
	for {
		var m wsMsg
		if err := conn.ReadJSON(&m); err != nil {
			return wsMsg{}, err
		}
		if m.ID != nil && *m.ID == id {
			return m, nil
		}
	}
}

// D5: d5Rate subscribe+unsubscribe pairs a second on pod-1 for d5For,
// newHeads and both logs filters, over d5Conns connections; each
// subscription lives up to d5Hold, so unsubscribes land during sends. No
// notification of a subscription follows its unsubscribe's answer, and
// erpc_taps reads 0 within d5Drain once the last one answered (§19 D5;
// §17 erpc_taps).
func d5() spec {
	return spec{name: "D5", bound: healthyDelivery, clients: func(*scenario) {}, body: func(s *scenario) {
		s.blocks(10)
		var subscribed, unsubscribed, late atomic.Int64
		var wg sync.WaitGroup
		deadline := time.Now().Add(d5For)
		for c := range d5Conns {
			conn, _, err := websocket.DefaultDialer.DialContext(s.ctx, s.h.pods[0].wsURL(s.project, s.chain.ID()), nil)
			if err != nil {
				s.violate(s.name, "pod-1: ws dial: %v", err)
				continue
			}
			var wmu sync.Mutex
			write := func(v interface{}) {
				wmu.Lock()
				defer wmu.Unlock()
				_ = conn.WriteJSON(v)
			}
			var pending sync.WaitGroup
			var mu sync.Mutex
			gone := map[string]bool{}
			unsubOf := map[int]string{}
			wg.Add(2)
			go func() {
				defer wg.Done()
				defer func() {
					answered := make(chan struct{})
					go func() { pending.Wait(); close(answered) }()
					select {
					case <-answered:
					case <-time.After(d5Hold + d3Reply):
						s.violate(s.name, "pod-1: conn %d: subscribes or unsubscribes unanswered %s after the last subscribe", c, d5Hold+d3Reply)
					}
					_ = conn.Close()
				}()
				t := time.NewTicker(time.Second * d5Conns / d5Rate)
				defer t.Stop()
				for id := 1; time.Now().Before(deadline); id += 2 {
					<-t.C
					params := []interface{}{"newHeads"}
					if k := id / 2 % 3; k > 0 {
						params = []interface{}{"logs", chainsim.Filters[k-1].Params()}
					}
					pending.Add(1)
					write(map[string]interface{}{"jsonrpc": "2.0", "id": id, "method": "eth_subscribe", "params": params})
				}
			}()
			go func() {
				defer wg.Done()
				for {
					var m wsMsg
					if err := conn.ReadJSON(&m); err != nil {
						return
					}
					switch {
					case m.ID == nil:
						mu.Lock()
						if gone[m.Params.Subscription] {
							late.Add(1)
							s.tr.logf("conn %d: notification of %s after its unsubscribe answered", c, m.Params.Subscription)
						}
						mu.Unlock()
					case *m.ID%2 == 1:
						var sub string
						if m.Error != nil || json.Unmarshal(m.Result, &sub) != nil {
							s.violate(s.name, "pod-1: subscribe %d refused: %+v", *m.ID, m.Error)
							pending.Done()
							continue
						}
						subscribed.Add(1)
						uid := *m.ID + 1
						mu.Lock()
						unsubOf[uid] = sub
						mu.Unlock()
						time.AfterFunc(rand.N(d5Hold), func() {
							write(map[string]interface{}{"jsonrpc": "2.0", "id": uid, "method": "eth_unsubscribe", "params": []string{sub}})
						})
					default:
						mu.Lock()
						gone[unsubOf[*m.ID]] = true
						mu.Unlock()
						unsubscribed.Add(1)
						pending.Done()
					}
				}
			}()
		}
		wg.Wait()
		last := time.Now()
		s.logf("%d subscribed, %d unsubscribed in %s", subscribed.Load(), unsubscribed.Load(), d5For)
		if subscribed.Load() < d5Rate*int64(d5For/time.Second)/2 {
			s.violate("harness", "%d subscribes in %s, below half of %d/s", subscribed.Load(), d5For, d5Rate)
		}
		if subscribed.Load() != unsubscribed.Load() {
			s.violate(s.name, "pod-1: %d subscribes answered, %d unsubscribes", subscribed.Load(), unsubscribed.Load())
		}
		if n := late.Load(); n > 0 {
			s.violate(s.name, "pod-1: %d notifications after their subscription's unsubscribe answered", n)
		}
		if v, ok := s.waitMetric(0, "erpc_taps", nil, d5Drain, func(v float64) bool { return v == 0 }); !ok {
			_, exported := s.metric(0, "erpc_taps", nil)
			s.violate(s.name, "pod-1: erpc_taps %v (exported %v) %s after the last unsubscribe answered, not 0", v, exported, time.Since(last).Round(100*time.Millisecond))
		}
	}}
}

// D6: a logs subscribe with a malformed filter is refused -32602 before
// any tap opens; StreamBlocks with fromHash and no fromBlock, or the
// reverse, ends INVALID_ARGUMENT (§19 D6; §10.3).
func d6() spec {
	return spec{name: "D6", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(10)
		for pod := range s.h.pods {
			conn, _, err := websocket.DefaultDialer.DialContext(s.ctx, s.h.pods[pod].wsURL(s.project, s.chain.ID()), nil)
			if err != nil {
				s.violate(s.name, "pod-%d: ws dial: %v", pod+1, err)
				continue
			}
			for i, f := range []map[string]interface{}{
				{"address": "0xnothex"},
				{"topics": "0x01"},
			} {
				m, err := wsCall(conn, i+1, "eth_subscribe", "logs", f)
				switch {
				case err != nil:
					s.violate(s.name, "pod-%d: logs subscribe %v: %v", pod+1, f, err)
				case m.Error == nil || m.Error.Code != -32602:
					s.violate(s.name, "pod-%d: logs subscribe with malformed filter %v answered %s %+v, not -32602", pod+1, f, m.Result, m.Error)
				}
			}
			_ = conn.Close()
			n, h := s.chain.Tip().Number, s.chain.Tip().Hash.Bytes()
			for _, c := range []struct {
				what string
				from *uint64
				hash []byte
			}{{"fromHash without fromBlock", nil, h}, {"fromBlock without fromHash", &n, nil}} {
				rctx, cancel := context.WithCancel(s.ctx)
				e, ok := next(s.openStream(rctx, pod, c.from, c.hash), d3Reply)
				if !ok || e.h != nil || e.code != codes.InvalidArgument {
					s.violate(s.name, "pod-%d: StreamBlocks with %s: header %v, end %s %s (ok %v), not INVALID_ARGUMENT", pod+1, c.what, e.h != nil, e.code, e.msg, ok)
				}
				cancel()
			}
		}
		s.blocks(10)
	}}
}

// D7: server.webSocket.maxSubscriptionsPerConnection is d7Limit. On one
// connection d7Limit subscribes succeed and the next is refused -32005;
// another connection still subscribes (§19 D7; §15.2).
func d7() spec {
	return spec{name: "D7", bound: healthyDelivery, webSocket: map[string]interface{}{"maxSubscriptionsPerConnection": d7Limit}, body: func(s *scenario) {
		s.blocks(10)
		for pod := range s.h.pods {
			url := s.h.pods[pod].wsURL(s.project, s.chain.ID())
			conn, _, err := websocket.DefaultDialer.DialContext(s.ctx, url, nil)
			if err != nil {
				s.violate(s.name, "pod-%d: ws dial: %v", pod+1, err)
				continue
			}
			for i := 1; i <= d7Limit+1; i++ {
				m, err := wsCall(conn, i, "eth_subscribe", "newHeads")
				switch {
				case err != nil:
					s.violate(s.name, "pod-%d: subscribe %d: %v", pod+1, i, err)
				case i <= d7Limit && m.Error != nil:
					s.violate(s.name, "pod-%d: subscribe %d of %d allowed refused: %+v", pod+1, i, d7Limit, m.Error)
				case i > d7Limit && (m.Error == nil || m.Error.Code != rateLimitedCode):
					s.violate(s.name, "pod-%d: subscribe %d past the per-connection limit %d answered %s %+v, not -32005", pod+1, i, d7Limit, m.Result, m.Error)
				}
			}
			other, _, err := websocket.DefaultDialer.DialContext(s.ctx, url, nil)
			if err == nil {
				if m, err := wsCall(other, 1, "eth_subscribe", "newHeads"); err != nil || m.Error != nil {
					s.violate(s.name, "pod-%d: subscribe on a second connection refused: %v %+v", pod+1, err, m.Error)
				}
				_ = other.Close()
			}
			_ = conn.Close()
		}
		s.blocks(10)
	}}
}

// D9: each pod has a newPendingTransactions subscriber; ws-a's pending
// stream then goes silent with its socket open. No pod resubscribes it
// before d9Silent; every pod resubscribes it within d9Silent plus slack
// (erpc_source_resubscribe_total rises, ws-a gets a new subscribe), and
// subscribers keep getting transactions from ws-b (§19 D9; §12; §15.1
// mempool).
func d9() spec {
	return spec{name: "D9", bound: healthyDelivery, body: func(s *scenario) {
		var got atomic.Int64
		for pod := range s.h.pods {
			cl, err := rpc.DialContext(s.ctx, s.h.pods[pod].wsURL(s.project, s.chain.ID()))
			if err != nil {
				s.violate(s.name, "pod-%d: ws dial: %v", pod+1, err)
				continue
			}
			ch := make(chan hash, 4096)
			sub, err := cl.EthSubscribe(s.ctx, ch, "newPendingTransactions")
			if err != nil {
				s.violate(s.name, "pod-%d: newPendingTransactions subscribe: %v", pod+1, err)
				cl.Close()
				continue
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer cl.Close()
				defer sub.Unsubscribe()
				for {
					select {
					case <-s.ctx.Done():
						return
					case err := <-sub.Err():
						s.violate(s.name, "pod-%d: newPendingTransactions subscription ended: %v", pod+1, err)
						return
					case <-ch:
						got.Add(1)
					}
				}
			}()
		}
		s.blocks(25)
		v, up := s.vendors["ws-a"], s.upstream("ws-a")
		labels := map[string]string{"upstream": up}
		res0 := make([]float64, len(s.h.pods))
		for pod := range s.h.pods {
			res0[pod], _ = s.metric(pod, "erpc_source_resubscribe_total", labels)
		}
		subs0 := v.Stats().PendingSubscribes
		v.Update(func(f *chainsim.Faults) { f.SilentPending = true })
		at := time.Now()
		s.logf("%s's pending stream silent, socket open", up)
		time.Sleep(time.Until(at.Add(d9Silent - reconnectSlack)))
		if n := v.Stats().PendingSubscribes - subs0; n > 0 {
			s.violate(s.name, "%s got %d pending subscribes %s into its silence, before %s", up, n, time.Since(at).Round(time.Second), d9Silent)
		}
		got0 := got.Load()
		for pod := range s.h.pods {
			if n, ok := s.waitMetric(pod, "erpc_source_resubscribe_total", labels, time.Until(at.Add(d9Silent+2*reconnectSlack)), func(v float64) bool { return v > res0[pod] }); !ok {
				s.violate(s.name, "pod-%d: erpc_source_resubscribe_total{upstream=%s} %v, not risen %s into its silence", pod+1, up, n, time.Since(at).Round(time.Second))
			}
		}
		if n := v.Stats().PendingSubscribes - subs0; n < int64(len(s.h.pods)) {
			s.violate(s.name, "%s got %d pending subscribes %s into its silence, fewer than one per pod", up, n, time.Since(at).Round(time.Second))
		}
		v.Update(func(f *chainsim.Faults) { f.SilentPending = false })
		if got.Load() == got0 {
			s.violate(s.name, "no pending transaction reached a subscriber during %s's silence: ws-b carries none", up)
		}
		s.blocks(10)
	}}
}
