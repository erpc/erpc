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
// time. No block is deliverable before floor: the tap's start, when its
// loop runs and the client has the id, so a loop that starts late begins
// with no lag; logs raise it to the Ready of each block they send. A burst
// never lags, since its blocks just arrived.
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

func (l *tapLag) lagging(tracker *evm.ChainTracker, next indexer.Block) bool {
	ready := next.Ready
	if ready.Before(l.floor) {
		ready = l.floor
	}
	return time.Since(ready) > time.Duration(tracker.ReorgWindow())*tracker.BlockTime()
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
		lag.start()
		if len(b.Reverted) == 0 && len(b.Applied) > 0 && lag.lagging(tracker, b.Applied[0]) {
			return 0, errNewHeadsLag
		}
		for i, blk := range b.Applied {
			payload, pinned, err := newHeadsEncoding.Of(tracker, blk)
			snap, sendable := sends.gate(blk)
			if !pinned || !sendable {
				return i, sends.hold(ctx, blk, snap)
			}
			if err != nil {
				sm.logger.Warn().Err(err).Uint64("block", blk.Number).Msg("cannot project a held header as newHeads")
				continue
			}
			if err := sends.write(blk, snap, func() error { return notify.write(payload) }); err != nil {
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
