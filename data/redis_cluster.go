package data

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"
)

var errInvalidPaginationToken = errors.New("invalid pagination token")

// redisClusterEnabled reports whether the server runs in cluster mode. A
// server that refuses INFO is treated as standalone, which keeps the
// single-node client as before.
func redisClusterEnabled(ctx context.Context, client *redis.Client) bool {
	info, err := client.Info(ctx, "cluster").Result()
	if err != nil {
		return false
	}
	return strings.Contains(info, "cluster_enabled:1")
}

// redisClusterOptions carries the parsed single-node options (URI, TLS, IAM
// credentials, database, timeouts, pool) over to a cluster client seeded with
// the same address. Every node connection then authenticates, selects and
// times out the same way.
func redisClusterOptions(o *redis.Options) *redis.ClusterOptions {
	onConnect := o.OnConnect
	if db := o.DB; db != 0 {
		// ClusterOptions has no DB field; Valkey 9+ allows numbered databases
		// in cluster mode, so select it on every node connection instead of
		// silently falling back to database 0.
		prev := onConnect
		onConnect = func(ctx context.Context, cn *redis.Conn) error {
			if prev != nil {
				if err := prev(ctx, cn); err != nil {
					return err
				}
			}
			return cn.Select(ctx, db).Err()
		}
	}
	return &redis.ClusterOptions{
		Addrs:                        []string{o.Addr},
		ClientName:                   o.ClientName,
		Dialer:                       o.Dialer,
		OnConnect:                    onConnect,
		Protocol:                     o.Protocol,
		Username:                     o.Username,
		Password:                     o.Password,
		CredentialsProvider:          o.CredentialsProvider,
		CredentialsProviderContext:   o.CredentialsProviderContext,
		StreamingCredentialsProvider: o.StreamingCredentialsProvider,
		MaxRetries:                   o.MaxRetries,
		MinRetryBackoff:              o.MinRetryBackoff,
		MaxRetryBackoff:              o.MaxRetryBackoff,
		DialTimeout:                  o.DialTimeout,
		ReadTimeout:                  o.ReadTimeout,
		WriteTimeout:                 o.WriteTimeout,
		ContextTimeoutEnabled:        o.ContextTimeoutEnabled,
		PoolFIFO:                     o.PoolFIFO,
		PoolSize:                     o.PoolSize,
		PoolTimeout:                  o.PoolTimeout,
		MinIdleConns:                 o.MinIdleConns,
		MaxIdleConns:                 o.MaxIdleConns,
		MaxActiveConns:               o.MaxActiveConns,
		ConnMaxIdleTime:              o.ConnMaxIdleTime,
		ConnMaxLifetime:              o.ConnMaxLifetime,
		ConnMaxLifetimeJitter:        o.ConnMaxLifetimeJitter,
		TLSConfig:                    o.TLSConfig,
		DisableIdentity:              o.DisableIdentity,
		IdentitySuffix:               o.IdentitySuffix,
		UnstableResp3:                o.UnstableResp3,
	}
}

// scanRedis runs one SCAN page on a standalone server; the token is the
// server cursor.
func scanRedis(ctx context.Context, client redis.Cmdable, token, pattern string, count int64) ([]string, string, error) {
	cursor := uint64(0)
	if token != "" {
		var err error
		cursor, err = strconv.ParseUint(token, 10, 64)
		if err != nil {
			return nil, "", fmt.Errorf("%w: %v", errInvalidPaginationToken, err)
		}
	}
	keys, next, err := client.Scan(ctx, cursor, pattern, count).Result()
	if err != nil {
		return nil, "", err
	}
	if next == 0 {
		return keys, "", nil
	}
	return keys, strconv.FormatUint(next, 10), nil
}

// scanRedisCluster runs one SCAN page across a cluster. SCAN only walks the
// node it is sent to, so masters are walked one after the other in address
// order and the token is "<master address>@<cursor>". When that master is gone
// (resharding, scale-in) the walk resumes from the next address at cursor 0.
func scanRedisCluster(ctx context.Context, cluster *redis.ClusterClient, token, pattern string, count int64) ([]string, string, error) {
	addr, cursor := "", uint64(0)
	if token != "" {
		at := strings.LastIndex(token, "@")
		if at <= 0 {
			return nil, "", fmt.Errorf("%w: %q", errInvalidPaginationToken, token)
		}
		var err error
		cursor, err = strconv.ParseUint(token[at+1:], 10, 64)
		if err != nil {
			return nil, "", fmt.Errorf("%w: %v", errInvalidPaginationToken, err)
		}
		addr = token[:at]
	}

	var mu sync.Mutex
	masters := map[string]*redis.Client{}
	err := cluster.ForEachMaster(ctx, func(_ context.Context, c *redis.Client) error {
		mu.Lock()
		masters[c.Options().Addr] = c
		mu.Unlock()
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	addrs := make([]string, 0, len(masters))
	for a := range masters {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)

	i := sort.SearchStrings(addrs, addr)
	if i == len(addrs) {
		return nil, "", nil
	}
	if addrs[i] != addr {
		cursor = 0
	}

	keys, next, err := masters[addrs[i]].Scan(ctx, cursor, pattern, count).Result()
	if err != nil {
		return nil, "", err
	}
	switch {
	case next != 0:
		return keys, fmt.Sprintf("%s@%d", addrs[i], next), nil
	case i+1 < len(addrs):
		return keys, addrs[i+1] + "@0", nil
	default:
		return keys, "", nil
	}
}
