package realworld

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/test/chainsim"
	"github.com/erpc/erpc/test/h2idle"
	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/gorilla/websocket"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// gRPC streams (specs/streams/feature.md §6; §13 G1–G7, and F1, F2
// through StreamLogs).
const (
	// grpcIdleCut: the ALB's default idle timeout; its proxy ignores PING.
	grpcIdleCut = 60 * time.Second
	// g2Txs: about 1.5 logs per transaction, 10,000 logs a non-empty block.
	g2Txs = 7000
	// pendingWake: a mempool Read wakes at most this often (CT§12).
	pendingWake = 50 * time.Millisecond
	// grpcWindow, grpcConnWindow: the tuned client's windows (§6.3).
	grpcWindow     = 4 << 20
	grpcConnWindow = 16 << 20
)

func (s *scenario) streamMD(ctx context.Context) context.Context {
	return metadata.NewOutgoingContext(ctx, metadata.Pairs("x-erpc-project", s.project, "x-erpc-chain-id", fmt.Sprint(s.chain.ID())))
}

// dialGrpc dials addr with the §6.3 windows; nil on a violation.
func (s *scenario) dialGrpc(name, addr string) *grpc.ClientConn {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithInitialWindowSize(grpcWindow), grpc.WithInitialConnWindowSize(grpcConnWindow))
	if err != nil {
		s.violate(name, "dial %s: %v", addr, err)
		return nil
	}
	return conn
}

// grpcLogsClient is a StreamLogs driver (§9.5 by hand): it applies a
// block's logs at its complete message, drops what an Undo names, and
// resumes from its cursor on the other pod every switchEvery blocks (0
// never) and after every UNAVAILABLE end.
type grpcLogsClient struct {
	s           *scenario
	name        string
	req         *bdsevm.StreamLogsRequest
	switchEvery int
	// filter is the chainsim filter req matches, -1 for every log.
	filter int

	mu        sync.Mutex
	pod       int
	cursor    *bdsevm.BlockRef
	held      map[chainsim.LogKey]uint64
	pend      map[chainsim.LogKey]uint64
	blockEnds []uint64
	tips      map[uint64]uint64
	undos     int
	msgs      []*bdsevm.StreamLogsResponse
}

func (s *scenario) newLogsClient(name string, pod, filter, switchEvery int, req *bdsevm.StreamLogsRequest) *grpcLogsClient {
	return &grpcLogsClient{s: s, name: name, req: req, switchEvery: switchEvery, filter: filter, pod: pod,
		held: map[chainsim.LogKey]uint64{}, pend: map[chainsim.LogKey]uint64{}, tips: map[uint64]uint64{}}
}

func (s *scenario) startLogsClient(c *grpcLogsClient) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		c.run(s.ctx)
	}()
}

func (c *grpcLogsClient) run(ctx context.Context) {
	for ctx.Err() == nil {
		code, msg, planned := c.stream(ctx)
		c.mu.Lock()
		c.pod = (c.pod + 1) % len(c.s.h.pods)
		c.mu.Unlock()
		switch {
		case ctx.Err() != nil, planned:
		case code == codes.Unavailable:
			sleepCtx(ctx, clientRedial)
		default:
			c.s.violate(c.name, "StreamLogs ended %s: %s", code, msg)
			return
		}
	}
}

func (c *grpcLogsClient) stream(ctx context.Context) (codes.Code, string, bool) {
	c.mu.Lock()
	pod := c.pod
	req := proto.Clone(c.req).(*bdsevm.StreamLogsRequest)
	if c.cursor != nil {
		n := c.cursor.Number
		req.FromBlock, req.FromHash = &n, c.cursor.Hash
	}
	clear(c.pend)
	c.mu.Unlock()
	conn := c.s.dialGrpc(c.name, c.s.h.pods[pod].grpcAddr())
	if conn == nil {
		return codes.Unavailable, "dial", false
	}
	defer conn.Close()
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := bdsevm.NewStreamServiceClient(conn).StreamLogs(c.s.streamMD(sctx), req)
	if err != nil {
		st, _ := status.FromError(err)
		return st.Code(), st.Message(), false
	}
	limit := 0
	if c.switchEvery > 0 {
		limit = c.switchEvery + rand.IntN(c.switchEvery+1)
	}
	blocks := 0
	for {
		m, err := stream.Recv()
		if err != nil {
			st, _ := status.FromError(err)
			return st.Code(), st.Message(), false
		}
		if c.apply(m) {
			blocks++
		}
		if limit > 0 && blocks >= limit {
			return codes.Canceled, "", true
		}
	}
}

// apply applies m; true at a block's end.
func (c *grpcLogsClient) apply(m *bdsevm.StreamLogsResponse) bool {
	tip := c.s.chain.Tip().Number
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, m)
	switch {
	case m.GetBlock() != nil:
		b := m.GetBlock()
		if len(b.Logs) > 1000 {
			c.s.violate(c.name, "block %d: %d logs in one message", b.Block.Number, len(b.Logs))
		}
		if n := proto.Size(m); n >= 4<<20 {
			c.s.violate(c.name, "block %d: a message of %d bytes", b.Block.Number, n)
		}
		for _, l := range b.Logs {
			c.pend[chainsim.LogKey{BlockHash: gethcommon.BytesToHash(l.BlockHash), LogIndex: int(l.LogIndex)}] = l.BlockNumber
		}
		if !b.Complete {
			return false
		}
		if prev := c.cursor; prev != nil && b.Block.Number != prev.Number+1 {
			c.s.violate(c.name, "block %d after %d", b.Block.Number, prev.Number)
		}
		for k, n := range c.pend {
			c.held[k] = n
		}
		clear(c.pend)
		c.cursor = b.Block
		c.blockEnds = append(c.blockEnds, b.Block.Number)
		c.tips[b.Block.Number] = tip
		return true
	case m.GetUndo() != nil:
		fp := m.GetUndo().ForkPoint
		c.undos++
		for k, n := range c.held {
			if n > fp.Number {
				delete(c.held, k)
			}
		}
		clear(c.pend)
		c.cursor = fp
	}
	return false
}

// judge: the logs held over the client's blocks are the canonical chain's;
// each block at least depth below the tip when it ended.
func (c *grpcLogsClient) judge(depth uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var first, last uint64
	for _, n := range c.blockEnds {
		if first == 0 {
			first = n
		}
		last = max(last, n)
	}
	if first == 0 || last-first < 20 {
		c.s.violate(c.name, "blocks %d-%d, want a flowing stream", first, last)
		return
	}
	for n, tip := range c.tips {
		if n+depth > tip {
			c.s.violate(c.name, "block %d ended at tip %d: above the bound, %d below", n, tip, depth)
		}
	}
	want := map[chainsim.LogKey]bool{}
	if c.filter >= 0 {
		for k := range c.s.chain.CanonicalLogs(chainsim.Filters[c.filter], first, last) {
			want[k] = true
		}
	} else {
		for n := first; n <= last; n++ {
			for _, l := range c.s.chain.CanonAt(n).Logs() {
				want[l.Key()] = true
			}
		}
	}
	for k, n := range c.held {
		if n < first || n > last {
			continue
		}
		if !want[k] {
			c.s.violate(c.name, "holds log %d of block %d, not canonical", k.LogIndex, n)
		}
		delete(want, k)
	}
	if len(want) > 0 {
		c.s.violate(c.name, "misses %d canonical logs of blocks %d-%d", len(want), first, last)
	}
}

func (c *grpcLogsClient) undoCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.undos
}

// G1: StreamLogs clients resume across pods every few blocks under C1
// reorgs: Undo then the new branch, a complete per block, logs canonical.
func g1() spec {
	return spec{name: "G1", bound: settleDelivery, body: func(s *scenario) {
		s.blocks(25)
		var cs []*grpcLogsClient
		for i := range 2 {
			c := s.newLogsClient(fmt.Sprintf("%s/logs-%d", s.name, i+1), i, i, 8,
				&bdsevm.StreamLogsRequest{Filter: filterOf(chainsim.Filters[i]), HeartbeatSeconds: new(uint32(0))})
			s.startLogsClient(c)
			cs = append(cs, c)
		}
		for range 8 {
			s.blocks(15)
			s.reorg(1 + rand.IntN(3))
		}
		s.blocks(20)
		for _, c := range cs {
			c.judge(0)
			if c.undoCount() == 0 {
				s.violate(c.name, "no Undo across 8 reorgs")
			}
		}
	}}
}

// G2: a block with 10,000 matching logs: messages of at most 1,000 logs,
// each under 4 MB, only the last complete.
func g2() spec {
	return spec{name: "G2", bound: deepReorgDelivery, body: func(s *scenario) {
		s.blocks(10)
		c := s.newLogsClient(s.name+"/all", 0, -1, 0, &bdsevm.StreamLogsRequest{Filter: &bdsevm.LogFilter{}})
		s.startLogsClient(c)
		s.blocks(5)
		s.chain.BigBlocks(g2Txs)
		s.blocks(3)
		s.chain.BigBlocks(0)
		s.blocks(25)
		c.mu.Lock()
		perBlock := map[hash][]*bdsevm.BlockLogs{}
		for _, m := range c.msgs {
			if b := m.GetBlock(); b != nil {
				h := gethcommon.BytesToHash(b.Block.Hash)
				perBlock[h] = append(perBlock[h], b)
			}
		}
		c.mu.Unlock()
		big := 0
		for h, parts := range perBlock {
			logs := 0
			for i, p := range parts {
				if p.Complete != (i == len(parts)-1) {
					s.violate(s.name, "block %d: message %d of %d complete=%v", p.Block.Number, i+1, len(parts), p.Complete)
				}
				logs += len(p.Logs)
			}
			if b := s.chain.Block(h); b != nil && len(b.Logs()) != logs {
				s.violate(s.name, "block %d: %d logs sent, the block holds %d", b.Number, logs, len(b.Logs()))
			}
			if logs >= 10000 {
				big++
			}
		}
		if big == 0 {
			s.violate(s.name, "no block of 10,000 logs reached the client")
		}
		c.judge(0)
	}}
}

// pendingClient is a StreamPendingTransactions driver: when each hash
// arrived, and each batch's size.
type pendingClient struct {
	mu    sync.Mutex
	at    map[hash]time.Time
	batch []int
	end   error
}

func (s *scenario) openPending(name string, pod int, full bool) *pendingClient {
	pc := &pendingClient{at: map[hash]time.Time{}}
	conn := s.dialGrpc(name, s.h.pods[pod].grpcAddr())
	if conn == nil {
		return pc
	}
	stream, err := bdsevm.NewStreamServiceClient(conn).StreamPendingTransactions(s.streamMD(s.ctx),
		&bdsevm.StreamPendingTransactionsRequest{FullTransactions: full})
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer conn.Close()
		for err == nil {
			var m *bdsevm.StreamPendingTransactionsResponse
			if m, err = stream.Recv(); err != nil {
				break
			}
			b := m.GetTransactions()
			if b == nil {
				continue
			}
			now := time.Now()
			pc.mu.Lock()
			pc.batch = append(pc.batch, len(b.Hashes)+len(b.Transactions))
			for _, h := range b.Hashes {
				pc.at[gethcommon.BytesToHash(h)] = now
			}
			for _, tx := range b.Transactions {
				pc.at[gethcommon.BytesToHash(tx.Hash)] = now
			}
			pc.mu.Unlock()
		}
		if s.ctx.Err() == nil {
			pc.mu.Lock()
			pc.end = err
			pc.mu.Unlock()
		}
	}()
	return pc
}

// G3: StreamPendingTransactions hashes and full on each pod beside a ws
// subscriber of the same variant: at most one upstream subscription per
// variant per pod; each transaction within the mempool wake of its push;
// batches of at most 1,000.
func g3() spec {
	return spec{name: "G3", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(10)
		var pcs []*pendingClient
		for pod := range s.h.pods {
			ec, _, err := s.dialExt(pod, "", websocket.DefaultDialer)
			if err != nil {
				s.violate(s.name, "pod-%d: ws dial: %v", pod+1, err)
				return
			}
			defer ec.close()
			if _, err := ec.subscribe("newPendingTransactions"); err != nil {
				s.violate(s.name, "pod-%d: ws pending subscribe: %v", pod+1, err)
				return
			}
			pcs = append(pcs, s.openPending(s.name, pod, false), s.openPending(s.name, pod, true))
		}
		from := s.chain.Tip().Number + 2
		s.blocks(40)
		to := s.chain.Tip().Number - 2
		for i, pc := range pcs {
			name := fmt.Sprintf("%s/pod-%d/%s", s.name, i/2+1, []string{"hashes", "full"}[i%2])
			pc.mu.Lock()
			if pc.end != nil {
				s.violate(name, "stream ended: %v", pc.end)
			}
			for _, n := range pc.batch {
				if n > 1000 {
					s.violate(name, "a batch of %d", n)
				}
			}
			got := 0
			for n := from; n <= to; n++ {
				b := s.chain.CanonAt(n)
				for t := range b.TxCount() {
					at, ok := pc.at[b.TxHash(t)]
					if !ok {
						continue
					}
					got++
					if d := at.Sub(b.MintedAt); d > pendingWake+healthyDelivery {
						s.violate(name, "transaction of block %d after %s", n, d)
					}
				}
			}
			pc.mu.Unlock()
			if got == 0 {
				s.violate(name, "no pending transaction of blocks %d-%d", from, to)
			}
		}
		for _, v := range s.vendors {
			if n := v.Stats().PendingSubscribes; v.WS && n > int64(2*len(s.h.pods)) {
				s.violate(s.name, "%s: %d pending subscriptions, want one per variant per pod", v.ID, n)
			}
		}
	}}
}

// G4: a quiet chain for 3 min through a proxy that ends connections idle
// 60s and ignores PING: StreamLogs and StreamBlocks with heartbeatSeconds
// stay open; StreamBlocks without is cut at 60s.
func g4() spec {
	return spec{name: "G4", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(10)
		p, err := h2idle.Start(s.h.pods[0].grpcAddr(), grpcIdleCut)
		if err != nil {
			s.violate(s.name, "proxy: %v", err)
			return
		}
		defer p.Close()
		type open struct {
			name string
			beat bool
			done chan error
		}
		var opens []*open
		run := func(name string, beat bool, call func(bdsevm.StreamServiceClient, context.Context) (func() error, error)) {
			conn := s.dialGrpc(name, p.Addr())
			if conn == nil {
				return
			}
			o := &open{name: name, beat: beat, done: make(chan error, 1)}
			opens = append(opens, o)
			recv, err := call(bdsevm.NewStreamServiceClient(conn), s.streamMD(s.ctx))
			go func() {
				defer conn.Close()
				for err == nil {
					err = recv()
				}
				o.done <- err
			}()
		}
		hb := new(uint32(15))
		run(s.name+"/logs-heartbeat", true, func(c bdsevm.StreamServiceClient, ctx context.Context) (func() error, error) {
			st, err := c.StreamLogs(ctx, &bdsevm.StreamLogsRequest{Filter: &bdsevm.LogFilter{}, HeartbeatSeconds: hb})
			return func() error { _, err := st.Recv(); return err }, err
		})
		run(s.name+"/blocks-heartbeat", true, func(c bdsevm.StreamServiceClient, ctx context.Context) (func() error, error) {
			st, err := c.StreamBlocks(ctx, &bdsevm.StreamBlocksRequest{HeartbeatSeconds: hb})
			return func() error { _, err := st.Recv(); return err }, err
		})
		run(s.name+"/blocks-silent", false, func(c bdsevm.StreamServiceClient, ctx context.Context) (func() error, error) {
			st, err := c.StreamBlocks(ctx, &bdsevm.StreamBlocksRequest{})
			return func() error { _, err := st.Recv(); return err }, err
		})
		s.chain.Halt(true)
		time.Sleep(3 * time.Minute)
		s.chain.Halt(false)
		for _, o := range opens {
			select {
			case err := <-o.done:
				if o.beat {
					s.violate(o.name, "ended on a quiet chain with heartbeats: %v", err)
				} else if status.Code(err) != codes.Unavailable {
					s.violate(o.name, "ended %v, want the proxy's cut, UNAVAILABLE", err)
				}
			default:
				if !o.beat {
					s.violate(o.name, "open after 3 quiet minutes without heartbeats: the proxy never cut it")
				}
			}
		}
		if p.Cuts() != 1 {
			s.violate(s.name, "%d idle cuts, want 1", p.Cuts())
		}
	}}
}

// G5: a #26-era client, no request field from 5 on: header messages only.
func g5() spec {
	return spec{name: "G5", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(10)
		conn := s.dialGrpc(s.name, s.h.pods[1].grpcAddr())
		if conn == nil {
			return
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(s.streamMD(s.ctx), 30*blockTime)
		defer cancel()
		st, err := bdsevm.NewStreamServiceClient(conn).StreamBlocks(ctx, &bdsevm.StreamBlocksRequest{})
		n := 0
		for err == nil {
			var m *bdsevm.StreamBlocksResponse
			if m, err = st.Recv(); err != nil {
				break
			}
			n++
			if m.GetHeader() == nil {
				s.violate(s.name, "message %d is not a header: %v", n, m)
			}
		}
		if n < 20 {
			s.violate(s.name, "%d headers in 30 blocks", n)
		}
	}}
}

// G6: StreamBlocks FINALITY_FINALIZED: headers at or below Finalized; a
// cursor below the window ends OUT_OF_RANGE with ErrorInfo oldestBlock,
// oldestHash.
func g6() spec {
	return spec{name: "G6", bound: healthyDelivery, evm: finalityEvm(), upEvm: finalizedEvery(finalizedPoll), body: func(s *scenario) {
		s.blocks(chainsim.FinalizedLag + 10)
		conn := s.dialGrpc(s.name, s.h.pods[0].grpcAddr())
		if conn == nil {
			return
		}
		defer conn.Close()
		client := bdsevm.NewStreamServiceClient(conn)
		ctx, cancel := context.WithTimeout(s.streamMD(s.ctx), 60*blockTime)
		defer cancel()
		st, err := client.StreamBlocks(ctx, &bdsevm.StreamBlocksRequest{Finality: bdsevm.Finality_FINALITY_FINALIZED})
		n := 0
		for err == nil {
			var m *bdsevm.StreamBlocksResponse
			if m, err = st.Recv(); err != nil {
				break
			}
			n++
			if h := m.GetHeader(); h != nil && h.Number+chainsim.FinalizedLag > s.chain.Tip().Number {
				s.violate(s.name, "header %d above Finalized (tip %d)", h.Number, s.chain.Tip().Number)
			}
		}
		if n < 30 {
			s.violate(s.name, "%d finalized headers in 60 blocks, ended %v", n, err)
		}
		// Below the window and the final hold the finalized stream had.
		old := s.chain.CanonAt(s.chain.Tip().Number - chainsim.FinalizedLag - 40)
		from := old.Number
		st, err = client.StreamBlocks(s.streamMD(s.ctx), &bdsevm.StreamBlocksRequest{FromBlock: &from, FromHash: old.Hash.Bytes()})
		if err == nil {
			_, err = st.Recv()
		}
		if status.Code(err) != codes.OutOfRange {
			s.violate(s.name, "cursor below the window ended %v, want OUT_OF_RANGE", err)
			return
		}
		for _, d := range status.Convert(err).Details() {
			if info, ok := d.(*errdetails.ErrorInfo); ok {
				var oldest uint64
				_, _ = fmt.Sscan(info.Metadata["oldestBlock"], &oldest)
				if b := s.chain.CanonAt(oldest); oldest <= from || b == nil || b.Hash.Hex() != info.Metadata["oldestHash"] {
					s.violate(s.name, "ErrorInfo %v names no canonical block above the cursor %d", info.Metadata, from)
				}
				return
			}
		}
		s.violate(s.name, "OUT_OF_RANGE without ErrorInfo: %v", err)
	}}
}

// G7: 101 long-lived streams on one connection: the 101st
// RESOURCE_EXHAUSTED, counted per_connection.
func g7() spec {
	return spec{name: "G7", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(10)
		conn := s.dialGrpc(s.name, s.h.pods[0].grpcAddr())
		if conn == nil {
			return
		}
		defer conn.Close()
		client := bdsevm.NewStreamServiceClient(conn)
		ctx, cancel := context.WithCancel(s.streamMD(s.ctx))
		defer cancel()
		for i := range 101 {
			st, err := client.StreamBlocks(ctx, &bdsevm.StreamBlocksRequest{})
			if err == nil {
				_, err = st.Recv()
			}
			switch {
			case i < 100 && err != nil:
				s.violate(s.name, "stream %d refused: %v", i+1, err)
				return
			case i == 100 && status.Code(err) != codes.ResourceExhausted:
				s.violate(s.name, "stream 101 ended %v, want RESOURCE_EXHAUSTED", err)
			}
		}
		// The counter has no network label (§12): match the project only.
		rejected := map[string]string{"reason": "per_connection"}
		v, _ := s.projectMetric(0, "erpc_grpc_streams_rejected_total", rejected)
		for deadline := time.Now().Add(10 * time.Second); v < 1 && time.Now().Before(deadline); {
			time.Sleep(scrapeEvery)
			v, _ = s.projectMetric(0, "erpc_grpc_streams_rejected_total", rejected)
		}
		if v < 1 {
			s.violate(s.name, "erpc_grpc_streams_rejected_total %v, want 1", v)
		}
	}}
}

// F1 through StreamLogs: FINALITY_FINALIZED, REORG_NONE on each pod:
// every block at or below Finalized when it ended, never an Undo, logs
// canonical.
func f1g() spec {
	return spec{name: "F1-grpc", bound: healthyDelivery, evm: finalityEvm(), body: func(s *scenario) {
		s.blocks(chainsim.FinalizedLag + 10)
		var cs []*grpcLogsClient
		for pod := range s.h.pods {
			c := s.newLogsClient(fmt.Sprintf("%s/pod-%d", s.name, pod+1), pod, 0, 0, &bdsevm.StreamLogsRequest{
				Filter: filterOf(chainsim.Filters[0]), Finality: bdsevm.Finality_FINALITY_FINALIZED, Reorg: bdsevm.Reorg_REORG_NONE})
			s.startLogsClient(c)
			cs = append(cs, c)
		}
		s.blocks(60)
		s.reorg(2)
		s.blocks(60)
		for _, c := range cs {
			c.judge(chainsim.FinalizedLag)
			if n := c.undoCount(); n > 0 {
				s.violate(c.name, "%d Undos on a final stream", n)
			}
		}
	}}
}

// F2 through StreamLogs: FINALITY_DEPTH 3: reorgs 1–3 deep never reach
// it; one reorgWindow − 1 deep arrives as one Undo; logs canonical.
func f2g() spec {
	return spec{name: "F2-grpc", bound: deepReorgDelivery, allow: []string{"ws:logs-end", "ws:jump"}, body: func(s *scenario) {
		s.blocks(25)
		c := s.newLogsClient(s.name, 0, 1, 0, &bdsevm.StreamLogsRequest{Filter: filterOf(chainsim.Filters[1]),
			Finality: bdsevm.Finality_FINALITY_DEPTH, Depth: 3})
		s.startLogsClient(c)
		for range 6 {
			s.blocks(15)
			s.reorg(1 + rand.IntN(3))
		}
		s.blocks(15)
		if n := c.undoCount(); n > 0 {
			s.violate(s.name, "%d Undos from reorgs at most 3 deep", n)
		}
		s.reorg(reorgWindow - 1)
		s.blocks(40)
		c.judge(3)
		if n := c.undoCount(); n != 1 {
			s.violate(s.name, "%d Undos, want one for the %d-deep reorg", n, reorgWindow-1)
		}
	}}
}

func filterOf(f chainsim.Filter) *bdsevm.LogFilter {
	if f.Address != nil {
		return &bdsevm.LogFilter{Address: [][]byte{f.Address.Bytes()}}
	}
	return &bdsevm.LogFilter{Topics: []*bdsevm.TopicFilter{{Values: [][]byte{f.Topic0.Bytes()}}}}
}
