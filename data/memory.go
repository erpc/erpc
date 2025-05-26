package data

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/dustin/go-humanize"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/klauspost/compress/zstd"
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
	cache       *ristretto.Cache[string, string]
	locks       sync.Map // map[string]*sync.Mutex
	emitMetrics bool

	// Compression settings
	enableCompression    bool
	compressionThreshold int
	// Thread-safe pools for zstd encoder/decoder
	encoderPool *sync.Pool
	decoderPool *sync.Pool

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

	// Enable compression by default with optimal threshold for EVM RPC responses
	enableCompression := true
	compressionThreshold := 512 // Compress values larger than 512 bytes

	// Initialize thread-safe pools for zstd encoder/decoder
	var encoderPool *sync.Pool
	var decoderPool *sync.Pool

	if enableCompression {
		// Pool of zstd encoders (thread-safe)
		encoderPool = &sync.Pool{
			New: func() interface{} {
				// Use SpeedFastest for optimal caching performance
				encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
				if err != nil {
					lg.Error().Err(err).Msg("failed to create zstd encoder in pool")
					return nil
				}
				return encoder
			},
		}

		// Pool of zstd decoders (thread-safe)
		decoderPool = &sync.Pool{
			New: func() interface{} {
				decoder, err := zstd.NewReader(nil)
				if err != nil {
					lg.Error().Err(err).Msg("failed to create zstd decoder in pool")
					return nil
				}
				return decoder
			},
		}
	}

	ristrettoCfg := &ristretto.Config[string, string]{
		NumCounters: int64(3 * cfg.MaxItems), // number of keys to track frequency of.
		MaxCost:     maxCost,                 // maximum cost of cache.
		BufferItems: 64,                      // number of keys per Get buffer.
		Metrics:     enableMetrics,           // enable metrics based on config
	}

	cache, err := ristretto.NewCache(ristrettoCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create ristretto cache: %w", err)
	}

	c := &MemoryConnector{
		id:                   id,
		logger:               &lg,
		cache:                cache,
		emitMetrics:          enableMetrics,
		enableCompression:    enableCompression,
		compressionThreshold: compressionThreshold,
		encoderPool:          encoderPool,
		decoderPool:          decoderPool,
	}

	// Start metrics collection goroutine if enabled
	if enableMetrics {
		metricsCtx, cancel := context.WithCancel(ctx)
		c.stopMetrics = cancel
		go c.metricsCollectionLoop(metricsCtx)
		lg.Info().Msg("Ristretto metrics collection enabled")
	}

	if enableCompression {
		lg.Info().Int("threshold", compressionThreshold).Msg("Zstd compression enabled for cache values (thread-safe)")
	}

	return c, nil
}

func (m *MemoryConnector) Id() string {
	return m.id
}

func (m *MemoryConnector) Set(ctx context.Context, partitionKey, rangeKey, value string, ttl *time.Duration) error {
	m.logger.Debug().Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Msg("writing to memory (ristretto)")

	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)

	// Compress value if compression is enabled and beneficial
	storeValue, isCompressed := m.compressValue(value)

	cost := int64(len(storeValue)) // Cost is the size of the stored value in bytes
	// Ristretto's Set might drop the item if the cache is full and the item isn't valuable enough.
	// It returns true if the item was added, false otherwise. We don't explicitly check this boolean
	// as per Ristretto's design philosophy (popular items will eventually get in).
	if ttl != nil && *ttl > 0 {
		m.cache.SetWithTTL(key, storeValue, cost, *ttl)
	} else {
		m.cache.Set(key, storeValue, cost)
	}

	// Log compression stats for debugging
	if isCompressed {
		originalSize := len(value)
		compressedSize := len(storeValue) - 5 // Subtract prefix length
		savings := float64(originalSize-compressedSize) / float64(originalSize) * 100
		m.logger.Debug().
			Int("originalSize", originalSize).
			Int("compressedSize", compressedSize).
			Float64("savings", savings).
			Msg("Compressed cache value")
	}

	/**
	 * TODO Find a better way to store a reverse index for cache entries with unknown block ref (*):
	 */
	if strings.HasPrefix(partitionKey, "evm:") && !strings.HasSuffix(partitionKey, "*") {
		m.cache.Set(memoryReverseIndexPrefix+"#"+rangeKey, partitionKey, int64(len(partitionKey)))
	}

	return nil
}

func (m *MemoryConnector) Get(ctx context.Context, index, partitionKey, rangeKey string) (string, error) {
	if index == ConnectorReverseIndex && strings.HasSuffix(partitionKey, "*") {
		fullKey, found := m.cache.Get(memoryReverseIndexPrefix + "#" + rangeKey)
		// Replace wildcard partitionKey with the resolved concrete value if found
		// otherwise we will continue with the original partitionKey for lookup.
		if found && fullKey != "" {
			partitionKey = fullKey
		}
	}

	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)
	m.logger.Debug().Str("key", key).Msg("getting item from memory (ristretto)")

	item, found := m.cache.Get(key)
	if !found {
		return "", common.NewErrRecordNotFound(partitionKey, rangeKey, MemoryDriverName)
	}

	// Check if value is compressed and decompress if needed
	if strings.HasPrefix(item, "ZSTD:") {
		decompressed, err := m.decompressValue(item[5:]) // Remove "ZSTD:" prefix
		if err != nil {
			m.logger.Error().Err(err).Msg("failed to decompress cached value")
			return "", fmt.Errorf("failed to decompress cached value: %w", err)
		}
		return decompressed, nil
	}

	return item, nil
}

// compressValue compresses a string value using zstd if it's larger than the threshold
func (m *MemoryConnector) compressValue(value string) (string, bool) {
	if !m.enableCompression || len(value) < m.compressionThreshold {
		return value, false
	}

	// Get encoder from pool
	encoderInterface := m.encoderPool.Get()
	if encoderInterface == nil {
		m.logger.Warn().Msg("failed to get encoder from pool, storing uncompressed")
		return value, false
	}
	encoder := encoderInterface.(*zstd.Encoder)
	defer m.encoderPool.Put(encoder)

	// Compress using the pooled encoder
	var buf bytes.Buffer
	encoder.Reset(&buf)

	if _, err := encoder.Write([]byte(value)); err != nil {
		m.logger.Warn().Err(err).Msg("failed to compress value, storing uncompressed")
		return value, false
	}

	if err := encoder.Close(); err != nil {
		m.logger.Warn().Err(err).Msg("failed to close zstd encoder, storing uncompressed")
		return value, false
	}

	compressed := buf.Bytes()

	// Only use compression if it actually saves space (accounting for prefix)
	if len(compressed)+5 < len(value) { // +5 for "ZSTD:" prefix
		return "ZSTD:" + string(compressed), true
	}

	return value, false
}

// decompressValue decompresses a zstd-compressed string value
func (m *MemoryConnector) decompressValue(compressedValue string) (string, error) {
	// Get decoder from pool
	decoderInterface := m.decoderPool.Get()
	if decoderInterface == nil {
		return "", fmt.Errorf("failed to get decoder from pool")
	}
	decoder := decoderInterface.(*zstd.Decoder)
	defer m.decoderPool.Put(decoder)

	// Reset decoder with the compressed data and handle error
	if err := decoder.Reset(strings.NewReader(compressedValue)); err != nil {
		return "", fmt.Errorf("failed to reset zstd decoder: %w", err)
	}

	// Read all decompressed data
	decompressed, err := io.ReadAll(decoder)
	if err != nil {
		return "", fmt.Errorf("failed to decompress value: %w", err)
	}

	return string(decompressed), nil
}

func (m *MemoryConnector) Lock(ctx context.Context, key string, ttl time.Duration) (DistributedLock, error) {
	value, _ := m.locks.LoadOrStore(key, &sync.Mutex{})
	mutex := value.(*sync.Mutex)

	mutex.Lock()
	return &memoryLock{
		mutex: mutex,
	}, nil
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
func (m *MemoryConnector) WatchCounterInt64(ctx context.Context, key string) (<-chan int64, func(), error) {
	ch := make(chan int64)
	return ch, func() {}, nil
}

// PublishCounterInt64 is a no-op for memory connector since distributed pub/sub
// is unnecessary when all operations are in-memory within the same process.
func (m *MemoryConnector) PublishCounterInt64(ctx context.Context, key string, value int64) error {
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
