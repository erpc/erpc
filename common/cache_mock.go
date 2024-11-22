package common

import (
	"context"

	"github.com/stretchr/testify/mock"
)

type MockCacheDal struct {
	mock.Mock
}

func (m *MockCacheDal) Get(ctx context.Context, nrq *NormalizedRequest) (*NormalizedResponse, error) {
	args := m.Called(ctx, nrq)
	return args.Get(0).(*NormalizedResponse), args.Error(1)
}

func (m *MockCacheDal) Set(ctx context.Context, nrq *NormalizedRequest, nrs *NormalizedResponse) error {
	args := m.Called(ctx, nrq, nrs)
	return args.Error(0)
}

func (m *MockCacheDal) MethodConfig(method string) *CacheMethodConfig {
	cfg := CacheConfig{}
	cfg.SetDefaults()
	return cfg.Methods[method]
}

func (m *MockCacheDal) IsObjectNull() bool {
	return m == nil
}
