package erpc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
	"github.com/go-redsync/redsync/v4"
	"github.com/rs/zerolog"
)

// Cache fill coordinates cacheable read misses across replicas through the
// shared-state connector (networks[].cacheFill). Contract:
//
//   - Only requests the cache could serve with a stable key take part: a GET
//     policy matches and the block reference is concrete (FillEligible).
//     Writes, unknown methods, tag-based reads, internal requests and
//     skip-cache-read requests never touch the lock and never wait.
//   - Local multiplexer first: only the in-process leader reaches this code.
//   - Lock won: forward, persist the cache entry synchronously, write a done
//     marker ("stored" or "uncached"), then unlock.
//   - Lock contended: re-read the replica's OWN cache every pollInterval up
//     to maxWait. Responses are never handed across replicas directly; a hit
//     is an ordinary cache hit under the waiter's own directives. An
//     "uncached" marker ends the wait early only when it carries the same
//     generation id as the "inflight" record the waiter observed, so a stale
//     leader (expired lock) can never end a newer generation's wait.
//   - Lock unavailable (timeout, connector error, memory connector): forward
//     immediately.
//
// This reduces duplicate upstream reads; it is not exactly-once (lock expiry,
// retries, hedges and maxWait timeouts can all add calls).

const (
	cacheFillStored   = "stored"
	cacheFillUncached = "uncached"
)

type cacheFillEligibility interface {
	FillEligible(ctx context.Context, req *common.NormalizedRequest) bool
}

type cacheFillMarker struct {
	S string `json:"s"`
	G string `json:"g"`
	T int64  `json:"t"`
}

// cacheFillLeader is non-nil while this request holds the fill lock.
type cacheFillLeader struct {
	key       string
	gen       string
	lock      data.DistributedLock
	connector data.Connector
	stored    bool
	start     time.Time
}

func (n *Network) cacheFillConfig() *common.CacheFillConfig {
	if n.cfg == nil || n.cfg.CacheFill == nil || !n.cfg.CacheFill.Enabled {
		return nil
	}
	return n.cfg.CacheFill
}

// cacheFillConnector returns the shared-state connector, or nil when there is
// no cross-replica store (memory connector) or no registry.
func (n *Network) cacheFillConnector() (data.Connector, string) {
	if n.upstreamsRegistry == nil {
		return nil, ""
	}
	ssr := n.upstreamsRegistry.SharedStateRegistry()
	if ssr == nil {
		return nil, ""
	}
	cp, ok := ssr.(interface{ Connector() data.Connector })
	if !ok {
		return nil, ""
	}
	conn := cp.Connector()
	if conn == nil {
		return nil, ""
	}
	if _, isMem := conn.(*data.MemoryConnector); isMem {
		return nil, ""
	}
	clusterKey := ""
	if ck, ok := ssr.(interface{ ClusterKey() string }); ok {
		clusterKey = ck.ClusterKey()
	}
	return conn, clusterKey
}

func (n *Network) cacheFillKey(ctx context.Context, clusterKey string, req *common.NormalizedRequest) (string, error) {
	hash, err := n.multiplexKey(ctx, req)
	if err != nil {
		return "", err
	}
	dirs, err := json.Marshal(req.Directives())
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(n.cacheFillScope))
	h.Write([]byte{0})
	h.Write([]byte(hash))
	h.Write([]byte{0})
	h.Write(dirs)
	sum := hex.EncodeToString(h.Sum(nil))
	return clusterKey + "/cachefill/" + n.projectId + "/" + n.networkId + "/" + sum, nil
}

func isLockContention(err error) bool {
	var taken *redsync.ErrTaken
	if errors.As(err, &taken) {
		return true
	}
	var takenV redsync.ErrTaken
	if errors.As(err, &takenV) {
		return true
	}
	return strings.Contains(err.Error(), "lock already taken")
}

func (n *Network) cacheFillMetric(outcome string, since time.Time) {
	telemetry.CounterHandle(telemetry.MetricCacheFillTotal, n.projectId, n.Label(), outcome).Inc()
	telemetry.ObserverHandle(telemetry.MetricCacheFillWaitSeconds, n.projectId, n.Label(), outcome).Observe(time.Since(since).Seconds())
}

// coordinateCacheFill runs after a local cache miss. It returns a cached
// response (waiter hit), or a leader handle the caller must finish, or
// neither (forward normally).
func (n *Network) coordinateCacheFill(ctx context.Context, lg *zerolog.Logger, req *common.NormalizedRequest) (*common.NormalizedResponse, *cacheFillLeader) {
	cfg := n.cacheFillConfig()
	if cfg == nil || n.cacheDal == nil || cacheWriteBypassed(ctx) || req.IsInternal() || req.ShouldSkipCacheRead("") {
		return nil, nil
	}
	el, ok := n.cacheDal.(cacheFillEligibility)
	if !ok || !el.FillEligible(ctx, req) {
		return nil, nil
	}
	conn, clusterKey := n.cacheFillConnector()
	if conn == nil {
		return nil, nil
	}
	key, err := n.cacheFillKey(ctx, clusterKey, req)
	if err != nil {
		return nil, nil
	}
	start := time.Now()
	lockCtx, cancel := context.WithTimeout(ctx, cfg.LockAcquireTimeout.Duration())
	lock, err := conn.Lock(lockCtx, key, cfg.LockTtl.Duration())
	cancel()
	if err == nil && lock != nil && !lock.IsNil() {
		// New generation: publish its id so waiters only trust a done marker
		// written by this leader.
		var gb [12]byte
		_, _ = rand.Read(gb[:])
		gen := hex.EncodeToString(gb[:])
		payload, _ := json.Marshal(cacheFillMarker{S: "inflight", G: gen, T: time.Now().UnixMilli()})
		ttl := cfg.LockTtl.Duration()
		ictx, icancel := context.WithTimeout(ctx, cfg.LockAcquireTimeout.Duration())
		_ = conn.Set(ictx, key, "inflight", payload, &ttl)
		icancel()
		return nil, &cacheFillLeader{key: key, gen: gen, lock: lock, connector: conn, start: start}
	}
	if err == nil || !isLockContention(err) {
		lg.Debug().Err(err).Str("key", key).Msg("cache fill lock unavailable, forwarding upstream")
		n.cacheFillMetric("error", start)
		return nil, nil
	}
	return n.waitForCacheFill(ctx, lg, req, conn, key, cfg, start), nil
}

func (n *Network) waitForCacheFill(ctx context.Context, lg *zerolog.Logger, req *common.NormalizedRequest, conn data.Connector, key string, cfg *common.CacheFillConfig, start time.Time) *common.NormalizedResponse {
	deadline := start.Add(cfg.MaxWait.Duration())
	ticker := time.NewTicker(cfg.PollInterval.Duration())
	defer ticker.Stop()
	opTimeout := cfg.LockAcquireTimeout.Duration()
	readMarker := func(rk string) (cacheFillMarker, bool) {
		mctx, mcancel := context.WithTimeout(ctx, opTimeout)
		defer mcancel()
		raw, err := conn.Get(mctx, data.ConnectorMainIndex, key, rk, nil)
		var m cacheFillMarker
		if err != nil || len(raw) == 0 || json.Unmarshal(raw, &m) != nil || m.G == "" {
			return m, false
		}
		return m, true
	}
	// The generation this waiter is waiting on (first inflight id seen).
	gen := ""
	if m, ok := readMarker("inflight"); ok {
		gen = m.G
	}
	for {
		select {
		case <-ctx.Done():
			n.cacheFillMetric("follower_timeout", start)
			return nil
		case <-ticker.C:
		}
		// Bound by the remaining wait budget so a slow cache read cannot
		// extend the wait beyond maxWait.
		gctx, gcancel := context.WithDeadline(ctx, deadline)
		resp, err := n.cacheDal.Get(gctx, req)
		gcancel()
		if err == nil && resp != nil && !resp.IsObjectNull(ctx) {
			n.cacheFillMetric("follower_hit", start)
			return resp
		}
		if gen == "" {
			if m, ok := readMarker("inflight"); ok {
				gen = m.G
			}
		}
		if gen != "" {
			// Each generation has its own done record, so an expired holder
			// can only ever write its own (already abandoned) record.
			if m, ok := readMarker("done:" + gen); ok && m.G == gen {
				if m.S == cacheFillUncached {
					lg.Debug().Str("key", key).Msg("cache fill leader did not store a response, forwarding upstream")
					n.cacheFillMetric("follower_uncached", start)
					return nil
				}
				// "stored" but our Get missed (eviction, policy mismatch):
				// one last read happened above; stop waiting.
				n.cacheFillMetric("follower_timeout", start)
				return nil
			}
		}
		if !time.Now().Before(deadline) {
			n.cacheFillMetric("follower_timeout", start)
			return nil
		}
	}
}

// finish writes the done marker and releases the lock. It uses the app
// context so a cancelled client still releases promptly.
func (f *cacheFillLeader) finish(n *Network, cfg *common.CacheFillConfig) {
	status := cacheFillUncached
	outcome := "leader"
	if f.stored {
		status = cacheFillStored
	}
	ctx, cancel := context.WithTimeout(n.appCtx, time.Second)
	defer cancel()
	// Only a leader still inside its lock TTL may publish; a leader whose
	// lock expired may be racing a newer generation. The generation id makes
	// a late write harmless anyway (waiters match on it).
	if time.Since(f.start) < cfg.LockTtl.Duration() {
		payload, _ := json.Marshal(cacheFillMarker{S: status, G: f.gen, T: time.Now().UnixMilli()})
		ttl := cfg.MaxWait.Duration() + 2*cfg.PollInterval.Duration()
		_ = f.connector.Set(ctx, f.key, "done:"+f.gen, payload, &ttl)
	}
	_ = f.lock.Unlock(ctx)
	n.cacheFillMetric(outcome, f.start)
}
