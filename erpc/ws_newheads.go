package erpc

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/adapters/wsclient"
	"github.com/rs/zerolog"
)

// errNewHeadsLag ends a newHeads tap whose next block has waited longer
// than the lag bound with nothing to revert; it restarts after the head.
var errNewHeadsLag = indexer.OutOfRangeError{Reason: indexer.OutOfRangeLag}

// tapLag is a ws tap's lag clock (spec §10.2): its next block is lagging
// once it has been deliverable for longer than reorgWindow blocks of chain
// time while the tap was not waiting in its Read. No block is deliverable
// before floor: the tap's start, when its loop runs and the client has the
// id, so a loop that starts late begins with no lag; each tap raises it to
// when each block it sends became deliverable. A burst never lags, since
// its blocks just arrived. A Read that waits, as on receipts the feed let
// go and refetches (§9), stops the clock: a block deliverable when the tap
// asked counts as deliverable that much later, a block that became
// deliverable during the Read counts from its return.
type tapLag struct {
	floor time.Time
	// asked is when the tap last asked for a batch, the end of its last
	// emit; read is when that Read returned and wait how long it took.
	asked, read time.Time
	wait        time.Duration
}

// got opens the clock on a batch once the client has the id: at the first
// it starts the tap; at the next the floor moves on by the Read's wait.
func (l *tapLag) got() {
	l.read = time.Now()
	if l.floor.IsZero() {
		l.floor = l.read
		return
	}
	if !l.asked.IsZero() {
		l.wait = l.read.Sub(l.asked)
	}
	l.floor = l.shift(l.floor)
}

// ask notes that the tap asks for its next batch.
func (l *tapLag) ask() { l.asked = time.Now() }

// shift moves at, a time before the Read returned, on by the Read's wait,
// never past its return.
func (l *tapLag) shift(at time.Time) time.Time {
	if at = at.Add(l.wait); at.After(l.read) {
		return l.read
	}
	return at
}

// ready is when blk became deliverable to the tap, its Read's wait left out.
func (l *tapLag) ready(blk indexer.Block) time.Time {
	if ready := l.shift(blk.Ready); ready.After(l.floor) {
		return ready
	}
	return l.floor
}

// sent raises the floor to when blk became deliverable: the blocks after a
// block the tap waited on are deliverable no sooner than it.
func (l *tapLag) sent(blk indexer.Block) {
	if ready := l.ready(blk); ready.After(l.floor) {
		l.floor = ready
	}
}

func (l *tapLag) lagging(tracker *evm.ChainTracker, next indexer.Block) bool {
	return time.Since(l.ready(next)) > time.Duration(tracker.ReorgWindow())*tracker.BlockTime()
}

// wsTapLoopStart, while set, runs as each ws newHeads or logs tap's loop
// begins; tests hold it there to start the loop late.
var wsTapLoopStart atomic.Pointer[func()]

func wsTapLoopStarts() {
	if hold := wsTapLoopStart.Load(); hold != nil {
		(*hold)()
	}
}

// startedFeed is a ws tap's feed whose start the subscribe fixed
// (ChainTracker.HeadAsOf).
type startedFeed struct {
	indexer.ChainFeed
	head func(context.Context) (indexer.Cursor, error)
}

func (f startedFeed) Head(ctx context.Context) (indexer.Cursor, error) { return f.head(ctx) }

// newHeadsRestartDelay spaces restarts of a tap that keeps failing.
var newHeadsRestartDelay = 100 * time.Millisecond

// newHeadsEncoding is each pinned header's newHeads payload.
var newHeadsEncoding = evm.NewHeaderEncoding("ws", evm.NewHeadsPayload)

// wsWriteError is a failed write to the client: the subscription ends.
type wsWriteError struct{ err error }

func (e wsWriteError) Error() string { return e.err.Error() }

// runNewHeads is one ws newHeads subscription: a tap on the network-wide
// tracker that writes each applied block, oldest first, so after a reorg
// the client gets each head of the new branch; a block that left the pins
// before its write is never written. It starts from head, fixed at
// subscribe time, but writes only once opened, after the client has its
// id. It ends only with ctx or a failed write; out of range, lag, a panic
// or any other error restart it after the head at the restart, so the
// client sees a jump in numbers, as with geth, and never a silent
// subscription. lane, when set, holds blocks above the connection's lane's
// Latest.
func (sm *SubscriptionManager) runNewHeads(ctx context.Context, wsc *WsConnection, sub wsclient.Subscription, opened <-chan struct{}, nw *Network, lane string, head func(context.Context) (indexer.Cursor, error)) {
	wsTapLoopStarts()
	tracker := nw.chainTracker
	sends := newTapSends(nw, lane, "ws", indexer.SubTypeNewHeads)
	notify := wsc.notifier(sub)
	var lag tapLag
	emit := func(b indexer.Batch) (int, error) {
		select {
		case <-opened:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		lag.got()
		defer lag.ask()
		if len(b.Reverted) == 0 && len(b.Applied) > 0 && lag.lagging(tracker, b.Applied[0]) {
			return 0, errNewHeadsLag
		}
		for i, blk := range b.Applied {
			payload, pinned, err := newHeadsEncoding.Of(tracker, blk)
			snap, sendable := sends.gate(blk)
			if !pinned || !sendable {
				return i, sends.hold(ctx, blk, snap)
			}
			lag.sent(blk)
			if err != nil {
				sm.logger.Warn().Err(err).Uint64("block", blk.Number).Msg("cannot project a held header as newHeads")
				continue
			}
			if err := sends.write(blk, snap, func(d tapDelivery) error { return notify.write(payload, d) }); err != nil {
				return i, wsWriteError{err}
			}
		}
		return len(b.Applied), nil
	}
	var feed indexer.ChainFeed = startedFeed{tracker, head}
	for {
		err := nw.taps.RunChain(ctx, nw.Label(), "ws", indexer.SubTypeNewHeads, gated(feed, nw, lane), nil, indexer.ReadOpts{}, emit)
		feed, lag = tracker, tapLag{}
		var writeErr wsWriteError
		if ctx.Err() != nil || errors.As(err, &writeErr) {
			return
		}
		if errors.Is(err, indexer.ErrOutOfRange) {
			common.TransitionLog(sm.logger, zerolog.InfoLevel, nw.Label(), "out_of_range").Err(err).
				Str("clientSubId", sub.ClientSubID).Msg("newHeads tap out of range, restarting after the head")
			continue
		}
		sm.logger.Warn().Err(err).Str("clientSubId", sub.ClientSubID).Msg("newHeads tap ended, restarting after the head")
		select {
		case <-ctx.Done():
			return
		case <-time.After(newHeadsRestartDelay):
		}
	}
}
