package data

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/go-redsync/redsync/v4"
	"github.com/go-redsync/redsync/v4/redis/goredis/v9"
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
	redsync     *redsync.Redsync

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
	lg.Debug().Interface("config", cfg).Msg("creating redis connector")

	connector := &RedisConnector{
		id:          id,
		logger:      &lg,
		cfg:         cfg,
		ttls:        make(map[string]time.Duration),
		initTimeout: cfg.InitTimeout.Duration(),
		getTimeout:  cfg.GetTimeout.Duration(),
		setTimeout:  cfg.SetTimeout.Duration(),
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

	r.logger.Debug().Str("addr", r.cfg.Addr).Msg("attempting to connect to Redis")
	client := redis.NewClient(options)

	// Test the connection with Ping.
	ctx, cancel := context.WithTimeout(ctx, r.initTimeout)
	defer cancel()
	_, err := client.Ping(ctx).Result()
	if err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
	}

	if r.client != nil {
		_ = r.client.Close()
	}
	r.client = client

	pool := goredis.NewPool(client)
	r.redsync = redsync.New(pool)

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
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
		return
	}
	r.initializer.MarkTaskAsFailed(fmt.Sprintf("redis-connect/%s", r.id), fmt.Errorf("connection lost or redis error: %w stack: %s", err, string(debug.Stack())))
}

// checkReady returns an error if Redis is not in a ready state.
func (r *RedisConnector) checkReady() error {
	if r.initializer == nil {
		return fmt.Errorf("initializer not set")
	}
	state := r.initializer.State()
	if state != util.StateReady {
		return fmt.Errorf("redis is not connected (%s)", state.String())
	}
	if r.client == nil {
		return fmt.Errorf("redis client not initialized yet")
	}
	if r.redsync == nil {
		return fmt.Errorf("redsync not initialized yet")
	}
	return nil
}

// Set stores a key-value pair in Redis with an optional TTL. Returns early if Redis is not ready.
func (r *RedisConnector) Set(ctx context.Context, partitionKey, rangeKey, value string, ttl *time.Duration) error {
	if err := r.checkReady(); err != nil {
		return err
	}

	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)
	if len(value) < 1024 {
		r.logger.Debug().Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Str("value", value).Msg("writing value to Redis")
	} else {
		r.logger.Debug().Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Int("len", len(value)).Msg("writing value to Redis")
	}

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

	r.logger.Trace().Str("key", key).Msg("getting item from Redis")
	value, err := r.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", common.NewErrRecordNotFound(partitionKey, rangeKey, RedisDriverName)
	} else if err != nil {
		r.logger.Warn().Err(err).Str("key", key).Msg("failed to GET in Redis")
		r.markConnectionAsLostIfNecessary(err)
		return "", err
	}
	if len(value) < 1024 {
		r.logger.Debug().Str("key", key).Str("value", value).Msg("received item from Redis")
	} else {
		r.logger.Debug().Str("key", key).Int("len", len(value)).Msg("received item from Redis")
	}
	return value, nil
}

// Lock attempts to acquire a distributed lock for the specified key.
// It uses SET NX with an expiration TTL. Returns a DistributedLock instance on success.
func (r *RedisConnector) Lock(ctx context.Context, lockKey string, ttl time.Duration) (DistributedLock, error) {
	if err := r.checkReady(); err != nil {
		return nil, err
	}

	// Generate a unique token for this specific lock operation
	token := uuid.New().String()
	ctx, cancel := context.WithTimeout(ctx, r.setTimeout)
	defer cancel()

	mutex := r.redsync.NewMutex(
		fmt.Sprintf("lock:%s", lockKey),
		redsync.WithExpiry(ttl),
		redsync.WithTries(1), // Only try once to match original behavior
	)

	if err := mutex.LockContext(ctx); err != nil {
		return nil, fmt.Errorf("failed to acquire lock: %w", err)
	}

	r.logger.Trace().Str("key", lockKey).Str("token", token).Msg("distributed lock acquired")

	return &redisLock{
		connector: r,
		key:       lockKey,
		mutex:     mutex,
	}, nil
}

// WatchCounterInt64 watches a counter in Redis. Returns a channel of updates and a cleanup function.
// Callers of this method are responsible to re-try the operation if "values" channel is closed.
func (r *RedisConnector) WatchCounterInt64(ctx context.Context, key string) (<-chan int64, func(), error) {
	r.logger.Debug().Str("key", key).Msg("trying to watch counter int64 in Redis")
	if err := r.checkReady(); err != nil {
		return nil, nil, err
	}
	updates := make(chan int64, 1)
	pubsub := r.client.Subscribe(ctx, "counter:"+key)

	// Start a goroutine to handle updates
	go func() {
		defer close(updates)
		defer func() {
			if err := pubsub.Close(); err != nil {
				r.logger.Warn().Err(err).Str("key", key).Msg("failed to close pubsub")
				r.markConnectionAsLostIfNecessary(err)
			}
		}()

		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		ch := pubsub.Channel()

		for {
			select {
			case <-ctx.Done():
				return

			case msg, ok := <-ch:
				if ok && msg != nil {
					if val, err := strconv.ParseInt(msg.Payload, 10, 64); err == nil {
						select {
						case updates <- val:
						default:
						}
					} else {
						r.logger.Warn().Str("key", key).Str("payload", msg.Payload).Msg("failed to parse received payload")
					}
				} else {
					r.logger.Warn().Str("key", key).Interface("msg", msg).Msg("pubsub channel closed")
					return
				}

			case <-ticker.C:
				r.logger.Debug().Str("key", key).Msg("polling current value")
				if val, err := r.getCurrentValue(ctx, key); err != nil {
					r.markConnectionAsLostIfNecessary(err)
					return
				} else {
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
			r.logger.Warn().Err(err).Str("key", key).Msg("failed to close pubsub")
			r.markConnectionAsLostIfNecessary(err)
		}
	}

	go func() {
		defer func() {
			if rc := recover(); rc != nil {
				r.logger.Error().Interface("panic", rc).Str("key", key).Msg("panic in WatchCounterInt64")
			}
		}()
		// Get initial value
		if val, err := r.getCurrentValue(ctx, key); err == nil {
			select {
			case updates <- val:
			default:
				r.logger.Warn().Str("key", key).Msg("skipping initial value send - channel full or closed")
			}
		} else {
			r.logger.Warn().Err(err).Str("key", key).Msg("failed to get initial value")
		}
	}()

	r.logger.Info().Str("key", key).Msg("started watching counter int64 in Redis")

	return updates, cleanup, nil
}

// PublishCounterInt64 publishes a counter value to Redis.
func (r *RedisConnector) PublishCounterInt64(ctx context.Context, key string, value int64) error {
	if err := r.checkReady(); err != nil {
		return err
	}
	r.logger.Debug().Str("key", key).Int64("value", value).Msg("publishing counter int64 update to Redis")
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

type redisLock struct {
	connector *RedisConnector
	key       string
	mutex     *redsync.Mutex
}

func (l *redisLock) Unlock(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, l.connector.setTimeout)
	defer cancel()
	ok, err := l.mutex.UnlockContext(ctx)
	if err != nil {
		return fmt.Errorf("error releasing lock: %w", err)
	}
	if !ok {
		return errors.New("failed to release lock")
	}
	l.connector.logger.Trace().Str("key", l.key).Msg("distributed lock released")
	return nil
}
