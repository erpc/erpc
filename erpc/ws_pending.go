package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/adapters/wsclient"
)

// mempoolFrom is a mempool feed whose first Tail is from: the tail when
// the subscribe was answered, so a transaction that arrives before the tap
// goroutine runs still reaches it. Used by one tap goroutine only.
type mempoolFrom struct {
	indexer.MempoolFeed
	from    uint64
	started bool
}

func (m *mempoolFrom) Tail(variant string) uint64 {
	if !m.started {
		m.started = true
		return m.from
	}
	return m.MempoolFeed.Tail(variant)
}

// runPendingTxs is one ws newPendingTransactions subscription: a tap on
// the network's mempool feed that starts after from, the variant's tail at
// subscribe time, and writes only once opened, after the client has its
// id. A start older than the ring skips to the newest, counted as lag
// (spec §11). It ends only with ctx or a failed write; a panic or any
// other error restarts it after the newest entry, so the subscription
// never goes silent.
func (sm *SubscriptionManager) runPendingTxs(ctx context.Context, wsc *WsConnection, sub wsclient.Subscription, opened <-chan struct{}, nw *Network, variant string, from uint64) {
	feed := &mempoolFrom{MempoolFeed: nw.mempool, from: from}
	notify := wsc.notifier(sub)
	emit := func(txs []json.RawMessage) error {
		select {
		case <-opened:
		case <-ctx.Done():
			return ctx.Err()
		}
		for _, tx := range txs {
			if err := notify.write(tx, tapDelivery{}); err != nil {
				return wsWriteError{err}
			}
		}
		return nil
	}
	for {
		err := nw.taps.RunMempool(ctx, nw.Label(), "ws", variant, feed, emit)
		var writeErr wsWriteError
		if ctx.Err() != nil || errors.As(err, &writeErr) {
			return
		}
		sm.logger.Warn().Err(err).Str("clientSubId", sub.ClientSubID).Msg("newPendingTransactions tap ended, restarting after the newest")
		select {
		case <-ctx.Done():
			return
		case <-time.After(newHeadsRestartDelay):
		}
	}
}
