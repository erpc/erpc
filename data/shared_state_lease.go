package data

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrLeaseUnsupported is returned by SharedStateRegistry.AcquireLease when the
// shared-state connector cannot provide an exclusive, expiring lease.
var ErrLeaseUnsupported = errors.New("shared state connector does not support leases")

// Lease is an exclusive, expiring right held by one instance. It is a token
// lease (SET NX PX + compare-and-renew), not a mutex: losing it is detected on
// the next Renew, and a holder that dies simply stops renewing so another
// instance acquires it at most one TTL later.
type Lease interface {
	// Renew extends the lease by ttl. false (with nil error) means the lease
	// was lost (expired and possibly taken by another instance).
	Renew(ctx context.Context, ttl time.Duration) (bool, error)
	// Release gives the lease up early so another instance can take over
	// immediately. Releasing a lost lease is a no-op.
	Release(ctx context.Context) error
}

// leaseKey namespaces a lease under the registry's cluster key.
func (r *sharedStateRegistry) leaseKey(key string) string {
	return fmt.Sprintf("%s/lease/%s", r.clusterKey, key)
}

// AcquireLease tries once to take the lease named key for ttl. It returns
// (nil, nil) when another instance currently holds it.
//
// Redis-backed shared state uses a random-token SET NX PX lease, the same
// shape as the blockstore fleet lease. The memory connector (single process)
// uses an in-process lease with the same semantics so tests and single-replica
// deployments behave identically. Other drivers return ErrLeaseUnsupported:
// callers must then NOT assume leadership, since two instances could both
// believe they lead.
func (r *sharedStateRegistry) AcquireLease(ctx context.Context, key string, ttl time.Duration) (Lease, error) {
	if ttl < time.Millisecond {
		return nil, fmt.Errorf("lease ttl must be positive")
	}
	conn := unwrapConnector(r.connector)
	switch c := conn.(type) {
	case *RedisConnector:
		return acquireRedisLease(ctx, c, r.leaseKey(key), ttl)
	case *MemoryConnector:
		return acquireMemoryLease(r.leaseKey(key), ttl), nil
	default:
		return nil, ErrLeaseUnsupported
	}
}

func unwrapConnector(c Connector) Connector {
	for {
		u, ok := c.(interface{ Unwrap() Connector })
		if !ok {
			return c
		}
		c = u.Unwrap()
	}
}

func leaseToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate lease token: %w", err)
	}
	return hex.EncodeToString(token[:]), nil
}

type redisLease struct {
	conn  *RedisConnector
	key   string
	token string
}

func acquireRedisLease(ctx context.Context, conn *RedisConnector, key string, ttl time.Duration) (Lease, error) {
	client := conn.Client()
	if client == nil {
		return nil, fmt.Errorf("acquire lease %s: redis not connected", key)
	}
	token, err := leaseToken()
	if err != nil {
		return nil, err
	}
	ok, err := client.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return nil, fmt.Errorf("acquire lease %s: %w", key, err)
	}
	if !ok {
		return nil, nil
	}
	return &redisLease{conn: conn, key: key, token: token}, nil
}

func (l *redisLease) client() (redis.UniversalClient, error) {
	c := l.conn.Client()
	if c == nil {
		return nil, fmt.Errorf("lease %s: redis not connected", l.key)
	}
	return c, nil
}

func (l *redisLease) Renew(ctx context.Context, ttl time.Duration) (bool, error) {
	const script = `if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('PEXPIRE', KEYS[1], ARGV[2]) else return 0 end`
	c, err := l.client()
	if err != nil {
		return false, err
	}
	n, err := c.Eval(ctx, script, []string{l.key}, l.token, ttl.Milliseconds()).Int()
	if err != nil {
		return false, fmt.Errorf("renew lease %s: %w", l.key, err)
	}
	return n == 1, nil
}

func (l *redisLease) Release(ctx context.Context) error {
	const script = `if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) else return 0 end`
	c, err := l.client()
	if err != nil {
		return err
	}
	if err := c.Eval(ctx, script, []string{l.key}, l.token).Err(); err != nil {
		return fmt.Errorf("release lease %s: %w", l.key, err)
	}
	return nil
}

// memoryLeases is process-wide: a memory connector is by definition not
// shared between processes, and keying by the cluster-scoped name keeps
// unrelated registries apart.
var memoryLeases = struct {
	sync.Mutex
	m map[string]*memoryLeaseState
}{m: map[string]*memoryLeaseState{}}

type memoryLeaseState struct {
	token   string
	expires time.Time
}

type memoryLease struct {
	key   string
	token string
}

func acquireMemoryLease(key string, ttl time.Duration) Lease {
	token, err := leaseToken()
	if err != nil {
		return nil
	}
	memoryLeases.Lock()
	defer memoryLeases.Unlock()
	if st, ok := memoryLeases.m[key]; ok && time.Now().Before(st.expires) {
		return nil
	}
	memoryLeases.m[key] = &memoryLeaseState{token: token, expires: time.Now().Add(ttl)}
	return &memoryLease{key: key, token: token}
}

func (l *memoryLease) Renew(_ context.Context, ttl time.Duration) (bool, error) {
	memoryLeases.Lock()
	defer memoryLeases.Unlock()
	st, ok := memoryLeases.m[l.key]
	if !ok || st.token != l.token || time.Now().After(st.expires) {
		return false, nil
	}
	st.expires = time.Now().Add(ttl)
	return true, nil
}

func (l *memoryLease) Release(context.Context) error {
	memoryLeases.Lock()
	defer memoryLeases.Unlock()
	if st, ok := memoryLeases.m[l.key]; ok && st.token == l.token {
		delete(memoryLeases.m, l.key)
	}
	return nil
}

// HeartbeatReplicas records this instance as alive for ttl and returns how
// many instances of the cluster are currently alive (including this one).
// Redis: a sorted set scored by expiry. Memory (single process): 1. Other
// drivers: ErrLeaseUnsupported.
func (r *sharedStateRegistry) HeartbeatReplicas(ctx context.Context, ttl time.Duration) (int, error) {
	switch c := unwrapConnector(r.connector).(type) {
	case *RedisConnector:
		client := c.Client()
		if client == nil {
			return 0, fmt.Errorf("replica heartbeat: redis not connected")
		}
		const script = `redis.call('ZADD', KEYS[1], ARGV[2], ARGV[1])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[3])
redis.call('PEXPIRE', KEYS[1], ARGV[4])
return redis.call('ZCARD', KEYS[1])`
		now := time.Now()
		n, err := client.Eval(ctx, script, []string{fmt.Sprintf("%s/replicas", r.clusterKey)},
			r.instanceId, now.Add(ttl).UnixMilli(), now.UnixMilli(), (2 * ttl).Milliseconds()).Int()
		if err != nil {
			return 0, fmt.Errorf("replica heartbeat: %w", err)
		}
		return n, nil
	case *MemoryConnector:
		return 1, nil
	default:
		return 0, ErrLeaseUnsupported
	}
}
