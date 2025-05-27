/* #nosec G404 */
package data

import (
	"context"
	"errors"
	"math/rand"
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
func (m *MockConnector) Get(ctx context.Context, index, partitionKey, rangeKey string) ([]byte, error) {
	args := m.Called(ctx, index, partitionKey, rangeKey)
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

// Lock mocks the Lock method of the Connector interface
func (m *MockConnector) Lock(ctx context.Context, key string, ttl time.Duration) (DistributedLock, error) {
	args := m.Called(ctx, key, ttl)
	var a0 DistributedLock = nil
	a0, _ = args.Get(0).(DistributedLock)
	a1 := args.Error(1)
	return a0, a1
}

// WatchCounterInt64 mocks the WatchCounterInt64 method of the Connector interface
func (m *MockConnector) WatchCounterInt64(ctx context.Context, key string) (<-chan int64, func(), error) {
	args := m.Called(ctx, key)
	var a0 <-chan int64 = nil
	a0, _ = args.Get(0).(chan int64)
	a1, _ := args.Get(1).(func())
	a2 := args.Error(2)
	return a0, a1, a2
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
func (m *MockMemoryConnector) Get(ctx context.Context, index, partitionKey, rangeKey string) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(m.fakeGetDelay):
	}
	if rand.Float64() < m.getErrorRate || m.getErrorRate == 1 {
		return nil, errors.New("fake random GET error")
	}
	return m.MemoryConnector.Get(ctx, index, partitionKey, rangeKey)
}
