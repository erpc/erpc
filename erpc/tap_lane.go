package erpc

import (
	"context"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
)

// laneFeed is the feed of a tap whose connection carries a use-upstream
// directive: the network-wide tracker's batches, cut at the lane's Head
// (headFor), so the connection never gets a block its own eth_blockNumber
// does not show (spec §4, §6.5). The pins stay network-wide; while the
// network-wide pin at the lane's Head is not the lane's hash, the lane's
// blocks wait.
type laneFeed struct {
	indexer.ChainFeed
	nw       *Network
	selector string
}

// laneSelector is the use-upstream selector a tap reads its lane's Head
// with, "" for a connection without one.
func laneSelector(nq *common.NormalizedRequest) string {
	if d := nq.Directives(); d != nil {
		return d.UseUpstream
	}
	return ""
}

// gated wraps f in a laneFeed when selector is set.
func gated(f indexer.ChainFeed, nw *Network, selector string) indexer.ChainFeed {
	if selector == "" {
		return f
	}
	return laneFeed{ChainFeed: f, nw: nw, selector: selector}
}

// Read returns the next batch from c up to the lane's Head: Applied blocks
// above it, or every one while the network-wide pins hold another block at
// its number, wait for the lane's view to move. Each Read is the lane's
// demand, so its block clock runs while the tap waits (spec §6.8).
// Reverted blocks pass, so a reorg still reaches the client. Each Applied
// block is ready from the later of its stamp and when the lane's view
// first reached it.
func (f laneFeed) Read(ctx context.Context, c indexer.Cursor, o indexer.ReadOpts) (indexer.Batch, error) {
	lt := f.nw.laneTracker(f.selector)
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		b, err := f.ChainFeed.Read(ctx, c, o)
		if err != nil {
			return b, err
		}
		var moved <-chan struct{}
		if lt != nil {
			lt.MarkRead()
			moved = lt.Changed()
		}
		h := f.nw.headFor(tipScope{sel: f.selector})
		keep := len(b.Applied)
		for keep > 0 && (h.Latest <= 0 || b.Applied[keep-1].Number > uint64(h.Latest) ||
			b.Applied[keep-1].Number == uint64(h.Latest) && b.Applied[keep-1].Hash != h.LatestHash) {
			keep--
		}
		switch {
		case keep == len(b.Applied):
			return f.ready(lt, b), nil
		case keep > 0:
			last := b.Applied[keep-1]
			b.Applied, b.Next = b.Applied[:keep], indexer.Cursor{Number: last.Number, Hash: last.Hash}
			return f.ready(lt, b), nil
		case len(b.Reverted) > 0:
			fork := b.Reverted[len(b.Reverted)-1]
			b.Applied, b.Next = nil, indexer.Cursor{Number: fork.Number - 1, Hash: fork.ParentHash}
			return b, nil
		}
		// MarkRead again before the demand's W ends.
		every := f.nw.blockTimeOrCold()
		if timer == nil {
			timer = time.NewTimer(every)
		} else {
			timer.Reset(every)
		}
		select {
		case <-ctx.Done():
			return indexer.Batch{}, ctx.Err()
		case <-moved:
		case <-timer.C:
		}
	}
}

// ready moves each Applied block's Ready to when the lane's view first
// reached it, when that is later (lt's stamps; none for a selector that
// forms no lane, which reads the network-wide view).
func (f laneFeed) ready(lt *evm.ChainTracker, b indexer.Batch) indexer.Batch {
	if lt == nil || lt == f.nw.chainTracker {
		return b
	}
	for i := range b.Applied {
		if at := lt.ReachedAt(b.Applied[i].Number); at.After(b.Applied[i].Ready) {
			b.Applied[i].Ready = at
		}
	}
	return b
}
