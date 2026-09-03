package evm

import (
	"context"
	"fmt"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog/log"
)

// The ChainView can work two ways.
//
// Passively (the original behaviour) it learns number→hash pins from whatever
// blocks client traffic happens to touch. That is cheap, but it only ever holds
// a sparse scatter of heights, so "does this response link to the chain?" can
// only be asked when the neighbouring height happens to have been seen, and a
// disagreement between two honest sources has no tie-break beyond whichever was
// observed first.
//
// Following (this file) makes it an actual chain follower, the way an indexer
// works: walk forward one block at a time, require every block to name its
// predecessor's hash as parentHash, and on a mismatch walk BACK to the common
// ancestor, unwind the abandoned branch and adopt the new one. The result is a
// contiguous verified segment rather than a scatter — which is what lets the
// module answer "is this block canonical" with evidence instead of a guess, and
// is the precondition for checks that compare CONSECUTIVE headers (base-fee
// derivation, timestamp monotonicity, gas-limit drift) which a sparse view can
// never evaluate.

// followerDefaultInterval is how often the follower checks for new blocks when
// the config does not say. Sub-block-time on every chain we run, so the head is
// picked up promptly without polling hot.
const followerDefaultInterval = 1 * time.Second

// followerDefaultMaxPerTick bounds catch-up work per tick, so a follower that
// starts far behind (or a chain that produces blocks faster than we ingest)
// converges steadily instead of issuing an unbounded burst of fetches.
const followerDefaultMaxPerTick = 16

// chainFollower drives one chainView forward block by block.
type chainFollower struct {
	view       *chainView
	latest     func(context.Context) int64
	interval   time.Duration
	maxPerTick int
	stop       chan struct{}
}

func newChainFollower(v *chainView, latest func(context.Context) int64, cfg *common.IntegrityFollowConfig) *chainFollower {
	f := &chainFollower{
		view:       v,
		latest:     latest,
		interval:   followerDefaultInterval,
		maxPerTick: followerDefaultMaxPerTick,
		stop:       make(chan struct{}),
	}
	if cfg != nil {
		if cfg.Interval > 0 {
			f.interval = cfg.Interval.Duration()
		}
		if cfg.MaxBlocksPerTick > 0 {
			f.maxPerTick = cfg.MaxBlocksPerTick
		}
	}
	return f
}

func (f *chainFollower) start() {
	go f.run()
}

func (f *chainFollower) run() {
	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()
	for {
		select {
		case <-f.stop:
			return
		case <-ticker.C:
			// Bound a tick so a stuck upstream can't wedge the follower: the
			// next tick re-reads the head and resumes from wherever we got to.
			ctx, cancel := context.WithTimeout(context.Background(), f.interval*10)
			f.advance(ctx)
			cancel()
		}
	}
}

// advance moves the followed chain toward the network's current head, at most
// maxPerTick blocks per call.
func (f *chainFollower) advance(ctx context.Context) {
	head := f.latest(ctx)
	if head <= 0 {
		return
	}
	c := f.view

	c.mu.RLock()
	cursor := c.followHead
	c.mu.RUnlock()

	if cursor == 0 {
		// Bootstrap at the head. Starting further back would mean back-filling
		// history we have no reason to hold: the window only needs to cover the
		// reorg depth we are willing to reconcile, and it fills as we advance.
		if !f.ingest(ctx, head) {
			return
		}
		c.mu.Lock()
		c.followBase, c.followHead = head, head
		c.mu.Unlock()
		telemetry.MetricIntegrityFollowHead.WithLabelValues(c.projectId(), c.networkLabel(), c.group).Set(float64(head))
		return
	}

	for n := cursor + 1; n <= head; n++ {
		if f.maxPerTick > 0 && int(n-cursor) > f.maxPerTick {
			break
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		if !f.step(ctx, n) {
			return // transient failure: retry from here on the next tick
		}
	}
	c.mu.RLock()
	newHead := c.followHead
	c.mu.RUnlock()
	if newHead > cursor {
		if p := stateProberFor(c.network.Id()); p != nil {
			p.onNewHead(newHead)
		}
	}
	telemetry.MetricIntegrityFollowHead.WithLabelValues(c.projectId(), c.networkLabel(), c.group).Set(float64(newHead))
	if lag := head - newHead; lag >= 0 {
		telemetry.MetricIntegrityFollowLag.WithLabelValues(c.projectId(), c.networkLabel(), c.group).Set(float64(lag))
	}
}

// step ingests block n, extending the followed chain when it links to the block
// we hold below it and reconciling a reorg when it does not. Reports whether
// the follower may continue.
func (f *chainFollower) step(ctx context.Context, n int64) bool {
	c := f.view
	h, ok := c.fetchFollowHeader(ctx, n)
	if !ok || h == nil {
		return false
	}

	c.mu.RLock()
	prev, held := c.canonical[n-1]
	c.mu.RUnlock()

	if held && eqHexFold(prev, h.ParentHash) {
		c.adoptFollowed(n, h)
		return true
	}
	if !held {
		// Nothing to link against (first block after bootstrap, or the parent
		// was evicted) — adopt it as the new anchor of the followed segment.
		c.adoptFollowed(n, h)
		return true
	}

	// The block does not name the block we hold below it as its parent: the
	// chain reorganised under us. Walk back to the common ancestor and adopt.
	depth, ok := c.reconcile(ctx, h)
	if !ok {
		log.Warn().Str("network", c.networkLabel()).Str("group", c.group).Int64("block", n).
			Str("parentHash", h.ParentHash).Str("heldParent", prev).
			Msg("integrity: could not reconcile a forked block within the reorg window")
		telemetry.MetricIntegrityFollowStall.WithLabelValues(c.projectId(), c.networkLabel(), c.group, "unreconciled").Inc()
		return false
	}
	log.Info().Str("network", c.networkLabel()).Str("group", c.group).
		Int64("block", n).Int("depth", depth).Msg("integrity: reorg reconciled, followed chain re-anchored")
	return true
}

// ingest fetches a single block and pins it, used to bootstrap the follower.
func (f *chainFollower) ingest(ctx context.Context, n int64) bool {
	h, ok := f.view.fetchFollowHeader(ctx, n)
	if !ok || h == nil {
		return false
	}
	f.view.adoptFollowed(n, h)
	return true
}

// fetchFollowHeader gets block n for the follower. Unfinalized heights are read
// FRESH: the follower's entire job is to know what the chain is right now, and
// unfinalized entries are cached without a ttl, so a cached read can hand back
// a block the chain has already abandoned.
func (c *chainView) fetchFollowHeader(ctx context.Context, n int64) (*integrity.Header, bool) {
	fresh := true
	if c.finalized != nil {
		if fin := c.finalized(); fin > 0 && n <= fin {
			fresh = false // settled history: the cache cannot be wrong about it
		}
	}
	return c.resolveHeaderKind(ctx, "eth_getBlockByNumber", fmt.Sprintf("0x%x", n), fresh, auxKindFollow)
}
