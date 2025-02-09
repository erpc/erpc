package data

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
)

const (
	RedisDriverName    = "redis"
	reverseIndexPrefix = "rvi"
)

var _ Connector = &RedisConnector{}

type RedisConnector struct {
	id          string
	logger      *zerolog.Logger
	client      *redis.Client
	initializer *util.Initializer
	cfg         *common.RedisConnectorConfig

	ttls         map[string]time.Duration
	initTimeout  time.Duration
	getTimeout   time.Duration
	setTimeout   time.Duration
	sessionToken string
}

func NewRedisConnector(
	appCtx context.Context,
	logger *zerolog.Logger,
	id string,
	cfg *common.RedisConnectorConfig,
) (*RedisConnector, error) {
	lg := logger.With().Str("connector", id).Logger()
	lg.Debug().Interface("config", cfg).Msg("creating RedisConnector")
	token := uuid.New().String()

	connector := &RedisConnector{
		id:           id,
		logger:       &lg,
		cfg:          cfg,
		ttls:         make(map[string]time.Duration),
		initTimeout:  cfg.InitTimeout,
		getTimeout:   cfg.GetTimeout,
		setTimeout:   cfg.SetTimeout,
		sessionToken: token,
	}

	// Create an initializer to manage (re)connecting to Redis.
	connector.initializer = util.NewInitializer(appCtx, &lg, nil) // pass config if needed

	// Define the redis connection task and let the Initializer handle retries.
	connectTask := util.NewBootstrapTask(fmt.Sprintf("redis-connect/%s", id), connector.connectTask)
	if err := connector.initializer.ExecuteTasks(appCtx, connectTask); err != nil {
		lg.Error().Err(err).Msg("failed to initialize redis connection on first attempt (will keep retrying in the background)")
		return connector, nil
	}

	return connector, nil
}

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

	// Test the connection with Ping.
	_, err := client.Ping(ctx).Result()
	if err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
	}

	r.client = client
	r.logger.Info().Str("addr", r.cfg.Addr).Msg("successfully connected to Redis")
	return nil
}

// createTLSConfig creates a tls.Config based on the provided TLS settings.
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
	if state != util.StateReady {
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

// Lock attempts to acquire a distributed lock for the specified key.
// It uses SET NX with an expiration TTL. Returns a DistributedLock instance on success.
func (r *RedisConnector) Lock(ctx context.Context, lockKey string, ttl time.Duration) (DistributedLock, error) {
	if err := r.checkReady(); err != nil {
		return nil, err
	}
	// Attempt to acquire the lock with SET NX.
	ok, err := r.client.SetNX(ctx, lockKey, r.sessionToken, ttl).Result()
	r.logger.Trace().Err(err).Str("lockKey", lockKey).Str("token", r.sessionToken).Bool("ok", ok).Msg("lock result")
	if err != nil {
		return nil, fmt.Errorf("error acquiring lock: %w", err)
	}
	if !ok {
		return nil, errors.New("failed to acquire lock")
	}

	return &redisLock{
		connector: r,
		key:       lockKey,
		token:     r.sessionToken,
		ttl:       ttl,
	}, nil
}

// WatchCounterInt64 watches a counter in Redis. Returns a channel of updates and a cleanup function.
func (r *RedisConnector) WatchCounterInt64(ctx context.Context, key string) (<-chan int64, func(), error) {
	if err := r.checkReady(); err != nil {
		return nil, nil, err
	}
	// Create buffered channel to avoid blocking
	updates := make(chan int64, 1)
	pubsub := r.client.Subscribe(ctx, "counter:"+key)

	// Start a goroutine to handle updates
	go func() {
		defer close(updates)
		defer pubsub.Close()

		// Poll every 30s as a safety net
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return

			case msg := <-pubsub.Channel():
				if val, err := strconv.ParseInt(msg.Payload, 10, 64); err == nil {
					select {
					case updates <- val:
					default: // Don't block if channel is full
					}
				}

			case <-ticker.C:
				// Safety net: poll current value
				if val, err := r.getCurrentValue(ctx, key); err == nil {
					select {
					case updates <- val:
					default:
					}
				}
			}
		}
	}()

	cleanup := func() {
		err := pubsub.Close()
		if err != nil {
			r.logger.Warn().Err(err).Str("key", key).Msg("failed to close pubsub, marking connection lost")
			r.markConnectionAsLostIfNecessary(err)
		}
	}

	// Get initial value
	if val, err := r.getCurrentValue(ctx, key); err == nil {
		updates <- val
	}

	return updates, cleanup, nil
}

// PublishCounterInt64 publishes a counter value to Redis.
func (r *RedisConnector) PublishCounterInt64(ctx context.Context, key string, value int64) error {
	if err := r.checkReady(); err != nil {
		return err
	}
	return r.client.Publish(ctx, "counter:"+key, value).Err()
}

func (r *RedisConnector) getCurrentValue(ctx context.Context, key string) (int64, error) {
	val, err := r.Get(ctx, ConnectorMainIndex, key, "value")
	if err != nil {
		if common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
			return 0, nil
		}
		return 0, err
	}
	return strconv.ParseInt(val, 10, 64)
}

// redisLock implements the DistributedLock interface for Redis.
type redisLock struct {
	connector *RedisConnector
	key       string
	token     string
	ttl       time.Duration
}

// Unlock releases the lock using a Lua script to ensure the token matches.
func (l *redisLock) Unlock(ctx context.Context) error {
	script := `
		if redis.call("get", KEYS[1]) == ARGV[1] then
			return redis.call("del", KEYS[1])
		else
			return 0
		end
	`
	result, err := l.connector.client.Eval(ctx, script, []string{l.key}, l.token).Result()
	l.connector.logger.Trace().Err(err).Str("key", l.key).Str("token", l.token).Interface("result", result).Msg("unlock result")
	if err != nil {
		return fmt.Errorf("error releasing lock: %w", err)
	}
	if result.(int64) == 0 {
		return errors.New("failed to release lock")
	}
	return nil
}
