package headcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore is a shared Store backed by Redis (standalone, sentinel or
// cluster). Every key of a scope embeds the same hash tag
// "{<prefix>|<project>/<network>}" so all multi-key Lua scripts touch a single
// cluster slot, keeping lease checks and snapshot publication atomic in
// cluster mode.
//
// Fencing model:
//   - <tag>:epoch is a persistent counter, INCR'd on every grant. It never
//     decreases, so a successor always holds a strictly larger epoch.
//   - <tag>:lease is a hash {holder, epoch} with a PX TTL. Its absence means
//     no live lease.
//   - <tag>:snap holds the JSON snapshot; <tag>:snapmeta holds {e, s}. A
//     publish succeeds only if the caller's (holder, epoch) matches the live
//     lease AND (epoch, seq) is strictly greater than the stored one, so a
//     stale writer can never overwrite a successor's head.
//
// The epoch counter has no TTL and is never deleted, so epochs never reset.
//
// Requirements: Redis >= 6.2 with Lua scripting (EVALSHA, TIME, cjson);
// validated against Redis 8 and miniredis. Cluster works by construction
// (single hash tag per scope) but is not integration-tested. Use a
// noeviction policy or dedicated DB where possible; epoch fencing survives
// eviction of the counter or snapmeta (floored from surviving snapshot state).
// Holder ids passed to AcquireLease must be unique per process.
//
// Any backend failure is reported wrapped in ErrStoreUnavailable (context
// cancellation/deadline is returned as the context error instead). The store
// never fabricates a lease or snapshot when Redis is unreachable.
type RedisStore struct {
	clientFn    func() redis.UniversalClient
	prefix      string
	snapshotTTL time.Duration
}

// RedisStoreOptions tune a RedisStore.
type RedisStoreOptions struct {
	// Prefix namespaces all keys (e.g. per deployment / trust set). Defaults
	// to "erpc:headcache".
	Prefix string
	// SnapshotTTL bounds how long a committed snapshot survives without being
	// republished, so a dead leader cannot leave a canonical head that is
	// served forever. 0 disables expiry.
	SnapshotTTL time.Duration
}

func NewRedisStore(client redis.UniversalClient, opts RedisStoreOptions) *RedisStore {
	return NewRedisStoreFunc(func() redis.UniversalClient { return client }, opts)
}

// NewRedisStoreFunc resolves the client per operation (a shared connector may
// reconnect and swap its client). A nil client reports ErrStoreUnavailable.
func NewRedisStoreFunc(clientFn func() redis.UniversalClient, opts RedisStoreOptions) *RedisStore {
	if opts.Prefix == "" {
		opts.Prefix = "erpc:headcache"
	}
	return &RedisStore{clientFn: clientFn, prefix: opts.Prefix, snapshotTTL: opts.SnapshotTTL}
}

// client returns the current client or ErrStoreUnavailable.
func (s *RedisStore) client() (redis.UniversalClient, error) {
	if c := s.clientFn(); c != nil {
		return c, nil
	}
	return nil, fmt.Errorf("%w: redis client not connected", ErrStoreUnavailable)
}

var _ Store = (*RedisStore)(nil)

// tag derives the cluster hash tag from an unambiguous, length-prefixed
// encoding of prefix and every Scope field, hashed so that '/', '|', '{' or
// '}' inside ids can neither collide two scopes nor break the hash tag.
func (s *RedisStore) tag(scope Scope) string {
	h := sha256.New()
	for _, f := range []string{s.prefix, scope.Namespace, scope.ProjectId, scope.NetworkId} {
		_, _ = fmt.Fprintf(h, "%d:%s;", len(f), f)
	}
	return "{hc:" + hex.EncodeToString(h.Sum(nil)[:16]) + "}"
}

func (s *RedisStore) blockKey(scope Scope, hash string) string {
	return s.tag(scope) + ":blk:" + hash
}
func (s *RedisStore) epochKey(scope Scope) string    { return s.tag(scope) + ":epoch" }
func (s *RedisStore) leaseKey(scope Scope) string    { return s.tag(scope) + ":lease" }
func (s *RedisStore) snapKey(scope Scope) string     { return s.tag(scope) + ":snap" }
func (s *RedisStore) snapMetaKey(scope Scope) string { return s.tag(scope) + ":snapmeta" }
func (s *RedisStore) channel(scope Scope) string     { return s.tag(scope) + ":ch" }

// MaxSnapshotFutureSkew is the tolerance for Snapshot.At ahead of the Redis
// server clock (TIME). Publications beyond it are rejected with
// ErrInvalidSnapshot so a skewed writer cannot mint snapshots that readers'
// freshness checks would treat as fresh for too long.
const MaxSnapshotFutureSkew = 5 * time.Second

// ErrInvalidRequest marks caller input errors (bad scope, missing holder,
// non-positive TTL). It is never ErrStoreUnavailable.
var ErrInvalidRequest = errors.New("headcache: invalid request")

func validScope(scope Scope) error {
	if scope.Namespace == "" || scope.ProjectId == "" || scope.NetworkId == "" {
		return fmt.Errorf("%w: scope requires namespace, projectId and networkId (got %q)", ErrInvalidRequest, scope.Key())
	}
	return nil
}

// wrapErr classifies a backend error.
func wrapErr(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, redis.Nil) {
		return ErrNotFound
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("headcache redis %s: %w", op, ctxErr)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("headcache redis %s: %w", op, err)
	}
	return fmt.Errorf("headcache redis %s: %w: %v", op, ErrStoreUnavailable, err)
}

func (s *RedisStore) PutBlock(ctx context.Context, scope Scope, rec *BlockRecord, ttl time.Duration) error {
	if err := validScope(scope); err != nil {
		return err
	}
	if rec == nil || rec.Hash == "" {
		return fmt.Errorf("%w: PutBlock requires a record with a hash", ErrInvalidRequest)
	}
	if ttl <= 0 {
		return fmt.Errorf("%w: PutBlock requires a positive ttl (storage must be bounded)", ErrInvalidRequest)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("headcache: marshal block record: %w", err)
	}
	// Records are content-addressed; overwriting with identical content just
	// refreshes the TTL.
	rc, cerr := s.client()
	if cerr != nil {
		return cerr
	}
	return wrapErr(ctx, "PutBlock", rc.Set(ctx, s.blockKey(scope, rec.Hash), data, ttl).Err())
}

func (s *RedisStore) GetBlock(ctx context.Context, scope Scope, hash string) (*BlockRecord, error) {
	if err := validScope(scope); err != nil {
		return nil, err
	}
	rc, cerr := s.client()
	if cerr != nil {
		return nil, cerr
	}
	data, err := rc.Get(ctx, s.blockKey(scope, hash)).Bytes()
	if err != nil {
		return nil, wrapErr(ctx, "GetBlock", err)
	}
	rec := &BlockRecord{}
	if err := json.Unmarshal(data, rec); err != nil {
		return nil, fmt.Errorf("headcache: corrupt block record %s: %w", hash, err)
	}
	if rec.Hash != hash {
		return nil, fmt.Errorf("headcache: block record hash mismatch: want %s got %s", hash, rec.Hash)
	}
	return rec, nil
}

// KEYS: lease, epoch, snapmeta, snap. ARGV: holder, ttlMs. Returns epoch or 0 if held.
var acquireScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then return 0 end
-- Floor the counter at the highest epoch still visible in snapshot state so
-- an evicted/lost counter can never re-issue an epoch that is already fenced.
local floor = 0
local me = redis.call('HGET', KEYS[3], 'e')
if me then floor = tonumber(me) end
local raw = redis.call('GET', KEYS[4])
if raw then
  local ok, d = pcall(cjson.decode, raw)
  if ok and type(d) == 'table' and tonumber(d['e']) and tonumber(d['e']) > floor then floor = tonumber(d['e']) end
end
local cur = tonumber(redis.call('GET', KEYS[2]) or '0')
if cur < floor then redis.call('SET', KEYS[2], floor) end
local e = redis.call('INCR', KEYS[2])
redis.call('HSET', KEYS[1], 'holder', ARGV[1], 'epoch', e)
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return e
`)

// KEYS: lease, epoch. ARGV: holder, epoch, ttlMs. Returns 1 ok, 0 lost.
var renewScript = redis.NewScript(`
local h = redis.call('HMGET', KEYS[1], 'holder', 'epoch')
if h[1] ~= ARGV[1] or h[2] ~= ARGV[2] then return 0 end
if redis.call('GET', KEYS[2]) ~= ARGV[2] then return 0 end
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return 1
`)

// KEYS: lease. ARGV: holder, epoch.
var releaseScript = redis.NewScript(`
local h = redis.call('HMGET', KEYS[1], 'holder', 'epoch')
if h[1] == ARGV[1] and h[2] == ARGV[2] then redis.call('DEL', KEYS[1]) return 1 end
return 0
`)

// KEYS: lease, epoch, snap, snapmeta. ARGV: holder, epoch, seq, json, ttlMs, channel.
// ARGV[7]: snapshot At (unix ms), ARGV[8]: max future skew (ms).
// Returns 1 ok, 0 lease lost, -1 stale sequence, -2 At in the future per
// Redis server TIME (the writer's clock is never authoritative).
var publishScript = redis.NewScript(`
local t = redis.call('TIME')
local nowMs = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
if tonumber(ARGV[7]) > nowMs + tonumber(ARGV[8]) then return -2 end
local h = redis.call('HMGET', KEYS[1], 'holder', 'epoch')
if h[1] ~= ARGV[1] or h[2] ~= ARGV[2] then return 0 end
if redis.call('GET', KEYS[2]) ~= ARGV[2] then return 0 end
local e = tonumber(ARGV[2])
local s = tonumber(ARGV[3])
local m = redis.call('HMGET', KEYS[4], 'e', 's')
if not m[1] then
  -- snapmeta lost (eviction): recover the fence from the snapshot itself.
  local raw = redis.call('GET', KEYS[3])
  if raw then
    local ok, d = pcall(cjson.decode, raw)
    if ok and type(d) == 'table' and tonumber(d['e']) then m = {d['e'], d['s'] or 0} end
  end
end
if m[1] then
  local ce = tonumber(m[1])
  local cs = tonumber(m[2])
  if e < ce then return 0 end
  if e == ce and s <= cs then return -1 end
end
local ttl = tonumber(ARGV[5])
if ttl > 0 then
  redis.call('SET', KEYS[3], ARGV[4], 'PX', ttl)
  redis.call('HSET', KEYS[4], 'e', e, 's', s)
  redis.call('PEXPIRE', KEYS[4], ttl)
else
  redis.call('SET', KEYS[3], ARGV[4])
  redis.call('HSET', KEYS[4], 'e', e, 's', s)
end
-- Fences passed and the new snapshot is committed: drop the declared block
-- keys it no longer references (computed by the caller, same hash tag).
for i = 5, #KEYS do redis.call('DEL', KEYS[i]) end
redis.call('PUBLISH', ARGV[6], ARGV[2] .. ':' .. ARGV[3])
return 1
`)

// maxPrunePerPublish caps how many superseded block keys one publish deletes.
// Anything beyond it (e.g. a first publish after a long outage) keeps relying
// on RecordTTL; the next publishes only diff consecutive windows.
const maxPrunePerPublish = 512

// pruneCandidates returns block keys referenced by the committed snapshot but
// not by next. It is a best-effort pre-read: on any error it returns nil and
// retention falls back to RecordTTL. Correctness does not depend on it being
// current, since only hashes absent from next are ever listed, and the delete
// runs inside the fenced publish script after next is committed.
//
// Residual (TTL-bounded, not chain-rate proportional): records that were put
// but never entered a committed snapshot (rejected publish, lease lost after
// the put, gap truncation) are not in any prev snapshot and still expire only
// via RecordTTL. That is at most MaxPerTick records per failed tick.
func (s *RedisStore) pruneCandidates(ctx context.Context, scope Scope, next *Snapshot) []string {
	rc, cerr := s.client()
	if cerr != nil {
		return nil
	}
	data, err := rc.Get(ctx, s.snapKey(scope)).Bytes()
	if err != nil {
		return nil
	}
	prev := &Snapshot{}
	if json.Unmarshal(data, prev) != nil {
		return nil
	}
	keep := make(map[string]struct{}, len(next.Hashes))
	for _, h := range next.Hashes {
		keep[h] = struct{}{}
	}
	var out []string
	for _, h := range prev.Hashes {
		if _, ok := keep[h]; ok || h == "" {
			continue
		}
		out = append(out, s.blockKey(scope, h))
		if len(out) >= maxPrunePerPublish {
			break
		}
	}
	return out
}

func (s *RedisStore) AcquireLease(ctx context.Context, scope Scope, holder string, ttl time.Duration) (*Lease, error) {
	if err := validScope(scope); err != nil {
		return nil, err
	}
	if holder == "" || ttl <= 0 {
		return nil, fmt.Errorf("%w: AcquireLease requires holder and positive ttl", ErrInvalidRequest)
	}
	rc, cerr := s.client()
	if cerr != nil {
		return nil, cerr
	}
	start := time.Now()
	res, err := acquireScript.Run(ctx, rc,
		[]string{s.leaseKey(scope), s.epochKey(scope), s.snapMetaKey(scope), s.snapKey(scope)},
		holder, ttl.Milliseconds()).Int64()
	if err != nil {
		return nil, wrapErr(ctx, "AcquireLease", err)
	}
	if res == 0 {
		return nil, ErrLeaseHeld
	}
	// ExpiresAt is measured from before the round trip so the local view
	// never outlives the server-side TTL.
	return &Lease{Scope: scope, Holder: holder, Epoch: uint64(res), ExpiresAt: start.Add(ttl)}, nil
}

func (s *RedisStore) RenewLease(ctx context.Context, lease *Lease, ttl time.Duration) (*Lease, error) {
	if lease == nil || ttl <= 0 {
		return nil, fmt.Errorf("%w: RenewLease requires lease and positive ttl", ErrInvalidRequest)
	}
	rc, cerr := s.client()
	if cerr != nil {
		return nil, cerr
	}
	start := time.Now()
	res, err := renewScript.Run(ctx, rc,
		[]string{s.leaseKey(lease.Scope), s.epochKey(lease.Scope)},
		lease.Holder, strconv.FormatUint(lease.Epoch, 10), ttl.Milliseconds()).Int64()
	if err != nil {
		return nil, wrapErr(ctx, "RenewLease", err)
	}
	if res != 1 {
		return nil, ErrLeaseLost
	}
	nl := *lease
	nl.ExpiresAt = start.Add(ttl)
	return &nl, nil
}

func (s *RedisStore) ReleaseLease(ctx context.Context, lease *Lease) error {
	if lease == nil {
		return nil
	}
	rc, cerr := s.client()
	if cerr != nil {
		return cerr
	}
	_, err := releaseScript.Run(ctx, rc, []string{s.leaseKey(lease.Scope)},
		lease.Holder, strconv.FormatUint(lease.Epoch, 10)).Result()
	return wrapErr(ctx, "ReleaseLease", err)
}

func (s *RedisStore) PublishSnapshot(ctx context.Context, lease *Lease, snap *Snapshot) error {
	if lease == nil || snap == nil {
		return fmt.Errorf("%w: PublishSnapshot requires lease and snapshot", ErrInvalidRequest)
	}
	if snap.Epoch != lease.Epoch {
		return ErrLeaseLost
	}
	if !snap.Valid() {
		return ErrInvalidSnapshot
	}
	if snap.At.IsZero() || snap.At.UnixMilli() <= 0 {
		return fmt.Errorf("%w: missing or malformed At", ErrInvalidSnapshot)
	}
	if !lease.ExpiresAt.IsZero() && time.Now().After(lease.ExpiresAt) {
		return ErrLeaseLost
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("headcache: marshal snapshot: %w", err)
	}
	sc := lease.Scope
	keys := append([]string{s.leaseKey(sc), s.epochKey(sc), s.snapKey(sc), s.snapMetaKey(sc)},
		s.pruneCandidates(ctx, sc, snap)...)
	rc, cerr := s.client()
	if cerr != nil {
		return cerr
	}
	res, err := publishScript.Run(ctx, rc, keys,
		lease.Holder, strconv.FormatUint(lease.Epoch, 10), strconv.FormatUint(snap.Seq, 10),
		data, s.snapshotTTL.Milliseconds(), s.channel(sc),
		snap.At.UnixMilli(), MaxSnapshotFutureSkew.Milliseconds()).Int64()
	if err != nil {
		return wrapErr(ctx, "PublishSnapshot", err)
	}
	switch res {
	case 1:
		return nil
	case -1:
		return fmt.Errorf("%w: seq %d does not advance committed seq in epoch %d", ErrInvalidSnapshot, snap.Seq, snap.Epoch)
	case -2:
		return fmt.Errorf("%w: At %s is more than %s ahead of redis server time", ErrInvalidSnapshot, snap.At.UTC().Format(time.RFC3339Nano), MaxSnapshotFutureSkew)
	default:
		return ErrLeaseLost
	}
}

func (s *RedisStore) LoadSnapshot(ctx context.Context, scope Scope) (*Snapshot, error) {
	if err := validScope(scope); err != nil {
		return nil, err
	}
	rc, cerr := s.client()
	if cerr != nil {
		return nil, cerr
	}
	data, err := rc.Get(ctx, s.snapKey(scope)).Bytes()
	if err != nil {
		return nil, wrapErr(ctx, "LoadSnapshot", err)
	}
	snap := &Snapshot{}
	if err := json.Unmarshal(data, snap); err != nil {
		return nil, fmt.Errorf("headcache: corrupt snapshot for %s: %w", scope.Key(), err)
	}
	return snap, nil
}

// WatchSnapshots subscribes to publication notices. Delivery is best-effort:
// go-redis transparently reconnects the pubsub connection, so notices
// published while disconnected are silently lost and the channel stays open.
// Consumers MUST also poll LoadSnapshot periodically. The channel closes only
// when ctx is done or stop is called.
func (s *RedisStore) WatchSnapshots(ctx context.Context, scope Scope) (<-chan struct{}, func(), error) {
	if err := validScope(scope); err != nil {
		return nil, nil, err
	}
	rc, cerr := s.client()
	if cerr != nil {
		return nil, nil, cerr
	}
	ps := rc.Subscribe(ctx, s.channel(scope))
	if _, err := ps.Receive(ctx); err != nil {
		_ = ps.Close()
		return nil, nil, wrapErr(ctx, "WatchSnapshots", err)
	}
	out := make(chan struct{}, 1)
	wctx, cancel := context.WithCancel(ctx)
	msgs := ps.Channel()
	go func() {
		defer close(out)
		defer ps.Close()
		for {
			select {
			case <-wctx.Done():
				return
			case _, ok := <-msgs:
				if !ok {
					return
				}
				select {
				case out <- struct{}{}:
				default: // coalesce
				}
			}
		}
	}()
	return out, cancel, nil
}
