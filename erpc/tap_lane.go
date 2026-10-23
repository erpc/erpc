package erpc

import (
	"context"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
)

// laneGateWait is how often a lane tap held at its lane's Latest reads that
// Latest again; the network computes a lane's Latest per request, so no
// change wakes it.
var laneGateWait = 50 * time.Millisecond

// laneFeed is the feed of a tap whose connection carries a use-upstream
// directive: the network-wide tracker's batches, cut at the lane's Latest,
// so the connection never gets a block its own eth_blockNumber does not
// show (spec §4, §6.5). The pins stay network-wide.
type laneFeed struct {
	indexer.ChainFeed
	nw *Network
	// lane is a request carrying the connection's selector.
	lane *common.NormalizedRequest
}

// laneRequest is the request a tap reads its lane's Latest with, nil for a
// connection without a use-upstream directive.
func laneRequest(nq *common.NormalizedRequest) *common.NormalizedRequest {
	d := nq.Directives()
	if d == nil || d.UseUpstream == "" {
		return nil
	}
	r := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
	r.SetDirectives(&common.RequestDirectives{UseUpstream: d.UseUpstream})
	return r
}

// gated wraps f in a laneFeed when lane is set.
func gated(f indexer.ChainFeed, nw *Network, lane *common.NormalizedRequest) indexer.ChainFeed {
	if lane == nil {
		return f
	}
	return laneFeed{ChainFeed: f, nw: nw, lane: lane}
}

func (f laneFeed) latest(ctx context.Context) uint64 {
	return uint64(max(f.nw.EvmHighestLatestBlockNumber(context.WithValue(ctx, common.RequestContextKey, f.lane)), 0))
}

// Read returns the next batch from c up to the lane's Latest: Applied
// blocks above it wait, and a batch with nothing left reads again after
// laneGateWait. Reverted blocks pass, so a reorg below Latest still reaches
// the client.
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
		latest := f.latest(ctx)
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
		if timer == nil {
			timer = time.NewTimer(laneGateWait)
		} else {
			timer.Reset(laneGateWait)
		}
		select {
		case <-ctx.Done():
			return indexer.Batch{}, ctx.Err()
		case <-timer.C:
		}
	}
}
