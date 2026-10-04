package blockstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// On-demand payloads. The background refresh only verifies headers; a block
// body or a block's logs are fetched the first time a request (or a logs
// subscription) needs them, validated against the verified window header,
// and stored by block hash in the shared store, so each payload is fetched
// from upstream at most once per hash across the fleet while it is retained.
//
// Lookup order for a hash in the served view: process-local map, shared
// store, then one coalesced upstream fetch (singleflight per process, plus
// the store's fill lock across replicas when it implements FillLocker).

// Maximum fill lock TTL: a crashed holder blocks peers for at most
// min(FetchTimeout, this), and waiters never wait longer than that anyway.
const payloadMaxLockTTL = 10 * time.Second

// Poll interval while waiting for a peer replica's fill.
const payloadPeerPoll = 50 * time.Millisecond

var errPayloadMismatch = errors.New("fetched payload does not match the verified window header")

// viewHeader returns the verified header at height n, or for hash when n < 0.
func (c *Cache) viewHeader(n int64, hash string) *header {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.viewLocked()
	if s == nil {
		return nil
	}
	if n < 0 {
		h := c.headers[normHash(hash)]
		if h == nil || s.HashAt(h.n) != h.b.Hash {
			return nil
		}
		return h
	}
	return c.headers[s.HashAt(n)]
}

// stillCanonical reports whether h is still in the served view.
func (c *Cache) stillCanonical(h *header) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snap != nil && c.snap.HashAt(h.n) == h.b.Hash
}

func (c *Cache) localPayload(kind PayloadKind, hash string) json.RawMessage {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if kind == PayloadBlock {
		return c.bodies[hash]
	}
	return c.logs[hash]
}

// keepPayload caches a validated payload for a hash still in the view,
// evicting the lowest heights first to stay within MaxBytes.
func (c *Cache) keepPayload(kind PayloadKind, h *header, raw json.RawMessage) {
	size := int64(len(raw))
	if c.opt.MaxBytes > 0 && size > c.opt.MaxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snap == nil || c.snap.HashAt(h.n) != h.b.Hash {
		return
	}
	m := c.logs
	if kind == PayloadBlock {
		m = c.bodies
	}
	if old, ok := m[h.b.Hash]; ok {
		c.payloadBytes -= int64(len(old))
	}
	m[h.b.Hash] = raw
	c.payloadBytes += size
	for c.opt.MaxBytes > 0 && c.payloadBytes > c.opt.MaxBytes {
		var victimMap map[string]json.RawMessage
		victim, victimN := "", int64(-1)
		for _, mm := range []map[string]json.RawMessage{c.bodies, c.logs} {
			for k := range mm {
				hd := c.headers[k]
				n := int64(-1)
				if hd != nil {
					n = hd.n
				}
				if victimMap == nil || n < victimN {
					victimMap, victim, victimN = mm, k, n
				}
			}
		}
		if victimMap == nil {
			break
		}
		c.payloadBytes -= int64(len(victimMap[victim]))
		delete(victimMap, victim)
	}
}

// validatePayload checks a body or log list against the verified header.
func (c *Cache) validatePayload(kind PayloadKind, h *header, raw json.RawMessage) error {
	if c.opt.MaxBlockSize > 0 && int64(len(raw)) > c.opt.MaxBlockSize {
		return errRecordTooLarge
	}
	if kind == PayloadLogs {
		txs, _, err := txHashesOf(h.b)
		if err != nil {
			return err
		}
		return validateCompleteLogs(h.b, h.n, txs, raw)
	}
	b, got, err := parseBlockHeader(raw)
	if err != nil {
		return err
	}
	if got != h.n || normHash(b.Hash) != h.b.Hash || normHash(b.ParentHash) != h.b.ParentHash {
		return errPayloadMismatch
	}
	txs, full, err := txHashesOf(b)
	if err != nil {
		return err
	}
	if !full && len(b.Transactions) > 0 {
		return errors.New("block fetched without full transactions")
	}
	want, _, err := txHashesOf(h.b)
	if err != nil {
		return err
	}
	if len(want) != len(txs) {
		return errPayloadMismatch
	}
	for tx := range want {
		if _, ok := txs[tx]; !ok {
			return errPayloadMismatch
		}
	}
	return nil
}

// payload returns the validated payload for h, fetching it once on a miss.
func (c *Cache) payload(ctx context.Context, kind PayloadKind, h *header, reason string) (json.RawMessage, error) {
	if raw := c.localPayload(kind, h.b.Hash); raw != nil {
		return raw, nil
	}
	if raw, ok := c.storedPayload(ctx, kind, h); ok {
		return raw, nil
	}
	ch := c.sf.DoChan(string(kind)+"/"+h.b.Hash, func() (interface{}, error) {
		// Detach from the first caller so its cancellation cannot fail
		// coalesced followers; bound the shared work instead.
		fctx, cancel := c.fetchContext(context.WithoutCancel(ctx))
		defer cancel()
		return c.coordinatedFetch(fctx, kind, h, reason)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.(json.RawMessage), nil
	}
}

func (c *Cache) storedPayload(ctx context.Context, kind PayloadKind, h *header) (json.RawMessage, bool) {
	if c.store == nil {
		return nil, false
	}
	raw, err := c.store.GetPayload(ctx, c.opt.Scope, kind, h.b.Hash)
	if err != nil || len(raw) == 0 {
		return nil, false
	}
	if c.validatePayload(kind, h, raw) != nil {
		c.Stats.Rejected.Add(1)
		return nil, false
	}
	c.keepPayload(kind, h, raw)
	return raw, true
}

// coordinatedFetch takes the payload's fill lock when the store offers one,
// so one replica fetches while the others wait up to PeerWait for its
// result. Any lock error, timeout or missing peer result falls back to a
// local fetch: correctness never depends on the peer.
func (c *Cache) coordinatedFetch(ctx context.Context, kind PayloadKind, h *header, reason string) (json.RawMessage, error) {
	locker, _ := c.store.(FillLocker)
	if locker == nil || c.opt.PeerWait <= 0 {
		return c.fetchPayload(ctx, kind, h, reason)
	}
	key := string(kind) + "/" + h.b.Hash
	lockTTL := payloadMaxLockTTL
	if c.opt.FetchTimeout > 0 && c.opt.FetchTimeout < lockTTL {
		lockTTL = c.opt.FetchTimeout
	}
	release, ok, err := locker.TryLock(ctx, c.opt.Scope, key, lockTTL)
	if err == nil && ok {
		defer func() {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer cancel()
			release(rctx)
		}()
		// A peer may have finished between our store read and the lock.
		if raw, ok := c.storedPayload(ctx, kind, h); ok {
			return raw, nil
		}
		return c.fetchPayload(ctx, kind, h, reason)
	}
	if err == nil {
		c.waitForPeer(ctx, locker, key, min(c.opt.PeerWait, lockTTL))
		if raw, ok := c.storedPayload(ctx, kind, h); ok {
			return raw, nil
		}
	}
	return c.fetchPayload(ctx, kind, h, reason)
}

func (c *Cache) waitForPeer(ctx context.Context, locker FillLocker, key string, wait time.Duration) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(payloadPeerPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-tick.C:
		}
		held, err := locker.Locked(ctx, c.opt.Scope, key)
		if err != nil || !held {
			return
		}
	}
}

// fetchPayload performs the one upstream call for a payload.
func (c *Cache) fetchPayload(ctx context.Context, kind PayloadKind, h *header, reason string) (json.RawMessage, error) {
	if c.fetcher == nil {
		return nil, errors.New("head cache fetcher is nil")
	}
	c.fetchMetric(kind, reason)
	var raw json.RawMessage
	var err error
	if kind == PayloadBlock {
		raw, err = c.fetcher.BlockByNumber(ctx, h.n)
	} else {
		raw, err = c.fetcher.LogsByBlockHash(ctx, h.b.Hash)
	}
	if err != nil {
		return nil, fmt.Errorf("fetch %s for block %d: %w", kind, h.n, err)
	}
	return c.acceptFetched(ctx, kind, h, raw)
}

// acceptFetched validates and stores an upstream payload. A block body whose
// hash differs from the window means the window may be stale at that height;
// the next tick re-verifies the tip.
func (c *Cache) acceptFetched(ctx context.Context, kind PayloadKind, h *header, raw json.RawMessage) (json.RawMessage, error) {
	if err := c.validatePayload(kind, h, raw); err != nil {
		c.Stats.Rejected.Add(1)
		if errors.Is(err, errPayloadMismatch) {
			c.suspect.Store(true)
			c.Kick()
		}
		return nil, fmt.Errorf("invalid %s for block %d: %w", kind, h.n, err)
	}
	raw = append(json.RawMessage(nil), raw...)
	c.Stats.Hydrated.Add(1)
	if c.store != nil {
		if err := c.store.PutPayload(ctx, c.opt.Scope, kind, h.b.Hash, raw, c.opt.RecordTTL); err != nil {
			c.logger.Debug().Err(err).Int64("number", h.n).Str("kind", string(kind)).Msg("failed to share head cache payload")
		}
	}
	c.keepPayload(kind, h, raw)
	return raw, nil
}

// served finishes a read: the result counts only while h is still canonical,
// so a reorg during a fetch never serves an orphaned payload.
func (c *Cache) served(h *header, err error) bool {
	ok := err == nil && c.stillCanonical(h)
	c.hit(ok)
	return ok
}

// BlockByNumber serves eth_getBlockByNumber for a window height. full=false is
// rendered from the verified header with no fetch; full=true fetches the body
// once on a miss.
func (c *Cache) BlockByNumber(ctx context.Context, n int64, full bool) (json.RawMessage, bool) {
	if n < 0 {
		c.hit(false)
		return nil, false
	}
	return c.block(ctx, c.viewHeader(n, ""), full)
}

// BlockByHash serves eth_getBlockByHash for a hash that is canonical in the window.
func (c *Cache) BlockByHash(ctx context.Context, hash string, full bool) (json.RawMessage, bool) {
	return c.block(ctx, c.viewHeader(-1, hash), full)
}

func (c *Cache) block(ctx context.Context, h *header, full bool) (json.RawMessage, bool) {
	if h == nil {
		c.hit(false)
		return nil, false
	}
	if !full {
		return h.raw, c.served(h, nil)
	}
	raw, err := c.payload(ctx, PayloadBlock, h, FetchReasonMiss)
	return raw, c.served(h, err)
}

// LogsByHash serves eth_getLogs{blockHash} for a canonical window hash.
func (c *Cache) LogsByHash(ctx context.Context, hash string, f *LogFilter) ([]json.RawMessage, bool) {
	h := c.viewHeader(-1, hash)
	if h == nil {
		c.hit(false)
		return nil, false
	}
	raw, err := c.payload(ctx, PayloadLogs, h, FetchReasonMiss)
	if err != nil || !c.stillCanonical(h) {
		c.hit(false)
		return nil, false
	}
	out, err := (&BlockRecord{Logs: raw}).FilterLogs(f, false)
	return out, c.served(h, err)
}

// LogsRange serves eth_getLogs{fromBlock,toBlock} when every height is in
// the window. Each height's logs are fetched once by block hash on a miss
// (bounded by Concurrency), then filtered locally.
func (c *Cache) LogsRange(ctx context.Context, from, to int64, f *LogFilter) ([]json.RawMessage, bool) {
	return c.logsRange(ctx, from, to, f, true)
}

// LogsRangeCached is LogsRange without upstream fetches: it answers only when
// every height's logs are already in the local or shared cache.
func (c *Cache) LogsRangeCached(ctx context.Context, from, to int64, f *LogFilter) ([]json.RawMessage, bool) {
	return c.logsRange(ctx, from, to, f, false)
}

func (c *Cache) logsRange(ctx context.Context, from, to int64, f *LogFilter, fetch bool) ([]json.RawMessage, bool) {
	if from < 0 || to < from || to-from >= c.opt.MaxLogsRange {
		c.hit(false)
		return nil, false
	}
	hs := make([]*header, 0, to-from+1)
	c.mu.RLock()
	if s := c.viewLocked(); s != nil {
		for n := from; n <= to; n++ {
			h := c.headers[s.HashAt(n)]
			if h == nil {
				hs = nil
				break
			}
			hs = append(hs, h)
		}
	}
	c.mu.RUnlock()
	if int64(len(hs)) != to-from+1 {
		c.hit(false)
		return nil, false
	}
	lists := make([]json.RawMessage, len(hs))
	errs := make([]error, len(hs))
	c.parallel(ctx, len(hs), func(i int) {
		if fetch {
			lists[i], errs[i] = c.payload(ctx, PayloadLogs, hs[i], FetchReasonMiss)
			return
		}
		if lists[i] = c.localPayload(PayloadLogs, hs[i].b.Hash); lists[i] == nil {
			lists[i], _ = c.storedPayload(ctx, PayloadLogs, hs[i])
		}
	})
	out := []json.RawMessage{}
	for i, h := range hs {
		if errs[i] != nil || lists[i] == nil || !c.stillCanonical(h) {
			c.hit(false)
			return nil, false
		}
		logs, err := (&BlockRecord{Logs: lists[i]}).FilterLogs(f, false)
		if err != nil {
			c.hit(false)
			return nil, false
		}
		out = append(out, logs...)
	}
	c.hit(true)
	return out, true
}

// AdoptLogs stores per-height log lists filled elsewhere (the logsFill
// range call) as the window's logs for those heights, so a later window read
// or logs subscription needs no upstream call of its own. An entry is adopted
// only for a height in the served view whose verified header it matches:
// a non-empty list must carry that header's hash, and every list must pass
// the same completeness and bloom checks as a direct fetch.
func (c *Cache) AdoptLogs(ctx context.Context, entries []*BlockLogs) {
	for _, e := range entries {
		if e == nil {
			continue
		}
		h := c.viewHeader(e.Number, "")
		if h == nil || (e.Hash != "" && normHash(e.Hash) != h.b.Hash) {
			continue
		}
		if c.localPayload(PayloadLogs, h.b.Hash) != nil {
			continue
		}
		if c.validatePayload(PayloadLogs, h, e.Logs) != nil {
			continue
		}
		raw := append(json.RawMessage(nil), e.Logs...)
		if c.store != nil {
			if err := c.store.PutPayload(ctx, c.opt.Scope, PayloadLogs, h.b.Hash, raw, c.opt.RecordTTL); err != nil {
				c.logger.Debug().Err(err).Int64("number", h.n).Msg("failed to share adopted head cache logs")
			}
		}
		c.keepPayload(PayloadLogs, h, raw)
	}
}

// EventLogs returns ev with the logs of every record filled, for logs
// subscribers. Added blocks are fetched once per hash on a miss (reason
// "subscription"); removed blocks are read only from the local or shared
// cache, because an orphan's logs were fetched when it was added if any logs
// subscriber needed them then. ok=false means an added block's logs could not
// be loaded and the stream must resync.
func (c *Cache) EventLogs(ctx context.Context, ev Event) (Event, bool) {
	out := Event{Removed: make([]*BlockRecord, 0, len(ev.Removed)), Added: make([]*BlockRecord, 0, len(ev.Added))}
	for _, r := range ev.Removed {
		cp := *r
		if cp.Logs == nil {
			if raw := c.localPayload(PayloadLogs, r.Hash); raw != nil {
				cp.Logs = raw
			} else if h, err := parseHeader(r.Block, r.Number); err == nil {
				cp.Logs, _ = c.storedPayload(ctx, PayloadLogs, h)
			}
		}
		out.Removed = append(out.Removed, &cp)
	}
	for _, r := range ev.Added {
		cp := *r
		if cp.Logs == nil {
			h, err := parseHeader(r.Block, r.Number)
			if err != nil {
				return Event{}, false
			}
			if cp.Logs, err = c.payload(ctx, PayloadLogs, h, FetchReasonSubscription); err != nil {
				return Event{}, false
			}
		}
		out.Added = append(out.Added, &cp)
	}
	return out, true
}

// HeaderJSON renders a window header as a newHeads notification payload.
func HeaderJSON(raw json.RawMessage) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	for _, k := range []string{"transactions", "uncles", "size", "totalDifficulty", "withdrawals"} {
		delete(m, k)
	}
	return json.Marshal(m)
}
