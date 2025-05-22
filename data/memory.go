package data

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/dustin/go-humanize"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

const (
	MemoryDriverName = "memory"
)

var _ Connector = (*MemoryConnector)(nil)

type MemoryConnector struct {
	id     string
	logger *zerolog.Logger
	cache  *ristretto.Cache[string, cacheItem]
	locks  sync.Map // map[string]*sync.Mutex
}

type cacheItem struct {
	value     string
	expiresAt *time.Time
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

	ristrettoCfg := &ristretto.Config[string, cacheItem]{
		NumCounters: int64(3 * cfg.MaxItems), // number of keys to track frequency of.
		MaxCost:     maxCost,                 // maximum cost of cache.
		BufferItems: 64,                      // number of keys per Get buffer.
		Metrics:     false,                   // set to true to track metrics
	}

	cache, err := ristretto.NewCache(ristrettoCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create ristretto cache: %w", err)
	}

	c := &MemoryConnector{
		id:     id,
		logger: &lg,
		cache:  cache,
	}

	return c, nil
}

func (m *MemoryConnector) Id() string {
	return m.id
}

func (m *MemoryConnector) Set(ctx context.Context, partitionKey, rangeKey, value string, ttl *time.Duration) error {
	m.logger.Debug().Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Msg("writing to memory (ristretto)")

	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)
	item := cacheItem{
		value: value,
	}

	if ttl != nil && *ttl > 0 {
		expiresAt := time.Now().Add(*ttl)
		item.expiresAt = &expiresAt
	}

	cost := int64(len(value)) // Cost is the size of the value in bytes
	// Ristretto's Set might drop the item if the cache is full and the item isn't valuable enough.
	// It returns true if the item was added, false otherwise. We don't explicitly check this boolean
	// as per Ristretto's design philosophy (popular items will eventually get in).
	m.cache.Set(key, item, cost)

	/**
	 * TODO Find a better way to store a reverse index for cache entries with unknown block ref (*):
	 */
	if strings.HasPrefix(partitionKey, "evm:") && !strings.HasSuffix(partitionKey, "*") {
		m.cache.Set("reverse-index#"+rangeKey, cacheItem{
			value: partitionKey,
		}, int64(len(partitionKey)))
	}

	return nil
}

func (m *MemoryConnector) Get(ctx context.Context, index, partitionKey, rangeKey string) (string, error) {
	if strings.HasSuffix(partitionKey, "*") {
		fullKey, found := m.cache.Get("reverse-index#" + rangeKey)
		if found && fullKey.value != "" {
			partitionKey = fullKey.value
		}
	}

	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)
	m.logger.Debug().Str("key", key).Msg("getting item from memory (ristretto)")

	item, found := m.cache.Get(key)
	if !found {
		return "", common.NewErrRecordNotFound(partitionKey, rangeKey, MemoryDriverName)
	}

	// Check if item has expired
	if item.expiresAt != nil && !time.Now().Before(*item.expiresAt) {
		m.cache.Del(key) // Remove expired item
		return "", common.NewErrRecordNotFound(partitionKey, rangeKey, MemoryDriverName)
	}

	return item.value, nil
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
