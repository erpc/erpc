package erpc

import (
	"context"
	"math"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
)

// finalityFeed is the feed of a tap whose finality is not latest (streams
// spec §3.7): the batches of the feed under it, laneFeed included, cut at
// the bound, as laneFeed cuts at the lane's Latest. Applied blocks above
// the bound wait for it to move; Reverted blocks pass. Each Applied block
// is ready from when the bound first reached it.
type finalityFeed struct {
	indexer.ChainFeed
	nw       *Network
	lane     string
	finality StreamFinality
}

// bound is the highest block the tap may apply now, 0 while unknown, and a
// channel closed once it may have moved: depth:N follows the tracker's
// confirmed head, safe and finalized the lane's Head.
func (f finalityFeed) bound() (uint64, <-chan struct{}) {
	tracker := f.nw.chainTracker
	if f.finality.Kind == finalityDepth {
		changed := tracker.Changed()
		c, ok := tracker.Confirmed()
		if !ok || c.Number < f.finality.Depth {
			return 0, changed
		}
		return c.Number - f.finality.Depth, changed
	}
	var moved <-chan struct{}
	if l := f.nw.laneFor(f.lane); l != nil {
		moved = l.finalityMoved()
	} else {
		moved = f.nw.headsComputed()
	}
	h := f.nw.headFor(f.lane)
	v := h.Finalized
	if f.finality.Kind == finalitySafe {
		v = h.Safe
	}
	return uint64(max(v, 0)), moved
}

// Read returns the next batch from c up to the bound. It reads the feed
// under it only once the bound stands above c, so a tap at the bound waits
// on the bound, never on the head; a final tap ends finality_span once its
// hold passed FinalHoldMax.
func (f finalityFeed) Read(ctx context.Context, c indexer.Cursor, o indexer.ReadOpts) (indexer.Batch, error) {
	tracker := f.nw.chainTracker
	for {
		if f.finality.final() && tracker.FinalitySpan() {
			return indexer.Batch{}, indexer.OutOfRangeError{Reason: indexer.OutOfRangeFinalitySpan}
		}
		changed := tracker.Changed()
		bound, moved := f.bound()
		if bound > c.Number {
			b, err := f.ChainFeed.Read(ctx, c, o)
			if err != nil {
				return b, err
			}
			if bound, _ = f.bound(); bound > c.Number || len(b.Reverted) > 0 {
				if b, ok := f.cut(b, bound); ok {
					return b, nil
				}
			}
			continue
		}
		select {
		case <-ctx.Done():
			return indexer.Batch{}, ctx.Err()
		case <-moved:
		case <-changed:
		}
	}
}

// cut keeps b's Applied blocks at or below bound; a reorg whose new branch
// lies wholly above it is its Reverted blocks alone, continuing at the
// fork point. ok is false for a batch with nothing left.
func (f finalityFeed) cut(b indexer.Batch, bound uint64) (indexer.Batch, bool) {
	keep := len(b.Applied)
	for keep > 0 && b.Applied[keep-1].Number > bound {
		keep--
	}
	switch {
	case keep > 0:
		last := b.Applied[keep-1]
		b.Applied, b.Next = b.Applied[:keep], indexer.Cursor{Number: last.Number, Hash: last.Hash}
		return f.ready(b), true
	case len(b.Reverted) > 0:
		fork := b.Reverted[len(b.Reverted)-1]
		b.Applied, b.Next = nil, indexer.Cursor{Number: fork.Number - min(fork.Number, 1), Hash: fork.ParentHash}
		return b, true
	}
	return indexer.Batch{}, false
}

// ready moves each Applied block's Ready to when the bound first reached
// it, when that is later: for depth:N when the block N above it reached
// the confirmed head.
func (f finalityFeed) ready(b indexer.Batch) indexer.Batch {
	var reached func(n uint64) time.Time
	switch f.finality.Kind {
	case finalityDepth:
		reached = func(n uint64) time.Time { return f.nw.chainTracker.ConfirmedAt(n + f.finality.Depth) }
	default:
		l := f.nw.laneFor(f.lane)
		if l == nil {
			return b
		}
		r := &l.finalReached
		if f.finality.Kind == finalitySafe {
			r = &l.safeReached
		}
		reached = r.at
	}
	for i := range b.Applied {
		if at := reached(b.Applied[i].Number); at.After(b.Applied[i].Ready) {
			b.Applied[i].Ready = at
		}
	}
	return b
}

// tapStart is a tap's start without from, fixed at subscribe: the
// confirmed head (HeadAsOf) for latest, else the bound (finalityStart).
func (n *Network) tapStart(fin StreamFinality, lane string) func(context.Context) (indexer.Cursor, error) {
	if fin.Kind == finalityLatest {
		return n.chainTracker.HeadAsOf()
	}
	return n.finalityStart(fin, lane)
}

// reorgWindow is the deepest depth:N a stream takes; unbounded on a
// network without a tracker, whose subscribes fail on that.
func (n *Network) reorgWindow() uint64 {
	if n.chainTracker == nil {
		return math.MaxUint64
	}
	return uint64(n.chainTracker.ReorgWindow())
}

// finalityStart is the start of a tap without from (streams spec §3.7):
// the bound at subscribe, or its first known value, once the tracker
// holds its hash; a final tap's hold fills down to it. A final tap whose
// hold passed FinalHoldMax ends finality_span.
func (n *Network) finalityStart(fin StreamFinality, lane string) func(context.Context) (indexer.Cursor, error) {
	feed := finalityFeed{nw: n, lane: lane, finality: fin}
	at, _ := feed.bound()
	return func(ctx context.Context) (indexer.Cursor, error) {
		tracker := n.chainTracker
		bound := at
		for {
			if fin.final() && tracker.FinalitySpan() {
				return indexer.Cursor{}, indexer.OutOfRangeError{Reason: indexer.OutOfRangeFinalitySpan}
			}
			changed := tracker.Changed()
			var moved <-chan struct{}
			if bound == 0 {
				bound, moved = feed.bound()
			}
			if bound > 0 {
				if h, ok := tracker.HashAt(int64(bound)); ok {
					if raw, err := common.HexToBytes(h); err == nil && len(raw) == 32 {
						c := indexer.Cursor{Number: bound}
						copy(c.Hash[:], raw)
						return c, nil
					}
				}
			}
			select {
			case <-ctx.Done():
				return indexer.Cursor{}, ctx.Err()
			case <-changed:
			case <-moved:
			}
		}
	}
}
