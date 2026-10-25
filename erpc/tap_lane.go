package erpc

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
)

// minLatestReachedKept is the fewest Latest increases a lane keeps, for a
// network whose tracker's reorgWindow is smaller or not known yet.
const minLatestReachedKept = 128

// latestReached is when a lane's Latest first reached each number, kept
// for its newest increases, at least reorgWindow of them: a lane tap's
// block is deliverable from the later of its stamp and that time, so a
// lane that trails the network-wide head is no lag (spec §10.2).
type latestReached struct {
	mu    sync.Mutex
	steps []latestStep // increasing numbers
	// floor: every number at or below it was reached before the steps.
	floor uint64
}

type latestStep struct {
	number uint64
	at     time.Time
}

// note records latest at at when it is the highest yet; kept bounds the
// steps.
func (r *latestReached) note(latest int64, at time.Time, kept int) {
	if latest <= 0 {
		return
	}
	n := uint64(latest)
	r.mu.Lock()
	defer r.mu.Unlock()
	if k := len(r.steps); n <= r.floor || k > 0 && r.steps[k-1].number >= n {
		return
	}
	r.steps = append(r.steps, latestStep{n, at})
	if over := len(r.steps) - kept; over > 0 {
		r.floor = r.steps[over-1].number
		r.steps = slices.Delete(r.steps, 0, over)
	}
}

// at is when Latest first reached n; zero when that was before the kept
// steps, or has not happened.
func (r *latestReached) at(n uint64) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= r.floor {
		return time.Time{}
	}
	i, _ := slices.BinarySearchFunc(r.steps, n, func(s latestStep, n uint64) int { return cmp.Compare(s.number, n) })
	if i == len(r.steps) {
		return time.Time{}
	}
	return r.steps[i].at
}

// latestReachedKept is how many Latest increases a lane keeps: the
// tracker's reorgWindow, at least minLatestReachedKept.
func (n *Network) latestReachedKept() int {
	if t := n.chainTracker; t != nil {
		return max(t.ReorgWindow(), minLatestReachedKept)
	}
	return minLatestReachedKept
}

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

// laneLatest is the Latest of selector sel, whose lane is l: nil for a
// selector that forms none, picked statelessly.
func (n *Network) laneLatest(l *servedTipLane, sel string) uint64 {
	if l != nil {
		return uint64(max(l.published().Latest, 0))
	}
	return uint64(max(n.computeHead(nil, sel).Latest, 0))
}

// laneLatest is the Latest of selector sel, whose lane is l: nil for a
// selector that forms none, picked statelessly.
func (n *Network) laneLatest(l *servedTipLane, sel string) uint64 {
	if l != nil {
	l := f.nw.laneFor(f.selector)
	var moved <-chan struct{}
	if l != nil {
		moved = l.latestMoved()
	} else {
		moved = f.nw.headsComputed()
}
	return f.nw.laneLatest(l, f.selector), moved
// the lane's own change, or, for a selector that forms no lane, the next
// pass over the network's lanes.
func (f laneFeed) latest() (uint64, <-chan struct{}) {
	l := f.nw.laneFor(f.selector)
	var moved <-chan struct{}
	if l != nil {
		moved = l.latestMoved()
	} else {
		moved = f.nw.headsComputed()
	}
	return f.nw.laneLatest(l, f.selector), moved
}

// Read returns the next batch from c up to the lane's Latest: Applied
// blocks above it wait for the lane's Latest to move. While one waits, the
// lane's voters below it are asked for their head once per block time, at
// most 1s apart (spec §6.5 catch-up): the network-wide tracker under the
// feed asks only the network's voters. Reverted blocks pass, so a reorg
// below Latest still reaches the client. Each Applied block is ready from
// the later of its stamp and when the lane's Latest first reached it.
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
			return f.ready(b), nil
		case keep > 0:
			last := b.Applied[keep-1]
			b.Applied, b.Next = b.Applied[:keep], indexer.Cursor{Number: last.Number, Hash: last.Hash}
			return f.ready(b), nil
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

// ready moves each Applied block's Ready to when the lane's Latest first
// reached it, when that is later.
func (f laneFeed) ready(b indexer.Batch) indexer.Batch {
	l := f.nw.laneFor(f.selector)
	if l == nil {
		return b
	}
	for i := range b.Applied {
		if at := l.reached.at(b.Applied[i].Number); at.After(b.Applied[i].Ready) {
			b.Applied[i].Ready = at
		}
	}
	return b
}
