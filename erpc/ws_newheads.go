package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/erpc/erpc/architecture/evm"
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
// the client gets each head of the new branch. It ends only with ctx or a
// failed write; out of range, lag, a panic or any other error restart it
// after the head, so the client sees a jump in numbers, as with geth, and
// never a silent subscription.
func (sm *SubscriptionManager) runNewHeads(ctx context.Context, wsc *WsConnection, sub wsclient.Subscription, nw *Network) {
	feed := nw.chainTracker
	window := uint64(feed.ReorgWindow())
	emit := func(b indexer.Batch) error {
		if len(b.Reverted) == 0 && len(b.Applied) > 0 {
			if tip, ok := feed.Tip(); ok && tip.Number > b.Applied[0].Number-1+window {
				return errNewHeadsLag
			}
		}
		for _, blk := range b.Applied {
			payload, err := sm.heads.get(blk)
			if err != nil {
				sm.logger.Warn().Err(err).Uint64("block", blk.Number).Msg("cannot project a held header as newHeads")
				continue
			}
			if err := wsc.WriteSubscriptionNotification(sub, payload, 0); err != nil {
				return wsWriteError{err}
			}
		}
		return nil
	}
	for {
		err := nw.taps.RunChain(ctx, sub.NetworkID, "ws", indexer.SubTypeNewHeads, feed, nil, indexer.ReadOpts{}, emit)
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

// newHeadsPayloadsKept bounds the projected blocks kept for reuse.
const newHeadsPayloadsKept = 256

// newHeadsPayloads projects each block once for every ws subscription:
// concurrent taps on one block wait for the one projection.
type newHeadsPayloads struct {
	mu    sync.Mutex
	byKey map[[32]byte]*newHeadsPayload
	order [][32]byte
}

type newHeadsPayload struct {
	once    sync.Once
	payload json.RawMessage
	err     error
}

func (p *newHeadsPayloads) get(b indexer.Block) (json.RawMessage, error) {
	p.mu.Lock()
	if p.byKey == nil {
		p.byKey = make(map[[32]byte]*newHeadsPayload, newHeadsPayloadsKept)
	}
	e := p.byKey[b.Hash]
	if e == nil {
		e = &newHeadsPayload{}
		p.byKey[b.Hash] = e
		p.order = append(p.order, b.Hash)
		if len(p.order) > newHeadsPayloadsKept {
			delete(p.byKey, p.order[0])
			p.order = p.order[1:]
		}
	}
	p.mu.Unlock()
	e.once.Do(func() { e.payload, e.err = evm.NewHeadsPayload(b.Header) })
	return e.payload, e.err
}
