package data

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/dustin/go-humanize"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
)

const (
	MemoryDriverName         = "memory"
	memoryReverseIndexPrefix = "rvi"
)

var _ Connector = (*MemoryConnector)(nil)

type MemoryConnector struct {
	id          string
	logger      *zerolog.Logger
	cache       *ristretto.Cache[string, []byte]
	locks       sync.Map // map[string]*sync.Mutex
	emitMetrics bool

	// Previous metric values for calculating deltas
	prevMetrics struct {
		setsDropped  uint64
		setsRejected uint64
	}
	metricsMutex sync.RWMutex
	stopMetrics  context.CancelFunc
}

func NewMemoryConnector(
	ctx context.Context,
	logger *zerolog.Logger,
	id string,
	cfg *common.MemoryConnectorConfig,
) (*MemoryConnector, error) {
	lg := logger.With().Str("connector", id).Logger()
	lg.Debug().Interface("config", cfg).Msg("creating memory connector with ristretto")

	if cfg.MaxItems <= 0 {
		return nil, fmt.Errorf("maxItems must be greater than 0")
	}

	maxTotalSizeBytes, err := humanize.ParseBytes(cfg.MaxTotalSize)
	if err != nil {
		return nil, fmt.Errorf("failed to parse maxTotalSize '%s': %w", cfg.MaxTotalSize, err)
	}
	if maxTotalSizeBytes <= 0 {
		return nil, fmt.Errorf("maxTotalSize must be greater than 0 bytes")
	}

	var maxCost int64
	if maxTotalSizeBytes > uint64(math.MaxInt64) {
		maxCost = math.MaxInt64
		lg.Warn().Uint64("configuredMaxTotalSize", maxTotalSizeBytes).Int64("cappedMaxCost", maxCost).Msg("MaxTotalSize exceeds int64 capacity, capping to math.MaxInt64")
	} else {
		maxCost = int64(maxTotalSizeBytes)
	}

	// Determine if metrics should be enabled
	enableMetrics := cfg.EmitMetrics != nil && *cfg.EmitMetrics

	ristrettoCfg := &ristretto.Config[string, []byte]{
		NumCounters: int64(3 * cfg.MaxItems), // number of keys to track frequency of.
		MaxCost:     maxCost,                 // maximum cost of cache.
		BufferItems: 64,                      // number of keys per Get buffer.
		Metrics:     enableMetrics,           // enable metrics based on config
		Cost: func(v []byte) int64 {
			return int64(len(v) + 256)
		},
	}

	cache, err := ristretto.NewCache(ristrettoCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create ristretto cache: %w", err)
	}

	c := &MemoryConnector{
		id:          id,
		logger:      &lg,
		cache:       cache,
		emitMetrics: enableMetrics,
	}

	// Start metrics collection goroutine if enabled
	if enableMetrics {
		metricsCtx, cancel := context.WithCancel(ctx)
		c.stopMetrics = cancel
		go c.metricsCollectionLoop(metricsCtx)
		lg.Info().Msg("Ristretto metrics collection enabled")
	}

	return c, nil
}

func (m *MemoryConnector) Id() string {
	return m.id
}

func (m *MemoryConnector) Set(ctx context.Context, partitionKey, rangeKey string, value []byte, ttl *time.Duration) error {
	m.logger.Debug().Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Int("len", len(value)).Msg("writing to memory (ristretto)")

	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)

	if ttl != nil && *ttl > 0 {
		m.cache.SetWithTTL(key, value, 0, *ttl)
	} else {
		m.cache.Set(key, value, 0)
	}

	/**
	 * TODO Find a better way to store a reverse index for cache entries with unknown block ref (*):
	 */
	if strings.HasPrefix(partitionKey, "evm:") && !strings.HasSuffix(partitionKey, "*") {
		parts := strings.SplitAfterN(partitionKey, ":", 3)
		if len(parts) >= 2 {
			wildcardPartitionKey := parts[0] + parts[1] + "*"
			m.cache.Set(memoryReverseIndexPrefix+"#"+wildcardPartitionKey+"#"+rangeKey, []byte(partitionKey), 0)
		}
	}

	return nil
}

func (m *MemoryConnector) Get(ctx context.Context, index, partitionKey, rangeKey string, _ interface{}) ([]byte, error) {
	if index == ConnectorReverseIndex && strings.HasSuffix(partitionKey, "*") {
		fullKey, found := m.cache.Get(memoryReverseIndexPrefix + "#" + partitionKey + "#" + rangeKey)
		// Replace wildcard partitionKey with the resolved concrete value if found
		// otherwise we will continue with the original partitionKey for lookup.
		if found && fullKey != nil {
			partitionKey = string(fullKey)
		}
	}

	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)
	m.logger.Debug().Str("key", key).Msg("getting item from memory (ristretto)")

	item, found := m.cache.Get(key)
	if !found {
		return nil, common.NewErrRecordNotFound(partitionKey, rangeKey, MemoryDriverName)
	}

	return item, nil
}

func (m *MemoryConnector) Lock(ctx context.Context, key string, ttl time.Duration) (DistributedLock, error) {
	value, _ := m.locks.LoadOrStore(key, &sync.Mutex{})
	mutex := value.(*sync.Mutex)

	// Honor context deadline/cancellation for best-effort locking in tests
	// Use TryLock when available; otherwise spin with backoff respecting ctx.
	tryInterval := 2 * time.Millisecond
	tryLockFailed := false
	for {
		if mutex.TryLock() {
			return &memoryLock{mutex: mutex}, nil
		}
		tryLockFailed = true
		select {
		case <-ctx.Done():
			if tryLockFailed {
				// TryLock has been failing → lock is held by another caller (contention).
				// Preserve ctx.Err() semantics for cancellation/timeout handling.
				return nil, fmt.Errorf("lock contention for in-memory lock: %w", errors.Join(ErrLockContention, ctx.Err()))
			}
			// Context cancelled before we observed contention — propagate the real cause.
			return nil, ctx.Err()
		case <-time.After(tryInterval):
			// Back off and retry until ctx deadline
			if tryInterval < 20*time.Millisecond {
				tryInterval += 1 * time.Millisecond
			}
		}
	}
}

var _ DistributedLock = &memoryLock{}

type memoryLock struct {
	mutex *sync.Mutex
}

func (l *memoryLock) IsNil() bool {
	return l == nil || l.mutex == nil
}

func (l *memoryLock) Unlock(ctx context.Context) error {
	l.mutex.Unlock()
	return nil
}

// WatchCounterInt64 is a no-op for memory connector since distributed pub/sub
// is unnecessary when all operations are in-memory within the same process.
// Any updates to counters are immediately visible to all code accessing the
// memory connector instance.
func (m *MemoryConnector) WatchCounterInt64(ctx context.Context, key string) (<-chan CounterInt64State, func(), error) {
	ch := make(chan CounterInt64State)
	return ch, func() {}, nil
}

// PublishCounterInt64 is a no-op for memory connector since distributed pub/sub
// is unnecessary when all operations are in-memory within the same process.
func (m *MemoryConnector) PublishCounterInt64(ctx context.Context, key string, value CounterInt64State) error {
	return nil
}

// metricsCollectionLoop runs in a background goroutine to periodically collect
// and emit Ristretto cache metrics to Prometheus.
func (m *MemoryConnector) metricsCollectionLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second) // Collect metrics every 30 seconds
	defer ticker.Stop()

	m.logger.Debug().Msg("Starting Ristretto metrics collection loop")

	for {
		select {
		case <-ctx.Done():
			m.logger.Debug().Msg("Stopping Ristretto metrics collection loop")
			return
		case <-ticker.C:
			m.collectAndEmitMetrics()
		}
	}
}

// collectAndEmitMetrics reads the current Ristretto metrics, calculates deltas
// from previous values, and emits them to Prometheus.
func (m *MemoryConnector) collectAndEmitMetrics() {
	if !m.emitMetrics || m.cache == nil || m.cache.Metrics == nil {
		return
	}

	m.metricsMutex.Lock()
	defer m.metricsMutex.Unlock()

	metrics := m.cache.Metrics

	// Get current metric values
	currentSetsDropped := metrics.SetsDropped()
	currentSetsRejected := metrics.SetsRejected()

	// Calculate current cost (memory usage) and emit as gauge
	costAdded := metrics.CostAdded()
	costEvicted := metrics.CostEvicted()

	// Safe conversion to avoid integer overflow
	var currentCost int64
	if costAdded >= costEvicted {
		diff := costAdded - costEvicted
		if diff > uint64(math.MaxInt64) {
			// Cap at MaxInt64 to prevent overflow
			currentCost = math.MaxInt64
			m.logger.Warn().
				Uint64("costAdded", costAdded).
				Uint64("costEvicted", costEvicted).
				Uint64("diff", diff).
				Msg("Current cost exceeds int64 capacity, capping to MaxInt64")
		} else {
			currentCost = int64(diff) // #nosec G115
		}
	} else {
		// This shouldn't happen in normal operation, but handle gracefully
		m.logger.Warn().
			Uint64("costAdded", costAdded).
			Uint64("costEvicted", costEvicted).
			Msg("Cost evicted exceeds cost added, setting current cost to 0")
		currentCost = 0
	}

	telemetry.MetricRistrettoCacheCurrentCost.WithLabelValues(m.id).Set(float64(currentCost))

	// Calculate deltas for sets failed and emit as counter
	setsDroppedDelta := currentSetsDropped - m.prevMetrics.setsDropped
	setsRejectedDelta := currentSetsRejected - m.prevMetrics.setsRejected
	totalSetsFailedDelta := setsDroppedDelta + setsRejectedDelta

	if totalSetsFailedDelta > 0 {
		telemetry.MetricRistrettoCacheSetsFailedTotal.WithLabelValues(m.id).Add(float64(totalSetsFailedDelta))
	}

	// Store current values for next iteration
	m.prevMetrics.setsDropped = currentSetsDropped
	m.prevMetrics.setsRejected = currentSetsRejected

	m.logger.Debug().
		Int64("currentCost", currentCost).
		Uint64("setsDropped", currentSetsDropped).
		Uint64("setsRejected", currentSetsRejected).
		Uint64("setsFailedDelta", totalSetsFailedDelta).
		Msg("Emitted Ristretto cache metrics")
}

func (m *MemoryConnector) Delete(ctx context.Context, partitionKey, rangeKey string) error {
	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)
	m.logger.Debug().Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Msg("deleting from memory (ristretto)")

	// Delete main entry
	m.cache.Del(key)

	// Clean up reverse index if it exists
	if strings.HasPrefix(partitionKey, "evm:") && !strings.HasSuffix(partitionKey, "*") {
		parts := strings.SplitAfterN(partitionKey, ":", 3)
		if len(parts) >= 2 {
			wildcardPartitionKey := parts[0] + parts[1] + "*"
			reverseKey := memoryReverseIndexPrefix + "#" + wildcardPartitionKey + "#" + rangeKey
			m.cache.Del(reverseKey)
		}
	}

	return nil
}

func (m *MemoryConnector) List(ctx context.Context, index string, limit int, paginationToken string) ([]KeyValuePair, string, error) {
	m.logger.Warn().Msg("List operation on MemoryConnector is not efficiently supported by underlying Ristretto cache")
	// TODO: Ristretto doesn't provide efficient iteration capabilities
	// This is a limitation of the memory connector for admin operations
	// For production use, consider using Redis, PostgreSQL, or DynamoDB connectors for admin operations
	return nil, "", fmt.Errorf("List operation not supported by MemoryConnector - Ristretto cache doesn't provide efficient iteration")
}

// Close cleans up resources including stopping the metrics collection goroutine
func (m *MemoryConnector) Close() error {
	if m.stopMetrics != nil {
		m.stopMetrics()
	}
	if m.cache != nil {
		m.cache.Close()
	}
	return nil
}
