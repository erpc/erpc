package data

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
)

const (
	RedisDriverName    = "redis"
	reverseIndexPrefix = "rvi"
)

type RedisConnector struct {
	id          string
	logger      *zerolog.Logger
	client      *redis.Client
	initializer *common.Initializer
	cfg         *common.RedisConnectorConfig

	ttls        map[string]time.Duration
	initTimeout time.Duration
	getTimeout  time.Duration
	setTimeout  time.Duration
}

func NewRedisConnector(
	appCtx context.Context,
	logger *zerolog.Logger,
	id string,
	cfg *common.RedisConnectorConfig,
) (*RedisConnector, error) {
	lg := logger.With().Str("connector", id).Logger()
	lg.Debug().Interface("config", cfg).Msg("creating RedisConnector")

	connector := &RedisConnector{
		id:          id,
		logger:      &lg,
		cfg:         cfg,
		ttls:        make(map[string]time.Duration),
		initTimeout: cfg.InitTimeout,
		getTimeout:  cfg.GetTimeout,
		setTimeout:  cfg.SetTimeout,
	}

	// Create an initializer to manage (re)connecting to Redis.
	connector.initializer = common.NewInitializer(appCtx, &lg, nil) // pass config if needed

	// Define the redis connection task and let the Initializer handle retries.
	connectTask := common.NewBootstrapTask(fmt.Sprintf("redis-connect/%s", id), connector.connectTask)
	if err := connector.initializer.ExecuteTasks(appCtx, connectTask); err != nil {
		lg.Error().Err(err).Msg("failed to initialize redis connection on first attempt (will keep retrying in the background)")
		return connector, nil
	}

	return connector, nil
}

// Id satisfies a hypothetical Connector interface.
func (r *RedisConnector) Id() string {
	return r.id
}

// connectTask is the function that tries to establish a Redis connection (and pings to verify).
func (r *RedisConnector) connectTask(ctx context.Context) error {
	options := &redis.Options{
		Addr:         r.cfg.Addr,
		Password:     r.cfg.Password,
		DB:           r.cfg.DB,
		PoolSize:     r.cfg.ConnPoolSize,
		DialTimeout:  r.initTimeout,
		ReadTimeout:  r.getTimeout,
		WriteTimeout: r.setTimeout,
	}

	if r.cfg.TLS != nil && r.cfg.TLS.Enabled {
		tlsConfig, err := createTLSConfig(r.cfg.TLS)
		if err != nil {
			return fmt.Errorf("failed to create TLS config: %w", err)
		}
		options.TLSConfig = tlsConfig
	}

	r.logger.Info().Str("addr", r.cfg.Addr).Msg("attempting to connect to Redis")
	client := redis.NewClient(options)

	// Test the connection
	_, err := client.Ping(ctx).Result()
	if err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
	}

	// If we succeed, store the new client.
	r.client = client
	r.logger.Info().Str("addr", r.cfg.Addr).Msg("successfully connected to Redis")
	return nil
}

// createTLSConfig is unchanged from your original snippet, included here for completeness.
func createTLSConfig(tlsCfg *common.TLSConfig) (*tls.Config, error) {
	config := &tls.Config{
		InsecureSkipVerify: tlsCfg.InsecureSkipVerify, // #nosec G402
	}

	if tlsCfg.CertFile != "" && tlsCfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(tlsCfg.CertFile, tlsCfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load client cert/key pair: %w", err)
		}
		config.Certificates = []tls.Certificate{cert}
	}

	if tlsCfg.CAFile != "" {
		caCert, err := os.ReadFile(tlsCfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA file: %w", err)
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)
		config.RootCAs = caCertPool
	}

	return config, nil
}

// markConnectionAsLostIfNecessary sets the connection task's state to "failed" so that the Initializer triggers a retry.
func (r *RedisConnector) markConnectionAsLostIfNecessary(err error) {
	if r.initializer == nil {
		return
	}
	if err == nil {
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	r.initializer.MarkTaskAsFailed(fmt.Sprintf("redis-connect/%s", r.id), fmt.Errorf("connection lost or redis error: %w", err))
}

// checkReady returns an error if Redis is not in a ready state.
func (r *RedisConnector) checkReady() error {
	if r.initializer == nil {
		return fmt.Errorf("initializer not set")
	}
	state := r.initializer.State()
	if state != common.StateReady {
		return fmt.Errorf("redis is not connected (initializer state=%d)", state)
	}
	if r.client == nil {
		return fmt.Errorf("redis client not initialized yet")
	}
	return nil
}

// Set stores a key-value pair in Redis with an optional TTL. Returns early if Redis is not ready.
func (r *RedisConnector) Set(ctx context.Context, partitionKey, rangeKey, value string, ttl *time.Duration) error {
	if err := r.checkReady(); err != nil {
		return err
	}

	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)
	r.logger.Debug().Msgf("writing to Redis with partition key: %s and range key: %s", partitionKey, rangeKey)

	ctx, cancel := context.WithTimeout(ctx, r.setTimeout)
	defer cancel()

	duration := time.Duration(0)
	if ttl != nil && *ttl > 0 {
		duration = *ttl
	}

	if err := r.client.Set(ctx, key, value, duration).Err(); err != nil {
		r.logger.Warn().Err(err).Str("key", key).Msg("failed to SET in Redis, marking connection lost")
		r.markConnectionAsLostIfNecessary(err)
		return err
	}
	return nil
}

// Get retrieves a value from Redis. If wildcard, retrieves the first matching key. Returns early if not ready.
func (r *RedisConnector) Get(ctx context.Context, index, partitionKey, rangeKey string) (string, error) {
	if err := r.checkReady(); err != nil {
		return "", err
	}

	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)
	ctx, cancel := context.WithTimeout(ctx, r.getTimeout)
	defer cancel()

	if strings.Contains(key, "*") {
		keys, err := r.client.Keys(ctx, key).Result()
		if err != nil {
			r.logger.Warn().Err(err).Str("pattern", key).Msg("failed to KEYS in Redis, marking connection lost")
			r.markConnectionAsLostIfNecessary(err)
			return "", err
		}
		if len(keys) == 0 {
			return "", common.NewErrRecordNotFound(partitionKey, rangeKey, RedisDriverName)
		}
		key = keys[0]
	}

	r.logger.Debug().Str("key", key).Msg("getting item from Redis")
	value, err := r.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", common.NewErrRecordNotFound(partitionKey, rangeKey, RedisDriverName)
	} else if err != nil {
		r.logger.Warn().Err(err).Str("key", key).Msg("failed to GET in Redis, marking connection lost")
		r.markConnectionAsLostIfNecessary(err)
		return "", err
	}
	return value, nil
}
