package data

import (
	"context"

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

// NewMockConnector creates a new instance of MockConnector
func NewMockConnector() *MockConnector {
	return &MockConnector{}
}
