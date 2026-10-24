package erpc

import (
	"context"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
)

// laneFeed is the feed of a tap whose connection carries a use-upstream
// directive: the network-wide tracker's batches, cut at the lane's Latest,
// so the connection never gets a block its own eth_blockNumber does not
// show (spec §4, §6.5). The pins stay network-wide.
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

// latest is the lane's Latest and a channel closed once it may have moved:
// the lane's own change, or, for a selector that forms no lane, the next
// pass over the network's lanes.
func (f laneFeed) latest() (uint64, <-chan struct{}) {
	if l := f.nw.laneFor(f.selector); l != nil {
		moved := l.latestMoved()
		return uint64(max(l.published().Latest, 0)), moved
	}
	moved := f.nw.headsComputed()
	return uint64(max(f.nw.computeHead(nil, f.selector).Latest, 0)), moved
}

// Read returns the next batch from c up to the lane's Latest: Applied
// blocks above it wait for the lane's Latest to move. While one waits, the
// lane's voters below it are asked for their head once per block time, at
// most 1s apart (spec §6.5 catch-up): the network-wide tracker under the
// feed asks only the network's voters. Reverted blocks pass, so a reorg
// below Latest still reaches the client.
func (f laneFeed) Read(ctx context.Context, c indexer.Cursor, o indexer.ReadOpts) (indexer.Batch, error) {
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
		latest, moved := f.latest()
		keep := len(b.Applied)
		for keep > 0 && b.Applied[keep-1].Number > latest {
			keep--
		}
		switch {
		case keep == len(b.Applied):
			return b, nil
		case keep > 0:
			last := b.Applied[keep-1]
			b.Applied, b.Next = b.Applied[:keep], indexer.Cursor{Number: last.Number, Hash: last.Hash}
			return b, nil
		case len(b.Reverted) > 0:
			fork := b.Reverted[len(b.Reverted)-1]
			b.Applied, b.Next = nil, indexer.Cursor{Number: fork.Number - 1, Hash: fork.ParentHash}
			return b, nil
		}
		f.nw.EvmRefreshHeadsBelow(f.selector, int64(b.Applied[0].Number))
		every := f.nw.EvmBlockTime()
		if every <= 0 || every > time.Second {
			every = time.Second
		}
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
