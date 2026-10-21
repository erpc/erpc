package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/adapters/wsclient"
)

// errNewHeadsLag ends a newHeads tap that fell more than reorgWindow blocks
// behind the head with nothing to revert; it restarts after the head.
var errNewHeadsLag = errors.New("newHeads subscription fell behind the head")

// runNewHeads is one ws newHeads subscription: a tap on the network-wide
// tracker that writes each applied block, oldest first. Out of range and
// lag restart it after the head, so the client sees a jump in numbers, as
// with geth.
func (sm *SubscriptionManager) runNewHeads(ctx context.Context, wsc *WsConnection, sub wsclient.Subscription, feed *evm.ChainTracker) {
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
				return err
			}
		}
		return nil
	}
	for ctx.Err() == nil {
		err := sm.taps.RunChain(ctx, sub.NetworkID, "ws", indexer.SubTypeNewHeads, feed, nil, indexer.ReadOpts{}, emit)
		if !errors.Is(err, indexer.ErrOutOfRange) && !errors.Is(err, errNewHeadsLag) {
			return
		}
	}
}

// newHeadsPayloadsKept bounds the projected blocks kept for reuse.
const newHeadsPayloadsKept = 256

// newHeadsPayloads projects each block once for every ws subscription.
type newHeadsPayloads struct {
	mu    sync.Mutex
	byKey map[[32]byte]json.RawMessage
	order [][32]byte
}

func (p *newHeadsPayloads) get(b indexer.Block) (json.RawMessage, error) {
	p.mu.Lock()
	payload, ok := p.byKey[b.Hash]
	p.mu.Unlock()
	if ok {
		return payload, nil
	}
	payload, err := evm.NewHeadsPayload(b.Header)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byKey == nil {
		p.byKey = make(map[[32]byte]json.RawMessage, newHeadsPayloadsKept)
	}
	if _, ok := p.byKey[b.Hash]; !ok {
		p.byKey[b.Hash] = payload
		p.order = append(p.order, b.Hash)
		if len(p.order) > newHeadsPayloadsKept {
			delete(p.byKey, p.order[0])
			p.order = p.order[1:]
		}
	}
	return payload, nil
}
