package headcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// Fetcher performs hydration reads against upstreams. Implementations must
// bypass this cache (otherwise the follower would read its own output).
type Fetcher interface {
	// BlockByNumber returns eth_getBlockByNumber(n, true) raw result.
	BlockByNumber(ctx context.Context, n int64) (json.RawMessage, error)
	// LogsByBlockHash returns eth_getLogs({blockHash}) raw result.
	LogsByBlockHash(ctx context.Context, hash string) (json.RawMessage, error)
}

// Options configure a Cache.
type Options struct {
	Scope        Scope
	Holder       string // unique per process
	Depth        int64
	MaxBytes     int64
	MaxBlockSize int64
	MaxPerTick   int64
	Concurrency  int
	PollInterval time.Duration
	FetchTimeout time.Duration
	MaxStaleness time.Duration
	LeaseTTL     time.Duration
	MaxLogsRange int64
	RecordTTL    time.Duration
}

// Event is delivered to subscribers after each committed canonical change.
// Removed blocks (orphaned by a reorg, highest first) precede Added blocks
// (new canonical, ascending).
type Event struct {
	Removed []*BlockRecord
	Added   []*BlockRecord
}

// Subscription receives Events. C is closed when the subscriber falls behind
// (queue full), when the cache stops, or on Close.
type Subscription struct {
	C      chan Event
	c      *Cache
	closed atomic.Bool
}

func (s *Subscription) Close() { s.c.unsubscribe(s) }

// Stats exposes counters (tests/metrics).
type Stats struct {
	Hydrated     atomic.Int64 // records fetched from upstream by this process
	Published    atomic.Int64 // snapshots published by this process
	Hits         atomic.Int64
	Misses       atomic.Int64
	Reorgs       atomic.Int64
	LeaderEpochs atomic.Int64
	Rejected     atomic.Int64 // hydrated blocks rejected by validation
}

// Cache is one network's head cache. A process runs one Cache per network;
// it hydrates when it holds the lease and consumes published snapshots
// otherwise. Reads never block on upstreams.
type Cache struct {
	opt     Options
	store   Store
	fetcher Fetcher
	headFn  func(context.Context) int64
	logger  *zerolog.Logger
	Stats   Stats

	mu        sync.RWMutex
	snap      *Snapshot
	records   map[string]*BlockRecord // hashes of snap only
	bytes     int64
	freshAt   time.Time
	subs      map[*Subscription]struct{}
	lease     *Lease
	nowFn     func() time.Time
	kick      chan struct{}
	stopOnce  sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
	stepMu    sync.Mutex
	publishCt uint64
}

func New(opt Options, store Store, fetcher Fetcher, headFn func(context.Context) int64, logger *zerolog.Logger) *Cache {
	if opt.Concurrency < 1 {
		opt.Concurrency = 1
	}
	if opt.MaxPerTick < 1 {
		opt.MaxPerTick = 1
	}
	if opt.RecordTTL <= 0 {
		opt.RecordTTL = time.Hour
	}
	if logger == nil {
		l := zerolog.Nop()
		logger = &l
	}
	return &Cache{
		opt: opt, store: store, fetcher: fetcher, headFn: headFn, logger: logger,
		records: map[string]*BlockRecord{},
		subs:    map[*Subscription]struct{}{},
		nowFn:   time.Now,
		kick:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
}

// Kick requests a prompt follow step (e.g. on a head advance).
func (c *Cache) Kick() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// Start runs the follow loop until ctx is done or Stop is called.
func (c *Cache) Start(ctx context.Context) {
	ctx, c.cancel = context.WithCancel(ctx)
	go c.run(ctx)
}

func (c *Cache) Stop() {
	c.stopOnce.Do(func() {
		if c.cancel != nil {
			c.cancel()
			<-c.done
		}
		c.mu.Lock()
		for s := range c.subs {
			c.closeSubLocked(s)
		}
		c.mu.Unlock()
	})
}

func (c *Cache) run(ctx context.Context) {
	defer close(c.done)
	defer c.releaseLease()
	var watch <-chan struct{}
	var stopWatch func()
	defer func() {
		if stopWatch != nil {
			stopWatch()
		}
	}()
	t := time.NewTicker(c.opt.PollInterval)
	defer t.Stop()
	for {
		if watch == nil {
			if w, stop, err := c.store.WatchSnapshots(ctx, c.opt.Scope); err == nil {
				watch, stopWatch = w, stop
			}
		}
		c.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-c.kick:
		case _, ok := <-watch:
			if !ok {
				// Subscription lost: resubscribe next iteration; the ticker
				// keeps LoadSnapshot polling meanwhile.
				watch = nil
				if stopWatch != nil {
					stopWatch()
					stopWatch = nil
				}
			}
		}
	}
}

// Tick performs one coordination step: renew/acquire the lease, then either
// follow the chain (leader) or load the latest snapshot (follower).
func (c *Cache) Tick(ctx context.Context) {
	c.stepMu.Lock()
	defer c.stepMu.Unlock()
	if c.lease != nil {
		l, err := c.store.RenewLease(ctx, c.lease, c.opt.LeaseTTL)
		if err != nil {
			if errors.Is(err, ErrLeaseLost) {
				c.logger.Warn().Msg("headcache lease lost, stopping hydration")
			}
			c.lease = nil
		} else {
			c.lease = l
		}
	}
	if c.lease == nil {
		l, err := c.store.AcquireLease(ctx, c.opt.Scope, c.opt.Holder, c.opt.LeaseTTL)
		if err == nil {
			c.lease = l
			c.Stats.LeaderEpochs.Add(1)
			c.publishCt = 0
			c.logger.Info().Uint64("epoch", l.Epoch).Msg("headcache acquired hydration lease")
		}
	}
	if c.lease != nil {
		c.lead(ctx)
		return
	}
	c.consume(ctx)
}

func (c *Cache) releaseLease() {
	c.stepMu.Lock()
	defer c.stepMu.Unlock()
	if c.lease != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = c.store.ReleaseLease(ctx, c.lease)
		cancel()
		c.lease = nil
	}
}

// consume adopts the latest committed snapshot (follower role).
func (c *Cache) consume(ctx context.Context) {
	snap, err := c.store.LoadSnapshot(ctx, c.opt.Scope)
	if err != nil || !snap.Valid() {
		return
	}
	c.mu.RLock()
	cur := c.snap
	c.mu.RUnlock()
	// Age guard on the writer timestamp: a snapshot older than MaxStaleness
	// (dead leader) or dated in the future beyond a small skew is not adopted.
	now := c.nowFn()
	if age := now.Sub(snap.At); age > c.opt.MaxStaleness || age < -maxFutureSkew {
		return
	}
	if cur != nil && cur.Epoch == snap.Epoch && cur.Seq == snap.Seq {
		return // unchanged: freshness is NOT extended
	}
	if cur != nil && (snap.Epoch < cur.Epoch || (snap.Epoch == cur.Epoch && snap.Seq < cur.Seq)) {
		return // never go backwards
	}
	recs := make(map[string]*BlockRecord, len(snap.Hashes))
	c.mu.RLock()
	for _, h := range snap.Hashes {
		if r, ok := c.records[h]; ok {
			recs[h] = r
		}
	}
	c.mu.RUnlock()
	for i, h := range snap.Hashes {
		if _, ok := recs[h]; ok {
			continue
		}
		r, err := c.store.GetBlock(ctx, c.opt.Scope, h)
		if err != nil {
			if errors.Is(err, ErrStoreUnavailable) {
				return
			}
			continue // missing/expired record => height is a cache miss
		}
		if r.Hash != h || r.Number != snap.Base()+int64(i) {
			continue
		}
		recs[h] = r
	}
	c.apply(snap, recs)
}

// lead follows the chain as lease holder and publishes a snapshot.
func (c *Cache) lead(ctx context.Context) {
	tip := c.headFn(ctx)
	if tip <= 0 {
		return
	}
	c.mu.RLock()
	var hashes []string
	var head int64 = -1
	recs := make(map[string]*BlockRecord, len(c.records))
	if c.snap != nil {
		hashes = append(hashes, c.snap.Hashes...)
		head = c.snap.Head
		for k, v := range c.records {
			recs[k] = v
		}
	}
	c.mu.RUnlock()

	// A new leader (or first tick) adopts the committed snapshot as a
	// starting point but re-verifies it against upstreams below.
	if len(hashes) == 0 {
		if s, err := c.store.LoadSnapshot(ctx, c.opt.Scope); err == nil && s.Valid() {
			hashes = append(hashes, s.Hashes...)
			head = s.Head
			for _, h := range hashes {
				if r, err := c.store.GetBlock(ctx, c.opt.Scope, h); err == nil {
					recs[h] = r
				}
			}
		}
	}
	budget := c.opt.MaxPerTick
	var newRecs []*BlockRecord

	// 1) Re-verify the window tip (catches same-height and shortening reorgs)
	//    and walk back to the common ancestor.
	for len(hashes) > 0 && budget > 0 {
		blk, err := c.fetchBlock(ctx, head)
		budget--
		if err != nil {
			return // cannot verify: do not publish (freshness lapses)
		}
		b, _, perr := parseBlockHeader(blk)
		if perr == nil && normHash(b.Hash) == hashes[len(hashes)-1] {
			break
		}
		hashes = hashes[:len(hashes)-1]
		head--
		c.Stats.Reorgs.Add(1)
	}
	if len(hashes) == 0 {
		head = -1
	}
	if budget == 0 && len(hashes) > 0 && head < tip {
		// walked back but out of budget; publish what is verified
	}

	// 2) Walk forward in parallel batches, linking sequentially.
	start := head + 1
	if head < 0 || tip-head > c.opt.Depth {
		start = tip - minI64(budget, c.opt.Depth) + 1
		if start < 0 {
			start = 0
		}
		hashes = nil
	}
	end := minI64(tip, start+budget-1)
	if end >= start {
		fetched := c.hydrateRange(ctx, start, end)
		for n := start; n <= end; n++ {
			r := fetched[n-start]
			if r == nil {
				break // gap: stop extending; never publish past a hole
			}
			if len(hashes) > 0 && r.ParentHash != hashes[len(hashes)-1] {
				// Reorg discovered while extending: drop our top; next tick
				// re-verifies and walks back further.
				hashes = hashes[:len(hashes)-1]
				c.Stats.Reorgs.Add(1)
				break
			}
			hashes = append(hashes, r.Hash)
			recs[r.Hash] = r
			newRecs = append(newRecs, r)
		}
	}
	if len(hashes) == 0 {
		return
	}
	if int64(len(hashes)) > c.opt.Depth {
		hashes = hashes[int64(len(hashes))-c.opt.Depth:]
	}
	newHead := start - 1 + int64(0)
	// head of chain = base + len - 1; recompute from the last record.
	last := recs[hashes[len(hashes)-1]]
	if last == nil {
		return
	}
	newHead = last.Number
	// Every referenced record must exist in the store before publication
	// (publish-after-hydrate).
	for _, r := range newRecs {
		if err := c.store.PutBlock(ctx, c.opt.Scope, r, c.opt.RecordTTL); err != nil {
			return
		}
	}
	c.publishCt++
	snap := &Snapshot{Epoch: c.lease.Epoch, Seq: c.publishCt, Head: newHead, Hashes: hashes, At: c.nowFn()}
	if err := c.store.PublishSnapshot(ctx, c.lease, snap); err != nil {
		if errors.Is(err, ErrLeaseLost) {
			c.logger.Warn().Msg("headcache publish rejected: lease lost")
			c.lease = nil
		}
		return
	}
	c.Stats.Published.Add(1)
	final := make(map[string]*BlockRecord, len(hashes))
	for _, h := range hashes {
		if r, ok := recs[h]; ok {
			final[h] = r
		}
	}
	c.apply(snap, final)
}

func minI64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func (c *Cache) fetchBlock(ctx context.Context, n int64) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, c.opt.FetchTimeout)
	defer cancel()
	return c.fetcher.BlockByNumber(ctx, n)
}

// hydrateRange fetches blocks [from..to] with bounded concurrency. A nil entry
// means that block could not be fully and consistently hydrated.
func (c *Cache) hydrateRange(ctx context.Context, from, to int64) []*BlockRecord {
	out := make([]*BlockRecord, to-from+1)
	sem := make(chan struct{}, c.opt.Concurrency)
	var wg sync.WaitGroup
	for n := from; n <= to; n++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(n int64) {
			defer wg.Done()
			defer func() { <-sem }()
			r, err := c.hydrate(ctx, n)
			if err != nil {
				c.logger.Debug().Err(err).Int64("block", n).Msg("headcache hydration failed")
				return
			}
			out[n-from] = r
		}(n)
	}
	wg.Wait()
	return out
}

func (c *Cache) hydrate(ctx context.Context, n int64) (*BlockRecord, error) {
	blk, err := c.fetchBlock(ctx, n)
	if err != nil {
		return nil, err
	}
	b, bn, err := parseBlockHeader(blk)
	if err != nil {
		return nil, err
	}
	if bn != n {
		return nil, fmt.Errorf("block number mismatch")
	}
	lctx, cancel := context.WithTimeout(ctx, c.opt.FetchTimeout)
	logs, err := c.fetcher.LogsByBlockHash(lctx, b.Hash)
	cancel()
	if err != nil {
		return nil, err
	}
	r, err := buildRecord(blk, logs, c.opt.MaxBlockSize)
	if err != nil {
		c.Stats.Rejected.Add(1)
		return nil, err
	}
	c.Stats.Hydrated.Add(1)
	return r, nil
}

// apply installs a committed snapshot locally and notifies subscribers.
func (c *Cache) apply(snap *Snapshot, recs map[string]*BlockRecord) {
	c.mu.Lock()
	old := c.snap
	var ev Event
	if old != nil {
		// Heights whose canonical hash changed (or vanished) were orphaned.
		for n := old.Head; n >= old.Base(); n-- {
			oh := old.HashAt(n)
			if snap.HashAt(n) == oh {
				continue
			}
			if n < snap.Base() {
				continue // just scrolled out of the window, not orphaned
			}
			if r := c.records[oh]; r != nil {
				ev.Removed = append(ev.Removed, r)
			}
		}
	}
	for n := snap.Base(); n <= snap.Head; n++ {
		h := snap.HashAt(n)
		if old != nil && old.HashAt(n) == h {
			continue
		}
		if old != nil && n < old.Base() {
			continue // backfill below the old window is not "new head"
		}
		if r := recs[h]; r != nil {
			ev.Added = append(ev.Added, r)
		}
	}
	// Enforce the byte budget by dropping the oldest records (those heights
	// become cache misses, never partial answers).
	var total int64
	for _, r := range recs {
		total += r.Size()
	}
	for n := snap.Base(); total > c.opt.MaxBytes && n <= snap.Head; n++ {
		if r := recs[snap.HashAt(n)]; r != nil {
			total -= r.Size()
			delete(recs, r.Hash)
		}
	}
	c.snap = snap
	c.records = recs
	c.bytes = total
	c.freshAt = c.nowFn()
	if len(ev.Removed) > 0 || len(ev.Added) > 0 {
		for s := range c.subs {
			select {
			case s.C <- ev:
			default:
				c.closeSubLocked(s) // slow consumer: drop, never buffer unbounded
			}
		}
	}
	c.mu.Unlock()
}

// Subscribe registers for canonical-change events with a bounded queue.
func (c *Cache) Subscribe(queue int) *Subscription {
	if queue < 1 {
		queue = 1
	}
	s := &Subscription{C: make(chan Event, queue), c: c}
	c.mu.Lock()
	c.subs[s] = struct{}{}
	c.mu.Unlock()
	return s
}

func (c *Cache) unsubscribe(s *Subscription) {
	c.mu.Lock()
	c.closeSubLocked(s)
	c.mu.Unlock()
}

func (c *Cache) closeSubLocked(s *Subscription) {
	if _, ok := c.subs[s]; ok {
		delete(c.subs, s)
	}
	if s.closed.CompareAndSwap(false, true) {
		close(s.C)
	}
}

// SubscriberCount (tests).
func (c *Cache) SubscriberCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.subs)
}

// view returns the snapshot and records when fresh, else nil.
func (c *Cache) viewLocked() *Snapshot {
	if c.snap == nil || c.nowFn().Sub(c.freshAt) > c.opt.MaxStaleness {
		return nil
	}
	return c.snap
}

// Fresh reports whether the cache currently serves.
func (c *Cache) Fresh() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.viewLocked() != nil
}

// Head returns the served head or -1.
func (c *Cache) Head() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if s := c.viewLocked(); s != nil {
		return s.Head
	}
	return -1
}

func (c *Cache) hit(ok bool) {
	if ok {
		c.Stats.Hits.Add(1)
	} else {
		c.Stats.Misses.Add(1)
	}
}

// BlockByNumber serves a canonical block within the window.
func (c *Cache) BlockByNumber(n int64, full bool) (json.RawMessage, bool) {
	c.mu.RLock()
	s := c.viewLocked()
	var r *BlockRecord
	if s != nil {
		r = c.records[s.HashAt(n)]
	}
	c.mu.RUnlock()
	if r == nil {
		c.hit(false)
		return nil, false
	}
	out, err := r.BlockJSON(full)
	c.hit(err == nil)
	return out, err == nil
}

// BlockByHash serves a block only if it is canonical in the current window.
func (c *Cache) BlockByHash(hash string, full bool) (json.RawMessage, bool) {
	c.mu.RLock()
	s := c.viewLocked()
	var r *BlockRecord
	if s != nil {
		if rr := c.records[normHash(hash)]; rr != nil && s.HashAt(rr.Number) == rr.Hash {
			r = rr
		}
	}
	c.mu.RUnlock()
	if r == nil {
		c.hit(false)
		return nil, false
	}
	out, err := r.BlockJSON(full)
	c.hit(err == nil)
	return out, err == nil
}

// LogsRange serves eth_getLogs for [from..to] only if every block is held.
func (c *Cache) LogsRange(from, to int64, f *LogFilter) ([]json.RawMessage, bool) {
	if from > to || to-from+1 > c.opt.MaxLogsRange {
		c.hit(false)
		return nil, false
	}
	c.mu.RLock()
	s := c.viewLocked()
	recs := make([]*BlockRecord, 0, to-from+1)
	if s != nil {
		for n := from; n <= to; n++ {
			r := c.records[s.HashAt(n)]
			if r == nil {
				recs = nil
				break
			}
			recs = append(recs, r)
		}
	}
	c.mu.RUnlock()
	if len(recs) != int(to-from+1) {
		c.hit(false)
		return nil, false
	}
	out := []json.RawMessage{}
	for _, r := range recs {
		l, err := r.FilterLogs(f, false)
		if err != nil {
			c.hit(false)
			return nil, false
		}
		out = append(out, l...)
	}
	c.hit(true)
	return out, true
}

// LogsByHash serves eth_getLogs({blockHash}) for a canonical window block.
func (c *Cache) LogsByHash(hash string, f *LogFilter) ([]json.RawMessage, bool) {
	c.mu.RLock()
	s := c.viewLocked()
	var r *BlockRecord
	if s != nil {
		if rr := c.records[normHash(hash)]; rr != nil && s.HashAt(rr.Number) == rr.Hash {
			r = rr
		}
	}
	c.mu.RUnlock()
	if r == nil {
		c.hit(false)
		return nil, false
	}
	l, err := r.FilterLogs(f, false)
	c.hit(err == nil)
	return l, err == nil
}

// maxFutureSkew bounds how far in the future a snapshot's writer timestamp
// may be before readers treat it as invalid.
const maxFutureSkew = 5 * time.Second
