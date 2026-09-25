package erpc

import (
	"context"
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
//     "uncached" marker newer than the wait start ends the wait early.
//   - Lock unavailable (timeout, connector error, memory connector): forward
//     immediately.
//
// This reduces duplicate upstream reads; it is not exactly-once (lock expiry,
// retries, hedges and maxWait timeouts can all add calls).

const (
	cacheFillStored   = "stored"
	cacheFillUncached = "uncached"
	// cacheFillMarkerSkew tolerates cross-replica clock skew when deciding
	// whether a done marker belongs to the fill this waiter is waiting on.
	cacheFillMarkerSkew = 2 * time.Second
)

type cacheFillEligibility interface {
	FillEligible(ctx context.Context, req *common.NormalizedRequest) bool
}

type cacheFillMarker struct {
	S string `json:"s"`
	T int64  `json:"t"`
}

// cacheFillLeader is non-nil while this request holds the fill lock.
type cacheFillLeader struct {
	key       string
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
		// New generation: drop any marker left by an earlier fill of this key.
		dctx, dcancel := context.WithTimeout(ctx, cfg.LockAcquireTimeout.Duration())
		_ = conn.Delete(dctx, key, "done")
		dcancel()
		return nil, &cacheFillLeader{key: key, lock: lock, connector: conn, start: start}
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
	for {
		select {
		case <-ctx.Done():
			n.cacheFillMetric("follower_timeout", start)
			return nil
		case <-ticker.C:
		}
		if resp, err := n.cacheDal.Get(ctx, req); err == nil && resp != nil && !resp.IsObjectNull(ctx) {
			n.cacheFillMetric("follower_hit", start)
			return resp
		}
		mctx, mcancel := context.WithTimeout(ctx, cfg.LockAcquireTimeout.Duration())
		raw, err := conn.Get(mctx, data.ConnectorMainIndex, key, "done", nil)
		mcancel()
		if err == nil && len(raw) > 0 {
			var m cacheFillMarker
			if json.Unmarshal(raw, &m) == nil && m.T >= start.Add(-cacheFillMarkerSkew).UnixMilli() {
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
	payload, _ := json.Marshal(cacheFillMarker{S: status, T: time.Now().UnixMilli()})
	ttl := cfg.MaxWait.Duration() + 2*cfg.PollInterval.Duration()
	_ = f.connector.Set(ctx, f.key, "done", payload, &ttl)
	_ = f.lock.Unlock(ctx)
	n.cacheFillMetric(outcome, f.start)
}
