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
}

// Get mocks the Get method of the Connector interface
func (m *MockConnector) Get(ctx context.Context, index, partitionKey, rangeKey string) (string, error) {
	args := m.Called(ctx, index, partitionKey, rangeKey)
	return args.String(0), args.Error(1)
}

// Set mocks the Set method of the Connector interface
func (m *MockConnector) Set(ctx context.Context, partitionKey, rangeKey, value string) error {
	args := m.Called(ctx, partitionKey, rangeKey, value)
	return args.Error(0)
}

// Delete mocks the Delete method of the Connector interface
func (m *MockConnector) Delete(ctx context.Context, index, partitionKey, rangeKey string) error {
	args := m.Called(ctx, index, partitionKey, rangeKey)
	return args.Error(0)
}

// SetTTL mocks the SetTTL method of the Connector interface
func (m *MockConnector) SetTTL(method string, ttlStr string) error {
	args := m.Called(method, ttlStr)
	return args.Error(0)
}

// HasTTL mocks the HasTTL method of the Connector interface
func (m *MockConnector) HasTTL(method string) bool {
	args := m.Called(method)
	return args.Bool(0)
}

// NewMockConnector creates a new instance of MockConnector
func NewMockConnector() *MockConnector {
	return &MockConnector{}
}

// MockMemoryConnector extends MemoryConnector with a fake delay feature
type MockMemoryConnector struct {
	MemoryConnector
	fakeDelay time.Duration
}

// NewMockMemoryConnector creates a new MockMemoryConnector
func NewMockMemoryConnector(ctx context.Context, logger *zerolog.Logger, cfg *common.MemoryConnectorConfig, fakeDelay time.Duration) (*MockMemoryConnector, error) {
	baseConnector, err := NewMemoryConnector(ctx, logger, cfg)
	if err != nil {
		return nil, err
	}

	return &MockMemoryConnector{
		MemoryConnector: *baseConnector,
		fakeDelay:       fakeDelay,
	}, nil
}

// Set overrides the base Set method to include a fake delay
func (m *MockMemoryConnector) Set(ctx context.Context, partitionKey, rangeKey, value string) error {
	time.Sleep(m.fakeDelay)
	return m.MemoryConnector.Set(ctx, partitionKey, rangeKey, value)
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
