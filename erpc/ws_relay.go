package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/adapters/wsclient"
)

// relayFrom is a relay feed whose first Tail is from: the tail when the
// subscribe was answered, so a message that arrives before the tap
// goroutine runs still reaches it. Used by one tap goroutine only.
type relayFrom struct {
	indexer.RelayFeed
	from    uint64
	started bool
}

func (m *relayFrom) Tail(t indexer.Topic) uint64 {
	if !m.started {
		m.started = true
		return m.from
	}
	return m.RelayFeed.Tail(t)
}

// runRelay is one ws subscription of a relayed kind: a tap on topic of the
// network's relay feed that starts after from, the topic's tail at
// subscribe time, and writes only once opened, after the client has its
// id. A start older than the ring skips to the newest, counted as lag
// (spec §11). It ends only with ctx or a failed write; a panic or any
// other error restarts it after the newest entry, so the subscription
// never goes silent.
func (sm *SubscriptionManager) runRelay(ctx context.Context, wsc *WsConnection, sub wsclient.Subscription, opened <-chan struct{}, nw *Network, topic indexer.Topic, from uint64) {
	feed := &relayFrom{RelayFeed: nw.relay, from: from}
	notify := wsc.notifier(sub)
	emit := func(msgs []json.RawMessage) error {
		select {
		case <-opened:
		case <-ctx.Done():
			return ctx.Err()
		}
		for _, msg := range msgs {
			if err := notify.write(msg, tapDelivery{}); err != nil {
				return wsWriteError{err}
			}
		}
		return nil
	}
	for {
		err := nw.taps.RunRelay(ctx, nw.Label(), "ws", topic, feed, emit)
		var writeErr wsWriteError
		if ctx.Err() != nil || errors.As(err, &writeErr) {
			return
		}
		sm.logger.Warn().Err(err).Str("clientSubId", sub.ClientSubID).Str("topic", string(topic)).Msg("relay tap ended, restarting after the newest")
		select {
		case <-ctx.Done():
			return
		case <-time.After(newHeadsRestartDelay):
		}
	}
}
