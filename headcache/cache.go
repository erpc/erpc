package headcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
)

// Fetcher reads from the configured upstream path with both cache layers bypassed.
type Fetcher interface {
	BlockByNumber(context.Context, int64) (json.RawMessage, error)
	LogsByBlockHash(context.Context, string) (json.RawMessage, error)
	HeaderByNumber(context.Context, int64) (json.RawMessage, error)
}

type Options struct {
	Scope        Scope
	Depth        int64
	MaxBytes     int64
	MaxBlockSize int64
	MaxPerTick   int64
	Concurrency  int
	PollInterval time.Duration
	FetchTimeout time.Duration
	MaxStaleness time.Duration
	MaxLogsRange int64
	RecordTTL    time.Duration
}

type Event struct {
	Removed []*BlockRecord
	Added   []*BlockRecord
}

type Subscription struct {
	C      chan Event
	c      *Cache
	closed atomic.Bool
}

func (s *Subscription) Close() {
	if s != nil && s.c != nil {
		s.c.unsubscribe(s)
	}
}

type Stats struct {
	Hydrated  atomic.Int64
	Published atomic.Int64
	Hits      atomic.Int64
	Misses    atomic.Int64
	Reorgs    atomic.Int64
	Rejected  atomic.Int64
}

type Cache struct {
	opt     Options
	store   Store
	fleet   FleetStore
	fetcher Fetcher
	headFn  func(context.Context) int64
	logger  *zerolog.Logger
	Stats   Stats

	mu         sync.RWMutex
	snap       *Snapshot
	records    map[string]*BlockRecord
	pending    map[string]*BlockRecord
	blocked    map[string]bool
	windowHead int64
	window     []*header
	bytes      int64
	freshAt    time.Time
	subs       map[*Subscription]struct{}
	nowFn      func() time.Time
	kick       chan struct{}
	start      sync.Once
	stop       sync.Once
	cancel     context.CancelFunc
	done       chan struct{}
	stepMu     sync.Mutex
	lease      Lease
	sharedAt   map[string]time.Time
}

func New(opt Options, store Store, fetcher Fetcher, headFn func(context.Context) int64, logger *zerolog.Logger) *Cache {
	if opt.Depth < 1 {
		opt.Depth = 1
	}
	if opt.MaxPerTick < 1 {
		opt.MaxPerTick = minI64(16, opt.Depth)
	}
	if opt.Concurrency < 1 {
		opt.Concurrency = 4
	}
	if opt.MaxLogsRange < 1 {
		opt.MaxLogsRange = opt.Depth
	}
	if opt.PollInterval <= 0 {
		opt.PollInterval = time.Second
	}
	if opt.MaxStaleness <= 0 {
		opt.MaxStaleness = 2 * opt.PollInterval
	}
	if opt.RecordTTL <= 0 {
		opt.RecordTTL = time.Hour
	}
	if logger == nil {
		l := zerolog.Nop()
		logger = &l
	}
	c := &Cache{opt: opt, store: store, fetcher: fetcher, headFn: headFn, logger: logger,
		records: map[string]*BlockRecord{}, pending: map[string]*BlockRecord{}, subs: map[*Subscription]struct{}{}, nowFn: time.Now,
		windowHead: -1, kick: make(chan struct{}, 1), done: make(chan struct{}), blocked: map[string]bool{}, sharedAt: map[string]time.Time{}}
	c.fleet, _ = store.(FleetStore)
	return c
}

// Kick requests an early verification pass.
func (c *Cache) Kick() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

func (c *Cache) Start(ctx context.Context) {
	c.start.Do(func() {
		ctx, c.cancel = context.WithCancel(ctx)
		go c.run(ctx)
	})
}

func (c *Cache) Stop() {
	c.stop.Do(func() {
		if c.cancel != nil {
			c.cancel()
			<-c.done
		}
		c.releaseLeaseBounded()
		c.mu.Lock()
		for s := range c.subs {
			c.closeSubLocked(s)
		}
		c.mu.Unlock()
		telemetry.MetricHeadCacheFresh.WithLabelValues(c.opt.Scope.ProjectId, c.opt.Scope.NetworkId).Set(0)
	})
}

func (c *Cache) run(ctx context.Context) {
	defer close(c.done)
	defer c.releaseLeaseBounded()
	t := time.NewTicker(c.opt.PollInterval)
	defer t.Stop()
	for {
		c.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-c.kick:
		}
	}
}

// Tick refreshes the local view from upstream when leading, or from the
// lease holder's verified snapshot when following.
func (c *Cache) Tick(ctx context.Context) {
	c.stepMu.Lock()
	defer c.stepMu.Unlock()
	ctx, span := common.StartDetailSpan(ctx, "HeadCache.Tick")
	defer span.End()
	if c.opt.FetchTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.opt.FetchTimeout)
		defer cancel()
	}
	var err error
	if c.fleet != nil {
		err = c.fleetTick(ctx)
	} else {
		err = c.refresh(ctx)
	}
	fresh := c.Fresh()
	labels := []string{c.opt.Scope.ProjectId, c.opt.Scope.NetworkId}
	telemetry.MetricHeadCacheFresh.WithLabelValues(labels...).Set(boolFloat64(fresh))
	outcome := "stale"
	if err != nil {
		outcome = "error"
		c.logger.Debug().Err(err).Msg("head cache refresh failed")
		common.SetTraceSpanError(span, err)
	} else if fresh {
		outcome = "fresh"
	}
	telemetry.MetricHeadCacheRefreshTotal.WithLabelValues(labels[0], labels[1], outcome).Inc()
	c.expireSubscribers()
}

func boolFloat64(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func (c *Cache) leaseTTL() time.Duration {
	ttl := 3 * c.opt.PollInterval
	if fetch := c.opt.FetchTimeout + c.opt.PollInterval; ttl <= fetch {
		ttl = 2 * fetch
	}
	return ttl
}

func (c *Cache) snapshotTTL() time.Duration {
	if c.opt.MaxStaleness > 0 {
		return c.opt.MaxStaleness
	}
	return 2 * c.opt.PollInterval
}

func (c *Cache) releaseLease(ctx context.Context) {
	lease := c.lease
	c.lease = nil
	if lease != nil {
		if err := lease.Release(ctx); err != nil {
			c.logger.Debug().Err(err).Msg("failed to release head cache fleet lease")
		}
	}
}

func (c *Cache) releaseLeaseBounded() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c.releaseLease(ctx)
}

func (c *Cache) fleetTick(ctx context.Context) error {
	if c.lease == nil {
		lease, err := c.fleet.Acquire(ctx, c.opt.Scope, c.leaseTTL())
		if err != nil {
			c.invalidateAndClose()
			return fmt.Errorf("acquire head cache lease: %w", err)
		}
		if lease == nil {
			return c.readFleetSnapshot(ctx)
		}
		c.lease = lease
		c.sharedAt = map[string]time.Time{}
	} else {
		ok, err := c.lease.Renew(ctx, c.leaseTTL())
		if err != nil || !ok {
			c.lease = nil
			c.sharedAt = map[string]time.Time{}
			c.invalidateAndClose()
			if err != nil {
				return fmt.Errorf("renew head cache lease: %w", err)
			}
			return c.readFleetSnapshot(ctx)
		}
	}
	if err := c.refresh(ctx); err != nil {
		return err
	}
	if !c.Fresh() {
		return nil
	}
	// A leader may publish only a complete locally validated snapshot whose
	// payloads are all present in the shared store.
	snap, records := c.localView()
	if snap == nil || !completeWindow(snap, records) {
		return nil
	}
	current := make(map[string]struct{}, len(snap.Hashes))
	for _, h := range snap.Hashes {
		current[h] = struct{}{}
		r := records[h]
		last := c.sharedAt[h]
		refreshAfter := c.opt.RecordTTL / 2
		if refreshAfter <= 0 || c.nowFn().Sub(last) >= refreshAfter {
			if err := c.store.PutBlock(ctx, c.opt.Scope, r, c.opt.RecordTTL); err != nil {
				delete(c.sharedAt, h)
				c.invalidateAndClose()
				return fmt.Errorf("repair shared head cache payload %s: %w", h, err)
			}
			c.sharedAt[h] = c.nowFn()
		}
	}
	for h := range c.sharedAt {
		if _, ok := current[h]; !ok {
			delete(c.sharedAt, h)
		}
	}
	ok, err := c.lease.Renew(ctx, c.leaseTTL())
	if err != nil || !ok {
		c.lease = nil
		c.sharedAt = map[string]time.Time{}
		c.invalidateAndClose()
		if err != nil {
			return fmt.Errorf("renew head cache lease before publish: %w", err)
		}
		return nil
	}
	ok, err = c.lease.Publish(ctx, snap, c.snapshotTTL())
	if err != nil || !ok {
		c.lease = nil
		c.sharedAt = map[string]time.Time{}
		c.invalidateAndClose()
		if err != nil {
			return fmt.Errorf("publish head cache snapshot: %w", err)
		}
	}
	return nil
}

func (c *Cache) readFleetSnapshot(ctx context.Context) error {
	snap, err := c.fleet.ReadSnapshot(ctx, c.opt.Scope)
	if err != nil || snap == nil || len(snap.Hashes) == 0 || c.nowFn().Sub(snap.At) > c.opt.MaxStaleness {
		c.invalidateAndClose()
		if err != nil {
			return fmt.Errorf("read head cache snapshot: %w", err)
		}
		return errors.New("missing or stale head cache snapshot")
	}
	if snap.At.After(c.nowFn().Add(maxFutureSkew)) {
		c.invalidateAndClose()
		return errors.New("head cache snapshot timestamp is too far in the future")
	}
	if snap.At.After(c.nowFn()) {
		snap.At = c.nowFn()
	}
	if snap.Base() < 0 || snap.Head < 0 || int64(len(snap.Hashes)) > c.opt.Depth {
		c.invalidateAndClose()
		return errors.New("invalid head cache snapshot range")
	}
	old, oldRecords := c.localView()
	records := make(map[string]*BlockRecord, len(snap.Hashes))
	for i, hash := range snap.Hashes {
		if !isHexOfLen(hash, 64) {
			c.invalidateAndClose()
			return errors.New("invalid hash in head cache snapshot")
		}
		n := snap.Base() + int64(i)
		var expected *rawBlock
		if r := oldRecords[normHash(hash)]; r != nil {
			expected, _, _ = parseBlockHeader(r.Block)
			if expected != nil {
				expected.Hash, expected.ParentHash = normHash(expected.Hash), normHash(expected.ParentHash)
			}
		}
		r := oldRecords[normHash(hash)]
		var e error
		if r == nil {
			r, e = c.store.GetBlock(ctx, c.opt.Scope, hash)
		}
		if e != nil || r == nil {
			c.invalidateAndClose()
			if e != nil {
				return fmt.Errorf("read snapshot payload %s: %w", hash, e)
			}
			return fmt.Errorf("missing snapshot payload %s", hash)
		}
		if expected == nil {
			b, got, parseErr := parseBlockHeader(r.Block)
			if parseErr != nil || b == nil || got != n || normHash(b.Hash) != normHash(hash) {
				c.invalidateAndClose()
				return fmt.Errorf("inconsistent snapshot payload %s", hash)
			}
			b.Hash, b.ParentHash = normHash(b.Hash), normHash(b.ParentHash)
			expected = b
		}
		verified, e := validateRecord(r, n, expected, c.opt.MaxBlockSize)
		if e != nil {
			c.invalidateAndClose()
			return fmt.Errorf("validate snapshot payload %s: %w", hash, e)
		}
		if i > 0 && verified.ParentHash != normHash(snap.Hashes[i-1]) {
			c.invalidateAndClose()
			return errors.New("non-contiguous head cache snapshot payloads")
		}
		records[verified.Hash] = verified
	}
	gap := false
	if old != nil {
		common := int64(-1)
		for n := maxI64(old.Base(), snap.Base()); n <= minI64(old.Head, snap.Head); n++ {
			if old.HashAt(n) == snap.HashAt(n) {
				common = n
			}
		}
		gap = common < 0 || snap.Base() > old.Head+1
		if !gap && common >= 0 && common < old.Head {
			c.Stats.Reorgs.Add(1)
		}
	}
	c.install(&Snapshot{Head: snap.Head, Hashes: append([]string(nil), snap.Hashes...), At: snap.At, Incomplete: snap.Incomplete}, records, gap)
	return nil
}

func (c *Cache) invalidateAndClose() {
	c.invalidate()
	c.mu.Lock()
	for s := range c.subs {
		c.closeSubLocked(s)
	}
	c.mu.Unlock()
}

type header struct {
	raw json.RawMessage
	b   *rawBlock
	n   int64
}

func (c *Cache) getHeader(ctx context.Context, n int64) (*header, error) {
	raw, err := c.fetcher.HeaderByNumber(ctx, n)
	if err != nil {
		return nil, fmt.Errorf("fetch header %d: %w", n, err)
	}
	b, got, err := parseBlockHeader(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid header at %d: %w", n, err)
	}
	if got != n {
		return nil, fmt.Errorf("invalid header at %d: returned block %d", n, got)
	}
	b.Hash = normHash(b.Hash)
	b.ParentHash = normHash(b.ParentHash)
	return &header{b: b, n: got}, nil
}

func (c *Cache) refresh(ctx context.Context) error {
	if c.fetcher == nil || c.headFn == nil {
		return errors.New("head cache fetcher or head function is nil")
	}
	tip := c.headFn(ctx)
	if tip < 0 {
		return errors.New("head cache has no live tip")
	}
	old, oldRecords := c.localView()
	top, err := c.getHeader(ctx, tip)
	if err != nil {
		return err
	}
	base := tip - c.opt.Depth + 1
	if base < 0 {
		base = 0
	}
	// A matching tip proves that the previously verified header window is still
	// canonical. This is the steady-state one-header verification path.
	c.mu.RLock()
	windowHead := c.windowHead
	window := append([]*header(nil), c.window...)
	c.mu.RUnlock()
	if old != nil && tip == old.Head && top.b.Hash == old.HashAt(tip) && !old.Incomplete && completeWindow(old, oldRecords) {
		c.refreshTime(old)
		return nil
	}
	if old != nil && tip < old.Head && tip >= old.Base() && top.b.Hash == old.HashAt(tip) {
		return nil
	}
	if windowHead != tip || len(window) != int(tip-base+1) || window[len(window)-1].b.Hash != top.b.Hash {
		window, err = c.fetchWindow(ctx, base, tip, top)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.windowHead, c.window = tip, window
		c.blocked = map[string]bool{}
		c.mu.Unlock()
	}

	hashes := make([]string, len(window))
	for i, h := range window {
		hashes[i] = h.b.Hash
	}
	common := int64(-1)
	if old != nil {
		for n := maxI64(base, old.Base()); n <= minI64(tip, old.Head); n++ {
			if old.HashAt(n) == hashes[n-base] {
				common = n
			}
		}
	}
	gap := old != nil && common < 0
	if old != nil && common >= 0 && common < old.Head {
		c.Stats.Reorgs.Add(1)
		c.invalidate()
	}
	if gap {
		c.invalidate()
	}

	loaded := make([]*BlockRecord, len(window))
	missing := make([]int, 0)
	for i, h := range window {
		if c.blocked[h.b.Hash] {
			continue
		}
		if r := oldRecords[h.b.Hash]; r != nil {
			loaded[i] = r
			continue
		}
		if r := c.pending[h.b.Hash]; r != nil {
			loaded[i] = r
			continue
		}
		missing = append(missing, i)
	}
	c.parallel(ctx, len(missing), func(j int) {
		i := missing[j]
		if c.store == nil {
			return
		}
		r, e := c.store.GetBlock(ctx, c.opt.Scope, window[i].b.Hash)
		if e != nil {
			return
		}
		if r, e = validateRecord(r, window[i].n, window[i].b, c.opt.MaxBlockSize); e == nil {
			loaded[i] = r
		}
	})
	missing = missing[:0]
	for i, r := range loaded {
		if r == nil {
			missing = append(missing, i)
		}
	}
	// Hydrate from the tip backwards so every published subset is a contiguous
	// suffix. Older blocks can be filled over later ticks.
	if int64(len(missing)) > c.opt.MaxPerTick {
		missing = missing[len(missing)-int(c.opt.MaxPerTick):]
	}
	newRecords := make([]*BlockRecord, len(missing))
	loadErrors := make([]error, len(missing))
	c.parallel(ctx, len(missing), func(j int) {
		i := missing[j]
		r, e := c.loadRecord(ctx, window[i].n, window[i].b)
		loadErrors[j] = e
		if e == nil {
			newRecords[j] = r
		}
	})
	for j, r := range newRecords {
		if r != nil {
			i := missing[j]
			loaded[i] = r
			c.pending[r.Hash] = r
		}
	}
	for j, r := range newRecords {
		if r == nil && errors.Is(loadErrors[j], errRecordTooLarge) {
			i := missing[j]
			// A block over maxBlockBytes is a permanent hole for this head.
			// The suffix above it remains complete and useful.
			c.blocked[window[i].b.Hash] = true
		}
	}
	suffix := len(window)
	for suffix > 0 && loaded[suffix-1] != nil {
		suffix--
	}
	if suffix == len(window) {
		return nil
	}
	blockedBelow := false
	for i, h := range window {
		if c.blocked[h.b.Hash] && i < suffix {
			blockedBelow = true
		}
	}
	incomplete := suffix > 0 && !blockedBelow
	if suffix > 0 {
		gap = true
	}
	pubHashes := append([]string(nil), hashes[suffix:]...)
	recs := make(map[string]*BlockRecord, len(pubHashes))
	for i := suffix; i < len(window); i++ {
		recs[window[i].b.Hash] = loaded[i]
	}
	for h := range c.pending {
		if recs[h] == nil {
			delete(c.pending, h)
		}
	}
	c.install(&Snapshot{Head: tip, Hashes: pubHashes, At: c.nowFn(), Incomplete: incomplete}, recs, gap)
	c.pending = map[string]*BlockRecord{}
	return nil
}

func completeWindow(s *Snapshot, records map[string]*BlockRecord) bool {
	for _, h := range s.Hashes {
		if records[h] == nil {
			return false
		}
	}
	return true
}

func (c *Cache) fetchWindow(ctx context.Context, base, tip int64, top *header) ([]*header, error) {
	window := make([]*header, tip-base+1)
	window[tip-base] = top
	failures := make([]error, len(window)-1)
	c.parallel(ctx, len(window)-1, func(j int) {
		n := base + int64(j)
		h, err := c.getHeader(ctx, n)
		if err != nil {
			failures[j] = err
			return
		}
		window[j] = h
	})
	for _, err := range failures {
		if err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for i := 1; i < len(window); i++ {
		if window[i].b.ParentHash != window[i-1].b.Hash {
			return nil, fmt.Errorf("header parent mismatch at %d", window[i].n)
		}
	}
	return window, nil
}

func (c *Cache) parallel(ctx context.Context, n int, fn func(int)) {
	workers := c.opt.Concurrency
	if workers > n {
		workers = n
	}
	if workers < 1 {
		return
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if ctx.Err() == nil {
					fn(i)
				}
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
}

func (c *Cache) localView() (*Snapshot, map[string]*BlockRecord) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.snap == nil {
		return nil, nil
	}
	s := *c.snap
	s.Hashes = append([]string(nil), c.snap.Hashes...)
	r := make(map[string]*BlockRecord, len(c.records))
	for k, v := range c.records {
		r[k] = v
	}
	return &s, r
}

func (c *Cache) loadRecord(ctx context.Context, n int64, expected *rawBlock) (*BlockRecord, error) {
	block, err := c.fetchBlock(ctx, n)
	if err != nil {
		return nil, err
	}
	b, got, err := parseBlockHeader(block)
	if b != nil {
		b.Hash, b.ParentHash = normHash(b.Hash), normHash(b.ParentHash)
	}
	if err != nil {
		c.Stats.Rejected.Add(1)
		return nil, fmt.Errorf("parse hydrated block %d: %w", n, err)
	}
	if got != n || b.Hash != expected.Hash || b.ParentHash != expected.ParentHash {
		c.Stats.Rejected.Add(1)
		return nil, fmt.Errorf("hydrated block %d does not match verified header", n)
	}
	logsCtx, cancel := c.fetchContext(ctx)
	logs, err := c.fetcher.LogsByBlockHash(logsCtx, b.Hash)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("fetch logs for block %d: %w", n, err)
	}
	rec, err := buildRecord(block, logs, c.opt.MaxBlockSize)
	if err != nil {
		c.Stats.Rejected.Add(1)
		return nil, fmt.Errorf("invalid hydrated block %d: %w", n, err)
	}
	if rec.Number != n || rec.Hash != expected.Hash || rec.ParentHash != expected.ParentHash {
		c.Stats.Rejected.Add(1)
		return nil, fmt.Errorf("hydrated block %d identity differs from verified header", n)
	}
	c.Stats.Hydrated.Add(1)
	if c.store != nil && c.fleet == nil {
		if err := c.store.PutBlock(ctx, c.opt.Scope, rec, c.opt.RecordTTL); err != nil {
			c.logger.Debug().Err(err).Int64("number", n).Msg("failed to share head cache record")
		}
	}
	return rec, nil
}

func validateRecord(rec *BlockRecord, n int64, expected *rawBlock, maxBytes int64) (*BlockRecord, error) {
	if rec == nil {
		return nil, errors.New("nil record")
	}
	verified, err := buildRecord(rec.Block, rec.Logs, maxBytes)
	if err != nil {
		return nil, err
	}
	if rec.Number != verified.Number || normHash(rec.Hash) != verified.Hash || normHash(rec.ParentHash) != verified.ParentHash {
		return nil, errors.New("record metadata does not match its payload")
	}
	if verified.Number != n || verified.Hash != expected.Hash || verified.ParentHash != expected.ParentHash {
		return nil, errors.New("record does not match locally verified header")
	}
	return verified, nil
}

func (c *Cache) fetchContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.opt.FetchTimeout > 0 {
		return context.WithTimeout(ctx, c.opt.FetchTimeout)
	}
	return context.WithCancel(ctx)
}

func (c *Cache) fetchBlock(ctx context.Context, n int64) (json.RawMessage, error) {
	fctx, cancel := c.fetchContext(ctx)
	defer cancel()
	block, err := c.fetcher.BlockByNumber(fctx, n)
	if err != nil {
		return nil, fmt.Errorf("fetch block %d: %w", n, err)
	}
	return block, nil
}

func (c *Cache) refreshTime(snap *Snapshot) {
	c.mu.Lock()
	if c.snap != nil && c.snap.Head == snap.Head && completeWindow(c.snap, c.records) {
		c.freshAt = c.nowFn()
		c.snap.At = c.freshAt
	}
	c.mu.Unlock()
}

func (c *Cache) invalidate() {
	c.mu.Lock()
	c.freshAt = time.Time{}
	c.mu.Unlock()
}

func (c *Cache) install(snap *Snapshot, recs map[string]*BlockRecord, gap bool) {
	c.mu.Lock()
	old := c.snap
	var total int64
	for _, r := range recs {
		total += r.Size()
	}
	trim := 0
	for c.opt.MaxBytes > 0 && total > c.opt.MaxBytes && trim < len(snap.Hashes) {
		h := snap.Hashes[trim]
		total -= recs[h].Size()
		delete(recs, h)
		trim++
	}
	if c.opt.MaxBytes > 0 && total > c.opt.MaxBytes {
		c.freshAt = time.Time{}
		for s := range c.subs {
			c.closeSubLocked(s)
		}
		c.mu.Unlock()
		return
	}
	if trim == len(snap.Hashes) {
		c.freshAt = time.Time{}
		for s := range c.subs {
			c.closeSubLocked(s)
		}
		c.mu.Unlock()
		return
	}
	snap.Hashes = snap.Hashes[trim:]
	var ev Event
	if old != nil {
		for n := old.Head; n >= old.Base(); n-- {
			if snap.HashAt(n) == old.HashAt(n) || n < snap.Base() {
				continue
			}
			if r := c.records[old.HashAt(n)]; r != nil {
				ev.Removed = append(ev.Removed, r)
			} else {
				gap = true
			}
		}
		for n := snap.Base(); n <= snap.Head; n++ {
			if old.HashAt(n) == snap.HashAt(n) || n < old.Base() {
				continue
			}
			if r := recs[snap.HashAt(n)]; r != nil {
				ev.Added = append(ev.Added, r)
			} else {
				gap = true
			}
		}
	} else {
		for n := snap.Base(); n <= snap.Head; n++ {
			if r := recs[snap.HashAt(n)]; r != nil {
				ev.Added = append(ev.Added, r)
			}
		}
	}
	if old != nil && snap.Base() > old.Head+1 {
		gap = true
	}
	c.snap, c.records, c.bytes, c.freshAt = snap, recs, total, snap.At
	c.Stats.Published.Add(1)
	if gap {
		for s := range c.subs {
			c.closeSubLocked(s)
		}
	} else if len(ev.Removed)+len(ev.Added) > 0 {
		for s := range c.subs {
			select {
			case s.C <- ev:
			default:
				c.closeSubLocked(s)
			}
		}
	}
	c.mu.Unlock()
}

func (c *Cache) expireSubscribers() {
	c.mu.Lock()
	if c.snap != nil && c.nowFn().Sub(c.freshAt) > c.opt.MaxStaleness {
		for s := range c.subs {
			c.closeSubLocked(s)
		}
	}
	c.mu.Unlock()
}

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

func (c *Cache) SubscriberCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.subs)
}

func (c *Cache) viewLocked() *Snapshot {
	if c.snap == nil || c.nowFn().Sub(c.freshAt) > c.opt.MaxStaleness {
		return nil
	}
	return c.snap
}

func (c *Cache) Fresh() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.viewLocked() != nil
}

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

func (c *Cache) LogsRange(from, to int64, f *LogFilter) ([]json.RawMessage, bool) {
	if from < 0 || to < from || to-from >= c.opt.MaxLogsRange {
		c.hit(false)
		return nil, false
	}
	count := to - from + 1
	c.mu.RLock()
	s := c.viewLocked()
	recs := make([]*BlockRecord, 0, count)
	if s != nil {
		for i := int64(0); i < count; i++ {
			n := from + i
			r := c.records[s.HashAt(n)]
			if r == nil {
				recs = nil
				break
			}
			recs = append(recs, r)
		}
	}
	c.mu.RUnlock()
	if int64(len(recs)) != count {
		c.hit(false)
		return nil, false
	}
	out := []json.RawMessage{}
	for _, r := range recs {
		logs, err := r.FilterLogs(f, false)
		if err != nil {
			c.hit(false)
			return nil, false
		}
		out = append(out, logs...)
	}
	c.hit(true)
	return out, true
}

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
	logs, err := r.FilterLogs(f, false)
	c.hit(err == nil)
	return logs, err == nil
}

const maxFutureSkew = 5 * time.Second

func maxI64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func minI64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
