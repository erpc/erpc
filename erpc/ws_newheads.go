package erpc

import (
	"context"
	"errors"
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

// lagging reports whether a ws tap is behind (spec §10.2): its next block
// has been deliverable for longer than reorgWindow blocks of chain time.
// A burst never is, since its blocks just arrived.
func lagging(tracker *evm.ChainTracker, next indexer.Block) bool {
	return time.Since(next.Ready) > time.Duration(tracker.ReorgWindow())*tracker.BlockTime()
}

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
// before its write is never written. It reads from subscribe
// time but writes only once opened, after the client has its id. It ends
// only with ctx or a failed write; out of range, lag, a panic or any other
// error restart it after the head, so the client sees a jump in numbers,
// as with geth, and never a silent subscription. lane, when set, holds
// blocks above the connection's lane's Latest.
func (sm *SubscriptionManager) runNewHeads(ctx context.Context, wsc *WsConnection, sub wsclient.Subscription, opened <-chan struct{}, nw *Network, lane string) {
	tracker := nw.chainTracker
	sends := newTapSends(nw, lane, "ws", indexer.SubTypeNewHeads)
	emit := func(b indexer.Batch) (int, error) {
		select {
		case <-opened:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		if len(b.Reverted) == 0 && len(b.Applied) > 0 && lagging(tracker, b.Applied[0]) {
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
			if err := sends.write(blk, snap, func() error { return wsc.WriteSubscriptionNotification(sub, payload) }); err != nil {
				return i, wsWriteError{err}
			}
		}
		return len(b.Applied), nil
	}
	for {
		err := nw.taps.RunChain(ctx, nw.Label(), "ws", indexer.SubTypeNewHeads, gated(tracker, nw, lane), nil, indexer.ReadOpts{}, emit)
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
