package blockstore

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
	"golang.org/x/sync/singleflight"
)

// Fetcher reads from the configured upstream path with both cache layers bypassed.
type Fetcher interface {
	BlockByNumber(context.Context, int64) (json.RawMessage, error)
	LogsByBlockHash(context.Context, string) (json.RawMessage, error)
	HeaderByNumber(context.Context, int64) (json.RawMessage, error)
}

type Options struct {
	Scope Scope
	Depth int64
	// MaxBytes bounds the process-local cache of on-demand block bodies and
	// log lists. Window headers are not counted.
	MaxBytes     int64
	MaxBlockSize int64
	// MaxPerTick bounds how many older headers one tick backfills while the
	// window is shorter than Depth (cold start). Forward extension always
	// fetches every new height.
	MaxPerTick   int64
	Concurrency  int
	PollInterval time.Duration
	FetchTimeout time.Duration
	MaxStaleness time.Duration
	MaxLogsRange int64
	RecordTTL    time.Duration
	// PeerWait bounds how long an on-demand miss waits for another replica
	// that holds the fill lock for the same payload. 0 disables the lock.
	PeerWait time.Duration
	// Latest is the network's in-memory latest block number (no upstream
	// call). It bounds which heights adopted from client responses are held.
	Latest func(context.Context) int64
	// Finalized is the network's in-memory finalized height (no upstream
	// call; <= 0 when unknown). A held header observed canonical at or below
	// it is served without fresh confirmation or parent linkage.
	Finalized func(context.Context) int64
	// AlwaysFollow keeps header following on without any subscriber. By
	// default the window follows the chain only while a WebSocket subscriber
	// exists anywhere in the fleet; otherwise it is built from client reads.
	AlwaysFollow bool
}

// Event reports a canonical window change. Records carry the window header
// as Block and the block's logs when they were already cached (nil otherwise).
type Event struct {
	Removed []*BlockRecord
	Added   []*BlockRecord
}

type Subscription struct {
	C      chan Event
	c      *Cache
	logs   bool
	closed atomic.Bool
	// primed is set once the subscription was sent an event. Until then it
	// has no continuity to lose, so staleness or a gap does not close it: it
	// starts at the next contiguous event once following is verified, unless
	// following has not produced a verified view within subscribeGrace.
	primed bool
	since  time.Time
}

func (s *Subscription) Close() {
	if s != nil && s.c != nil {
		s.c.unsubscribe(s)
	}
}

type Stats struct {
	// Hydrated counts on-demand body/log payloads fetched from upstream.
	Hydrated  atomic.Int64
	Published atomic.Int64
	Hits      atomic.Int64
	Misses    atomic.Int64
	Reorgs    atomic.Int64
	Rejected  atomic.Int64
}

// Fetch reasons for erpc_blockstore_fetch_total. "background" is used only
// with Options.AlwaysFollow; subscriber-driven header following is
// "subscription"; on-demand payloads are "miss". Adoption from client
// responses (pull.go) never fetches.
const (
	FetchReasonBackground   = "background"
	FetchReasonMiss         = "miss"
	FetchReasonSubscription = "subscription"
	// FetchReasonFill is the logs fill's one unfiltered range call that
	// answers a client's eth_getLogs miss.
	FetchReasonFill = "fill"
)

type Cache struct {
	opt     Options
	store   Store
	fleet   FleetStore
	fetcher Fetcher
	headFn  func(context.Context) int64
	logger  *zerolog.Logger
	Stats   Stats

	mu sync.RWMutex
	// snap and headers are the served view: the canonical hashes and their
	// verified headers.
	snap    *Snapshot
	headers map[string]*header
	// bodies and logs are on-demand payloads for hashes in the served view.
	bodies       map[string]json.RawMessage
	logs         map[string]json.RawMessage
	payloadBytes int64
	// window is the refresher's verified header chain ending at windowHead.
	windowHead int64
	window     []*header
	freshAt    time.Time
	subs       map[*Subscription]struct{}
	logsSubs   int
	nowFn      func() time.Time
	kick       chan struct{}
	start      sync.Once
	stop       sync.Once
	cancel     context.CancelFunc
	done       chan struct{}
	stepMu     sync.Mutex
	lease      Lease
	sharedAt   map[string]time.Time
	// suspect is set when an on-demand fetch returned a different hash than
	// the window holds; the next tick re-verifies the tip even if unchanged.
	suspect atomic.Bool
	sf      singleflight.Group
	// leaderFailures counts consecutive leader ticks that failed to refresh or
	// publish; at maxLeaderFailures the leader steps down (see fleetTick).
	leaderFailures int

	// Pull view: headers adopted from client responses (see pull.go).
	pull        map[int64]*pullEntry
	pullHashes  map[string]int64
	pullTop     int64
	canon       CanonicalIndex
	sharedCanon map[string]time.Time
	presence    PresenceStore
	// following is whether the last tick followed headers.
	following bool
	// adoptParses counts AdoptBlock calls that took the full parse (tests).
	adoptParses atomic.Int64
}

// maxLeaderFailures is how many consecutive failed leader ticks are tolerated
// before the lease is released. Renewal alone succeeds while Redis is healthy,
// so without this a replica whose upstream path is broken would keep the lease
// forever, publish nothing, and fail the cache closed on every follower once
// MaxStaleness passed, even though healthy replicas could take over.
const maxLeaderFailures = 3

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
		headers: map[string]*header{}, bodies: map[string]json.RawMessage{}, logs: map[string]json.RawMessage{},
		subs: map[*Subscription]struct{}{}, nowFn: time.Now,
		windowHead: -1, kick: make(chan struct{}, 1), done: make(chan struct{}), sharedAt: map[string]time.Time{},
		pull: map[int64]*pullEntry{}, pullHashes: map[string]int64{}, pullTop: -1, sharedCanon: map[string]time.Time{}}
	c.fleet, _ = store.(FleetStore)
	c.canon, _ = store.(CanonicalIndex)
	c.presence, _ = store.(PresenceStore)
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
		telemetry.MetricBlockStoreFresh.WithLabelValues(c.opt.Scope.ProjectId, c.opt.Scope.NetworkId).Set(0)
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

// Tick refreshes the local view from upstream headers when leading, or from
// the lease holder's verified snapshot when following.
func (c *Cache) Tick(ctx context.Context) {
	c.stepMu.Lock()
	defer c.stepMu.Unlock()
	ctx, span := common.StartDetailSpan(ctx, "BlockStore.Tick")
	defer span.End()
	if c.opt.FetchTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.opt.FetchTimeout)
		defer cancel()
	}
	var err error
	if !c.shouldFollow(ctx) {
		c.stopFollowing()
	} else {
		c.following = true
		if c.fleet != nil {
			err = c.fleetTick(ctx)
		} else {
			err = c.refresh(ctx)
		}
	}
	fresh := c.Fresh()
	labels := []string{c.opt.Scope.ProjectId, c.opt.Scope.NetworkId}
	telemetry.MetricBlockStoreFresh.WithLabelValues(labels...).Set(boolFloat64(fresh))
	outcome := "stale"
	if err != nil {
		outcome = "error"
		c.logger.Debug().Err(err).Msg("head cache refresh failed")
		common.SetTraceSpanError(span, err)
	} else if fresh {
		outcome = "fresh"
	}
	telemetry.MetricBlockStoreRefreshTotal.WithLabelValues(labels[0], labels[1], outcome).Inc()
	c.expireSubscribers()
}

// shouldFollow reports whether this tick follows headers: always with
// Options.AlwaysFollow, otherwise only while a subscriber exists locally or,
// through the shared presence mark, on another replica. A replica holding
// subscribers renews the mark every tick.
func (c *Cache) shouldFollow(ctx context.Context) bool {
	if c.opt.AlwaysFollow {
		return true
	}
	if c.SubscriberCount() > 0 {
		if c.presence != nil {
			if err := c.presence.MarkPresence(ctx, c.opt.Scope, c.presenceTTL()); err != nil {
				c.logger.Debug().Err(err).Msg("failed to mark blockstore subscriber presence")
			}
		}
		return true
	}
	if c.presence == nil {
		return false
	}
	ok, err := c.presence.HasPresence(ctx, c.opt.Scope)
	if err != nil {
		// Fail toward following while it already runs, so a Redis blip does
		// not break remote subscribers; never start following on an error.
		return c.following
	}
	return ok
}

// presenceTTL bounds how long following continues fleet-wide after the last
// subscriber leaves (or its replica dies). A replica holding subscribers
// renews it every tick (PollInterval).
func (c *Cache) presenceTTL() time.Duration { return 3 * c.opt.PollInterval }

// stopFollowing ends header following: the lease is released so no replica
// keeps fetching, and the verified window is kept so a later subscriber
// resumes from it (or from the published snapshot) when still within depth.
func (c *Cache) stopFollowing() {
	if !c.following {
		return
	}
	c.following = false
	if c.lease != nil {
		c.releaseLeaseBounded()
	}
	c.sharedAt = map[string]time.Time{}
	c.leaderFailures = 0
}

func (c *Cache) followReason() string {
	if c.opt.AlwaysFollow {
		return FetchReasonBackground
	}
	return FetchReasonSubscription
}

func boolFloat64(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func (c *Cache) fetchMetric(kind PayloadKind, reason string) {
	telemetry.MetricBlockStoreFetchTotal.WithLabelValues(c.opt.Scope.ProjectId, c.opt.Scope.NetworkId, string(kind), reason).Inc()
}

func (c *Cache) leaseTTL() time.Duration {
	ttl := 3 * c.opt.PollInterval
	if fetch := c.opt.FetchTimeout + c.opt.PollInterval; ttl <= fetch {
		ttl = 2 * fetch
	}
	return ttl
}

func (c *Cache) snapshotTTL() time.Duration {
	// Keep recovery metadata as long as the headers. Serving freshness is
	// checked independently against Snapshot.At, including on followers.
	if c.opt.MaxStaleness > c.opt.RecordTTL {
		return c.opt.MaxStaleness
	}
	return c.opt.RecordTTL
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
	err := c.fleetTickOnce(ctx)
	if c.lease == nil {
		// Following, or the lease was just lost: nothing to count.
		c.leaderFailures = 0
		return err
	}
	if err == nil {
		c.leaderFailures = 0
		return nil
	}
	c.leaderFailures++
	if c.leaderFailures >= maxLeaderFailures {
		c.logger.Warn().Err(err).Int("consecutiveFailures", c.leaderFailures).
			Msg("head cache leader stepping down after repeated refresh/publish failures")
		c.releaseLeaseBounded()
		c.sharedAt = map[string]time.Time{}
		c.leaderFailures = 0
	}
	return err
}

func (c *Cache) fleetTickOnce(ctx context.Context) error {
	if c.lease == nil {
		lease, err := c.fleet.Acquire(ctx, c.opt.Scope, c.leaseTTL())
		if err != nil {
			if !c.Fresh() {
				c.invalidateAndClose()
			}
			return fmt.Errorf("acquire head cache lease: %w", err)
		}
		if lease == nil {
			return c.readFleetSnapshot(ctx)
		}
		c.lease = lease
		c.sharedAt = map[string]time.Time{}
		// Acquisition can precede the first follower tick. Reuse the last
		// published, validated headers instead of cold-filling them upstream.
		// Missing/expired snapshots are normal on a genuine cold start.
		if err := c.loadFleetSnapshot(ctx, true); err != nil {
			c.logger.Debug().Err(err).Msg("head cache takeover snapshot unavailable")
		}
	} else {
		ok, err := c.lease.Renew(ctx, c.leaseTTL())
		if err != nil || !ok {
			if err != nil {
				c.releaseLeaseBounded()
			} else {
				c.lease = nil
			}
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
	// A leader publishes only a locally verified header window whose header
	// payloads are all present in the shared store.
	snap, headers := c.localView()
	if snap == nil {
		return nil
	}
	current := make(map[string]struct{}, len(snap.Hashes))
	for _, h := range snap.Hashes {
		current[h] = struct{}{}
		hd := headers[h]
		last := c.sharedAt[h]
		refreshAfter := c.opt.RecordTTL / 2
		if refreshAfter <= 0 || c.nowFn().Sub(last) >= refreshAfter {
			if err := c.store.PutPayload(ctx, c.opt.Scope, PayloadHeader, h, hd.raw, c.opt.RecordTTL); err != nil {
				delete(c.sharedAt, h)
				c.invalidateAndClose()
				return fmt.Errorf("share head cache header %s: %w", h, err)
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
		if err != nil {
			c.releaseLeaseBounded()
		} else {
			c.lease = nil
		}
		c.sharedAt = map[string]time.Time{}
		c.invalidateAndClose()
		if err != nil {
			return fmt.Errorf("renew head cache lease before publish: %w", err)
		}
		return nil
	}
	ok, err = c.lease.Publish(ctx, snap, c.snapshotTTL())
	if err != nil || !ok {
		if err != nil {
			c.releaseLeaseBounded()
		} else {
			c.lease = nil
		}
		c.sharedAt = map[string]time.Time{}
		c.invalidateAndClose()
		if err != nil {
			return fmt.Errorf("publish head cache snapshot: %w", err)
		}
	}
	return nil
}

func (c *Cache) readFleetSnapshot(ctx context.Context) error {
	return c.loadFleetSnapshot(ctx, false)
}

// Recovery may reuse stale headers, but never installs them as a fresh served
// view. Only a successful refresh can publish a fresh snapshot after takeover.
func (c *Cache) loadFleetSnapshot(ctx context.Context, recovery bool) error {
	snap, err := c.fleet.ReadSnapshot(ctx, c.opt.Scope)
	if err != nil {
		if !c.Fresh() {
			c.invalidateAndClose()
		}
		return fmt.Errorf("read head cache snapshot: %w", err)
	}
	if snap == nil || len(snap.Hashes) == 0 || (!recovery && c.nowFn().Sub(snap.At) > c.opt.MaxStaleness) {
		c.invalidateAndClose()
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
	old, oldHeaders := c.localView()
	if recovery {
		c.mu.RLock()
		hasNewerWindow := len(c.window) > 0 && c.windowHead > snap.Head
		c.mu.RUnlock()
		if hasNewerWindow {
			return nil
		}
	}
	headers := make(map[string]*header, len(snap.Hashes))
	chain := make([]*header, len(snap.Hashes))
	for i, hash := range snap.Hashes {
		if !isHexOfLen(hash, 64) {
			c.invalidateAndClose()
			return errors.New("invalid hash in head cache snapshot")
		}
		n := snap.Base() + int64(i)
		norm := normHash(hash)
		snap.Hashes[i] = norm
		// Installed headers were validated and are immutable, so a header
		// already in the local view at this snapshot position is reused.
		if h := oldHeaders[norm]; h != nil && h.n == n && (i == 0 || h.b.ParentHash == normHash(snap.Hashes[i-1])) {
			headers[norm], chain[i] = h, h
			continue
		}
		raw, e := c.store.GetPayload(ctx, c.opt.Scope, PayloadHeader, norm)
		if e != nil && errors.Is(e, ErrStoreUnavailable) && c.sameBranchAdvance(old, snap) {
			// The store could not be reached. The still-fresh local view is on the
			// same branch as this snapshot, so keep it until it expires on its own.
			return fmt.Errorf("read snapshot header %s: %w", hash, e)
		}
		if e != nil || len(raw) == 0 {
			c.invalidateAndClose()
			if e != nil {
				return fmt.Errorf("read snapshot header %s: %w", hash, e)
			}
			return fmt.Errorf("missing snapshot header %s", hash)
		}
		h, e := parseHeader(raw, n)
		if e != nil || h.b.Hash != norm {
			c.invalidateAndClose()
			return fmt.Errorf("inconsistent snapshot header %s", hash)
		}
		if i > 0 && h.b.ParentHash != normHash(snap.Hashes[i-1]) {
			c.invalidateAndClose()
			return errors.New("non-contiguous head cache snapshot headers")
		}
		headers[norm], chain[i] = h, h
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
	c.mu.Lock()
	// A follower keeps the verified chain so that, if promoted, it extends
	// from the published window instead of re-fetching it.
	c.window, c.windowHead = chain, snap.Head
	c.mu.Unlock()
	if recovery && c.nowFn().Sub(snap.At) > c.opt.MaxStaleness {
		return nil
	}
	c.install(&Snapshot{Head: snap.Head, Hashes: append([]string(nil), snap.Hashes...), At: snap.At, Incomplete: snap.Incomplete}, headers, gap)
	return nil
}

// sameBranchAdvance reports whether the local view is still fresh and the
// snapshot only extends or backfills it: they overlap and agree on every
// overlapping height, and the snapshot is not behind the view.
func (c *Cache) sameBranchAdvance(old, snap *Snapshot) bool {
	if old == nil || !c.Fresh() || snap.Head < old.Head {
		return false
	}
	lo, hi := maxI64(old.Base(), snap.Base()), minI64(old.Head, snap.Head)
	if lo > hi {
		return false
	}
	for n := lo; n <= hi; n++ {
		if normHash(old.HashAt(n)) != normHash(snap.HashAt(n)) {
			return false
		}
	}
	return true
}

func (c *Cache) invalidateAndClose() {
	c.invalidate()
	c.mu.Lock()
	c.closePrimedLocked()
	c.mu.Unlock()
}

// header is one verified eth_getBlockByNumber(n, false) result.
type header struct {
	raw json.RawMessage
	b   *rawBlock
	n   int64
}

// parseHeader validates a hash-only block header at height n.
func parseHeader(raw json.RawMessage, n int64) (*header, error) {
	// Parse the kept copy so the header references only its own bytes.
	raw = append(json.RawMessage(nil), raw...)
	sb, got, err := parseScannedBlock(raw)
	if err != nil {
		return nil, err
	}
	return headerFromScan(sb.b, raw, got, n)
}

// headerFromScan finishes parseHeader for a parsed hash-only block whose
// bytes (raw) the header keeps.
func headerFromScan(b *rawBlock, raw json.RawMessage, got, n int64) (*header, error) {
	if got != n {
		return nil, fmt.Errorf("returned block %d", got)
	}
	_, full, err := txHashesOf(b)
	if err != nil {
		return nil, err
	}
	if full && len(b.Transactions) > 0 {
		return nil, errors.New("header carries full transactions")
	}
	b.Hash, b.ParentHash = normHash(b.Hash), normHash(b.ParentHash)
	return &header{raw: raw, b: b, n: n}, nil
}

func (c *Cache) getHeader(ctx context.Context, n int64, reason string) (*header, error) {
	c.fetchMetric(PayloadHeader, reason)
	raw, err := c.fetcher.HeaderByNumber(ctx, n)
	if err != nil {
		return nil, fmt.Errorf("fetch header %d: %w", n, err)
	}
	h, err := parseHeader(raw, n)
	if err != nil {
		c.Stats.Rejected.Add(1)
		return nil, fmt.Errorf("invalid header at %d: %w", n, err)
	}
	return h, nil
}

// getHeaders fetches heights [from, to] in parallel.
func (c *Cache) getHeaders(ctx context.Context, from, to int64) ([]*header, error) {
	if to < from {
		return nil, nil
	}
	out := make([]*header, to-from+1)
	errs := make([]error, len(out))
	c.parallel(ctx, len(out), func(i int) {
		out[i], errs[i] = c.getHeader(ctx, from+int64(i), c.followReason())
	})
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, h := range out {
		if h == nil {
			return nil, errors.New("header fetch did not complete")
		}
	}
	return out, nil
}

func linked(hs []*header) bool {
	for i := 1; i < len(hs); i++ {
		if hs[i].b.ParentHash != hs[i-1].b.Hash {
			return false
		}
	}
	return true
}

// refresh advances the verified header window. Steady state costs one header
// per new block: the tip comes from headFn (an in-memory value in eRPC), new
// heights are fetched and linked by parent hash onto the retained window, and
// a broken link walks back one header at a time until it rejoins. No block
// bodies or logs are fetched here.
func (c *Cache) refresh(ctx context.Context) error {
	if c.fetcher == nil || c.headFn == nil {
		return errors.New("head cache fetcher or head function is nil")
	}
	tip := c.headFn(ctx)
	if tip < 0 {
		return errors.New("head cache has no live tip")
	}
	old, _ := c.localView()
	c.mu.RLock()
	windowHead := c.windowHead
	window := append([]*header(nil), c.window...)
	c.mu.RUnlock()
	if len(window) == 0 || window[len(window)-1].n != windowHead {
		window, windowHead = nil, -1
	}
	seeded := false
	if seed, seedHead := c.seedWindowFromPull(); seedHead > windowHead && seedHead <= tip {
		// Following (re)starts above the retained window: extend the headers
		// clients already fetched instead of refetching them.
		window, windowHead, seeded = seed, seedHead, true
	}
	if len(window) > 0 && tip < windowHead {
		// A poller can observe a lagging upstream. A lower number alone is
		// not evidence of a reorg, and must not discard verified headers.
		// Do not renew freshness or consume a pending reorg verification.
		// Jitter within MaxStaleness costs nothing; once the served view has
		// gone stale this tick made no progress, so it counts as a failure
		// (a leader steps down after maxLeaderFailures, see fleetTick).
		if !c.Fresh() {
			return fmt.Errorf("live tip %d behind verified window head %d: %w", tip, windowHead, errNoProgress)
		}
		return nil
	}
	suspect := c.suspect.Swap(false)
	var err error
	switch {
	case len(window) == 0 || tip-windowHead > c.opt.Depth:
		var top *header
		if top, err = c.getHeader(ctx, tip, c.followReason()); err == nil {
			window = []*header{top}
		}
	case tip > windowHead:
		var added []*header
		// Probe the advertised tip first. If it is not available yet, retry
		// only that height next tick, not the whole forward range.
		var top *header
		if top, err = c.getHeader(ctx, tip, c.followReason()); err == nil {
			added, err = c.getHeaders(ctx, windowHead+1, tip-1)
		}
		if err == nil {
			added = append(added, top)
			if !linked(added) {
				err = fmt.Errorf("new headers %d..%d: %w", windowHead+1, tip, errUnlinked)
			} else {
				window = append(window, added...)
				window, err = c.walkBack(ctx, window, len(window)-len(added))
			}
		}
	case tip == windowHead && !suspect:
		if !seeded && !c.Fresh() {
			// The retained window (an expired view, or headers recovered from
			// a stale snapshot) is unverified against the current chain. An
			// unchanged tip proves nothing, so it stays unfresh until the tip
			// advances and the new header links onto it. Nothing is fetched.
			return nil
		}
		if old != nil && old.Head == tip && !old.Incomplete {
			c.refreshTime(old)
			return nil
		}
	default:
		// Same-height re-verification after an independently suspected reorg.
		var top *header
		if top, err = c.getHeader(ctx, tip, c.followReason()); err == nil {
			i := int(tip - window[0].n)
			if window[i].b.Hash == top.b.Hash {
				window = window[:i+1]
			} else {
				window = append(window[:i], top)
				window, err = c.walkBack(ctx, window, i)
			}
		}
	}
	if err != nil {
		if suspect {
			c.suspect.Store(true)
		}
		if errors.Is(err, errUnlinked) {
			// Upstream answers disagree with the retained chain in a way one
			// walk-back cannot repair: rebuild from the tip next tick.
			c.mu.Lock()
			c.window, c.windowHead = nil, -1
			c.mu.Unlock()
		}
		return err
	}
	if n := int64(len(window)); n > c.opt.Depth {
		window = window[n-c.opt.Depth:]
	}
	if window, err = c.backfill(ctx, window); err != nil {
		return err
	}
	c.mu.Lock()
	c.window, c.windowHead = window, tip
	c.mu.Unlock()

	hashes := make([]string, len(window))
	headers := make(map[string]*header, len(window))
	for i, h := range window {
		hashes[i] = h.b.Hash
		headers[h.b.Hash] = h
	}
	snap := &Snapshot{Head: tip, Hashes: hashes, At: c.nowFn(), Incomplete: int64(len(window)) < c.opt.Depth && window[0].n > 0}
	gap := false
	if old != nil {
		common := int64(-1)
		for n := maxI64(snap.Base(), old.Base()); n <= minI64(tip, old.Head); n++ {
			if old.HashAt(n) == snap.HashAt(n) {
				common = n
			}
		}
		gap = common < 0
		if common >= 0 && common < old.Head && snap.HashAt(old.Head) != old.HashAt(old.Head) {
			c.Stats.Reorgs.Add(1)
		}
	}
	c.install(snap, headers, gap)
	return nil
}

var errUnlinked = errors.New("headers do not link by parent hash")

// errNoProgress is a refresh that could not advance a stale view without
// fetching anything (the live tip source lags the verified window).
var errNoProgress = errors.New("head cache made no progress on a stale view")

// walkBack repairs window[i-1..] after a parent mismatch at window[i] by
// re-fetching retained headers downward until one links. If the whole
// retained window is replaced, the result is a fresh window with no proven
// link to the previous one (install reports a gap).
func (c *Cache) walkBack(ctx context.Context, window []*header, i int) ([]*header, error) {
	for ; i > 0; i-- {
		if window[i].b.ParentHash == window[i-1].b.Hash {
			if !linked(window[i:]) {
				return nil, fmt.Errorf("walked-back headers: %w", errUnlinked)
			}
			return window, nil
		}
		h, err := c.getHeader(ctx, window[i-1].n, c.followReason())
		if err != nil {
			return nil, err
		}
		if h.b.Hash == window[i-1].b.Hash {
			return nil, fmt.Errorf("header %d hash %s vs retained parent: %w", window[i].n, window[i].b.Hash, errUnlinked)
		}
		window[i-1] = h
	}
	if !linked(window) {
		return nil, fmt.Errorf("walked-back headers: %w", errUnlinked)
	}
	return window, nil
}

// backfill prepends up to MaxPerTick older headers while the window is
// shorter than Depth (cold start or recovery).
func (c *Cache) backfill(ctx context.Context, window []*header) ([]*header, error) {
	missing := c.opt.Depth - int64(len(window))
	if missing <= 0 || window[0].n == 0 {
		return window, nil
	}
	n := minI64(minI64(missing, c.opt.MaxPerTick), window[0].n)
	older, err := c.getHeaders(ctx, window[0].n-n, window[0].n-1)
	if err != nil {
		return nil, err
	}
	out := append(older, window...)
	if !linked(out) {
		// The retained bottom no longer links to the chain below it: a
		// reorg reached it. Rebuild from the tip on the next tick.
		c.mu.Lock()
		c.window, c.windowHead = nil, -1
		c.mu.Unlock()
		return nil, fmt.Errorf("backfilled headers below %d do not link", window[0].n)
	}
	return out, nil
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

func (c *Cache) localView() (*Snapshot, map[string]*header) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.snap == nil {
		return nil, nil
	}
	s := *c.snap
	s.Hashes = append([]string(nil), c.snap.Hashes...)
	h := make(map[string]*header, len(c.headers))
	for k, v := range c.headers {
		h[k] = v
	}
	return &s, h
}

func (c *Cache) fetchContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.opt.FetchTimeout > 0 {
		return context.WithTimeout(ctx, c.opt.FetchTimeout)
	}
	return context.WithCancel(ctx)
}

func (c *Cache) refreshTime(snap *Snapshot) {
	c.mu.Lock()
	if c.snap != nil && c.snap.Head == snap.Head {
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

// recordLocked builds the event record of a window header with its cached logs.
func (c *Cache) recordLocked(h *header) *BlockRecord {
	return &BlockRecord{Number: h.n, Hash: h.b.Hash, ParentHash: h.b.ParentHash, Block: h.raw, Logs: c.logs[h.b.Hash]}
}

func (c *Cache) install(snap *Snapshot, headers map[string]*header, gap bool) {
	c.mu.Lock()
	old := c.snap
	restart := old == nil || c.nowFn().Sub(c.freshAt) > c.opt.MaxStaleness
	if old != nil && snap.Head < old.Head && snap.HashAt(snap.Head) == old.HashAt(snap.Head) {
		// A lower matching tip may be a lagging observation, not a reorg.
		gap = true
	}
	if old != nil && snap.Base() > old.Base() && snap.Base() <= old.Head+1 {
		// The window base moved: the lowest new header must link to the
		// previous view, or a reorg below the new base is hidden.
		if first := headers[snap.Hashes[0]]; first == nil || first.b.ParentHash != old.HashAt(snap.Base()-1) {
			gap = true
		}
	}
	var ev Event
	if old != nil {
		for n := old.Head; n >= old.Base(); n-- {
			if snap.HashAt(n) == old.HashAt(n) || n < snap.Base() {
				continue
			}
			if h := c.headers[old.HashAt(n)]; h != nil {
				ev.Removed = append(ev.Removed, c.recordLocked(h))
			} else {
				gap = true
			}
		}
		for n := snap.Base(); n <= snap.Head; n++ {
			if old.HashAt(n) == snap.HashAt(n) || n < old.Base() {
				continue
			}
			if h := headers[snap.HashAt(n)]; h != nil {
				ev.Added = append(ev.Added, c.recordLocked(h))
			} else {
				gap = true
			}
		}
	} else {
		for n := snap.Base(); n <= snap.Head; n++ {
			if h := headers[snap.HashAt(n)]; h != nil {
				ev.Added = append(ev.Added, c.recordLocked(h))
			}
		}
	}
	if old != nil && snap.Base() > old.Head+1 {
		gap = true
	}
	c.snap, c.headers, c.freshAt = snap, headers, snap.At
	// Payloads of hashes that left the view are dropped; they stay in the
	// shared store and are re-validated if the hash returns.
	for h := range c.bodies {
		c.dropPayloadsLocked(h)
	}
	for h := range c.logs {
		c.dropPayloadsLocked(h)
	}
	c.Stats.Published.Add(1)
	if restart {
		// The previous view was missing or stale: subscribers have no
		// verified baseline to continue from. Primed ones were already
		// closed; unprimed ones start at the next contiguous event.
		c.closePrimedLocked()
	} else if gap {
		c.closePrimedLocked()
	} else if len(ev.Removed)+len(ev.Added) > 0 {
		for s := range c.subs {
			select {
			case s.C <- ev:
				s.primed = true
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
		c.closePrimedLocked()
	}
	c.mu.Unlock()
}

// subscribeGrace is how long a subscription that has not received its first
// event waits for header following to (re)start and verify a view.
func (c *Cache) subscribeGrace() time.Duration { return c.leaseTTL() + c.opt.MaxStaleness }

// closePrimedLocked closes subscriptions that were sent events and so lost
// continuity, and unprimed ones whose grace for a first event has elapsed.
func (c *Cache) closePrimedLocked() {
	now := c.nowFn()
	for s := range c.subs {
		if s.primed || now.Sub(s.since) > c.subscribeGrace() {
			c.closeSubLocked(s)
		}
	}
}

// Subscribe registers a newHeads-style subscriber: events carry headers only
// and never cause a body or logs fetch.
func (c *Cache) Subscribe(queue int) *Subscription { return c.subscribe(queue, false) }

// SubscribeLogs registers a logs subscriber. While at least one exists, the
// logs of each new block are fetched once (see EventLogs).
func (c *Cache) SubscribeLogs(queue int) *Subscription { return c.subscribe(queue, true) }

func (c *Cache) subscribe(queue int, logs bool) *Subscription {
	if queue < 1 {
		queue = 1
	}
	s := &Subscription{C: make(chan Event, queue), c: c, logs: logs, since: c.nowFn()}
	c.mu.Lock()
	// On a fresh followed view the subscriber's baseline is the current head;
	// otherwise (following not running yet) it starts at its first event.
	s.primed = c.viewLocked() != nil
	c.subs[s] = struct{}{}
	if logs {
		c.logsSubs++
	}
	c.mu.Unlock()
	// Start following (and mark fleet presence) without waiting a poll.
	c.Kick()
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
		if s.logs {
			c.logsSubs--
		}
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

// LogsSubscriberCount is the number of open logs subscriptions.
func (c *Cache) LogsSubscriberCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.logsSubs
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

// CanonicalHash returns the verified hash at n from the followed window or
// fresh adopted headers, or empty when n is not held.
func (c *Cache) CanonicalHash(n int64) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if s := c.viewLocked(); s != nil {
		if h := s.HashAt(n); h != "" {
			return h
		}
	}
	if h := c.pullFreshLocked(n, ""); h != nil {
		return h.b.Hash
	}
	return ""
}

func (c *Cache) hit(ok bool) {
	if ok {
		c.Stats.Hits.Add(1)
	} else {
		c.Stats.Misses.Add(1)
	}
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
