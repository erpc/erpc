package data

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/rs/zerolog"
)

const (
	MemoryDriverName = "memory"
)

var _ Connector = (*MemoryConnector)(nil)

type MemoryConnector struct {
	id            string
	logger        *zerolog.Logger
	cache         *lru.Cache[string, cacheItem]
	cleanupTicker *time.Ticker
	locks         sync.Map // map[string]*sync.Mutex
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
	lg.Debug().Interface("config", cfg).Msg("creating memory connector")

	if cfg != nil && cfg.MaxItems <= 0 {
		return nil, fmt.Errorf("maxItems must be greater than 0")
	}

	maxItems := 100
	if cfg != nil && cfg.MaxItems > 0 {
		maxItems = cfg.MaxItems
	}

	cache, err := lru.New[string, cacheItem](maxItems)
	if err != nil {
		return nil, fmt.Errorf("failed to create LRU cache: %w", err)
	}

	c := &MemoryConnector{
		id:            id,
		logger:        &lg,
		cache:         cache,
		cleanupTicker: time.NewTicker(1 * time.Minute),
	}

	go c.startCleanup(ctx)

	return c, nil
}

func (m *MemoryConnector) startCleanup(ctx context.Context) {
	m.logger.Debug().Msg("starting expired items cleanup routine")
	for {
		select {
		case <-ctx.Done():
			m.logger.Debug().Msg("stopping cleanup routine due to context cancellation")
			return
		case <-m.cleanupTicker.C:
			m.cleanupExpired()
		}
	}
}

func (m *MemoryConnector) Id() string {
	return m.id
}

func (m *MemoryConnector) Set(ctx context.Context, partitionKey, rangeKey, value string, ttl *time.Duration) error {
	m.logger.Debug().Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Msg("writing to memory")

	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)
	item := cacheItem{
		value: value,
	}

	if ttl != nil && *ttl > 0 {
		expiresAt := time.Now().Add(*ttl)
		item.expiresAt = &expiresAt
	}

	m.cache.Add(key, item)
	return nil
}

func (m *MemoryConnector) Get(ctx context.Context, index, partitionKey, rangeKey string) (string, error) {
	if strings.HasSuffix(partitionKey, "*") {
		return m.getWithWildcard(ctx, index, partitionKey, rangeKey)
	}

	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)
	m.logger.Debug().Str("key", key).Msg("getting item from memory")

	item, ok := m.cache.Get(key)
	if !ok {
		return "", common.NewErrRecordNotFound(partitionKey, rangeKey, MemoryDriverName)
	}

	// Check if item has expired
	if item.expiresAt != nil && !time.Now().Before(*item.expiresAt) {
		m.cache.Remove(key)
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

func (m *MemoryConnector) getWithWildcard(_ context.Context, _, partitionKey, rangeKey string) (string, error) {
	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)
	for _, k := range m.cache.Keys() {
		match, err := common.WildcardMatch(key, k)
		if err != nil {
			return "", err
		}
		if match {
			item, _ := m.cache.Get(k)
			// Check expiration
			if item.expiresAt != nil && time.Now().After(*item.expiresAt) {
				m.cache.Remove(k)
				continue
			}
			return item.value, nil
		}
	}
	return "", common.NewErrRecordNotFound(partitionKey, rangeKey, MemoryDriverName)
}

func (m *MemoryConnector) cleanupExpired() {
	now := time.Now()
	expiredKeys := make([]string, 0)

	// First pass: collect expired keys
	for _, key := range m.cache.Keys() {
		if item, ok := m.cache.Peek(key); ok {
			if item.expiresAt != nil && now.After(*item.expiresAt) {
				expiredKeys = append(expiredKeys, key)
			}
		}
	}

	// Second pass: remove expired items
	if len(expiredKeys) > 0 {
		m.logger.Trace().Int("count", len(expiredKeys)).Msg("removing expired items")
		for _, key := range expiredKeys {
			m.cache.Remove(key)
		}
	}
}
