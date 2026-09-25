package erpc

import (
	"context"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
)

// headSourceSelector decides the tip the head cache extends to.
//
// "served": the network's served latest tip when it is known and fresh, else
// live eth_blockNumber discovery, so a cold (unknown) or dormant/stale served
// tip can never freeze subscriptions. Live discovery also caps the served tip
// (never publish above what the live path sees). "max": live discovery only.
//
// A tip below the current window never removes blocks: the cache only uses
// the tip to bound forward extension, and orphans are detected solely by
// hash/parent re-verification. So a fallback that ran ahead (live 25) and a
// later slower served tip (20) holds the window at 25 with no removals.
type headSourceSelector struct {
	mode   string
	maxAge time.Duration
	served func(context.Context) int64
	live   func(context.Context) int64
	now    func() time.Time

	mu        sync.Mutex
	last      int64
	changedAt time.Time
}

func newHeadSourceSelector(mode string, maxAge time.Duration, served, live func(context.Context) int64) *headSourceSelector {
	return &headSourceSelector{mode: mode, maxAge: maxAge, served: served, live: live, now: time.Now}
}

// Head returns the tip to extend to (0 = unknown, skip this tick).
//
// The served tip is stale only when it lags live discovery AND has not
// advanced for maxAge: on a quiet chain where served == live it stays
// authoritative however long blocks take.
func (s *headSourceSelector) Head(ctx context.Context) int64 {
	live := s.live(ctx)
	if s.mode != common.HeadCacheHeadSourceServed || s.served == nil {
		return live
	}
	served := s.served(ctx)
	if served <= 0 {
		return live // cold: served tip unknown
	}
	if live > 0 && live <= served {
		return live // never publish above what live discovery confirms
	}
	now := s.now()
	s.mu.Lock()
	if served != s.last {
		s.last, s.changedAt = served, now
	}
	stale := s.maxAge > 0 && now.Sub(s.changedAt) > s.maxAge
	s.mu.Unlock()
	if stale && live > 0 {
		return live // dormant/stuck served tip: don't freeze the window
	}
	return served
}
