package blockstore

import (
	"bytes"
	"context"
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
}

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
	opt.Scope.Namespace += ":logsfill"
	return &LogsFiller{opt: opt, store: store, fetch: fetch, latest: latest, finalized: finalized}
}

func (f *LogsFiller) MaxRange() int64 { return f.opt.MaxRange }

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

	if entries, ok := f.lookup(ctx, from, to); ok {
		logs, err := filterBlockLogs(entries, filter)
		if err == nil {
			return LogsFillResult{Logs: logs, OK: true, Outcome: LogsFillHit}
		}
	}

	key := fmt.Sprintf("%d-%d", from, to)
	ch := f.sf.DoChan(key, func() (interface{}, error) {
		// Detach from the first caller so its cancellation cannot fail
		// coalesced followers; bound the shared work instead.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), f.opt.FetchTimeout)
		defer cancel()
		return f.fill(fctx, from, to, latest)
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
		logs, err := filterBlockLogs(res.Val.([]*BlockLogs), filter)
		if err != nil {
			return LogsFillResult{Outcome: LogsFillFallback, Reason: "parse_error"}
		}
		return LogsFillResult{Logs: logs, OK: true, Outcome: LogsFillFill}
	}
}

// lookup returns entries for every height in [from,to], or false on any miss
// or store error.
func (f *LogsFiller) lookup(ctx context.Context, from, to int64) ([]*BlockLogs, bool) {
	n := int(to - from + 1)
	out := make([]*BlockLogs, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e, err := f.store.GetBlockLogs(ctx, f.opt.Scope, from+int64(i))
			if err == nil && e != nil && e.Number == from+int64(i) {
				out[i] = e
			}
		}(i)
	}
	wg.Wait()
	for _, e := range out {
		if e == nil {
			return nil, false
		}
	}
	return out, true
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
	finalized := f.finalized(ctx)
	var wg sync.WaitGroup
	for _, e := range entries {
		ttl, ok := f.entryTTL(e, latest, finalized)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(e *BlockLogs, ttl time.Duration) {
			defer wg.Done()
			_ = f.store.PutBlockLogs(ctx, f.opt.Scope, e, ttl)
		}(e, ttl)
	}
	wg.Wait()
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
		if err != nil {
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
