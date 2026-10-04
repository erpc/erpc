package blockstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// BlockLogs is the complete, unfiltered log list of one block height as
// returned by an unfiltered eth_getLogs range. Hash is empty when the block
// had no logs (the response carries no block identity then).
type BlockLogs struct {
	Number int64           `json:"n"`
	Hash   string          `json:"h,omitempty"`
	Logs   json.RawMessage `json:"l"`
	// Fill identifies the upstream response this entry came from. A hit over
	// unfinalized heights is served only when they all share one Fill, so a
	// single answer can never mix pre- and post-reorg data. Entries written
	// before this field existed have Fill == "" and count as a miss there.
	Fill string `json:"f,omitempty"`
}

func (b *BlockLogs) size() int64 { return int64(len(b.Logs) + len(b.Hash) + 32) }

// LogsFillStore persists per-height log lists. Get returns ErrNotFound (or a
// wrapped form) on a miss. Put overwrites any previous entry for the height,
// which is how a reorged height's stale entry is replaced.
type LogsFillStore interface {
	GetBlockLogs(ctx context.Context, scope Scope, height int64) (*BlockLogs, error)
	PutBlockLogs(ctx context.Context, scope Scope, entry *BlockLogs, ttl time.Duration) error
}

// RangeLogsFetcher performs one unfiltered eth_getLogs{fromBlock,toBlock}
// and returns the raw JSON result.
type RangeLogsFetcher func(ctx context.Context, from, to int64) (json.RawMessage, error)

type LogsFillOptions struct {
	Scope    Scope
	MaxRange int64
	// FinalizedTTL applies to heights <= the finalized height.
	FinalizedTTL time.Duration
	// UnfinalizedTTL returns the TTL for heights above the finalized height.
	UnfinalizedTTL func() time.Duration
	// EmptyTipGuard: an unfinalized empty height h is stored only when
	// h <= latest-EmptyTipGuard (latest sampled before the fetch).
	EmptyTipGuard int64
	// FetchTimeout bounds one coalesced fill (upstream call + store writes).
	FetchTimeout time.Duration
	// MaxEntryBytes skips storing a single height whose logs exceed it.
	MaxEntryBytes int64
	// PeerWait bounds how long a miss waits for another replica that holds
	// the fill lock for the same range. 0 disables cross-replica locking.
	// Only used when the store implements LogsFillLocker.
	PeerWait time.Duration
	// Concurrency bounds simultaneous per-height store reads and writes for
	// one request (evm.blockStore.concurrency). Default 4.
	Concurrency int
	// OnFill, when set, receives the per-height entries of every successful
	// upstream fill (the live window adopts them as its logs).
	OnFill func(ctx context.Context, entries []*BlockLogs)
}

// LogsFillLocker is optionally implemented by a shared LogsFillStore to let
// replicas coalesce fills of the same range. The lock holder stores its
// entries before releasing, so "lock no longer held" doubles as the peer's
// completion signal (successful, failed or expired alike).
type LogsFillLocker interface {
	// TryLockFill acquires the range's fill lock for ttl. ok=false means
	// another holder has it. release is non-nil only when ok.
	TryLockFill(ctx context.Context, scope Scope, from, to int64, ttl time.Duration) (release func(context.Context), ok bool, err error)
	// FillLocked reports whether some holder currently has the range's lock.
	FillLocked(ctx context.Context, scope Scope, from, to int64) (bool, error)
}

// Poll interval while waiting for a peer replica's fill.
const logsFillPeerPoll = 50 * time.Millisecond

// Maximum fill lock TTL: a crashed holder blocks peers for at most
// min(FetchTimeout, this), and waiters never wait longer than that anyway.
const logsFillMaxLockTTL = 10 * time.Second

// Reasons attached to fills and hits under cross-replica coordination.
const (
	LogsFillReasonPeerFill       = "peer_fill"
	LogsFillReasonPeerTimeout    = "peer_timeout"
	LogsFillReasonPeerIncomplete = "peer_incomplete"
	LogsFillReasonLockError      = "lock_error"
)

// Outcome labels for LogsFiller.Serve.
const (
	LogsFillHit      = "hit"
	LogsFillFill     = "fill"
	LogsFillFallback = "fallback"
	LogsFillSkipped  = "skipped"
)

// LogsFiller answers small explicit-range eth_getLogs requests from per-block
// log lists filled by one unfiltered upstream range call.
type LogsFiller struct {
	opt       LogsFillOptions
	store     LogsFillStore
	fetch     RangeLogsFetcher
	latest    func(context.Context) int64
	finalized func(context.Context) int64
	sf        singleflight.Group
}

func NewLogsFiller(opt LogsFillOptions, store LogsFillStore, fetch RangeLogsFetcher, latest, finalized func(context.Context) int64) *LogsFiller {
	if opt.MaxRange < 1 {
		opt.MaxRange = 10
	}
	if opt.FinalizedTTL <= 0 {
		opt.FinalizedTTL = time.Hour
	}
	if opt.UnfinalizedTTL == nil {
		opt.UnfinalizedTTL = func() time.Duration { return 2 * time.Second }
	}
	if opt.EmptyTipGuard < 1 {
		opt.EmptyTipGuard = 1
	}
	if opt.FetchTimeout <= 0 {
		opt.FetchTimeout = 10 * time.Second
	}
	if opt.Concurrency < 1 {
		opt.Concurrency = 4
	}
	opt.Scope.Namespace += ":logsfill"
	return &LogsFiller{opt: opt, store: store, fetch: fetch, latest: latest, finalized: finalized}
}

func (f *LogsFiller) MaxRange() int64 { return f.opt.MaxRange }

// coherent reports whether stored entries can be served together. Finalized
// heights cannot reorg, so any mix of their entries is consistent. Unfinalized
// heights must all come from one upstream response (same Fill): entries from
// different fills may straddle a reorg, and an empty height carries no block
// hash to check. Otherwise the range is refilled with one fresh call.
func (f *LogsFiller) coherent(ctx context.Context, entries []*BlockLogs) bool {
	finalized := f.finalized(ctx)
	fill := ""
	for _, e := range entries {
		if finalized > 0 && e.Number <= finalized {
			continue
		}
		if e.Fill == "" {
			return false
		}
		if fill == "" {
			fill = e.Fill
		} else if e.Fill != fill {
			return false
		}
	}
	return true
}

func newFillID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// LogsFillResult describes how Serve handled a request. Logs is non-nil
// (possibly empty) only when OK.
type LogsFillResult struct {
	Logs    []json.RawMessage
	OK      bool
	Outcome string
	Reason  string
}

func skipped(reason string) LogsFillResult {
	return LogsFillResult{Outcome: LogsFillSkipped, Reason: reason}
}

// Serve answers filter over [from,to] from stored per-block lists, filling
// the whole range with one coalesced unfiltered upstream call on any miss.
// maxRange further caps the configured range (0 = configured only). !OK means
// the caller must forward the original request unchanged.
func (f *LogsFiller) Serve(ctx context.Context, from, to, maxRange int64, filter *LogFilter) LogsFillResult {
	limit := f.opt.MaxRange
	if maxRange > 0 && maxRange < limit {
		limit = maxRange
	}
	if from < 0 || to < from {
		return skipped("invalid_range")
	}
	if to-from+1 > limit {
		return skipped("range_too_large")
	}
	latest := f.latest(ctx)
	if latest <= 0 {
		return skipped("head_unknown")
	}
	if to > latest {
		return skipped("above_head")
	}

	if entries, ok := f.lookup(ctx, from, to); ok && f.coherent(ctx, entries) {
		logs, err := filterBlockLogs(entries, filter)
		if err == nil {
			return LogsFillResult{Logs: logs, OK: true, Outcome: LogsFillHit}
		}
	}

	key := fmt.Sprintf("%d-%d", from, to)
	ch := f.sf.DoChan(key, func() (interface{}, error) {
		// Detach from the first caller so its cancellation cannot fail
		// coalesced followers; bound the shared work instead.
		return f.coordinatedFill(context.WithoutCancel(ctx), from, to, latest)
	})
	select {
	case <-ctx.Done():
		return LogsFillResult{Outcome: LogsFillFallback, Reason: "canceled"}
	case res := <-ch:
		if res.Err != nil {
			reason := "fetch_error"
			var pe *logsParseError
			switch {
			case errors.Is(res.Err, errRemovedLogs):
				reason = "removed_logs"
			case errors.As(res.Err, &pe):
				reason = "parse_error"
			}
			return LogsFillResult{Outcome: LogsFillFallback, Reason: reason}
		}
		fr := res.Val.(fillResult)
		logs, err := filterBlockLogs(fr.entries, filter)
		if err != nil {
			return LogsFillResult{Outcome: LogsFillFallback, Reason: "parse_error"}
		}
		if fr.peer {
			return LogsFillResult{Logs: logs, OK: true, Outcome: LogsFillHit, Reason: LogsFillReasonPeerFill}
		}
		return LogsFillResult{Logs: logs, OK: true, Outcome: LogsFillFill, Reason: fr.reason}
	}
}

type fillResult struct {
	entries []*BlockLogs
	// peer: entries were stored by another replica's fill.
	peer   bool
	reason string
}

// coordinatedFill runs one replica's fill of [from,to]. With a shared
// LogsFillLocker it first takes the range's fill lock; if another replica
// holds it, it waits up to PeerWait for that replica to release (it stores
// before releasing) and serves the stored entries. Any lock error, wait
// timeout or incomplete peer result falls back to an unlocked local fill, so
// correctness and latency never depend on the peer.
func (f *LogsFiller) coordinatedFill(ctx context.Context, from, to, latest int64) (fillResult, error) {
	locker, _ := f.store.(LogsFillLocker)
	if locker == nil || f.opt.PeerWait <= 0 {
		return f.localFill(ctx, from, to, latest, "")
	}
	lockTTL := min(f.opt.FetchTimeout, logsFillMaxLockTTL)
	lctx, cancel := context.WithTimeout(ctx, lockTTL)
	release, ok, err := locker.TryLockFill(lctx, f.opt.Scope, from, to, lockTTL)
	cancel()
	if err != nil {
		return f.localFill(ctx, from, to, latest, LogsFillReasonLockError)
	}
	if ok {
		defer func() {
			rctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			release(rctx)
		}()
		// A peer may have finished between our lookup and the lock.
		if entries, ok := f.lookup(ctx, from, to); ok && f.coherent(ctx, entries) {
			return fillResult{entries: entries, peer: true}, nil
		}
		return f.localFill(ctx, from, to, latest, "")
	}
	reason := f.waitForPeer(ctx, locker, from, to, min(f.opt.PeerWait, lockTTL))
	if entries, ok := f.lookup(ctx, from, to); ok && f.coherent(ctx, entries) {
		return fillResult{entries: entries, peer: true}, nil
	}
	return f.localFill(ctx, from, to, latest, reason)
}

// waitForPeer polls the range's lock until it is released or wait elapses and
// returns the fallback reason to use if the stored entries turn out
// incomplete.
func (f *LogsFiller) waitForPeer(ctx context.Context, locker LogsFillLocker, from, to int64, wait time.Duration) string {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(logsFillPeerPoll)
	defer tick.Stop()
	for {
		select {
		case <-deadline.C:
			return LogsFillReasonPeerTimeout
		case <-tick.C:
		}
		pctx, cancel := context.WithTimeout(ctx, wait)
		held, err := locker.FillLocked(pctx, f.opt.Scope, from, to)
		cancel()
		if err != nil {
			return LogsFillReasonLockError
		}
		if !held {
			// The peer stored what it could (the empty-tip guard, size and
			// TTL rules may leave heights unstored) and released.
			return LogsFillReasonPeerIncomplete
		}
	}
}

func (f *LogsFiller) localFill(ctx context.Context, from, to, latest int64, reason string) (fillResult, error) {
	fctx, cancel := context.WithTimeout(ctx, f.opt.FetchTimeout)
	defer cancel()
	entries, err := f.fill(fctx, from, to, latest)
	if err != nil {
		return fillResult{}, err
	}
	return fillResult{entries: entries, reason: reason}, nil
}

// lookup returns entries for every height in [from,to], or false on any miss
// or store error.
func (f *LogsFiller) lookup(ctx context.Context, from, to int64) ([]*BlockLogs, bool) {
	n := int(to - from + 1)
	out := make([]*BlockLogs, n)
	f.parallel(n, func(i int) {
		e, err := f.store.GetBlockLogs(ctx, f.opt.Scope, from+int64(i))
		if err == nil && e != nil && e.Number == from+int64(i) {
			out[i] = e
		}
	})
	for _, e := range out {
		if e == nil {
			return nil, false
		}
	}
	return out, true
}

// parallel runs fn(0..n-1) on at most opt.Concurrency workers.
func (f *LogsFiller) parallel(n int, fn func(int)) {
	workers := min(f.opt.Concurrency, n)
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
				fn(i)
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
}

var errRemovedLogs = errors.New("logs fill response contains removed logs")

type logsParseError struct{ err error }

func (e *logsParseError) Error() string { return "unparsable logs fill response: " + e.err.Error() }
func (e *logsParseError) Unwrap() error { return e.err }

func (f *LogsFiller) fill(ctx context.Context, from, to, latest int64) ([]*BlockLogs, error) {
	raw, err := f.fetch(ctx, from, to)
	if err != nil {
		return nil, err
	}
	entries, removed, err := SplitRangeLogs(raw, from, to)
	if err != nil {
		return nil, &logsParseError{err}
	}
	if removed {
		// Removed logs signal an in-flight reorg: persist nothing and let
		// the caller forward its original request.
		return nil, errRemovedLogs
	}
	fillID := newFillID()
	for _, e := range entries {
		e.Fill = fillID
	}
	finalized := f.finalized(ctx)
	f.parallel(len(entries), func(i int) {
		if ttl, ok := f.entryTTL(entries[i], latest, finalized); ok {
			_ = f.store.PutBlockLogs(ctx, f.opt.Scope, entries[i], ttl)
		}
	})
	if f.opt.OnFill != nil {
		f.opt.OnFill(ctx, entries)
	}
	return entries, nil
}

// entryTTL decides whether and for how long one height is stored.
func (f *LogsFiller) entryTTL(e *BlockLogs, latest, finalized int64) (time.Duration, bool) {
	if f.opt.MaxEntryBytes > 0 && e.size() > f.opt.MaxEntryBytes {
		return 0, false
	}
	if finalized > 0 && e.Number <= finalized {
		return f.opt.FinalizedTTL, true
	}
	// An unfinalized empty list is the one answer a lagging upstream can
	// produce for a block it has not imported yet; only trust it once the
	// network head is EmptyTipGuard blocks past it.
	if e.Hash == "" && e.Number > latest-f.opt.EmptyTipGuard {
		return 0, false
	}
	ttl := f.opt.UnfinalizedTTL()
	if ttl <= 0 {
		return 0, false
	}
	return ttl, true
}

// SplitRangeLogs splits an unfiltered eth_getLogs result for [from,to] into
// one entry per height (empty list for heights without logs), each sorted by
// logIndex. removed reports whether any log has removed:true.
func SplitRangeLogs(raw json.RawMessage, from, to int64) ([]*BlockLogs, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, false, fmt.Errorf("null result")
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(trimmed, &raws); err != nil {
		return nil, false, err
	}
	type item struct {
		idx int64
		raw json.RawMessage
	}
	n := int(to - from + 1)
	groups := make([][]item, n)
	hashes := make([]string, n)
	removed := false
	for i, r := range raws {
		var l rawLog
		if err := json.Unmarshal(r, &l); err != nil {
			return nil, false, fmt.Errorf("log %d: %w", i, err)
		}
		bn, err := parseHexInt(l.BlockNumber)
		if err != nil || bn < from || bn > to {
			return nil, false, fmt.Errorf("log %d blockNumber %q outside %d..%d", i, l.BlockNumber, from, to)
		}
		if !isHexOfLen(l.BlockHash, 64) {
			return nil, false, fmt.Errorf("log %d invalid blockHash", i)
		}
		idx, err := parseHexInt(l.LogIndex)
		if err != nil || idx < 0 {
			return nil, false, fmt.Errorf("log %d invalid logIndex", i)
		}
		if l.Removed {
			removed = true
		}
		slot := int(bn - from)
		h := normHash(l.BlockHash)
		if hashes[slot] == "" {
			hashes[slot] = h
		} else if hashes[slot] != h {
			return nil, false, fmt.Errorf("height %d has logs from two block hashes", bn)
		}
		groups[slot] = append(groups[slot], item{idx: idx, raw: r})
	}
	out := make([]*BlockLogs, n)
	for slot := range groups {
		g := groups[slot]
		sort.SliceStable(g, func(a, b int) bool { return g[a].idx < g[b].idx })
		list := make([]json.RawMessage, len(g))
		for j := range g {
			if j > 0 && g[j].idx == g[j-1].idx {
				return nil, false, fmt.Errorf("height %d duplicate logIndex %d", from+int64(slot), g[j].idx)
			}
			list[j] = g[j].raw
		}
		b, err := json.Marshal(list)
		if err != nil {
			return nil, false, err
		}
		out[slot] = &BlockLogs{Number: from + int64(slot), Hash: hashes[slot], Logs: b}
	}
	return out, removed, nil
}

// filterBlockLogs concatenates the filtered logs of entries in height order.
// The result is never nil.
func filterBlockLogs(entries []*BlockLogs, filter *LogFilter) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0)
	for _, e := range entries {
		matched, err := (&BlockRecord{Logs: e.Logs}).FilterLogs(filter, false)
		if err != nil {
			return nil, err
		}
		out = append(out, matched...)
	}
	return out, nil
}

// MemoryLogsFillStore is a byte-bounded, TTL-aware LRU used when no shared
// connector is configured.
type MemoryLogsFillStore struct {
	mu       sync.Mutex
	maxBytes int64
	bytes    int64
	items    map[string]*memLogsItem
	head     *memLogsItem // most recent
	tail     *memLogsItem
	now      func() time.Time
}

type memLogsItem struct {
	key        string
	entry      *BlockLogs
	expires    time.Time
	prev, next *memLogsItem
}

func NewMemoryLogsFillStore(maxBytes int64) *MemoryLogsFillStore {
	return &MemoryLogsFillStore{maxBytes: maxBytes, items: make(map[string]*memLogsItem), now: time.Now}
}

func memLogsKey(scope Scope, height int64) string {
	return fmt.Sprintf("%s#%d", scope.Key(), height)
}

func (s *MemoryLogsFillStore) GetBlockLogs(_ context.Context, scope Scope, height int64) (*BlockLogs, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.items[memLogsKey(scope, height)]
	if it == nil {
		return nil, ErrNotFound
	}
	if !s.now().Before(it.expires) {
		s.removeLocked(it)
		return nil, ErrNotFound
	}
	s.unlinkLocked(it)
	s.pushFrontLocked(it)
	return it.entry, nil
}

func (s *MemoryLogsFillStore) PutBlockLogs(_ context.Context, scope Scope, entry *BlockLogs, ttl time.Duration) error {
	if entry == nil {
		return fmt.Errorf("nil logs entry")
	}
	size := entry.size()
	if size > s.maxBytes {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := memLogsKey(scope, entry.Number)
	if old := s.items[key]; old != nil {
		s.removeLocked(old)
	}
	it := &memLogsItem{key: key, entry: entry, expires: s.now().Add(ttl)}
	s.items[key] = it
	s.pushFrontLocked(it)
	s.bytes += size
	for s.bytes > s.maxBytes && s.tail != nil {
		s.removeLocked(s.tail)
	}
	return nil
}

func (s *MemoryLogsFillStore) removeLocked(it *memLogsItem) {
	s.unlinkLocked(it)
	delete(s.items, it.key)
	s.bytes -= it.entry.size()
}

func (s *MemoryLogsFillStore) unlinkLocked(it *memLogsItem) {
	if it.prev != nil {
		it.prev.next = it.next
	} else if s.head == it {
		s.head = it.next
	}
	if it.next != nil {
		it.next.prev = it.prev
	} else if s.tail == it {
		s.tail = it.prev
	}
	it.prev, it.next = nil, nil
}

func (s *MemoryLogsFillStore) pushFrontLocked(it *memLogsItem) {
	it.next = s.head
	if s.head != nil {
		s.head.prev = it
	}
	s.head = it
	if s.tail == nil {
		s.tail = it
	}
}
