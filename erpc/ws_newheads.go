package erpc

import (
	"context"
	"errors"
	"time"

	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/adapters/wsclient"
)

// errNewHeadsLag ends a newHeads tap that fell more than reorgWindow blocks
// behind the head with nothing to revert; it restarts after the head.
var errNewHeadsLag = indexer.OutOfRangeError{Reason: indexer.OutOfRangeLag}

// newHeadsRestartDelay spaces restarts of a tap that keeps failing.
var newHeadsRestartDelay = 100 * time.Millisecond

// wsWriteError is a failed write to the client: the subscription ends.
type wsWriteError struct{ err error }

func (e wsWriteError) Error() string { return e.err.Error() }

// runNewHeads is one ws newHeads subscription: a tap on the network-wide
// tracker that writes each applied block, oldest first, so after a reorg
// the client gets each head of the new branch. It reads from subscribe
// time but writes only once opened, after the client has its id. It ends
// only with ctx or a failed write; out of range, lag, a panic or any other
// error restart it after the head, so the client sees a jump in numbers,
// as with geth, and never a silent subscription.
func (sm *SubscriptionManager) runNewHeads(ctx context.Context, wsc *WsConnection, sub wsclient.Subscription, opened <-chan struct{}, nw *Network) {
	feed := nw.chainTracker
	window := uint64(feed.ReorgWindow())
	emit := func(b indexer.Batch) error {
		select {
		case <-opened:
		case <-ctx.Done():
			return ctx.Err()
		}
		// Lag counts from the sendable head: a delivery gate holding the
		// tracker head back is not the tap falling behind.
		if len(b.Reverted) == 0 && len(b.Applied) > 0 {
			if send, ok := feed.SendTip(); ok && send.Number > b.Applied[0].Number-1+window {
				return errNewHeadsLag
			}
		}
		for _, blk := range b.Applied {
			payload, err := sm.heads.get(blk)
			if err != nil {
				sm.logger.Warn().Err(err).Uint64("block", blk.Number).Msg("cannot project a held header as newHeads")
				continue
			}
			if err := wsc.WriteSubscriptionNotification(sub, payload); err != nil {
				return wsWriteError{err}
			}
		}
		return nil
	}
	for {
		err := nw.taps.RunChain(ctx, nw.Label(), "ws", indexer.SubTypeNewHeads, feed, nil, indexer.ReadOpts{}, emit)
		var writeErr wsWriteError
		if ctx.Err() != nil || errors.As(err, &writeErr) {
			return
		}
		if !errors.Is(err, indexer.ErrOutOfRange) {
			sm.logger.Warn().Err(err).Str("clientSubId", sub.ClientSubID).Msg("newHeads tap ended, restarting after the head")
			select {
			case <-ctx.Done():
				return
			case <-time.After(newHeadsRestartDelay):
			}
		}
	}
}
