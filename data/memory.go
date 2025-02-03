package data

import (
	"context"
	"fmt"
	"strconv"
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

	// Add new fields for shared state support
	locks    sync.Map // map[string]*sync.Mutex
	watchers sync.Map // map[string][]chan int64
	mu       sync.RWMutex
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
	lg.Debug().Interface("config", cfg).Msg("creating MemoryConnector")

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
	// Get or create mutex for this key
	value, _ := m.locks.LoadOrStore(key, &sync.Mutex{})
	mutex := value.(*sync.Mutex)

	// Try to acquire lock with context timeout
	done := make(chan struct{})
	acquired := false
	go func() {
		mutex.Lock()
		acquired = true
		close(done)
	}()

	select {
	case <-ctx.Done():
		if acquired {
			mutex.Unlock()
		}
		return nil, ctx.Err()
	case <-done:
		return &memoryLock{
			mutex: mutex,
		}, nil
	}
}

type memoryLock struct {
	mutex *sync.Mutex
}

func (l *memoryLock) Unlock(ctx context.Context) error {
	l.mutex.Unlock()
	return nil
}

// Implement WatchCounterInt64 for memory connector
func (m *MemoryConnector) WatchCounterInt64(ctx context.Context, key string) (<-chan int64, func(), error) {
	updates := make(chan int64, 1)

	// Create watcher list if doesn't exist
	m.mu.Lock()
	watchers, _ := m.watchers.LoadOrStore(key, make([]chan int64, 0))
	watcherList := watchers.([]chan int64)
	watcherList = append(watcherList, updates)
	m.watchers.Store(key, watcherList)
	m.mu.Unlock()

	// Send initial value if exists
	if val, err := m.getCurrentValue(ctx, key); err == nil {
		select {
		case updates <- val:
		default:
		}
	}

	cleanup := func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		if watchers, ok := m.watchers.Load(key); ok {
			watcherList := watchers.([]chan int64)
			for i, ch := range watcherList {
				if ch == updates {
					watcherList = append(watcherList[:i], watcherList[i+1:]...)
					break
				}
			}
			if len(watcherList) == 0 {
				m.watchers.Delete(key)
			} else {
				m.watchers.Store(key, watcherList)
			}
		}
		close(updates)
	}

	return updates, cleanup, nil
}

// Implement PublishCounterInt64 for memory connector
func (m *MemoryConnector) PublishCounterInt64(ctx context.Context, key string, value int64) error {
	m.mu.RLock()
	watchers, exists := m.watchers.Load(key)
	if !exists {
		m.mu.RUnlock()
		return nil
	}
	watcherList := watchers.([]chan int64)
	m.mu.RUnlock()

	// Notify all watchers
	for _, ch := range watcherList {
		select {
		case ch <- value:
		default: // Don't block if channel is full
		}
	}

	return nil
}

// Helper function to get current value
func (m *MemoryConnector) getCurrentValue(ctx context.Context, key string) (int64, error) {
	val, err := m.Get(ctx, ConnectorMainIndex, key, "value")
	if err != nil {
		if common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
			return 0, nil
		}
		return 0, err
	}
	return strconv.ParseInt(val, 10, 64)
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
