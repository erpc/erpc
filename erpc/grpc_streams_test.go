package erpc

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/test/chainsim"
	"github.com/erpc/erpc/test/h2idle"
	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// grpcMsg is one message a gRPC stream client read, with the chain's tip
// when it arrived.
type grpcMsg[T any] struct {
	m   *T
	tip uint64
}

// grpcRead is one stream call read in the background until it ends.
type grpcRead[T any] struct {
	mu   sync.Mutex
	msgs []grpcMsg[T]
	end  error
	done chan struct{}
}

// grpcOpen is a stream call's open: the stream, or why it failed.
type grpcOpen[T any] struct {
	stream grpc.ServerStreamingClient[T]
	err    error
}

func opened[T any](stream grpc.ServerStreamingClient[T], err error) grpcOpen[T] {
	return grpcOpen[T]{stream, err}
}

func readGrpc[T any](s *chaosSeed, o grpcOpen[T]) *grpcRead[T] {
	stream, err := o.stream, o.err
	r := &grpcRead[T]{done: make(chan struct{})}
	go func() {
		defer close(r.done)
		for err == nil {
			var m *T
			if m, err = stream.Recv(); err == nil {
				r.mu.Lock()
				r.msgs = append(r.msgs, grpcMsg[T]{m, s.chain.Tip().Number})
				r.mu.Unlock()
			}
		}
		r.mu.Lock()
		r.end = err
		r.mu.Unlock()
	}()
	return r
}

func (r *grpcRead[T]) all() []grpcMsg[T] {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]grpcMsg[T](nil), r.msgs...)
}

func (r *grpcRead[T]) ended() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.end
}

// completeBlocks counts the BlockLogs messages that end a block, and
// returns the last one's block: a client's cursor.
func completeBlocks(msgs []grpcMsg[bdsevm.StreamLogsResponse]) (n int, last *bdsevm.BlockRef) {
	for _, m := range msgs {
		if b := m.m.GetBlock(); b != nil && b.Complete {
			n, last = n+1, b.Block
		}
	}
	return n, last
}

// applyGrpcLogs is what a StreamLogs client holds after these messages,
// in order: each block's logs, block ends at complete, an undo dropping
// what lies above its fork point.
func applyGrpcLogs(t *testing.T, msgs ...[]grpcMsg[bdsevm.StreamLogsResponse]) logsApplied {
	t.Helper()
	a := logsApplied{held: map[chainsim.LogKey]uint64{}}
	for _, run := range msgs {
		for _, m := range run {
			switch {
			case m.m.GetBlock() != nil:
				b := m.m.GetBlock()
				for _, l := range b.Logs {
					require.Equal(t, b.Block.Hash, l.BlockHash, "log of block %d names its block", b.Block.Number)
					a.held[chainsim.LogKey{BlockHash: gethcommon.BytesToHash(l.BlockHash), LogIndex: int(l.LogIndex)}] = l.BlockNumber
				}
				if b.Complete {
					a.blockEnds = append(a.blockEnds, b.Block.Number)
				}
			case m.m.GetUndo() != nil:
				fp := m.m.GetUndo().ForkPoint.Number
				a.undos = append(a.undos, fp)
				for k, n := range a.held {
					if n > fp {
						delete(a.held, k)
					}
				}
				a.blockEnds = append(a.blockEnds, 0, fp)
			}
		}
	}
	return a
}

func filterOf(f chainsim.Filter) *bdsevm.LogFilter {
	if f.Address != nil {
		return &bdsevm.LogFilter{Address: [][]byte{f.Address.Bytes()}}
	}
	return &bdsevm.LogFilter{Topics: []*bdsevm.TopicFilter{{Values: [][]byte{f.Topic0.Bytes()}}}}
}

// rawCodec reads stream messages as their bytes; it sends proto.
type rawCodec struct{ encoding.CodecV2 }

func (c rawCodec) Unmarshal(data mem.BufferSlice, v any) error {
	if b, ok := v.(*[]byte); ok {
		*b = data.Materialize()
		return nil
	}
	return c.CodecV2.Unmarshal(data, v)
}

func dialGrpc(t *testing.T, addr string, opts ...grpc.DialOption) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, opts...)...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// G1 and G2 (streams spec §6.2, §13): StreamLogs sends one complete
// BlockLogs per block; a client that drops and resumes from its cursor on
// the other instance, across a reorg, gets an Undo then the new branch,
// and holds the canonical logs; another reorg reaches the live stream as
// one Undo. A block with more matched logs than one message takes spans
// messages of at most 1,000 logs, only the last complete, each under 4 MB.
func TestGrpcStreamLogs_ResumeUndoAndSplit(t *testing.T) {
	s := startFinalitySeed(t, 211, chainsim.Faults{})
	g0 := s.startGrpc(0, s.srv, nil, s.nw.Load())
	g1 := s.startSecond()
	s.blocks(10)

	all := readGrpc(s, opened(g0.client.StreamLogs(streamMD(context.Background()), &bdsevm.StreamLogsRequest{Filter: &bdsevm.LogFilter{}})))
	ctxA, cutA := context.WithCancel(context.Background())
	a := readGrpc(s, opened(g0.client.StreamLogs(streamMD(ctxA), &bdsevm.StreamLogsRequest{Filter: filterOf(chainsim.Filters[0])})))
	require.Eventually(t, func() bool { n, _ := completeBlocks(a.all()); return n >= 6 }, 20*time.Second, 20*time.Millisecond, "logs flow")
	cutA()
	<-a.done
	runA := a.all()
	_, cursor := completeBlocks(runA)
	s.reorg(3)
	s.blocks(5)

	from := cursor.Number
	b := readGrpc(s, opened(g1.client.StreamLogs(streamMD(context.Background()), &bdsevm.StreamLogsRequest{
		Filter: filterOf(chainsim.Filters[0]), FromBlock: &from, FromHash: cursor.Hash, HeartbeatSeconds: new(uint32(0))})))
	require.Eventually(t, func() bool { n, _ := completeBlocks(b.all()); return n >= 8 }, 20*time.Second, 20*time.Millisecond, "the resumed stream flows")
	s.reorg(2)
	s.chain.BigBlocks(1400)
	s.blocks(3)
	s.chain.BigBlocks(0)
	s.blocks(6)

	runB := b.all()
	require.NotNil(t, runB[0].m.GetUndo(), "the resume past a reverted cursor starts with an Undo: %v", runB[0].m)
	applied := applyGrpcLogs(t, runA, runB)
	applied.requireCanonical(t, s, "resumed StreamLogs", 0)
	assert.GreaterOrEqual(t, len(applied.undos), 2, "an Undo at resume and one for the live reorg")
	for _, m := range runB {
		if blk := m.m.GetBlock(); blk != nil {
			assert.True(t, blk.Complete, "a small block is one complete message: %d", blk.Block.Number)
		}
	}
	require.NoError(t, b.ended())

	split := 0
	perBlock := map[[32]byte][]*bdsevm.BlockLogs{}
	var order [][32]byte
	for _, m := range all.all() {
		blk := m.m.GetBlock()
		if blk == nil {
			continue
		}
		h := [32]byte(blk.Block.Hash)
		if perBlock[h] == nil {
			order = append(order, h)
		}
		perBlock[h] = append(perBlock[h], blk)
		assert.LessOrEqual(t, len(blk.Logs), 1000, "block %d: at most 1,000 logs per message", blk.Block.Number)
		assert.Less(t, proto.Size(m.m), 4<<20, "block %d: each message under 4 MB", blk.Block.Number)
	}
	for _, h := range order[:len(order)-1] {
		parts := perBlock[h]
		logs := 0
		for i, p := range parts {
			assert.Equal(t, i == len(parts)-1, p.Complete, "block %d: only the last message is complete", p.Block.Number)
			logs += len(p.Logs)
		}
		if blk := s.chain.Block(h); blk != nil {
			assert.Len(t, blk.Logs(), logs, "block %d: every log, once", parts[0].Block.Number)
		}
		if len(parts) > 1 {
			split++
		}
	}
	assert.Positive(t, split, "a block of more than 1,000 logs spans messages")
}

// F1 and F2 through StreamLogs: a FINALITY_FINALIZED, REORG_NONE stream
// gets every block at or below Finalized when sent, never an Undo; a
// FINALITY_DEPTH 3 stream never sees a 2-deep reorg and gets one Undo for
// an 8-deep one; both hold the canonical logs. REORG_NONE without
// FINALITY_FINALIZED is refused, naming reorg.
func TestGrpcStreamLogs_Finality(t *testing.T) {
	s := startFinalitySeed(t, 223, chainsim.Faults{})
	g := s.startGrpc(0, s.srv, nil, s.nw.Load())
	s.blocks(chainsim.FinalizedLag)
	ctx := streamMD(context.Background())

	stream, err := g.client.StreamLogs(ctx, &bdsevm.StreamLogsRequest{Filter: &bdsevm.LogFilter{}, Reorg: bdsevm.Reorg_REORG_NONE})
	require.NoError(t, err)
	_, err = stream.Recv()
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
	assert.Contains(t, status.Convert(err).Message(), "reorg")

	final := readGrpc(s, opened(g.client.StreamLogs(ctx, &bdsevm.StreamLogsRequest{Filter: filterOf(chainsim.Filters[0]),
		Finality: bdsevm.Finality_FINALITY_FINALIZED, Reorg: bdsevm.Reorg_REORG_NONE})))
	depth := readGrpc(s, opened(g.client.StreamLogs(ctx, &bdsevm.StreamLogsRequest{Filter: filterOf(chainsim.Filters[1]),
		Finality: bdsevm.Finality_FINALITY_DEPTH, Depth: 3})))
	s.blocks(15)
	s.reorg(2)
	s.blocks(10)
	s.reorg(8)
	s.blocks(15)

	f := applyGrpcLogs(t, final.all())
	f.requireCanonical(t, s, "finalized StreamLogs", 0)
	assert.Empty(t, f.undos, "a final stream never gets an Undo")
	assert.GreaterOrEqual(t, len(f.blockEnds), 25, "finalized logs flow")
	for _, m := range final.all() {
		if blk := m.m.GetBlock(); blk != nil {
			assert.LessOrEqual(t, blk.Block.Number+chainsim.FinalizedLag, m.tip, "block %d at or below Finalized when sent", blk.Block.Number)
		}
	}
	d := applyGrpcLogs(t, depth.all())
	d.requireCanonical(t, s, "depth:3 StreamLogs", 1)
	assert.Len(t, d.undos, 1, "only the 8-deep reorg reaches a depth:3 stream")
	for _, m := range depth.all() {
		if blk := m.m.GetBlock(); blk != nil {
			assert.LessOrEqual(t, blk.Block.Number+3, m.tip, "block %d at least 3 below the tip", blk.Block.Number)
		}
	}
	require.NoError(t, final.ended())
	require.NoError(t, depth.ended())
}

// G4–G7 on StreamBlocks (streams spec §3.4, §3.6, §6, §10): a request with
// no field from 5 on gets header messages only, each exactly the header
// field; FINALITY_FINALIZED headers are at or below Finalized; a cursor
// below the window ends OUT_OF_RANGE with an ErrorInfo naming the oldest
// block; bad options are INVALID_ARGUMENT naming the field. The 101st
// stream on a connection is RESOURCE_EXHAUSTED. Through a proxy that cuts
// connections idle 3s and ignores PING, a quiet chain keeps a stream with
// heartbeatSeconds open on heartbeats and cuts one without; the server
// announces fixed 4 MiB stream and 16 MiB connection windows. With every
// vendor down the heartbeat stream reads DEGRADED, then OK.
func TestGrpcStreamBlocks_FinalityHeartbeatsAndLimits(t *testing.T) {
	s := startFinalitySeed(t, 227, chainsim.Faults{})
	g := s.startGrpc(0, s.srv, nil, s.nw.Load())
	s.blocks(chainsim.FinalizedLag)
	ctx, cancel := context.WithCancel(streamMD(context.Background()))
	defer cancel()

	// G5: header-only messages, the bytes of the pre-oneof message.
	raw, err := g.conn.NewStream(ctx, &bdsevm.StreamService_ServiceDesc.Streams[0], "/bds.evm.StreamService/StreamBlocks",
		grpc.ForceCodecV2(rawCodec{encoding.GetCodecV2("proto")}))
	require.NoError(t, err)
	require.NoError(t, raw.SendMsg(&bdsevm.StreamBlocksRequest{}))
	require.NoError(t, raw.CloseSend())
	var prev uint64
	for range 3 {
		var b []byte
		require.NoError(t, raw.RecvMsg(&b))
		num, typ, n := protowire.ConsumeTag(b)
		require.Equal(t, protowire.Number(1), num)
		require.Equal(t, protowire.BytesType, typ)
		inner, m := protowire.ConsumeBytes(b[n:])
		require.Equal(t, len(b), n+m, "the message holds the header field alone")
		var h bdsevm.BlockHeader
		require.NoError(t, proto.Unmarshal(inner, &h))
		again, err := proto.Marshal(&h)
		require.NoError(t, err)
		assert.Equal(t, inner, again)
		if prev != 0 {
			assert.Equal(t, prev+1, h.Number)
		}
		prev = h.Number
	}

	// G6: finalized headers; OUT_OF_RANGE with ErrorInfo.
	fin := readGrpc(s, opened(g.client.StreamBlocks(ctx, &bdsevm.StreamBlocksRequest{Finality: bdsevm.Finality_FINALITY_FINALIZED})))
	require.Eventually(t, func() bool { return len(fin.all()) >= 5 }, 20*time.Second, 20*time.Millisecond, "finalized headers flow")
	for _, m := range fin.all() {
		h := m.m.GetHeader()
		require.NotNil(t, h, "header messages only without heartbeatSeconds")
		assert.LessOrEqual(t, h.Number+chainsim.FinalizedLag, m.tip, "header %d at or below Finalized when sent", h.Number)
	}
	// Below the window and below the final hold the finalized stream keeps.
	old := s.chain.CanonAt(s.chain.Tip().Number - chainsim.FinalizedLag - 20)
	_, err = openOnce(g, resumeFrom(old.Number, old.Hash), 10*time.Second)
	require.Equal(t, codes.OutOfRange, status.Code(err), "%v", err)
	var info *errdetails.ErrorInfo
	for _, d := range status.Convert(err).Details() {
		if e, ok := d.(*errdetails.ErrorInfo); ok {
			info = e
		}
	}
	require.NotNil(t, info, "OUT_OF_RANGE carries an ErrorInfo: %v", err)
	assert.Equal(t, "below_window", info.Reason)
	oldest, err := strconv.ParseUint(info.Metadata["oldestBlock"], 10, 64)
	require.NoError(t, err, "oldestBlock %q", info.Metadata["oldestBlock"])
	assert.Greater(t, oldest, old.Number)
	assert.Equal(t, s.chain.CanonAt(oldest).Hash.Hex(), info.Metadata["oldestHash"])

	for _, tc := range []struct {
		req   *bdsevm.StreamBlocksRequest
		field string
	}{
		{&bdsevm.StreamBlocksRequest{Depth: 3}, "depth"},
		{&bdsevm.StreamBlocksRequest{Finality: bdsevm.Finality_FINALITY_DEPTH}, "depth"},
		{&bdsevm.StreamBlocksRequest{Finality: bdsevm.Finality_FINALITY_DEPTH, Depth: uint32(chaosReorgWindow + 1)}, "depth"},
		{&bdsevm.StreamBlocksRequest{Finality: 9}, "finality"},
		{&bdsevm.StreamBlocksRequest{HeartbeatSeconds: new(uint32(31))}, "heartbeatSeconds"},
	} {
		_, err := openOnce(g, tc.req, 5*time.Second)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "%v: %v", tc.req, err)
		assert.Contains(t, status.Convert(err).Message(), "invalid "+tc.field, "%v names the field", tc.req)
	}

	// G7: at most 100 long-lived streams per connection.
	capConn := dialGrpc(t, g.addr)
	capClient := bdsevm.NewStreamServiceClient(capConn)
	rejected := map[string]string{"project": "test_ws", "reason": "per_connection"}
	before, _ := gathered(t, telemetry.MetricGrpcStreamsRejectedTotal, rejected)
	streams := make([]context.CancelFunc, 0, 100)
	for i := range 100 {
		sctx, scancel := context.WithCancel(ctx)
		streams = append(streams, scancel)
		st, err := capClient.StreamBlocks(sctx, &bdsevm.StreamBlocksRequest{})
		require.NoError(t, err)
		_, err = st.Recv()
		require.NoError(t, err, "stream %d is admitted", i+1)
	}
	st, err := capClient.StreamBlocks(ctx, &bdsevm.StreamBlocksRequest{})
	require.NoError(t, err)
	_, err = st.Recv()
	assert.Equal(t, codes.ResourceExhausted, status.Code(err), "the 101st stream: %v", err)
	after, _ := gathered(t, telemetry.MetricGrpcStreamsRejectedTotal, rejected)
	assert.Equal(t, before+1, after)
	streams[0]()
	require.Eventually(t, func() bool {
		st, err := capClient.StreamBlocks(ctx, &bdsevm.StreamBlocksRequest{})
		if err != nil {
			return false
		}
		_, err = st.Recv()
		return err == nil
	}, 10*time.Second, 100*time.Millisecond, "a closed stream frees its place")
	for _, c := range streams {
		c()
	}

	// G4: heartbeats are DATA messages, which an idle proxy counts.
	p, err := h2idle.Start(g.addr, 3*time.Second)
	require.NoError(t, err)
	defer p.Close()
	s.chain.Halt(true)
	s.blocks(2)
	beating := readGrpc(s, opened(bdsevm.NewStreamServiceClient(dialGrpc(t, p.Addr())).StreamBlocks(ctx, &bdsevm.StreamBlocksRequest{HeartbeatSeconds: new(uint32(1))})))
	silent := readGrpc(s, opened(bdsevm.NewStreamServiceClient(dialGrpc(t, p.Addr())).StreamBlocks(ctx, &bdsevm.StreamBlocksRequest{})))
	time.Sleep(8 * time.Second)
	require.NoError(t, beating.ended(), "heartbeats keep the stream open through the idle proxy")
	beats := 0
	for _, m := range beating.all() {
		if hb := m.m.GetHeartbeat(); hb != nil {
			beats++
			assert.NotZero(t, hb.Head.GetNumber())
			assert.Len(t, hb.Head.GetHash(), 32)
			assert.Positive(t, hb.Finalized)
		}
	}
	assert.GreaterOrEqual(t, beats, 5, "a heartbeat each idle second")
	select {
	case <-silent.done:
	case <-time.After(5 * time.Second):
	}
	assert.Equal(t, codes.Unavailable, status.Code(silent.ended()), "the idle proxy cuts a stream without heartbeats: %v", silent.ended())
	assert.Positive(t, p.Cuts())
	streamWindow, connIncrement := p.ServerWindows()
	assert.EqualValues(t, 4<<20, streamWindow, "fixed 4 MiB stream window")
	assert.EqualValues(t, 16<<20-65535, connIncrement, "fixed 16 MiB connection window")

	// Status: degraded while no vendor answers, then ok.
	states := func() (out []bdsevm.StreamStatus_State) {
		for _, m := range beating.all() {
			if st := m.m.GetStatus(); st != nil {
				out = append(out, st.State)
			}
		}
		return out
	}
	// A halted chain keeps its hash voters: no lag reference moves.
	s.chain.Halt(false)
	s.setAll(func(f *chainsim.Faults) { f.Down = true })
	require.Eventually(t, func() bool {
		return assert.ObjectsAreEqual([]bdsevm.StreamStatus_State{bdsevm.StreamStatus_STATE_DEGRADED}, states())
	},
		30*time.Second, 50*time.Millisecond, "DEGRADED with no hash voter: %v", states())
	s.setAll(func(f *chainsim.Faults) { f.Down = false })
	require.Eventually(t, func() bool { return len(states()) == 2 && states()[1] == bdsevm.StreamStatus_STATE_OK },
		30*time.Second, 50*time.Millisecond, "OK once vendors answer: %v", states())
	require.NoError(t, beating.ended())
}

// G3 (streams spec §6.2): StreamPendingTransactions sends hashes, or with
// fullTransactions whole transactions, in batches of at most 1,000; a ws
// subscriber of the same params shares the variant, so each push source
// holds one upstream subscription per variant.
func TestGrpcStreamPending_HashesAndFull(t *testing.T) {
	s := startFinalitySeed(t, 229, chainsim.Faults{})
	g := s.startGrpc(0, s.srv, nil, s.nw.Load())
	c, _, err := dialStream(t, s, "", nil)
	require.NoError(t, err)
	ws := c.subscribe("newPendingTransactions")
	ctx := streamMD(context.Background())
	hashes := readGrpc(s, opened(g.client.StreamPendingTransactions(ctx, &bdsevm.StreamPendingTransactionsRequest{})))
	full := readGrpc(s, opened(g.client.StreamPendingTransactions(ctx, &bdsevm.StreamPendingTransactionsRequest{FullTransactions: true, HeartbeatSeconds: new(uint32(1))})))
	s.blocks(15)

	got := map[gethcommon.Hash]bool{}
	for _, m := range hashes.all() {
		b := m.m.GetTransactions()
		require.NotNil(t, b, "hash batches only without heartbeatSeconds: %v", m.m)
		assert.NotEmpty(t, b.Hashes)
		assert.LessOrEqual(t, len(b.Hashes), 1000)
		assert.Empty(t, b.Transactions)
		for _, h := range b.Hashes {
			hh := gethcommon.BytesToHash(h)
			assert.True(t, s.chain.Pushed(hh), "hash %s was pushed", hh)
			got[hh] = true
		}
	}
	require.NotEmpty(t, got, "hashes flow")
	wsSeen := 0
	for _, f := range c.of(ws) {
		var h string
		if f.method == "eth_subscription" && json.Unmarshal(f.data, &h) == nil {
			wsSeen++
		}
	}
	assert.Positive(t, wsSeen, "the ws subscriber of the variant gets them too")
	fullTxs := 0
	for _, m := range full.all() {
		b := m.m.GetTransactions()
		if b == nil {
			continue
		}
		assert.Empty(t, b.Hashes)
		assert.LessOrEqual(t, len(b.Transactions), 1000)
		for _, tx := range b.Transactions {
			fullTxs++
			assert.True(t, s.chain.Pushed(gethcommon.BytesToHash(tx.Hash)), "transaction %x was pushed", tx.Hash)
			assert.Equal(t, chainsim.Addrs[1].Bytes(), tx.From)
		}
	}
	assert.Positive(t, fullTxs, "full transactions flow")
	for _, v := range s.vendors {
		if v.WS {
			assert.LessOrEqual(t, v.Stats().PendingSubscribes, int64(2), "%s: one upstream subscription per variant", v.ID)
		}
	}
	require.NoError(t, hashes.ended())
	require.NoError(t, full.ended())
}
