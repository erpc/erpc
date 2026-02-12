/* #nosec G404 */
package data

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/mock"
)

// MockConnector is a mock implementation of the Connector interface
type MockConnector struct {
	mock.Mock
	id string
}

var _ Connector = (*MockConnector)(nil)

func (m *MockConnector) Id() string {
	return m.id
}

// Get mocks the Get method of the Connector interface
func (m *MockConnector) Get(ctx context.Context, index, partitionKey, rangeKey string, metadata interface{}) ([]byte, error) {
	args := m.Called(ctx, index, partitionKey, rangeKey, metadata)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]byte), args.Error(1)
}

// Set mocks the Set method of the Connector interface
func (m *MockConnector) Set(ctx context.Context, partitionKey, rangeKey string, value []byte, ttl *time.Duration) error {
	args := m.Called(ctx, partitionKey, rangeKey, value, ttl)
	return args.Error(0)
}

// Delete mocks the Delete method of the Connector interface
func (m *MockConnector) Delete(ctx context.Context, partitionKey, rangeKey string) error {
	args := m.Called(ctx, partitionKey, rangeKey)
	return args.Error(0)
}

// List mocks the List method of the Connector interface
func (m *MockConnector) List(ctx context.Context, index string, limit int, paginationToken string) ([]KeyValuePair, string, error) {
	args := m.Called(ctx, index, limit, paginationToken)
	if args.Get(0) == nil {
		return nil, args.String(1), args.Error(2)
	}
	return args.Get(0).([]KeyValuePair), args.String(1), args.Error(2)
}

// Lock mocks the Lock method of the Connector interface
func (m *MockConnector) Lock(ctx context.Context, key string, ttl time.Duration) (lock DistributedLock, err error) {
	// Lock is frequently best-effort (background paths). If a test didn't stub the exact key,
	// avoid crashing the whole suite; treat as contention.
	defer func() {
		if r := recover(); r != nil {
			if s, ok := r.(string); ok && strings.Contains(s, "mock: Unexpected Method Call") {
				lock = nil
				err = ErrLockContention
				return
			}
			panic(r)
		}
	}()

	args := m.Called(ctx, key, ttl)
	lock, _ = args.Get(0).(DistributedLock)
	err = args.Error(1)
	return lock, err
}

// WatchCounterInt64 mocks the WatchCounterInt64 method of the Connector interface
func (m *MockConnector) WatchCounterInt64(ctx context.Context, key string) (<-chan CounterInt64State, func(), error) {
	args := m.Called(ctx, key)
	var a0 <-chan CounterInt64State = nil
	a0, _ = args.Get(0).(chan CounterInt64State)
	a1, _ := args.Get(1).(func())
	a2 := args.Error(2)
	return a0, a1, a2
}

// PublishCounterInt64 mocks the PublishCounterInt64 method of the Connector interface
func (m *MockConnector) PublishCounterInt64(ctx context.Context, key string, value CounterInt64State) error {
	// Publish is best-effort in most call sites (e.g., background propagation).
	// If a test doesn't care about publish behavior, avoid panicking on an unstubbed call.
	for _, c := range m.ExpectedCalls {
		if c.Method == "PublishCounterInt64" {
			args := m.Called(ctx, key, value)
			return args.Error(0)
		}
	}
	return nil
}

// NewMockConnector creates a new instance of MockConnector
func NewMockConnector(id string) *MockConnector {
	return &MockConnector{id: id}
}

// MockMemoryConnector extends MemoryConnector with a fake delay feature
type MockMemoryConnector struct {
	*MemoryConnector
	fakeGetDelay time.Duration
	fakeSetDelay time.Duration
	getErrorRate float64
	setErrorRate float64
}

// NewMockMemoryConnector creates a new MockMemoryConnector
func NewMockMemoryConnector(ctx context.Context, logger *zerolog.Logger, id string, cfg *common.MockConnectorConfig) (*MockMemoryConnector, error) {
	baseConnector, err := NewMemoryConnector(ctx, logger, id, &cfg.MemoryConnectorConfig)
	if err != nil {
		return nil, err
	}

	return &MockMemoryConnector{
		MemoryConnector: baseConnector,
		fakeGetDelay:    cfg.GetDelay,
		fakeSetDelay:    cfg.SetDelay,
		getErrorRate:    cfg.GetErrorRate,
		setErrorRate:    cfg.SetErrorRate,
	}, nil
}

// Set overrides the base Set method to include a fake delay
func (m *MockMemoryConnector) Set(ctx context.Context, partitionKey, rangeKey string, value []byte, ttl *time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(m.fakeSetDelay):
	}
	if rand.Float64() < m.setErrorRate || m.setErrorRate == 1 {
		return errors.New("fake random SET error")
	}
	return m.MemoryConnector.Set(ctx, partitionKey, rangeKey, value, ttl)
}

// Get overrides the base Get method to include a fake delay
func (m *MockMemoryConnector) Get(ctx context.Context, index, partitionKey, rangeKey string, _ interface{}) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(m.fakeGetDelay):
	}
	if rand.Float64() < m.getErrorRate || m.getErrorRate == 1 {
		return nil, errors.New("fake random GET error")
	}
	return m.MemoryConnector.Get(ctx, index, partitionKey, rangeKey, nil)
}
