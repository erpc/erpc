package data

import (
	"context"
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
func (m *MockConnector) Get(ctx context.Context, index, partitionKey, rangeKey string) (string, error) {
	args := m.Called(ctx, index, partitionKey, rangeKey)
	return args.String(0), args.Error(1)
}

// Set mocks the Set method of the Connector interface
func (m *MockConnector) Set(ctx context.Context, partitionKey, rangeKey, value string, ttl *time.Duration) error {
	args := m.Called(ctx, partitionKey, rangeKey, value, ttl)
	return args.Error(0)
}

// Lock mocks the Lock method of the Connector interface
func (m *MockConnector) Lock(ctx context.Context, key string, ttl time.Duration) (DistributedLock, error) {
	args := m.Called(ctx, key, ttl)
	return args.Get(0).(DistributedLock), args.Error(1)
}

// WatchCounterInt64 mocks the WatchCounterInt64 method of the Connector interface
func (m *MockConnector) WatchCounterInt64(ctx context.Context, key string) (<-chan int64, func(), error) {
	args := m.Called(ctx, key)
	return args.Get(0).(<-chan int64), args.Get(1).(func()), args.Error(2)
}

// PublishCounterInt64 mocks the PublishCounterInt64 method of the Connector interface
func (m *MockConnector) PublishCounterInt64(ctx context.Context, key string, value int64) error {
	args := m.Called(ctx, key, value)
	return args.Error(0)
}

// NewMockConnector creates a new instance of MockConnector
func NewMockConnector(id string) *MockConnector {
	return &MockConnector{id: id}
}

// MockMemoryConnector extends MemoryConnector with a fake delay feature
type MockMemoryConnector struct {
	*MemoryConnector
	fakeDelay time.Duration
}

// NewMockMemoryConnector creates a new MockMemoryConnector
func NewMockMemoryConnector(ctx context.Context, logger *zerolog.Logger, id string, cfg *common.MemoryConnectorConfig, fakeDelay time.Duration) (*MockMemoryConnector, error) {
	baseConnector, err := NewMemoryConnector(ctx, logger, id, cfg)
	if err != nil {
		return nil, err
	}

	return &MockMemoryConnector{
		MemoryConnector: baseConnector,
		fakeDelay:       fakeDelay,
	}, nil
}

// Set overrides the base Set method to include a fake delay
func (m *MockMemoryConnector) Set(ctx context.Context, partitionKey, rangeKey, value string, ttl *time.Duration) error {
	time.Sleep(m.fakeDelay)
	return m.MemoryConnector.Set(ctx, partitionKey, rangeKey, value, ttl)
}

// Get overrides the base Get method to include a fake delay
func (m *MockMemoryConnector) Get(ctx context.Context, index, partitionKey, rangeKey string) (string, error) {
	time.Sleep(m.fakeDelay)
	return m.MemoryConnector.Get(ctx, index, partitionKey, rangeKey)
}

// SetFakeDelay allows changing the fake delay dynamically
func (m *MockMemoryConnector) SetFakeDelay(delay time.Duration) {
	m.fakeDelay = delay
}
