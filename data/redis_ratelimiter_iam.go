package data

import (
	"crypto/tls"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	envoyratelimitredis "github.com/envoyproxy/ratelimit/src/redis"
	"github.com/mediocregopher/radix/v3"

	"github.com/erpc/erpc/common"
)

// iamRateLimitClient implements envoyratelimitredis.Client over a radix pool
// with per-connection IAM token minting. Used in place of NewClientImpl when
// rateLimiters.store.redis.iamAuth.enabled is true.
type iamRateLimitClient struct {
	pool *radix.Pool
}

// NewIAMRateLimitClient builds an envoyratelimitredis.Client backed by a radix
// pool that mints a fresh ElastiCache IAM token on every new connection.
func NewIAMRateLimitClient(cfg *common.RedisConnectorConfig) (envoyratelimitredis.Client, error) {
	iam := cfg.IAMAuth

	sess, err := createAWSSession(iam.Auth, iam.Region)
	if err != nil {
		return nil, fmt.Errorf("elasticache iam rate-limiter: create AWS session: %w", err)
	}

	var tlsConfig *tls.Config
	if cfg.TLS != nil && cfg.TLS.Enabled {
		tlsConfig, err = common.CreateTLSConfig(cfg.TLS)
		if err != nil {
			return nil, fmt.Errorf("elasticache iam rate-limiter: build TLS config: %w", err)
		}
	}

	addr, db, err := extractRateLimiterAddrAndDB(cfg.URI, cfg.Addr, cfg.DB)
	if err != nil {
		return nil, fmt.Errorf("elasticache iam rate-limiter: resolve address: %w", err)
	}

	df := func(network, addr string) (radix.Conn, error) {
		token, tokenErr := generateElastiCacheIAMToken(sess, iam)
		if tokenErr != nil {
			return nil, fmt.Errorf("elasticache iam rate-limiter: generate token: %w", tokenErr)
		}
		opts := []radix.DialOpt{radix.DialAuthUser(iam.UserID, token)}
		if tlsConfig != nil {
			opts = append(opts, radix.DialUseTLS(tlsConfig))
		}
		if db != 0 {
			opts = append(opts, radix.DialSelectDB(db))
		}
		return radix.Dial(network, addr, opts...)
	}

	pool, err := radix.NewPool("tcp", addr, cfg.ConnPoolSize,
		radix.PoolConnFunc(df),
		// Must match NewClientImpl defaults — remoteAdmissionCap() sizes the
		// per-budget admission channel as connPoolSize*32, which depends on
		// implicit pipelining at exactly this window and limit.
		radix.PoolPipelineWindow(5*time.Millisecond, 32),
		// Proactively recycle connections before ElastiCache's 12h server-side
		// force-disconnect; each replacement dials with a fresh IAM token.
		// ponytail: no jitter (radix lacks the knob); connections spread
		// naturally across the pool's dial history.
		radix.PoolMaxLifetime(iamRedisConnMaxLifetime),
	)
	if err != nil {
		return nil, fmt.Errorf("elasticache iam rate-limiter: create pool: %w", err)
	}

	return &iamRateLimitClient{pool: pool}, nil
}

// extractRateLimiterAddrAndDB returns a bare host:port and logical DB number
// for the radix pool constructor. SetDefaults emits "rediss://host:port/N" for
// IAM-enabled configs, so the scheme, credentials, and path must be stripped,
// and the DB index must be recovered from the path before stripping.
func extractRateLimiterAddrAndDB(uri, addr string, cfgDB int) (string, int, error) {
	if uri != "" {
		u, err := url.Parse(uri)
		if err != nil {
			return "", 0, fmt.Errorf("parse redis URI %q: %w", uri, err)
		}
		db := cfgDB
		if path := strings.TrimPrefix(u.Path, "/"); path != "" && path != "0" {
			if n, err := strconv.Atoi(path); err == nil {
				db = n
			}
		}
		return u.Host, db, nil
	}
	if addr != "" {
		return addr, cfgDB, nil
	}
	return "", 0, fmt.Errorf("neither uri nor addr is set in RedisConnectorConfig")
}

func (c *iamRateLimitClient) DoCmd(rcv interface{}, cmd, key string, args ...interface{}) error {
	return c.pool.Do(radix.FlatCmd(rcv, cmd, key, args...))
}

func (c *iamRateLimitClient) PipeAppend(pipeline envoyratelimitredis.Pipeline, rcv interface{}, cmd, key string, args ...interface{}) envoyratelimitredis.Pipeline {
	return append(pipeline, radix.FlatCmd(rcv, cmd, key, args...))
}

// PipeDo runs each command individually and returns on the first error,
// matching clientImpl.PipeDo's implicit-pipelining path (driver_impl.go).
// The envoy ratelimit library constructs one pipeline per DoLimit call
// (a single INCRBY+EXPIRE pair for one key), so early-return on error
// never abandons commands for a different key mid-pipeline.
func (c *iamRateLimitClient) PipeDo(pipeline envoyratelimitredis.Pipeline) error {
	for _, action := range pipeline {
		if err := c.pool.Do(action); err != nil {
			return err
		}
	}
	return nil
}

func (c *iamRateLimitClient) Close() error {
	return c.pool.Close()
}

// NumActiveConns returns 0 — radix/v3 does not expose an active-connection
// counter. The original clientImpl derives this from a gostats gauge via
// PoolWithTrace; wiring that here is not worth it given this method is
// only called in envoyproxy/ratelimit's own benchmark tests.
func (c *iamRateLimitClient) NumActiveConns() int { return 0 }

func (c *iamRateLimitClient) ImplicitPipeliningEnabled() bool { return true }
