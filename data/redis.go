package data

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/go-redsync/redsync/v4"
	"github.com/go-redsync/redsync/v4/redis/goredis/v9"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	RedisDriverName         = "redis"
	redisReverseIndexPrefix = "rvi"
)

var _ Connector = &RedisConnector{}

// zerologAdapter adapts zerolog to work with go-redis internal logger
type zerologAdapter struct {
	logger *zerolog.Logger
}

// Printf implements the internal.Logging interface for go-redis
func (z *zerologAdapter) Printf(ctx context.Context, format string, v ...interface{}) {
	// Parse and clean up the message
	msg := fmt.Sprintf(format, v...)

	// Remove trailing newline if present
	msg = strings.TrimSuffix(msg, "\n")

	// Detect error conditions in pubsub and other components
	if strings.Contains(msg, "discarding bad PubSub connection") ||
		strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset") {
		z.logger.Warn().Str("component", "redis").Msg(msg)
	} else {
		// Log other messages at trace level to reduce noise
		z.logger.Trace().Str("component", "redis").Msg(msg)
	}
}

type RedisConnector struct {
	id            string
	logger        *zerolog.Logger
	client        *redis.Client
	initializer   *util.Initializer
	cfg           *common.RedisConnectorConfig
	redsync       *redsync.Redsync
	pubsubManager *RedisPubSubManager
	appCtx        context.Context

	ttls        map[string]time.Duration
	initTimeout time.Duration
	getTimeout  time.Duration
	setTimeout  time.Duration
}

func init() {
	// Configure go-redis internal logger to use zerolog
	lg := log.Logger.With().Str("component", "redis").Logger()
	redis.SetLogger(&zerologAdapter{logger: &lg})
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
		appCtx:      appCtx,
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
	// First, check if existing connection is still healthy
	if r.client != nil {
		healthCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := r.client.Ping(healthCtx).Result()
		cancel()

		if err == nil {
			r.logger.Debug().Msg("existing Redis connection is healthy, skipping reconnection")
			return nil // Connection is healthy, no need to reconnect
		}
		r.logger.Debug().Err(err).Msg("existing Redis connection unhealthy, proceeding with reconnection")
	}

	var options *redis.Options
	var err error

	redisURI := strings.TrimSpace(r.cfg.URI)
	r.logger.Debug().Str("uri", util.RedactEndpoint(redisURI)).Msg("attempting to connect to Redis using provided URI")
	options, err = redis.ParseURL(redisURI)
	if err != nil {
		return common.NewTaskFatal(fmt.Errorf("failed to parse Redis URI: %w", err))
	}

	if r.initTimeout == 0 && options.DialTimeout > 0 {
		r.initTimeout = options.DialTimeout
	}
	if r.getTimeout == 0 && options.ReadTimeout > 0 {
		r.getTimeout = options.ReadTimeout
	}
	if r.setTimeout == 0 && options.WriteTimeout > 0 {
		r.setTimeout = options.WriteTimeout
	}

	/**
	* if tls.enabled: true in config → build a tls.Config and
	* apply or merge it.
	*
	* Otherwise, keep whatever came from the URI (rediss:// or none).
	* redis.ParseURL("rediss://…") inserts a minimal tls.Config whose only special bit is
	* InsecureSkipVerify = true (i.e. accept any server-cert)
	 */
	if cfgTLS := r.cfg.TLS; cfgTLS != nil && cfgTLS.Enabled {
		tlsConfig, err := common.CreateTLSConfig(cfgTLS)
		if err != nil {
			return common.NewTaskFatal(fmt.Errorf("failed to create TLS config: %w", err))
		}

		if options.TLSConfig == nil {
			// URI was redis:// — take YAML config wholesale.
			options.TLSConfig = tlsConfig
			r.logger.Debug().Msg("enabled TLS via YAML configuration")
		} else {
			// URI was rediss:// — merge YAML extras onto the baseline.
			if len(tlsConfig.Certificates) > 0 {
				options.TLSConfig.Certificates = tlsConfig.Certificates
				options.TLSConfig.InsecureSkipVerify = false
			}
			if tlsConfig.RootCAs != nil {
				options.TLSConfig.RootCAs = tlsConfig.RootCAs
				options.TLSConfig.InsecureSkipVerify = false
			}
			r.logger.Debug().Msg("merged YAML TLS certificates/CA into rediss:// config")
		}
	} else if options.TLSConfig != nil {
		r.logger.Debug().Msg("using TLS configuration implied by rediss:// URI (verify against system CAs or InsecureSkipVerify)")
	}

	r.logger.Debug().Str("addr", options.Addr).Msg("attempting to connect to Redis")
	client := redis.NewClient(options)

	// Test the connection with Ping.
	ctx, cancel := context.WithTimeout(ctx, r.initTimeout)
	defer cancel()
	_, err = client.Ping(ctx).Result()
	if err != nil {
		if options.TLSConfig != nil && strings.Contains(err.Error(), "certificate") {
			errMsg := fmt.Sprintf("failed to connect to Redis with TLS enabled: %v.", err)
			if !options.TLSConfig.InsecureSkipVerify && options.TLSConfig.RootCAs == nil {
				errMsg += " Ensure the server certificate is valid and trusted by the system CAs, or provide a custom CA using 'tls.caFile'."
			} else if !options.TLSConfig.InsecureSkipVerify && options.TLSConfig.RootCAs != nil {
				errMsg += " Ensure the server certificate is valid and signed by the provided 'tls.caFile'."
			}
			if len(options.TLSConfig.Certificates) > 0 {
				errMsg += " Also verify the client certificate and key ('tls.certFile', 'tls.keyFile') if used."
			}
			return common.NewTaskFatal(fmt.Errorf(errMsg))
		}
		return err
	}

	if r.client != nil {
		_ = r.client.Close()
	}
	r.client = client

	pool := goredis.NewPool(client)
	r.redsync = redsync.New(pool)

	// Handle pubsub manager - create once and let it handle reconnections
	if r.pubsubManager == nil {
		// First time initialization
		r.pubsubManager = NewRedisPubSubManager(r.appCtx, r.logger, r)
	}
	// Manager will detect the client change and reconnect internally

	r.logger.Info().Str("addr", options.Addr).Msg("successfully connected to Redis") // Use options.Addr for logging consistency
	return nil
}

// markConnectionAsLostIfNecessary sets the connection task's state to "failed" so that the Initializer triggers a retry.
func (r *RedisConnector) markConnectionAsLostIfNecessary(err error) {
	if r.initializer == nil {
		return
	}
	if err == nil {
		return
	}
	if err == redis.Nil || err == redis.TxFailedErr {
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
		return
	}

	// Let go-redis handle temporary network issues, timeouts, and retries internally
	errStr := err.Error()

	// Only trigger reconnection for critical connection failures
	if strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "no such host") ||
		strings.Contains(errStr, "network is unreachable") ||
		strings.Contains(errStr, "connection reset by peer") ||
		strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "invalid connection") ||
		strings.Contains(errStr, "connection closed") {

		// Add some backoff to reduce pressure
		r.logger.Warn().Err(err).Msg("detected critical connection failure, marking for reconnection")
		r.initializer.MarkTaskAsFailed(fmt.Sprintf("redis-connect/%s", r.id), fmt.Errorf("critical connection failure: %w", err))
		return
	}

	// For other errors, just log and let go-redis handle internally
	r.logger.Debug().Err(err).Msg("redis operation failed, letting go-redis handle internally")
}

// checkReady returns an error if Redis is not in a ready state.
func (r *RedisConnector) checkReady() error {
	if r.initializer == nil {
		return fmt.Errorf("initializer not set")
	}
	state := r.initializer.State()
	if state != util.StateReady {
		return fmt.Errorf("redis is not connected (state: %s), errors: %v", state.String(), r.initializer.Errors())
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
func (r *RedisConnector) Set(ctx context.Context, partitionKey, rangeKey string, value []byte, ttl *time.Duration) error {
	ctx, span := common.StartSpan(ctx, "RedisConnector.Set")
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("partition_key", partitionKey),
			attribute.String("range_key", rangeKey),
			attribute.Int("value_size", len(value)),
		)
	}

	if err := r.checkReady(); err != nil {
		common.SetTraceSpanError(span, err)
		return err
	}

	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)
	if len(value) < 1024 {
		r.logger.Debug().Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Int("len", len(value)).Msg("writing value to Redis")
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
		r.logger.Warn().Err(err).Str("key", key).Msg("failed to SET in Redis")
		r.markConnectionAsLostIfNecessary(err)
		common.SetTraceSpanError(span, err)
		return err
	}

	/**
	 * TODO Find a better way to store a reverse index for cache entries with unknown block ref (*):
	 */
	if strings.HasPrefix(partitionKey, "evm:") && !strings.HasSuffix(partitionKey, "*") {
		// Maintain a reverse index for fast wildcard lookups (idx_reverse) similar to Memory connector.
		// Only index EVM partition keys that are not already wildcarded.
		parts := strings.SplitAfterN(partitionKey, ":", 3)
		if len(parts) >= 2 {
			wildcardPartitionKey := parts[0] + parts[1] + "*"
			reverseKey := fmt.Sprintf("%s#%s#%s", redisReverseIndexPrefix, wildcardPartitionKey, rangeKey)
			// Best-effort: log on error but do not fail the primary SET.
			if err := r.client.Set(ctx, reverseKey, partitionKey, duration).Err(); err != nil {
				r.logger.Warn().Err(err).Str("key", reverseKey).Msg("failed to SET reverse index in Redis")
			}
		}
	}

	return nil
}

// Get retrieves a value from Redis. If wildcard, retrieves the first matching key. Returns early if not ready.
func (r *RedisConnector) Get(ctx context.Context, index, partitionKey, rangeKey string, _ interface{}) ([]byte, error) {
	ctx, span := common.StartSpan(ctx, "RedisConnector.Get",
		trace.WithAttributes(
			attribute.String("index", index),
			attribute.String("partition_key", partitionKey),
			attribute.String("range_key", rangeKey),
		),
	)
	defer span.End()

	if err := r.checkReady(); err != nil {
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	// If the caller specifies the special index "idx_reverse" and the partitionKey contains a wildcard
	// we attempt to resolve the concrete partition key through the reverse index (to avoid SCAN).
	if index == ConnectorReverseIndex && strings.HasSuffix(partitionKey, "*") {
		revKey := fmt.Sprintf("%s#%s#%s", redisReverseIndexPrefix, partitionKey, rangeKey)
		lookupCtx, lookupCancel := context.WithTimeout(ctx, r.getTimeout)
		revPartitionKey, revErr := r.client.Get(lookupCtx, revKey).Result()
		lookupCancel()
		if revErr != nil {
			r.logger.Debug().Err(revErr).Str("key", revKey).Msg("failed to GET reverse index in Redis")
			r.markConnectionAsLostIfNecessary(revErr)
			common.SetTraceSpanError(span, revErr)
		}
		// Replace wildcard partitionKey with the resolved concrete value if found
		// otherwise we will continue with the original partitionKey for lookup.
		if revPartitionKey != "" {
			partitionKey = revPartitionKey

			// Verify the resolved key still exists and hasn't expired
			// This handles the edge case where reverse index points to an expired key
			resolvedKey := fmt.Sprintf("%s:%s", partitionKey, rangeKey)
			ttlCtx, ttlCancel := context.WithTimeout(ctx, r.getTimeout)
			ttl, ttlErr := r.client.TTL(ttlCtx, resolvedKey).Result()
			ttlCancel()

			if ttlErr != nil {
				r.logger.Debug().Err(ttlErr).Str("key", resolvedKey).Msg("failed to check TTL for resolved key")
			} else if ttl == -2*time.Second {
				// Key doesn't exist (Redis returns -2 when key doesn't exist); treat as a miss
				r.logger.Debug().Str("key", resolvedKey).Msg("resolved key from reverse index no longer exists")
				err := common.NewErrRecordNotFound(partitionKey, rangeKey, RedisDriverName)
				common.SetTraceSpanError(span, err)
				return nil, err
			} else if ttl == -1*time.Second {
				// Key exists but has no TTL (persistent key), which is fine
				r.logger.Trace().Str("key", resolvedKey).Msg("resolved key has no TTL (persistent)")
			} else if ttl > 0 {
				// Key exists and has TTL, which is fine
				r.logger.Trace().Str("key", resolvedKey).Dur("ttl", ttl).Msg("resolved key still valid")
			}
		}
	}

	// Construct the final key and continue with the regular retrieval path.
	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)

	ctx, cancel := context.WithTimeout(ctx, r.getTimeout)
	defer cancel()

	r.logger.Trace().Str("key", key).Msg("getting item from Redis")
	value, err := r.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		err = common.NewErrRecordNotFound(partitionKey, rangeKey, RedisDriverName)
		common.SetTraceSpanError(span, err)
		return nil, err
	} else if err != nil {
		r.logger.Warn().Err(err).Str("key", key).Msg("failed to GET in Redis")
		r.markConnectionAsLostIfNecessary(err)
		common.SetTraceSpanError(span, err)
		return nil, err
	}
	if len(value) < 1024 {
		r.logger.Debug().Str("key", key).Int("len", len(value)).Msg("received item from Redis")
	} else {
		r.logger.Debug().Str("key", key).Int("len", len(value)).Msg("received item from Redis")
	}

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.Int("value_size", len(value)),
		)
	}

	return value, nil
}

// Lock attempts to acquire a distributed lock for the specified key.
// It uses SET NX with an expiration TTL. Returns a DistributedLock instance on success.
// The method will attempt to acquire the lock for the duration of the provided context, retrying periodically.
// If acquired, the lock will be held in Redis for the 'ttl' duration.
func (r *RedisConnector) Lock(ctx context.Context, lockKey string, ttl time.Duration) (DistributedLock, error) {
	ctx, span := common.StartSpan(ctx, "RedisConnector.Lock",
		trace.WithAttributes(
			attribute.String("lock_key", lockKey),
			attribute.Int64("ttl_ms", ttl.Milliseconds()),
		),
	)
	defer span.End()

	if err := r.checkReady(); err != nil {
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	// ttl parameter is now primarily for the lock's expiry in Redis.
	// The acquisition timeout is governed by the deadline of the parent 'ctx'.
	retryInterval := 500 * time.Millisecond // Faster retry interval for responsive operations
	if r.cfg != nil && r.cfg.LockRetryInterval.Duration() > 0 {
		retryInterval = r.cfg.LockRetryInterval.Duration()
	}

	// Calculate max retries based on context deadline and lock TTL
	// We want to retry during the lock TTL period, but also ensure we leave time for operations
	var maxRetries int
	deadline, hasDeadline := ctx.Deadline()

	// Calculate how long we should try to acquire the lock
	// This is the minimum of: context deadline minus operation buffer, or lock TTL
	var maxLockWaitTime time.Duration

	if hasDeadline {
		timeUntilDeadline := time.Until(deadline)
		if timeUntilDeadline > DefaultOperationBuffer {
			maxLockWaitTime = timeUntilDeadline - DefaultOperationBuffer
		} else {
			maxLockWaitTime = timeUntilDeadline / 2 // Use half if not enough time
		}

		// Cap at lock TTL since that's when the lock would be released anyway
		if maxLockWaitTime > ttl {
			maxLockWaitTime = ttl
		}
	} else {
		// No deadline, wait up to lock TTL
		maxLockWaitTime = ttl
	}

	// Calculate retries based on wait time
	if maxLockWaitTime < retryInterval {
		maxRetries = 1 // At least try once
	} else {
		maxRetries = int(maxLockWaitTime / retryInterval)
		// Cap based on lockTtl to allow waiting for healthy contention
		maxRetriesBasedOnTtl := int(ttl / retryInterval)
		if maxRetries > maxRetriesBasedOnTtl {
			maxRetries = maxRetriesBasedOnTtl
		}
		// Only enforce minimum retries if we have enough time for healthy contention
		// Don't override short context deadlines that expect fast failure
		if maxRetries < 3 && maxLockWaitTime > 2*retryInterval {
			maxRetries = 3
		}
	}

	r.logger.Debug().
		Str("key", lockKey).
		Dur("maxLockWaitTime", maxLockWaitTime).
		Dur("retryInterval", retryInterval).
		Int("maxRetries", maxRetries).
		Msg("calculated lock acquisition strategy")

	mutex := r.redsync.NewMutex(
		fmt.Sprintf("lock:%s", lockKey),
		redsync.WithExpiry(ttl),               // Lock key in Redis expires after ttl
		redsync.WithRetryDelay(retryInterval), // Wait this long between retries
		redsync.WithTries(maxRetries),         // Attempt this many times (or until context is done)
	)

	r.logger.Debug().Str("key", lockKey).Dur("ttl", ttl).Dur("retryInterval", retryInterval).Int("max_tries", maxRetries).Msg("attempting to acquire distributed lock")

	// LockContext will use the parent ctx. It will retry according to Tries and RetryDelay,
	// but will stop early if ctx's deadline is met or ctx is cancelled.
	if err := mutex.LockContext(ctx); err != nil {
		common.SetTraceSpanError(span, err)
		// Check if the error is due to the parent context being done
		if ctx.Err() != nil { // Parent context was cancelled or timed out
			r.logger.Warn().Err(err).Str("key", lockKey).Msgf("failed to acquire lock; parent context cancelled or deadline exceeded: %v", ctx.Err())
			// Return the parent context's error, as it's the root cause for stopping.
			// Redsync's error (err) might be a generic "context done" or more specific.
			return nil, fmt.Errorf("failed to acquire lock for key '%s': %w", lockKey, ctx.Err())
		}
		// If ctx.Err() is nil, but LockContext failed, it's another redsync error (e.g. Tries exhausted before context, connection issue)
		r.logger.Warn().Err(err).Str("key", lockKey).Msg("failed to acquire lock")
		return nil, fmt.Errorf("failed to acquire lock for key '%s': %w", lockKey, err)
	}

	r.logger.Info().Str("key", lockKey).Dur("ttl", ttl).Msg("distributed lock acquired")

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

	// Ensure pubsub manager is initialized
	if r.pubsubManager == nil {
		return nil, nil, fmt.Errorf("pubsub manager not initialized")
	}

	// Subscribe through the centralized manager
	return r.pubsubManager.Subscribe(key)
}

// PublishCounterInt64 publishes a counter value to Redis.
func (r *RedisConnector) PublishCounterInt64(ctx context.Context, key string, value int64) error {
	ctx, span := common.StartSpan(ctx, "RedisConnector.PublishCounterInt64",
		trace.WithAttributes(
			attribute.String("key", key),
		),
	)
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.Int64("value", value),
		)
	}

	if err := r.checkReady(); err != nil {
		common.SetTraceSpanError(span, err)
		return err
	}
	r.logger.Debug().Str("key", key).Int64("value", value).Msg("publishing counter int64 update to redis")
	err := r.client.Publish(ctx, "counter:"+key, value).Err()
	if err != nil {
		common.SetTraceSpanError(span, err)
	}
	r.logger.Debug().Str("key", key).Int64("value", value).Msg("published counter int64 update to redis")
	return err
}

var _ DistributedLock = &redisLock{}

type redisLock struct {
	connector *RedisConnector
	key       string
	mutex     *redsync.Mutex
}

func (l *redisLock) IsNil() bool {
	return l == nil || l.mutex == nil
}

func (l *redisLock) Unlock(ctx context.Context) error {
	ctx, span := common.StartSpan(ctx, "RedisConnector.Unlock",
		trace.WithAttributes(
			attribute.String("lock_key", l.key),
		),
	)
	defer span.End()

	ctx, cancel := context.WithTimeout(ctx, l.connector.setTimeout)
	defer cancel()
	ok, err := l.mutex.UnlockContext(ctx)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return fmt.Errorf("error releasing lock: %w", err)
	}
	if !ok {
		err := errors.New("failed to release lock")
		common.SetTraceSpanError(span, err)
		return err
	}
	l.connector.logger.Trace().Str("key", l.key).Msg("distributed lock released")
	return nil
}

func (r *RedisConnector) Delete(ctx context.Context, partitionKey, rangeKey string) error {
	ctx, span := common.StartSpan(ctx, "RedisConnector.Delete")
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("partition_key", partitionKey),
			attribute.String("range_key", rangeKey),
		)
	}

	if err := r.checkReady(); err != nil {
		common.SetTraceSpanError(span, err)
		return err
	}

	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)
	r.logger.Debug().Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Msg("deleting from Redis")

	ctx, cancel := context.WithTimeout(ctx, r.setTimeout)
	defer cancel()

	// Delete main key
	if err := r.client.Del(ctx, key).Err(); err != nil {
		r.logger.Warn().Err(err).Str("key", key).Msg("failed to DELETE in Redis")
		r.markConnectionAsLostIfNecessary(err)
		common.SetTraceSpanError(span, err)
		return err
	}

	// Clean up reverse index if it exists
	if strings.HasPrefix(partitionKey, "evm:") && !strings.HasSuffix(partitionKey, "*") {
		parts := strings.SplitAfterN(partitionKey, ":", 3)
		if len(parts) >= 2 {
			wildcardPartitionKey := parts[0] + parts[1] + "*"
			reverseKey := fmt.Sprintf("%s#%s#%s", redisReverseIndexPrefix, wildcardPartitionKey, rangeKey)
			// Best-effort: log on error but do not fail the primary DELETE
			if err := r.client.Del(ctx, reverseKey).Err(); err != nil {
				r.logger.Warn().Err(err).Str("key", reverseKey).Msg("failed to DELETE reverse index in Redis")
			}
		}
	}

	return nil
}

func (r *RedisConnector) List(ctx context.Context, index string, limit int, paginationToken string) ([]KeyValuePair, string, error) {
	ctx, span := common.StartSpan(ctx, "RedisConnector.List")
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("index", index),
			attribute.Int("limit", limit),
		)
	}

	if err := r.checkReady(); err != nil {
		common.SetTraceSpanError(span, err)
		return nil, "", err
	}

	ctx, cancel := context.WithTimeout(ctx, r.getTimeout)
	defer cancel()

	// Use SCAN for efficient pagination
	cursor := uint64(0)
	if paginationToken != "" {
		var err error
		cursor, err = strconv.ParseUint(paginationToken, 10, 64)
		if err != nil {
			return nil, "", fmt.Errorf("invalid pagination token: %w", err)
		}
	}

	// Determine the pattern to scan for
	pattern := "*"
	if index == ConnectorReverseIndex {
		pattern = redisReverseIndexPrefix + "#*"
	}

	keys, nextCursor, err := r.client.Scan(ctx, cursor, pattern, int64(limit)).Result()
	if err != nil {
		r.logger.Warn().Err(err).Msg("failed to SCAN in Redis")
		r.markConnectionAsLostIfNecessary(err)
		common.SetTraceSpanError(span, err)
		return nil, "", err
	}

	results := make([]KeyValuePair, 0, len(keys))

	// Get values for all keys in a pipeline for efficiency
	pipe := r.client.Pipeline()
	cmds := make([]*redis.StringCmd, len(keys))
	for i, key := range keys {
		cmds[i] = pipe.Get(ctx, key)
	}

	_, err = pipe.Exec(ctx)
	if err != nil && err != redis.Nil {
		r.logger.Warn().Err(err).Msg("failed to execute pipeline in Redis List")
		r.markConnectionAsLostIfNecessary(err)
		common.SetTraceSpanError(span, err)
		return nil, "", err
	}

	for i, cmd := range cmds {
		value, err := cmd.Bytes()
		if err == redis.Nil {
			continue // Key was deleted between SCAN and GET
		}
		if err != nil {
			r.logger.Warn().Err(err).Str("key", keys[i]).Msg("failed to GET key value in Redis List")
			continue
		}

		// Parse the key to extract partition and range keys
		var partitionKey, rangeKey string
		if index == ConnectorReverseIndex && strings.HasPrefix(keys[i], redisReverseIndexPrefix+"#") {
			// For reverse index keys: rvi#partitionKey#rangeKey
			parts := strings.SplitN(keys[i], "#", 3)
			if len(parts) == 3 {
				partitionKey = parts[1]
				rangeKey = parts[2]
			}
		} else {
			// For regular keys: partitionKey:rangeKey
			parts := strings.SplitN(keys[i], ":", 2)
			if len(parts) == 2 {
				partitionKey = parts[0]
				rangeKey = parts[1]
			}
		}

		if partitionKey != "" && rangeKey != "" {
			results = append(results, KeyValuePair{
				PartitionKey: partitionKey,
				RangeKey:     rangeKey,
				Value:        value,
			})
		}
	}

	nextToken := ""
	if nextCursor != 0 {
		nextToken = strconv.FormatUint(nextCursor, 10)
	}

	return results, nextToken, nil
}
