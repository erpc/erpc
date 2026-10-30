package streams

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// The flow-control windows of streams spec §6.3: the receiver's window
// paces the server's sends, so the client announces the server's.
const (
	streamWindow = 4 << 20
	connWindow   = 16 << 20
)

// Reconnect backoff (spec §9.5): jittered, doubling from backoffMin to
// backoffMax, back to backoffMin after a session that lived backoffReset.
const (
	backoffMin   = 500 * time.Millisecond
	backoffMax   = 30 * time.Second
	backoffReset = 60 * time.Second
)

var (
	errWatchdog = errors.New("no message within 3 heartbeats")
	errRestart  = errors.New("the stream broke its chain linkage")
)

type eventKind int

const (
	evOpen eventKind = iota
	evBlock
	evUndo
	evStatus
	evEnd
)

// event is what one endpoint's session tells the Run loop, in order.
type event struct {
	link int
	sess uint64
	kind eventKind
	// from is the cursor an evOpen session resumed from, nil for none.
	from   *BlockRef
	block  Block
	ref    BlockRef
	status Status
	// err, on evEnd, is the terminal error the session ended with, nil
	// when it reconnects; perEndpoint: err ends Run only while no other
	// endpoint's stream is open. reason is why the session ended.
	err         error
	perEndpoint bool
	reason      string
}

// link is one endpoint: its sessions run one after another on their own
// goroutine.
type link struct {
	idx int
	ep  endpoint

	mu     sync.Mutex
	sess   uint64
	cancel context.CancelCauseFunc
}

func (l *link) begin(cancel context.CancelCauseFunc) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sess++
	l.cancel = cancel
	return l.sess
}

// restart ends session sess, if it still runs, to resume it from the
// merged cursor.
func (l *link) restart(sess uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sess == sess && l.cancel != nil {
		l.cancel(errRestart)
	}
}

// sessionEnd is how one session ended.
type sessionEnd struct {
	sess uint64
	// from is the cursor the session resumed from.
	from   *BlockRef
	err    error
	reason string
	// flowed: the session received a message.
	flowed bool
	lived  time.Duration
}

// runLink runs the endpoint's sessions until ctx ends or one ends with an
// error that ends Run (spec §9.5 Terminal). A session the library
// restarted backs off as any other, so a server that keeps breaking the
// chain linkage is not dialled in a loop.
func (r *runner) runLink(ctx context.Context, l *link) {
	attempt := 0
	for ctx.Err() == nil {
		end := r.session(ctx, l)
		if ctx.Err() != nil {
			return
		}
		t, perEndpoint := r.terminal(l, end.from, end.err)
		r.emit(ctx, event{link: l.idx, sess: end.sess, kind: evEnd, err: t, perEndpoint: perEndpoint, reason: end.reason})
		if t != nil && !perEndpoint {
			return
		}
		if end.lived >= backoffReset {
			attempt = 0
		}
		if t == nil && end.flowed && (end.reason == "watchdog" || status.Code(end.err) == codes.Unavailable) {
			continue
		}
		d := min(backoffMax, backoffMin<<min(attempt, 6))
		attempt++
		select {
		case <-ctx.Done():
			return
		case <-time.After(d/2 + rand.N(d/2+1)):
		}
	}
}

// session dials the endpoint, opens the stream from the merged cursor and
// forwards its events until it ends. Each session dials its own channel: a
// connection a black-holed link holds open is never reused.
func (r *runner) session(ctx context.Context, l *link) (end sessionEnd) {
	creds := insecure.NewCredentials()
	if l.ep.tls {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	cc, err := grpc.NewClient(l.ep.target, grpc.WithTransportCredentials(creds),
		grpc.WithInitialWindowSize(streamWindow), grpc.WithInitialConnWindowSize(connWindow))
	if err != nil {
		end.err = status.Error(codes.InvalidArgument, err.Error())
		return end
	}
	defer cc.Close()
	sctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	end.sess = l.begin(cancel)
	from := r.resume.Load()
	end.from = from
	if !r.emit(ctx, event{link: l.idx, sess: end.sess, kind: evOpen, from: from}) {
		return end
	}
	began := time.Now()
	var last atomic.Int64
	var busy atomic.Bool
	last.Store(began.UnixNano())
	go watchdog(sctx, cancel, &last, &busy, r.p.watchdog)

	var recv func() ([]event, error)
	recv, err = r.open(metadata.NewOutgoingContext(sctx, l.ep.md), bdsevm.NewStreamServiceClient(cc), from, l.ep.raw)
	for err == nil {
		var evs []event
		if evs, err = recv(); err != nil {
			break
		}
		end.flowed = true
		busy.Store(true)
		for _, ev := range evs {
			ev.link, ev.sess = l.idx, end.sess
			if !r.emit(ctx, ev) {
				return end
			}
		}
		last.Store(time.Now().UnixNano())
		busy.Store(false)
	}
	if errors.Is(err, io.EOF) {
		err = status.Error(codes.Unavailable, "the server ended the stream")
	}
	end.err, end.lived = err, time.Since(began)
	switch cause := context.Cause(sctx); {
	case errors.Is(cause, errWatchdog):
		end.reason = "watchdog"
	case errors.Is(cause, errRestart):
		end.reason = "restart"
	default:
		end.reason = codeName(status.Code(err))
	}
	return end
}

// watchdog cancels the session once no message arrived for limit while the
// session was not waiting on the handler (spec §3.6, §9.5).
func watchdog(ctx context.Context, cancel context.CancelCauseFunc, last *atomic.Int64, busy *atomic.Bool, limit time.Duration) {
	t := time.NewTicker(max(limit/12, 50*time.Millisecond))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if busy.Load() {
				last.Store(now.UnixNano())
				continue
			}
			if now.Sub(time.Unix(0, last.Load())) > limit {
				cancel(errWatchdog)
				return
			}
		}
	}
}

// open opens the plan's stream from from and returns its reader: each call
// reads one message and returns its events.
func (r *runner) open(ctx context.Context, client bdsevm.StreamServiceClient, from *BlockRef, ep string) (func() ([]event, error), error) {
	hb := r.p.heartbeat
	var fromBlock *uint64
	var fromHash []byte
	if from != nil {
		n, h := from.Number, from.Hash
		fromBlock, fromHash = &n, h[:]
	}
	statusEv := func(s *bdsevm.StreamStatus) []event {
		st := map[bdsevm.StreamStatus_State]State{
			bdsevm.StreamStatus_STATE_OK:       StateOK,
			bdsevm.StreamStatus_STATE_DEGRADED: StateDegraded,
			bdsevm.StreamStatus_STATE_LAGGING:  StateLagging,
		}[s.State]
		return []event{{kind: evStatus, status: Status{State: st, Reason: s.Reason, Endpoint: ep}}}
	}
	switch r.p.kind {
	case NewHeads:
		st, err := client.StreamBlocks(ctx, &bdsevm.StreamBlocksRequest{FromBlock: fromBlock, FromHash: fromHash,
			Finality: r.p.finality, Depth: r.p.depth, HeartbeatSeconds: &hb})
		if err != nil {
			return nil, err
		}
		return func() ([]event, error) {
			m, err := st.Recv()
			if err != nil {
				return nil, err
			}
			switch {
			case m.GetHeader() != nil:
				h := m.GetHeader()
				b, err := refBlock(h.Number, h.Hash, h.ParentHash)
				b.Header = h
				return []event{{kind: evBlock, block: b}}, err
			case m.GetStatus() != nil:
				return statusEv(m.GetStatus()), nil
			}
			return nil, nil
		}, nil
	case Logs:
		st, err := client.StreamLogs(ctx, &bdsevm.StreamLogsRequest{FromBlock: fromBlock, FromHash: fromHash,
			Filter: r.p.filter, Finality: r.p.finality, Depth: r.p.depth, Reorg: r.p.reorg, HeartbeatSeconds: &hb})
		if err != nil {
			return nil, err
		}
		// part is the block whose messages arrived so far; its complete
		// message delivers it. A session that ends mid-block drops it: the
		// next one resends the whole block.
		var part *Block
		return func() ([]event, error) {
			m, err := st.Recv()
			if err != nil {
				return nil, err
			}
			switch {
			case m.GetBlock() != nil:
				bl := m.GetBlock()
				ref := bl.GetBlock()
				b, err := refBlock(ref.GetNumber(), ref.GetHash(), ref.GetParentHash())
				if err != nil {
					return nil, err
				}
				if part == nil || part.BlockRef != b.BlockRef {
					part = &b
				}
				for _, lg := range bl.Logs {
					part.Logs = append(part.Logs, Log{lg})
				}
				if !bl.Complete {
					return nil, nil
				}
				done := *part
				part = nil
				return []event{{kind: evBlock, block: done}}, nil
			case m.GetUndo() != nil:
				part = nil
				fp := m.GetUndo().GetForkPoint()
				h, err := hashOf(fp.GetHash())
				return []event{{kind: evUndo, ref: BlockRef{Number: fp.GetNumber(), Hash: h}}}, err
			case m.GetStatus() != nil:
				return statusEv(m.GetStatus()), nil
			}
			return nil, nil
		}, nil
	default:
		st, err := client.StreamPendingTransactions(ctx, &bdsevm.StreamPendingTransactionsRequest{
			FullTransactions: r.p.full, HeartbeatSeconds: &hb})
		if err != nil {
			return nil, err
		}
		return func() ([]event, error) {
			m, err := st.Recv()
			if err != nil {
				return nil, err
			}
			switch {
			case m.GetTransactions() != nil:
				txs := m.GetTransactions()
				var b Block
				for _, raw := range txs.Hashes {
					h, err := hashOf(raw)
					if err != nil {
						return nil, err
					}
					b.Hashes = append(b.Hashes, h)
				}
				b.Transactions = txs.Transactions
				return []event{{kind: evBlock, block: b}}, nil
			case m.GetStatus() != nil:
				return statusEv(m.GetStatus()), nil
			}
			return nil, nil
		}, nil
	}
}

func refBlock(number uint64, hash, parent []byte) (Block, error) {
	h, err := hashOf(hash)
	if err != nil {
		return Block{}, fmt.Errorf("block %d: %w", number, err)
	}
	p, err := hashOf(parent)
	if err != nil {
		return Block{}, fmt.Errorf("block %d parent: %w", number, err)
	}
	return Block{BlockRef: BlockRef{Number: number, Hash: h}, ParentHash: p}, nil
}

// terminal is the error a session's end ends Run with (spec §9.5), nil
// when the session reconnects. perEndpoint: the error is this endpoint's
// view of the chain, so it ends Run only while no other endpoint streams;
// else it ends Run at once. from is the cursor the session resumed from.
func (r *runner) terminal(l *link, from *BlockRef, err error) (t error, perEndpoint bool) {
	st, ok := status.FromError(err)
	if !ok {
		return nil, false
	}
	switch st.Code() {
	case codes.OutOfRange:
		e := &OutOfRangeError{Endpoint: l.ep.raw, Reason: st.Message()}
		if from != nil {
			e.Cursor = from.Token()
		}
		for _, d := range st.Details() {
			info, ok := d.(*errdetails.ErrorInfo)
			if !ok {
				continue
			}
			e.Reason = info.Reason
			n, nerr := strconv.ParseUint(info.Metadata["oldestBlock"], 10, 64)
			raw, herr := hexBytes(info.Metadata["oldestHash"], 32)
			if nerr == nil && herr == nil {
				e.Oldest = &BlockRef{Number: n, Hash: Hash(raw)}
			}
		}
		return e, true
	case codes.Aborted:
		return &FinalRevertedError{Endpoint: l.ep.raw, Message: st.Message()}, true
	case codes.Unauthenticated, codes.PermissionDenied, codes.InvalidArgument:
		return fmt.Errorf("%s: %w", l.ep.raw, err), false
	}
	return nil, false
}

// codeName is a gRPC code as the protocol names it: UNAVAILABLE.
func codeName(c codes.Code) string {
	var b strings.Builder
	prev := 'A'
	for _, ch := range c.String() {
		if unicode.IsUpper(ch) && unicode.IsLower(prev) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToUpper(ch))
		prev = ch
	}
	return b.String()
}
