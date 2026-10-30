package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/architecture/evm/logfilter"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

// Stream bounds (streams spec §3.4, §3.6; CT§10.2); vars so tests run
// fast.
var (
	// streamStallWait: a resumable tap whose Read waits this long while
	// this instance lags ends stalled.
	streamStallWait = util.NewDuration(60 * time.Second)
	// logsReceiptsWait: a logs tap that waits this long on one block's
	// receipts is out of range.
	logsReceiptsWait = util.NewDuration(60 * time.Second)
	// streamRestartDelay spaces restarts of a tap that keeps failing.
	streamRestartDelay = 100 * time.Millisecond
)

// Heartbeat interval bounds (§3.6): the default is W clamped to them; a
// client's own is at most heartbeatMax.
const (
	heartbeatMin = 5 * time.Second
	heartbeatMax = 30 * time.Second
)

var (
	errStreamStalled       = errors.New("no block within the read wait")
	errStreamDraining      = errors.New("instance shutting down")
	errTapLag              = indexer.OutOfRangeError{Reason: indexer.OutOfRangeLag}
	errLogsReorgDepth      = indexer.OutOfRangeError{Reason: indexer.OutOfRangeReorgDepth}
	errLogsReceiptsTimeout = indexer.OutOfRangeError{Reason: indexer.OutOfRangeReceiptsTimeout}
)

// finalRevertedError is a reorg none stream's end: a reorg reverted
// block, a block the client applied (§3.4).
type finalRevertedError struct{ block indexer.Cursor }

func (e finalRevertedError) Error() string {
	return "a reorg reverted a final block the client applied"
}

// streamWriteError is a write to the client that failed; slow: it spent
// the write bound (10s), so the client is slow_client (§3.4).
type streamWriteError struct {
	err  error
	slow bool
}

func (e streamWriteError) Error() string { return e.err.Error() }
func (e streamWriteError) Unwrap() error { return e.err }

// writeFailed wraps a failed write; a timeout is the write bound.
func writeFailed(err error) error {
	var ne net.Error
	return streamWriteError{err: err, slow: errors.As(err, &ne) && ne.Timeout()}
}

// headerEncoder writes a heads stream's data: the send of blk's header
// message, and whether blk is still pinned (HeaderEncoding.Of). A nil
// send skips blk.
type headerEncoder interface {
	Header(blk indexer.Block) (send func() error, pinned bool, err error)
}

// logsEncoder writes a logs stream's data.
type logsEncoder interface {
	Logs(blk indexer.Block, logs []json.RawMessage) error
	// Removed writes logs of a reverted block the client applied, with
	// removed:true, newest first (geth mode).
	Removed(blk indexer.Block, logs []json.RawMessage) error
	BlockEnd(blk indexer.Block, logs int) error
}

// controlEncoder writes control messages; a stream without one, as a
// standard ws subscription, sends none (§3.4, §5.4).
type controlEncoder interface {
	Heartbeat(hb streamHeartbeat) error
	Status(state, reason string) error
	End(e streamEnd) error
	// Undo tells the client to drop every block above forkPoint (reorg
	// undo).
	Undo(forkPoint indexer.Cursor) error
}

// streamSession is one client stream (§2): its options, its feed
// composition, one RunChain, its encoders, timers and end mapping. It
// keeps the client's cursor, so a tap that ends with no reason §3.4 names
// restarts where the client is.
type streamSession struct {
	nw              *Network
	transport, kind string
	lane            string
	opts            StreamOptions
	// start is the tap's start without opts.From, fixed at subscribe
	// (ChainTracker.HeadAsOf); nil starts at the tracker's Head.
	start func(context.Context) (indexer.Cursor, error)
	// hold is a final stream's place in the final hold (streams spec §4),
	// set at subscribe to its start; nil for any other finality.
	hold   *indexer.FinalHold
	filter *logfilter.LogsFilter
	// opened is closed once the client has the stream's id; nil: now.
	// gone is closed once the client's connection is gone.
	opened, gone <-chan struct{}
	// drain is closed at instance shutdown; nil: the transport ends the
	// stream itself.
	drain   <-chan struct{}
	headers headerEncoder
	logs    logsEncoder
	control controlEncoder
	// lag: the ws lag bound ends the tap (CT§10.2); resumable: the tap
	// reads with ReadOpts.Resumable and ends stalled after
	// streamStallWait.
	lag, resumable bool

	sends *tapSends
	beat  streamBeat
	// cursor is the last block the client got whole, once delivered.
	cursor    indexer.Cursor
	delivered bool
	// The logs a reorg reverts are the client's: sentHashes are the hashes
	// this session sent, within the window below the newest; from priorTop
	// down the client applied the from cursor's branch in an earlier
	// connection (§4 removed_unsent), -1 when none.
	sentFrom   uint64
	sent       bool
	sentHashes map[uint64][32]byte
	priorTop   int64
	// resumeBehind is how far from stood behind the confirmed head when
	// the stream opened; resumePending until its outcome is counted.
	resumeBehind  uint64
	resumePending bool

	emitting atomic.Bool
	lagging  atomic.Bool
	kick     chan struct{}
}

func newStreamSession(nw *Network, transport, kind, lane string, opts StreamOptions) *streamSession {
	s := &streamSession{nw: nw, transport: transport, kind: kind, lane: lane, opts: opts, priorTop: -1,
		sends: newTapSends(nw, lane, transport, kind, opts.Finality.label()), kick: make(chan struct{}, 1)}
	if kind == indexer.SubTypeLogs {
		s.sentHashes = make(map[uint64][32]byte, nw.chainTracker.ReorgWindow()+1)
		if opts.From != nil {
			s.priorTop = int64(opts.From.Number)
		}
	}
	return s
}

// run runs the stream until it ends for a reason §3.4 names, the client
// leaves (nil), or ctx ends (nil). With control, heartbeats and status go
// to the client, the end is written to it, and an end of no named reason
// restarts the tap from the client's cursor, so the stream never goes
// silent; without, the caller maps every end.
func (s *streamSession) run(ctx context.Context) *streamEnd {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	tracker := s.nw.chainTracker
	// The final hold stays registered across the tap's restarts, so the
	// headers under the client's cursor are not evicted while the next
	// RunChain waits out its restart delay (streams spec §4).
	if s.hold != nil {
		defer tracker.Want(indexer.ReadOpts{Hold: s.hold})()
	}
	if s.opts.From != nil {
		if c, ok := tracker.Confirmed(); ok && c.Number > s.opts.From.Number {
			s.resumeBehind = c.Number - s.opts.From.Number
		}
		s.resumePending = true
	}
	if s.control != nil {
		if s.drain != nil {
			go func() {
				select {
				case <-s.drain:
					cancel(errStreamDraining)
				case <-ctx.Done():
				}
			}()
		}
		go func() {
			every := streamHeartbeatEvery(s.nw, s.opts)
			err := s.beat.run(ctx, every, s.opened, func() error {
				hb, ok := s.nw.streamHeartbeat(s.lane)
				if !ok {
					return nil
				}
				return s.control.Heartbeat(hb)
			})
			if err != nil {
				cancel(err)
			}
		}()
		go s.watchStatus(ctx, cancel)
	}
	for {
		e := s.runOnce(ctx)
		if e == nil {
			return nil
		}
		if e.reason == "" && s.control != nil {
			s.nw.logger.Warn().Err(e.err).Str("transport", s.transport).Str("kind", s.kind).
				Uint64("cursor", s.cursor.Number).Msg("stream tap ended, restarting from the client's cursor")
			select {
			case <-ctx.Done():
			case <-time.After(streamRestartDelay):
			}
			continue
		}
		if e.reason != "" {
			ev := common.TransitionLog(s.nw.logger, zerolog.InfoLevel, s.nw.Label(), "stream_end").
				Str("transport", s.transport).Str("kind", s.kind).Str("lane", s.lane).
				Str("reason", e.reason).Str("detail", e.detail).Bool("delivered", s.delivered).Uint64("cursor", s.cursor.Number)
			if c, ok := tracker.Confirmed(); ok {
				ev = ev.Uint64("bound", c.Number)
			}
			if e.oldest != nil {
				ev = ev.Uint64("oldest", e.oldest.Number)
			}
			ev.Msg("stream ended")
		}
		if s.control != nil && e.reason != indexer.TapEndSlowClient && streamOpened(s.gone, s.opened) {
			_ = s.control.End(*e)
		}
		return e
	}
}

// streamOpened waits until the client has the stream's id (§5.3: the id
// comes before any message), even past the tap's end: a subscribe's
// answer is written after its tap starts. False once gone closes first:
// the connection is gone, so the answer never comes.
func streamOpened(gone, opened <-chan struct{}) bool {
	if opened == nil {
		return true
	}
	select {
	case <-opened:
		return true
	default:
	}
	select {
	case <-opened:
		return true
	case <-gone:
		return false
	}
}

// runOnce is one RunChain of the session, from the client's cursor once
// it has one.
func (s *streamSession) runOnce(ctx context.Context) *streamEnd {
	tracker := s.nw.chainTracker
	var feed indexer.ChainFeed = tracker
	if s.kind == indexer.SubTypeLogs {
		feed = logsFeed{tracker}
	}
	var start *indexer.Cursor
	switch {
	case s.delivered:
		c := s.cursor
		start = &c
	case s.opts.From != nil:
		start = s.opts.From
	case s.start != nil:
		feed = startedFeed{feed, s.start}
	}
	if s.resumable {
		feed = stallFeed{ChainFeed: feed, tracker: tracker, wait: streamStallWait.Load(), lastVote: s.nw.newestHashVoteAt}
	}
	o := indexer.ReadOpts{Resumable: s.resumable, Hold: s.hold}
	if s.filter != nil {
		o.MayMatch = s.filter.MayMatch
	}
	feed = gated(feed, s.nw, s.lane)
	if s.opts.Finality.Kind != finalityLatest {
		feed = finalityFeed{ChainFeed: feed, nw: s.nw, lane: s.lane, finality: s.opts.Finality}
	}
	var lag tapLag
	err := s.nw.taps.RunChain(ctx, s.nw.Label(), s.transport, s.kind, feed, start, o,
		func(b indexer.Batch) (int, error) { return s.emit(ctx, &lag, b) })
	return s.endOf(ctx, err)
}

// endOf maps the error a tap ended with to its end (§3.4); nil when the
// client left or ctx ended.
func (s *streamSession) endOf(ctx context.Context, err error) *streamEnd {
	// The session's own writers (heartbeat, status) end it through ctx;
	// shutdown ends it through drain, or the network's context first.
	cause := context.Cause(ctx)
	if s.drain != nil && (errors.Is(cause, errStreamDraining) || ctx.Err() != nil && s.nw.appCtx.Err() != nil) {
		return s.counted(streamEnd{reason: indexer.TapEndDraining})
	}
	if ctx.Err() != nil {
		if !errors.As(cause, new(streamWriteError)) {
			return nil
		}
		err = cause
	}
	var we streamWriteError
	if errors.As(err, &we) {
		if we.slow {
			return s.counted(streamEnd{reason: indexer.TapEndSlowClient})
		}
		return nil
	}
	var oor indexer.OutOfRangeError
	if errors.As(err, &oor) {
		if s.resumePending {
			s.resumePending = false
			s.countResume("out_of_range")
		}
		if oor.Reason == indexer.OutOfRangeLag {
			return &streamEnd{reason: indexer.TapEndLagging, detail: oor.Reason}
		}
		return &streamEnd{reason: indexer.TapEndOutOfRange, detail: oor.Reason, oldest: s.oldest()}
	}
	if errors.Is(err, errStreamStalled) {
		return s.counted(streamEnd{reason: indexer.TapEndStalled})
	}
	var fr finalRevertedError
	if errors.As(err, &fr) {
		return s.counted(streamEnd{reason: indexer.TapEndFinalReverted, block: &fr.block})
	}
	return &streamEnd{err: err}
}

// counted counts an end the session decided; RunChain counts the ends a
// feed returned (out of range, lag).
func (s *streamSession) counted(e streamEnd) *streamEnd {
	telemetry.CounterHandle(telemetry.MetricTapEndTotal, s.nw.projectId, s.nw.Label(), s.transport, s.kind, e.reason, e.detail).Inc()
	return &e
}

// oldest is the lowest pin, what an out-of-range client can still resume
// from.
func (s *streamSession) oldest() *indexer.Cursor {
	from, _, ok := s.nw.chainTracker.FollowedRange()
	if !ok {
		return nil
	}
	h, ok := s.nw.chainTracker.HashAt(from)
	if !ok {
		return nil
	}
	c := indexer.Cursor{Number: uint64(from)}
	if b, err := common.HexToBytes(h); err == nil && len(b) == 32 {
		copy(c.Hash[:], b)
		return &c
	}
	return nil
}

func (s *streamSession) countResume(outcome string) {
	telemetry.CounterHandle(telemetry.MetricStreamResumeTotal, s.nw.projectId, s.nw.Label(), s.transport, s.kind, outcome).Inc()
	if outcome != "out_of_range" {
		telemetry.ObserverHandle(telemetry.MetricStreamReplayBlocks, s.nw.projectId, s.nw.Label(), s.transport, s.kind).
			Observe(float64(s.resumeBehind))
	}
}

// emit writes one batch, once the client has the stream's id.
func (s *streamSession) emit(ctx context.Context, lag *tapLag, b indexer.Batch) (int, error) {
	if s.opened != nil {
		select {
		case <-s.opened:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	s.emitting.Store(true)
	defer func() {
		s.emitting.Store(false)
		s.nudge()
	}()
	if s.resumePending {
		s.resumePending = false
		if s.resumeBehind > 0 {
			s.countResume("replayed")
		} else {
			s.countResume("at_head")
		}
	}
	lag.start()
	// newHeads sends the new branch of a reorg however long it waited.
	if s.lag && len(b.Applied) > 0 && (s.kind == indexer.SubTypeLogs || len(b.Reverted) == 0) {
		age, bound := lag.age(b.Applied[0]), lagBound(s.nw.chainTracker)
		if age > bound {
			return 0, errTapLag
		}
		if age > bound/2 {
			s.lagging.Store(true)
		} else if age < bound/4 {
			s.lagging.Store(false)
		}
	}
	if len(b.Reverted) > 0 && s.opts.Reorg != reorgGeth && (s.delivered || s.opts.From != nil) {
		fork := b.Reverted[len(b.Reverted)-1]
		if s.opts.Reorg == reorgNone {
			top := b.Reverted[0]
			return 0, finalRevertedError{block: indexer.Cursor{Number: top.Number, Hash: top.Hash}}
		}
		if err := s.control.Undo(indexer.Cursor{Number: fork.Number - min(fork.Number, 1), Hash: fork.ParentHash}); err != nil {
			return 0, err
		}
		s.beat.wrote()
	}
	if s.kind == indexer.SubTypeLogs {
		return s.emitLogs(ctx, lag, b)
	}
	return s.emitHeads(ctx, lag, b)
}

func (s *streamSession) emitHeads(ctx context.Context, lag *tapLag, b indexer.Batch) (int, error) {
	s.reverted(b)
	for i, blk := range b.Applied {
		send, pinned, err := s.headers.Header(blk)
		snap, sendable := s.sends.gate(blk)
		if !pinned || !sendable {
			return i, s.sends.hold(ctx, blk, snap)
		}
		lag.sent(blk)
		if err != nil {
			return i, err
		}
		if send != nil {
			if err := s.sends.write(blk, snap, send); err != nil {
				return i, err
			}
			s.beat.wrote()
		}
		s.applied(blk)
	}
	return len(b.Applied), nil
}

// emitLogs writes, for a reorg in geth mode, the removed:true logs of each
// Reverted block the client applied, newest first, then each Applied
// block's logs and blockEnd. In undo mode the undo before it told the
// client.
func (s *streamSession) emitLogs(ctx context.Context, lag *tapLag, b indexer.Batch) (int, error) {
	tracker := s.nw.chainTracker
	window := tracker.ReorgWindow()
	if s.opts.Reorg != reorgGeth {
		for _, blk := range b.Reverted {
			s.forget(blk)
		}
	} else {
		// removed:true logs need the Reverted blocks' receipts, which the
		// feed holds only reorgWindow deep.
		need := 0
		for _, blk := range b.Reverted {
			if s.sentState(blk) != blockUnsent {
				need++
			}
		}
		if need > window {
			return 0, errLogsReorgDepth
		}
		for _, blk := range b.Reverted {
			switch s.sentState(blk) {
			case blockUnsent:
				continue
			case blockOtherSent:
				h := s.sentHashes[blk.Number]
				common.ViolateInvariant(s.nw.logger, s.nw.projectId, s.nw.Label(), common.InvariantRemovedUnsent).
					Str("lane", s.lane).Uint64("block", blk.Number).Hex("blockHash", blk.Hash[:]).Hex("sentHash", h[:]).
					Msg("logs tap reverts a block it never applied; it sends no removed:true for it")
				continue
			}
			s.forget(blk)
			if logs := tracker.MatchedLogs(blk, s.filter); len(logs) > 0 {
				if err := s.logs.Removed(blk, logs); err != nil {
					return 0, err
				}
				s.beat.wrote()
			}
		}
	}
	s.reverted(b)
	for i, blk := range b.Applied {
		snap, sendable := s.sends.gate(blk)
		if !sendable {
			return i, s.sends.hold(ctx, blk, snap)
		}
		if !s.sent || blk.Number < s.sentFrom {
			s.sentFrom, s.sent = blk.Number, true
		}
		s.sentHashes[blk.Number] = blk.Hash
		if blk.Number > uint64(window) {
			delete(s.sentHashes, blk.Number-uint64(window)-1)
		}
		lag.sent(blk)
		logs := tracker.MatchedLogs(blk, s.filter)
		if len(logs) > 0 || s.opts.BlockEnd {
			if err := s.sends.write(blk, snap, func() error {
				if len(logs) > 0 {
					if err := s.logs.Logs(blk, logs); err != nil {
						return err
					}
				}
				if s.opts.BlockEnd {
					return s.logs.BlockEnd(blk, len(logs))
				}
				return nil
			}); err != nil {
				return i, err
			}
			s.beat.wrote()
		}
		s.applied(blk)
	}
	return len(b.Applied), nil
}

type blockSentState uint8

const (
	blockUnsent    blockSentState = iota // never the client's: below its start
	blockSent                            // the client applied this block
	blockOtherSent                       // the client applied another block at its number
)

// sentState is whether the client applied blk, a block a reorg reverts:
// this session sent it, or it is at or below priorTop on the from
// cursor's branch, which the tap's cursor descends from.
func (s *streamSession) sentState(blk indexer.Block) blockSentState {
	if h, ok := s.sentHashes[blk.Number]; ok {
		if h == blk.Hash {
			return blockSent
		}
		return blockOtherSent
	}
	if int64(blk.Number) <= s.priorTop {
		return blockSent
	}
	if s.sent && blk.Number >= s.sentFrom {
		return blockOtherSent
	}
	return blockUnsent
}

// forget drops a reverted block from what the client applied.
func (s *streamSession) forget(blk indexer.Block) {
	delete(s.sentHashes, blk.Number)
	if int64(blk.Number) <= s.priorTop {
		s.priorTop = int64(blk.Number) - 1
	}
}

// reverted moves the client's cursor to a reorg's fork point.
func (s *streamSession) reverted(b indexer.Batch) {
	if n := len(b.Reverted); n > 0 && s.delivered {
		fork := b.Reverted[n-1]
		s.cursor = indexer.Cursor{Number: fork.Number - min(fork.Number, 1), Hash: fork.ParentHash}
	}
}

func (s *streamSession) applied(blk indexer.Block) {
	s.cursor, s.delivered = indexer.Cursor{Number: blk.Number, Hash: blk.Hash}, true
}

func (s *streamSession) nudge() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Stream states (§3.4).
const (
	streamOK       = "ok"
	streamDegraded = "degraded"
	streamLagging  = "lagging"
)

// watchStatus writes each change of the stream's state, from opened on:
// degraded while the tap waits and the tracker has no hash voter, else
// lagging while the tap's next block has been deliverable for more than
// half the lag bound, until under a quarter of it, else ok. It is the
// status messages' only writer, so they keep their order.
func (s *streamSession) watchStatus(ctx context.Context, cancel context.CancelCauseFunc) {
	if s.opened != nil {
		select {
		case <-s.opened:
		case <-ctx.Done():
			return
		}
	}
	tracker := s.nw.chainTracker
	state := streamOK
	for {
		changed := tracker.Changed()
		next, reason := streamOK, ""
		switch {
		case !s.emitting.Load() && !tracker.HashVoting():
			next, reason = streamDegraded, "no_fresh_source"
		case s.lagging.Load():
			next, reason = streamLagging, indexer.OutOfRangeLag
		}
		if next != state {
			state = next
			telemetry.CounterHandle(telemetry.MetricStreamStatusTotal, s.nw.projectId, s.nw.Label(), s.transport, s.kind, state).Inc()
			common.TransitionLog(s.nw.logger, zerolog.InfoLevel, s.nw.Label(), "stream_status:"+s.kind).
				Str("transport", s.transport).Str("lane", s.lane).Str("state", state).Str("reason", reason).
				Msg("stream status changed")
			if err := s.control.Status(state, reason); err != nil {
				cancel(err)
				return
			}
			s.beat.wrote()
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-s.kick:
		}
	}
}

// streamBeat sends a heartbeat once a stream has been idle for its
// interval (§3.6): any message resets the timer.
type streamBeat struct{ last atomic.Int64 }

func (b *streamBeat) wrote() { b.last.Store(time.Now().UnixNano()) }

// run sends heartbeats from opened on until ctx ends or send fails, whose
// error it returns.
func (b *streamBeat) run(ctx context.Context, every time.Duration, opened <-chan struct{}, send func() error) error {
	if opened != nil {
		select {
		case <-opened:
		case <-ctx.Done():
			return nil
		}
	}
	b.wrote()
	timer := time.NewTimer(every)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		if idle := time.Since(time.Unix(0, b.last.Load())); idle < every {
			timer.Reset(every - idle)
			continue
		}
		if err := send(); err != nil {
			return err
		}
		b.wrote()
		timer.Reset(every)
	}
}

// streamHeartbeatEvery is a stream's heartbeat interval: its option, else
// W clamped to 5–30s.
func streamHeartbeatEvery(nw *Network, o StreamOptions) time.Duration {
	if o.Heartbeat > 0 {
		return o.Heartbeat
	}
	if nw.chainTracker == nil {
		return heartbeatMax
	}
	return min(max(nw.chainTracker.W(), heartbeatMin), heartbeatMax)
}

// streamHeartbeat is a heartbeat's content now: the confirmed head and the
// lane's Finalized and Safe (0 while not computed); false before a
// confirmed head.
func (n *Network) streamHeartbeat(lane string) (streamHeartbeat, bool) {
	if n.chainTracker == nil {
		return streamHeartbeat{}, false
	}
	head, ok := n.chainTracker.Confirmed()
	if !ok {
		return streamHeartbeat{}, false
	}
	h := n.headFor(lane)
	return streamHeartbeat{head: head, finalized: uint64(max(h.Finalized, 0)), safe: uint64(max(h.Safe, 0))}, true
}

// tapLag is a ws tap's lag clock (CT§10.2): its next block is lagging
// once it has been deliverable for longer than reorgWindow blocks of chain
// time. No block is deliverable before floor: the tap's start, when its
// loop runs and the client has the id, so a loop that starts late begins
// with no lag; each tap raises it to the Ready of each block it sends. A
// burst never lags, since its blocks just arrived.
type tapLag struct{ floor time.Time }

// start opens the clock at the tap's first batch once the client has the
// id; later calls keep it.
func (l *tapLag) start() {
	if l.floor.IsZero() {
		l.floor = time.Now()
	}
}

// sent raises the floor to blk's Ready: the blocks after a block the tap
// waited on are deliverable no sooner than it.
func (l *tapLag) sent(blk indexer.Block) {
	if blk.Ready.After(l.floor) {
		l.floor = blk.Ready
	}
}

// age is how long next has been deliverable.
func (l *tapLag) age(next indexer.Block) time.Duration {
	ready := next.Ready
	if ready.Before(l.floor) {
		ready = l.floor
	}
	return time.Since(ready)
}

// lagBound is the ws lag bound: reorgWindow blocks of chain time.
func lagBound(t *evm.ChainTracker) time.Duration {
	return time.Duration(t.ReorgWindow()) * t.BlockTime()
}

// startedFeed is a tap's feed whose start the subscribe fixed
// (ChainTracker.HeadAsOf).
type startedFeed struct {
	indexer.ChainFeed
	head func(context.Context) (indexer.Cursor, error)
}

func (f startedFeed) Head(ctx context.Context) (indexer.Cursor, error) { return f.head(ctx) }

// stallFeed ends a wait a resumable client should not sit through with
// errStreamStalled, so an instance that lags, restarted or fills slowly
// tells the client to resume elsewhere (CT§10.2): a Head wait longer than
// wait, and a Read wait that spends wait while the confirmed head is not
// the cursor and stands at or above it, or while no hash voter exists. A
// Read at the head of a chain that makes no blocks waits without end: no
// lag reference moves, so its hash voters stay.
type stallFeed struct {
	indexer.ChainFeed
	tracker *evm.ChainTracker
	wait    time.Duration
	// lastVote is when the newest hash vote reached this instance; zero
	// before one.
	lastVote func() time.Time
}

func (f stallFeed) Head(ctx context.Context) (indexer.Cursor, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, f.wait, errStreamStalled)
	defer cancel()
	c, err := f.ChainFeed.Head(ctx)
	if err != nil && errors.Is(context.Cause(ctx), errStreamStalled) {
		return c, errStreamStalled
	}
	return c, err
}

func (f stallFeed) Read(ctx context.Context, c indexer.Cursor, o indexer.ReadOpts) (indexer.Batch, error) {
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go f.watchStall(rctx, c, cancel)
	b, err := f.ChainFeed.Read(rctx, c, o)
	if err != nil && context.Cause(rctx) == errStreamStalled && ctx.Err() == nil {
		return indexer.Batch{}, errStreamStalled
	}
	return b, err
}

// watchStall ends a Read on c with errStreamStalled once it waited f.wait
// in a row while stalled; it is checked on each published state and every
// quarter of the wait. Without a hash voter the wait counts from the
// newest hash vote: a cut-off instance's sockets close only at their
// liveness deadline, well after its last header.
func (f stallFeed) watchStall(ctx context.Context, c indexer.Cursor, cancel context.CancelCauseFunc) {
	recheck := f.wait / 4
	timer := time.NewTimer(recheck)
	defer timer.Stop()
	var since time.Time
	for {
		changed := f.tracker.Changed()
		next := recheck
		voting := f.tracker.HashVoting()
		if f.stalled(c, voting) {
			if since.IsZero() {
				since = time.Now()
				if at := f.lastVote(); !voting && !at.IsZero() && at.Before(since) {
					since = at
				}
			}
			left := f.wait - time.Since(since)
			if left <= 0 {
				cancel(errStreamStalled)
				return
			}
			next = min(next, left)
		} else {
			since = time.Time{}
		}
		timer.Reset(next)
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-timer.C:
		}
	}
}

// stalled reports whether a Read on c waits on this instance rather than
// on the chain: no hash voter exists (voting false), or the confirmed head
// stands above c, or at c's number on another branch.
func (f stallFeed) stalled(c indexer.Cursor, voting bool) bool {
	if !voting {
		return true
	}
	send, ok := f.tracker.Confirmed()
	return ok && (send.Number > c.Number || send.Number == c.Number && send.Hash != c.Hash)
}

// logsFeed is the network-wide tracker as a logs tap reads it: a Read that
// waits logsReceiptsWait on one block's receipts is out of range. The wait
// counts from when the confirmed head first stands above the cursor, so a
// wait for the next head never counts.
type logsFeed struct{ *evm.ChainTracker }

func (f logsFeed) Read(ctx context.Context, c indexer.Cursor, o indexer.ReadOpts) (indexer.Batch, error) {
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go f.watchReceipts(rctx, c, cancel)
	b, err := f.ChainTracker.Read(rctx, c, o)
	if err != nil && context.Cause(rctx) == error(errLogsReceiptsTimeout) && ctx.Err() == nil {
		return indexer.Batch{}, errLogsReceiptsTimeout
	}
	return b, err
}

func (f logsFeed) watchReceipts(ctx context.Context, c indexer.Cursor, cancel context.CancelCauseFunc) {
	var timeout <-chan time.Time
	for {
		changed := f.Changed()
		if timeout == nil {
			if send, ok := f.Confirmed(); ok && send.Number > c.Number {
				t := time.NewTimer(logsReceiptsWait.Load())
				defer t.Stop()
				timeout = t.C
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-timeout:
			cancel(errLogsReceiptsTimeout)
			return
		}
	}
}
