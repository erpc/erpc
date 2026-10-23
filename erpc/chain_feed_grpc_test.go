package erpc

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/test/chainsim"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// chaosStreamPad pads the gRPC seed's headers to about 16 KB, so a dozen
// headers fill a stream's 64 KB flow-control window and its send quota.
const chaosStreamPad = 16 << 10

// chaosStreams is how many background stream clients the gRPC seed runs.
const chaosStreams = 3

// streamPinGrace is how long after its block left the sending instance's
// pins a header may still arrive: it was checked before its send.
var streamPinGrace = 2 * chaosBlock

// grpcInstance is one eRPC instance with a gRPC server that the gRPC
// seed's streams connect to. srv and cancel are set when the instance is
// the second one, which it owns; the primary's are the seed's.
type grpcInstance struct {
	name       string
	nw         *Network
	addr       string
	conn       *grpc.ClientConn
	client     bdsevm.StreamServiceClient
	stopServer func()
	srv        *HttpServer
	cancel     context.CancelFunc

	pinsMu sync.Mutex
	left   map[[32]byte]time.Time // when a block left the pins, while it stays out
}

// startGrpc serves the gRPC server of instance i on srv's eRPC; own,
// when set, makes the instance own srv and its network.
func (s *chaosSeed) startGrpc(i int, srv *HttpServer, own context.CancelFunc, nw *Network) *grpcInstance {
	addr, stopServer := serveGrpc(s.t, context.Background(), srv.erpc)
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(s.t, err)
	g := &grpcInstance{name: fmt.Sprintf("instance-%d", i), nw: nw, addr: addr, conn: conn,
		client: bdsevm.NewStreamServiceClient(conn), stopServer: stopServer, left: make(map[[32]byte]time.Time)}
	go g.watchPins()
	if own != nil {
		g.srv, g.cancel = srv, own
	}
	s.grpcMu.Lock()
	s.grpcs[i] = g
	s.grpcMu.Unlock()
	return g
}

// watchPins records when each block leaves the instance's pins, until the
// instance's network stops.
func (g *grpcInstance) watchPins() {
	tr := g.nw.chainTracker
	pinned := make(map[indexer.Cursor]bool)
	for {
		changed := tr.Changed()
		now := time.Now()
		for c := range pinned {
			if !tr.Pinned(c) {
				delete(pinned, c)
				g.pinsMu.Lock()
				g.left[c.Hash] = now
				g.pinsMu.Unlock()
			}
		}
		// New pins come from the head down to the first pin seen before.
		if tip, ok := tr.Tip(); ok {
			for n := tip.Number; ; n-- {
				hx, ok := tr.HashAt(int64(n))
				raw, err := hex.DecodeString(strings.TrimPrefix(hx, "0x"))
				if !ok || err != nil || len(raw) != 32 {
					break
				}
				c := indexer.Cursor{Number: n, Hash: [32]byte(raw)}
				if pinned[c] || !tr.Pinned(c) {
					break
				}
				pinned[c] = true
				g.pinsMu.Lock()
				delete(g.left, c.Hash)
				g.pinsMu.Unlock()
				if n == 0 {
					break
				}
			}
		}
		select {
		case <-g.nw.appCtx.Done():
			return
		case <-changed:
		}
	}
}

// leftPins is when block h left the instance's pins, if it is out of them.
func (g *grpcInstance) leftPins(h [32]byte) (time.Time, bool) {
	g.pinsMu.Lock()
	defer g.pinsMu.Unlock()
	at, ok := g.left[h]
	return at, ok
}

// startSecond boots the second eRPC instance and its gRPC server.
func (s *chaosSeed) startSecond() *grpcInstance {
	srv, _, cancel, nw := s.bootERPC()
	return s.startGrpc(1, srv, cancel, nw)
}

// stopGrpc stops instance i's gRPC server, which ends its streams, and the
// instance itself when it owns it.
func (s *chaosSeed) stopGrpc(i int) {
	s.grpcMu.Lock()
	g := s.grpcs[i]
	s.grpcs[i] = nil
	s.grpcMu.Unlock()
	if g == nil {
		return
	}
	g.stopServer()
	_ = g.conn.Close()
	if g.srv != nil {
		_ = g.srv.Shutdown(&zerolog.Logger{})
		g.cancel()
		g.nw.wait()
	}
}

func (s *chaosSeed) grpcAt(i int) *grpcInstance {
	s.grpcMu.Lock()
	defer s.grpcMu.Unlock()
	return s.grpcs[i%len(s.grpcs)]
}

func streamMD(ctx context.Context) context.Context {
	return metadata.NewOutgoingContext(ctx, metadata.New(map[string]string{"x-erpc-project": "test_ws", "x-erpc-chain-id": "123"}))
}

func resumeFrom(n uint64, h [32]byte) *bdsevm.StreamBlocksRequest {
	return &bdsevm.StreamBlocksRequest{FromBlock: &n, FromHash: h[:]}
}

// byNumberAsked counts eth_getBlockByNumber requests through the request
// path of either instance.
func byNumberAsked() float64 {
	return metricSum(telemetry.MetricNetworkRequestsReceived, chaosLabels("category", "eth_getBlockByNumber"))
}

// startStreams boots the second instance and the background streams.
func (s *chaosSeed) startStreams(ctx context.Context, stop context.CancelFunc) {
	s.startSecond()
	s.stopStreams = stop
	s.byNumberAsked = byNumberAsked()
	for i := range chaosStreams {
		st := &chaosStream{name: fmt.Sprintf("stream-%d", i), chain: make(map[uint64][32]byte),
			rng: rand.New(rand.NewPCG(uint64(s.seed), uint64(10+i)))}
		s.streams = append(s.streams, st)
		s.streamsWG.Add(1)
		go func() {
			defer s.streamsWG.Done()
			st.run(ctx, s, i)
		}()
	}
}

// chaosStream is one gRPC StreamBlocks client. It applies each header by
// the gRPC client rule: a number at or below its last replaces that number
// and every number above. Each session ends at random, or with the
// server's UNAVAILABLE, and the next resumes from its last header on the
// other instance.
type chaosStream struct {
	name string
	rng  *rand.Rand

	mu          sync.Mutex
	chain       map[uint64][32]byte
	first, last uint64
	opens       int
	resumes     int
	moves       int // resumes on another instance than the last header's
	unavailable int
	internal    int // INTERNAL ends while a header that cannot be encoded is pinned
	resends     []streamResend
	problems    []string
	lastFrom    int
}

// streamResend is a header at or below the stream's last: from is its
// number, the first block after the fork point.
type streamResend struct {
	at   time.Time
	from uint64
	last uint64
}

func (st *chaosStream) problem(format string, args ...interface{}) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.problems = append(st.problems, fmt.Sprintf(format, args...))
}

func (st *chaosStream) run(ctx context.Context, s *chaosSeed, at int) {
	for ctx.Err() == nil {
		g := s.grpcAt(at)
		if g == nil {
			at++
			time.Sleep(chaosBlock / 4)
			continue
		}
		st.session(ctx, s, g, at%len(s.grpcs))
		at++
	}
}

func (st *chaosStream) session(ctx context.Context, s *chaosSeed, g *grpcInstance, idx int) {
	life := time.Duration(3+st.rng.IntN(8)) * chaosBlock
	sctx, cancel := context.WithTimeout(ctx, life)
	defer cancel()
	req := &bdsevm.StreamBlocksRequest{}
	st.mu.Lock()
	st.opens++
	if st.last != 0 {
		req = resumeFrom(st.last, st.chain[st.last])
		st.resumes++
		if idx != st.lastFrom {
			st.moves++
		}
	}
	st.mu.Unlock()
	stream, err := g.client.StreamBlocks(streamMD(sctx), req)
	for err == nil {
		var resp *bdsevm.StreamBlocksResponse
		if resp, err = stream.Recv(); err == nil {
			st.apply(s, resp.GetHeader(), g, idx)
		}
	}
	switch status.Code(err) {
	case codes.Canceled, codes.DeadlineExceeded:
	case codes.Unavailable:
		st.mu.Lock()
		st.unavailable++
		st.mu.Unlock()
		time.Sleep(chaosBlock / 4)
	case codes.Internal:
		if !s.internalAllowed(time.Now()) {
			st.problem("session on %s ended with %v", g.name, err)
			return
		}
		st.mu.Lock()
		st.internal++
		st.mu.Unlock()
		time.Sleep(chaosBlock / 4)
	default:
		if ctx.Err() == nil {
			st.problem("session on %s ended with %v", g.name, err)
		}
	}
}

// internalAllowed reports whether a stream may end with INTERNAL at at.
func (s *chaosSeed) internalAllowed(at time.Time) bool {
	s.grpcMu.Lock()
	defer s.grpcMu.Unlock()
	for _, iv := range s.internalOK {
		if !at.Before(iv.from) && (iv.to.IsZero() || !at.After(iv.to)) {
			return true
		}
	}
	return false
}

// apply applies a header g sent; one that arrives more than streamPinGrace
// after its block left g's pins is a problem.
func (st *chaosStream) apply(s *chaosSeed, h *bdsevm.BlockHeader, g *grpcInstance, idx int) {
	if len(h.GetHash()) != 32 || len(h.GetParentHash()) != 32 {
		st.problem("malformed header %d", h.GetNumber())
		return
	}
	var hash, parent [32]byte
	copy(hash[:], h.Hash)
	copy(parent[:], h.ParentHash)
	n := h.Number
	if left, ok := g.leftPins(hash); ok {
		if late := time.Since(left); late > streamPinGrace {
			st.problem("header %d %x arrived %s after it left %s's pins", n, hash[:4], late.Round(time.Millisecond), g.name)
		}
	}
	b := s.chain.Block(hash)
	st.mu.Lock()
	defer st.mu.Unlock()
	switch {
	case b == nil:
		st.problems = append(st.problems, fmt.Sprintf("header %d %x: no vendor ever served it", n, hash[:4]))
	case b.Minority():
		st.problems = append(st.problems, fmt.Sprintf("header %d %x: a minority fork reached the stream", n, hash[:4]))
	case b.Number != n:
		st.problems = append(st.problems, fmt.Sprintf("header %x: number %d, block %d", hash[:4], n, b.Number))
	case b.Parent != nil && b.Parent.Hash != parent:
		st.problems = append(st.problems, fmt.Sprintf("header %d: parentHash %x, block's parent %x", n, parent[:4], b.Parent.Hash[:4]))
	}
	if st.last != 0 {
		if n > st.last+1 {
			st.problems = append(st.problems, fmt.Sprintf("gap %d → %d", st.last, n))
		}
		if n <= st.last {
			st.resends = append(st.resends, streamResend{at: time.Now(), from: n, last: st.last})
			for k := range st.chain {
				if k >= n {
					delete(st.chain, k)
				}
			}
		}
		if below, ok := st.chain[n-1]; ok && below != parent {
			st.problems = append(st.problems, fmt.Sprintf("header %d does not link to the %x the client holds at %d", n, below[:4], n-1))
		}
	}
	st.chain[n] = hash
	if st.first == 0 {
		st.first = n
	}
	st.last = n
	st.lastFrom = idx
}

// awaitStreams waits until every stream holds the canonical head, then
// stops them.
func (s *chaosSeed) awaitStreams(tip *chainsim.Block) {
	require.Eventually(s.t, func() bool {
		for _, st := range s.streams {
			st.mu.Lock()
			last, h := st.last, st.chain[st.last]
			st.mu.Unlock()
			if last != tip.Number || h != tip.Hash {
				return false
			}
		}
		return true
	}, 15*time.Second, 20*time.Millisecond, "every stream reaches the canonical head %d", tip.Number)
	s.stopStreams()
	s.streamsWG.Wait()
}

// judgeStreams: each stream's chain equals the canonical chain from its
// first header on, and no stream sent eth_getBlockByNumber through the
// request path.
func (s *chaosSeed) judgeStreams() {
	t := s.t
	for _, st := range s.streams {
		st.mu.Lock()
		require.Empty(t, st.problems, "stream %s", st.name)
		require.NotZero(t, st.first, "stream %s got no header", st.name)
		for n := st.first; n <= st.last; n++ {
			want := s.chain.CanonAt(n)
			got, ok := st.chain[n]
			require.True(t, ok, "stream %s misses block %d (first %d, last %d)", st.name, n, st.first, st.last)
			require.NotNil(t, want)
			require.Equal(t, hex.EncodeToString(want.Hash[:]), hex.EncodeToString(got[:]), "stream %s block %d", st.name, n)
		}
		t.Logf("stream %s: blocks %d..%d over %d sessions, %d resumes (%d on the other instance), %d UNAVAILABLE, %d INTERNAL, %d resends",
			st.name, st.first, st.last, st.opens, st.resumes, st.moves, st.unavailable, st.internal, len(st.resends))
		st.mu.Unlock()
	}
	require.Zero(t, byNumberAsked()-s.byNumberAsked, "streams send no eth_getBlockByNumber through the request path")
}

// streamResume waits until every stream resumed on the other instance.
func (s *chaosSeed) streamResume() {
	require.Eventually(s.t, func() bool {
		for _, st := range s.streams {
			st.mu.Lock()
			moves := st.moves
			st.mu.Unlock()
			if moves < 2 {
				return false
			}
		}
		return true
	}, 10*time.Second, 20*time.Millisecond, "every stream resumes on the other instance twice")
	var got []string
	for _, st := range s.streams {
		st.mu.Lock()
		got = append(got, fmt.Sprintf("%s %d moves at %d", st.name, st.moves, st.last))
		st.mu.Unlock()
	}
	s.occur(fStreamResume, strings.Join(got, ", "))
}

// awaitResend waits for a stream that resends from first, the block after
// a reorg's fork point, after since.
func (s *chaosSeed) awaitResend(first uint64, since time.Time) string {
	var who string
	require.Eventually(s.t, func() bool {
		for _, st := range s.streams {
			st.mu.Lock()
			for _, r := range st.resends {
				if r.from == first && r.at.After(since) {
					who = fmt.Sprintf("%s resent from %d after %d", st.name, r.from, r.last)
				}
			}
			st.mu.Unlock()
		}
		return who != ""
	}, 10*chaosBlock, 10*time.Millisecond, "a stream gets the reorg as a resend from block %d", first)
	return who
}

// openOnce opens one stream from req on g and reads it until it ends or
// within; it returns the headers it got and how the stream ended.
func openOnce(g *grpcInstance, req *bdsevm.StreamBlocksRequest, within time.Duration) ([]*bdsevm.BlockHeader, error) {
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	stream, err := g.client.StreamBlocks(streamMD(ctx), req)
	var got []*bdsevm.BlockHeader
	for err == nil {
		var resp *bdsevm.StreamBlocksResponse
		if resp, err = stream.Recv(); err == nil {
			got = append(got, resp.Header)
		}
	}
	return got, err
}

// streamOutOfRange resumes from a canonical header older than the window,
// by header time and by reorgWindow, and from an orphan no vendor ever
// served, on either instance: each ends with OUT_OF_RANGE and sends
// nothing. A resume with one field alone is INVALID_ARGUMENT.
func (s *chaosSeed) streamOutOfRange() {
	tip := s.chain.Tip()
	// Header times are whole seconds: 2s clear of the window's edge.
	edge := tip.Time - uint64(s.maxBackfill()/time.Second) - 2
	var old *chainsim.Block
	for b := tip; b != nil && old == nil; b = b.Parent {
		if b.Time < edge && b.Number+uint64(chaosReorgWindow) < tip.Number {
			old = b
		}
	}
	require.NotNil(s.t, old, "the chain holds a header older than the window")
	orphan := s.chain.Orphan(s.chain.CanonAt(tip.Number - 3))
	var got []string
	for _, cur := range []struct {
		name string
		b    *chainsim.Block
	}{{"older than the window", old}, {"an orphan no vendor serves", orphan}} {
		g := s.grpcAt(s.rng.IntN(2))
		require.NotNil(s.t, g)
		sent, err := openOnce(g, resumeFrom(cur.b.Number, cur.b.Hash), 10*time.Second)
		require.Equal(s.t, codes.OutOfRange, status.Code(err), "resume from %s (%d, head %d) on %s: %v", cur.name, cur.b.Number, tip.Number, g.name, err)
		require.Empty(s.t, sent, "resume from %s sends nothing", cur.name)
		got = append(got, fmt.Sprintf("%s %d on %s", cur.name, cur.b.Number, g.name))
	}
	n := tip.Number
	_, err := openOnce(s.grpcAt(0), &bdsevm.StreamBlocksRequest{FromBlock: &n}, 5*time.Second)
	require.Equal(s.t, codes.InvalidArgument, status.Code(err), "fromBlock without fromHash: %v", err)
	s.occur(fStreamOutOfRange, "OUT_OF_RANGE for "+strings.Join(got, ", "))
}

// streamAhead restarts the second instance while the chain stands still
// and every vendor holds its block answers, so the fresh instance holds no
// header and no source is live there. A resume there from the canonical
// head that waits past the read wait ends with UNAVAILABLE, sending
// nothing; one opened later waits, sending nothing, then continues with
// the next blocks once vendors answer.
func (s *chaosSeed) streamAhead() {
	from := time.Now()
	s.stopGrpc(1)
	s.chain.Halt(true)
	s.setAll(func(f *chainsim.Faults) { f.Held = true })
	tip := s.chain.Tip()
	release := func() {
		s.setAll(func(f *chainsim.Faults) { f.Held = false })
		s.chain.Halt(false)
	}
	defer release()
	g := s.startSecond()
	tr := g.nw.chainTracker
	if head, ok := tr.Tip(); ok {
		require.Less(s.t, head.Number, tip.Number, "the fresh instance's head is below the cursor")
	}

	type stallEnd struct {
		got []*bdsevm.BlockHeader
		err error
		in  time.Duration
	}
	readWait := streamBlocksReadWait.Load()
	stalled := make(chan stallEnd, 1)
	go func() {
		opened := time.Now()
		got, err := openOnce(g, resumeFrom(tip.Number, tip.Hash), readWait+10*chaosBlock)
		stalled <- stallEnd{got, err, time.Since(opened)}
	}()
	time.Sleep(readWait - 5*chaosBlock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := g.client.StreamBlocks(streamMD(ctx), resumeFrom(tip.Number, tip.Hash))
	require.NoError(s.t, err)
	got := make(chan *bdsevm.BlockHeader, 16)
	ended := make(chan error, 1)
	go func() {
		for {
			resp, err := stream.Recv()
			if err != nil {
				ended <- err
				return
			}
			got <- resp.Header
		}
	}()
	select {
	case h := <-got:
		s.t.Fatalf("the fresh instance sent %d before its head reached the cursor %d", h.Number, tip.Number)
	case err := <-ended:
		s.t.Fatalf("the resume on the fresh instance ended while it waited: %v", err)
	case <-time.After(4 * chaosBlock):
	}
	st := <-stalled
	require.Equal(s.t, codes.Unavailable, status.Code(st.err), "a resume that waits past the read wait with vendors held: %v", st.err)
	require.Contains(s.t, status.Convert(st.err).Message(), errStreamBlocksStalled.Error())
	require.Empty(s.t, st.got, "the stalled resume sends nothing")
	require.GreaterOrEqual(s.t, st.in, readWait, "the stalled resume waits the read wait")
	_, _, held := s.hook.firstHeader(tr)
	release()
	s.addQuiet(interval{from, time.Now()})
	want := tip
	for range 2 {
		select {
		case h := <-got:
			require.Equal(s.t, want.Number+1, h.Number, "the resume continues after the cursor")
			require.Equal(s.t, hex.EncodeToString(want.Hash[:]), hex.EncodeToString(h.ParentHash), "block %d links to the cursor's branch", h.Number)
			want = s.chain.Block(chainsim.Hash(h.Hash))
			require.NotNil(s.t, want)
		case err := <-ended:
			s.t.Fatalf("the resume on the fresh instance ended: %v", err)
		case <-time.After(10 * time.Second):
			s.t.Fatalf("the resume on the fresh instance sent nothing after %d", want.Number)
		}
	}
	s.occur(fStreamAhead, fmt.Sprintf("resumed at %d on a fresh instance (held a header while it waited: %t); then %d; a resume held %s ended UNAVAILABLE",
		tip.Number, held, want.Number, st.in.Round(time.Millisecond)))
}

// streamSlow resumes a stream from a dozen blocks back on a client with the
// smallest flow-control windows, and stops reading: the replay fills the
// windows, the server's send blocks, and the stream ends with UNAVAILABLE
// within the send wait.
func (s *chaosSeed) streamSlow() {
	g := s.grpcAt(s.rng.IntN(2))
	require.NotNil(s.t, g)
	conn, err := grpc.NewClient(g.addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithInitialWindowSize(64<<10), grpc.WithInitialConnWindowSize(64<<10))
	require.NoError(s.t, err)
	defer conn.Close()
	cur := s.chain.CanonAt(s.chain.Tip().Number - 12)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opened := time.Now()
	stream, err := bdsevm.NewStreamServiceClient(conn).StreamBlocks(streamMD(ctx), resumeFrom(cur.Number, cur.Hash))
	require.NoError(s.t, err)
	// Wait for the headers to arrive, then hold them unread past the send wait.
	_, err = stream.Header()
	require.NoError(s.t, err)
	time.Sleep(streamBlocksSendWait.Load() + 5*chaosBlock)
	read := 0
	for {
		if _, err = stream.Recv(); err != nil {
			break
		}
		read++
	}
	require.Equal(s.t, codes.Unavailable, status.Code(err), "a client that stops reading ends with UNAVAILABLE: %v", err)
	require.Contains(s.t, status.Convert(err).Message(), errStreamBlocksSlow.Error())
	require.Less(s.t, read, 12, "the stream ended before it sent the replay")
	s.occur(fStreamSlow, fmt.Sprintf("UNAVAILABLE after %d headers, read %s after open", read, time.Since(opened).Round(time.Millisecond)))
}

// streamBadHeader makes the canonical tip, for a moment, a block whose
// gasLimit does not fit in uint64, so StreamBlocks cannot encode it: on each
// instance a resume from its parent ends with INTERNAL and sends nothing,
// and so does the next one, as the encoding's error is kept. A 1-block
// reorg then replaces it.
func (s *chaosSeed) streamBadHeader() {
	s.chain.Halt(true)
	parent := s.chain.Tip()
	s.grpcMu.Lock()
	s.internalOK = append(s.internalOK, interval{from: time.Now()})
	k := len(s.internalOK) - 1
	s.grpcMu.Unlock()
	bad := s.chain.Append(chainsim.KindEmpty, func(fields map[string]json.RawMessage) {
		fields["gasLimit"] = json.RawMessage(`"0x10000000000000000"`)
	})

	bc := indexer.Cursor{Number: bad.Number, Hash: bad.Hash}
	var got []string
	for i := range 2 {
		g := s.grpcAt(i)
		require.NotNil(s.t, g)
		tr := g.nw.chainTracker
		require.Eventually(s.t, func() bool {
			send, ok := tr.Confirmed()
			return ok && send == bc
		}, 10*time.Second, 10*time.Millisecond, "%s may send block %d", g.name, bad.Number)
		for range 2 {
			sent, err := openOnce(g, resumeFrom(parent.Number, parent.Hash), 10*time.Second)
			require.Equal(s.t, codes.Internal, status.Code(err), "resume before a header that cannot be encoded, on %s: %v", g.name, err)
			require.Contains(s.t, status.Convert(err).Message(), fmt.Sprintf("cannot encode block %d", bad.Number))
			require.Empty(s.t, sent, "nothing after the previous header %d", parent.Number)
		}
		got = append(got, g.name)
	}

	s.reorg(1)
	s.chain.Halt(false)
	for i := range 2 {
		tr := s.grpcAt(i).nw.chainTracker
		require.Eventually(s.t, func() bool { return !tr.Pinned(bc) }, 10*time.Second, 10*time.Millisecond, "a reorg replaces block %d", bad.Number)
	}
	s.grpcMu.Lock()
	s.internalOK[k].to = time.Now().Add(streamPinGrace)
	s.grpcMu.Unlock()
	s.occur(fStreamBadHeader, fmt.Sprintf("block %d: INTERNAL twice on %s, nothing sent", bad.Number, strings.Join(got, ", ")))
}

// resumeOrphan resumes on g, which never held it, from orphan: a block of a
// branch a reorg replaced, that vendors still serve by hash. The stream
// starts at the fork point + 1, linked to the fork point.
func (s *chaosSeed) resumeOrphan(g *grpcInstance, orphan *chainsim.Block, fork uint64) string {
	forkAt := s.chain.CanonAt(fork)
	require.NotNil(s.t, forkAt)
	require.NotEqual(s.t, orphan, s.chain.CanonAt(orphan.Number), "block %d is an orphan", orphan.Number)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := g.client.StreamBlocks(streamMD(ctx), resumeFrom(orphan.Number, orphan.Hash))
	require.NoError(s.t, err)
	resp, err := stream.Recv()
	require.NoError(s.t, err, "resume from orphan %d on %s", orphan.Number, g.name)
	h := resp.Header
	require.Equal(s.t, fork+1, h.Number, "the resume from orphan %d starts after the fork point", orphan.Number)
	require.Equal(s.t, hex.EncodeToString(forkAt.Hash[:]), hex.EncodeToString(h.ParentHash), "block %d links to the fork point", h.Number)
	return fmt.Sprintf("resume from orphan %d on the fresh %s started at %d", orphan.Number, g.name, h.Number)
}
