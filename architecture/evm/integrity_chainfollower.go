package evm

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog/log"
)

// The follower walks the chain forward one block at a time and hands each
// block that links to the pins to the tracker as the no-source vote (spec §6
// rule 6): that vote counts only while no live source votes. On a block that
// does not link it walks back by hash to the common ancestor, so the tracker
// holds the whole branch before the vote moves the pins onto it.

// followerDefaultInterval is how often the follower checks for new blocks when
// the config does not say.
const followerDefaultInterval = 1 * time.Second

// followerDefaultMaxPerTick bounds catch-up work per tick, so a follower that
// starts far behind converges steadily instead of bursting.
const followerDefaultMaxPerTick = 16

// chainFollower drives one tracker forward block by block.
type chainFollower struct {
	tracker    *ChainTracker
	latest     func(context.Context) int64
	interval   time.Duration
	maxPerTick int
}

func newChainFollower(t *ChainTracker, latest func(context.Context) int64, cfg *common.IntegrityFollowConfig) *chainFollower {
	f := &chainFollower{
		tracker:    t,
		latest:     latest,
		interval:   followerDefaultInterval,
		maxPerTick: followerDefaultMaxPerTick,
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

func (f *chainFollower) start(ctx context.Context) {
	go f.run(ctx)
}

func (f *chainFollower) run(ctx context.Context) {
	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Bound a tick so a stuck upstream can't wedge the follower: the
			// next tick re-reads the head and resumes from the pins.
			tickCtx, cancel := context.WithTimeout(ctx, f.interval*10)
			f.advance(tickCtx)
			cancel()
		}
	}
}

// advance moves the pins toward the network's current head, at most
// maxPerTick blocks per call.
func (f *chainFollower) advance(ctx context.Context) {
	if f.latest == nil {
		return
	}
	head := f.latest(ctx)
	if head <= 0 {
		return
	}
	t := f.tracker

	tip, ok := t.Tip()
	if !ok {
		// Start at the head: the window only needs to cover the reorg depth
		// we are willing to follow, and it fills as we advance.
		h, ok := t.fetchFollowHeader(ctx, head)
		if !ok {
			return
		}
		t.voteAsk(ctx, h)
		telemetry.MetricIntegrityFollowHead.WithLabelValues(t.projectId(), t.networkLabel(), t.group).Set(float64(h.number))
		return
	}

	cursor := int64(tip.Number)
	for n := cursor + 1; n <= head; n++ {
		if f.maxPerTick > 0 && int(n-cursor) > f.maxPerTick {
			break
		}
		if ctx.Err() != nil {
			return
		}
		if !f.step(ctx, n) {
			return // retry from the pins on the next tick
		}
	}
	newTip, _ := t.Tip()
	if int64(newTip.Number) > cursor {
		if p := stateProberFor(t.network.Id()); p != nil {
			p.onNewHead(int64(newTip.Number))
		}
	}
	telemetry.MetricIntegrityFollowHead.WithLabelValues(t.projectId(), t.networkLabel(), t.group).Set(float64(newTip.Number))
	if lag := head - int64(newTip.Number); lag >= 0 {
		telemetry.MetricIntegrityFollowLag.WithLabelValues(t.projectId(), t.networkLabel(), t.group).Set(float64(lag))
	}
}

// step fetches block n and votes for it when it links to the pin below it,
// or when nothing is pinned there; otherwise it walks back to the common
// ancestor first. Reports whether the follower may continue.
func (f *chainFollower) step(ctx context.Context, n int64) bool {
	t := f.tracker
	h, ok := t.fetchFollowHeader(ctx, n)
	if !ok {
		return false
	}
	if p := t.state.Load().pinAt(uint64(n - 1)); p == nil || p.hash == h.parentHash {
		t.voteAsk(ctx, h)
		return true
	}
	depth, ok := f.reconcile(ctx, h)
	if !ok {
		log.Warn().Str("network", t.networkLabel()).Str("group", t.group).Int64("block", n).
			Str("parentHash", "0x"+hex.EncodeToString(h.parentHash[:])).
			Msg("integrity: could not reconcile a forked block within the reorg window")
		telemetry.MetricIntegrityFollowStall.WithLabelValues(t.projectId(), t.networkLabel(), t.group, "unreconciled").Inc()
		return false
	}
	log.Info().Str("network", t.networkLabel()).Str("group", t.group).
		Int64("block", n).Int("depth", depth).Msg("integrity: reorg reconciled, followed chain re-anchored")
	return true
}

// reconcile walks h's ancestry back by hash until a block links to the pins
// (the common ancestor), so the tracker holds the branch, then votes for h.
// It refuses a branch that leaves the pins without rejoining them: that is
// unrelated history, not a reorg of the chain we follow.
func (f *chainFollower) reconcile(ctx context.Context, h *chainHeader) (int, bool) {
	t := f.tracker
	cur := h
	for depth := 1; depth <= t.window && cur.number > 0; depth++ {
		p := t.state.Load().pinAt(cur.number - 1)
		if p == nil {
			return 0, false
		}
		if p.hash == cur.parentHash {
			t.voteAsk(ctx, h)
			return depth, true
		}
		parent, ok := t.fetchHeader(ctx, classHead, "eth_getBlockByHash", "0x"+hex.EncodeToString(cur.parentHash[:]), auxKindHeader)
		if !ok || parent.number+1 != cur.number {
			return 0, false
		}
		cur = parent
	}
	return 0, false
}

// fetchFollowHeader gets block n for the follower, the interim no-source
// ask (class 1).
func (t *ChainTracker) fetchFollowHeader(ctx context.Context, n int64) (*chainHeader, bool) {
	return t.fetchHeader(ctx, classHead, "eth_getBlockByNumber", fmt.Sprintf("0x%x", n), auxKindFollow)
}
