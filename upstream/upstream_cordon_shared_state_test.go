package upstream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// mockSharedStateRegistry is a minimal mock for data.SharedStateRegistry.
type mockSharedStateRegistry struct {
	mock.Mock
}

func (m *mockSharedStateRegistry) GetCounterInt64(key string, ignoreRollbackOf int64) data.CounterInt64SharedVariable {
	args := m.Called(key, ignoreRollbackOf)
	v, _ := args.Get(0).(data.CounterInt64SharedVariable)
	return v
}
func (m *mockSharedStateRegistry) GetLockTtl() time.Duration         { return time.Second }
func (m *mockSharedStateRegistry) GetFallbackTimeout() time.Duration { return time.Second }
func (m *mockSharedStateRegistry) IsRemote() bool                    { return true }
func (m *mockSharedStateRegistry) SetCordonState(ctx context.Context, projectId, upstreamId string, entry data.CordonStateEntry) error {
	return m.Called(ctx, projectId, upstreamId, entry).Error(0)
}
func (m *mockSharedStateRegistry) DeleteCordonState(ctx context.Context, projectId, upstreamId, method string) error {
	return m.Called(ctx, projectId, upstreamId, method).Error(0)
}
func (m *mockSharedStateRegistry) LoadCordonStates(ctx context.Context, projectId, upstreamId string) ([]data.CordonStateEntry, error) {
	args := m.Called(ctx, projectId, upstreamId)
	entries, _ := args.Get(0).([]data.CordonStateEntry)
	return entries, args.Error(1)
}
func (m *mockSharedStateRegistry) WatchCordonNotifications(projectId, upstreamId string) data.CounterInt64SharedVariable {
	args := m.Called(projectId, upstreamId)
	v, _ := args.Get(0).(data.CounterInt64SharedVariable)
	return v
}

func newTestUpstream(t *testing.T, ssr data.SharedStateRegistry) *Upstream {
	t.Helper()
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "proj1", time.Minute)
	cfg := &common.UpstreamConfig{Id: "ups1", Endpoint: "http://localhost"}
	u := &Upstream{
		ProjectId:           "proj1",
		config:              cfg,
		logger:              &logger,
		metricsTracker:      tracker,
		sharedStateRegistry: ssr,
		appCtx:              context.Background(),
	}
	return u
}

func TestCordonAdmin_PersistsAndCordons(t *testing.T) {
	ssr := &mockSharedStateRegistry{}
	ssr.On("SetCordonState", mock.Anything, "proj1", "ups1", mock.MatchedBy(func(e data.CordonStateEntry) bool {
		return e.Method == "eth_call" && e.Reason == "test"
	})).Return(nil)

	u := newTestUpstream(t, ssr)
	err := u.CordonAdmin("eth_call", "test")
	assert.NoError(t, err)
	assert.True(t, u.metricsTracker.IsCordoned(u, "eth_call"))
	_, ok := u.adminCordonedMethods.Load("eth_call")
	assert.True(t, ok)
	ssr.AssertExpectations(t)
}

func TestCordonAdmin_SharedStateFailure_ReturnsError(t *testing.T) {
	ssr := &mockSharedStateRegistry{}
	ssr.On("SetCordonState", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(errors.New("redis down"))

	u := newTestUpstream(t, ssr)
	err := u.CordonAdmin("eth_call", "test")
	assert.ErrorContains(t, err, "redis down")
	// in-memory cordon applied despite error
	assert.True(t, u.metricsTracker.IsCordoned(u, "eth_call"))
}

func TestUncordonAdmin_RemovesAndUncordons(t *testing.T) {
	ssr := &mockSharedStateRegistry{}
	ssr.On("SetCordonState", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	ssr.On("DeleteCordonState", mock.Anything, "proj1", "ups1", "eth_call").Return(nil)

	u := newTestUpstream(t, ssr)
	_ = u.CordonAdmin("eth_call", "test")
	err := u.UncordonAdmin("eth_call", "admin uncordon")
	assert.NoError(t, err)
	assert.False(t, u.metricsTracker.IsCordoned(u, "eth_call"))
	_, ok := u.adminCordonedMethods.Load("eth_call")
	assert.False(t, ok)
	ssr.AssertExpectations(t)
}

func TestReconcileCordonState_AppliesRemote(t *testing.T) {
	ssr := &mockSharedStateRegistry{}
	ts := time.Now().Add(-5 * time.Minute).UnixMilli()
	ssr.On("LoadCordonStates", mock.Anything, "proj1", "ups1").Return([]data.CordonStateEntry{
		{Method: "eth_call", Reason: "incident", CordonedAtMs: ts},
	}, nil)

	u := newTestUpstream(t, ssr)
	u.reconcileCordonState(context.Background())

	assert.True(t, u.metricsTracker.IsCordoned(u, "eth_call"))
	_, ok := u.adminCordonedMethods.Load("eth_call")
	assert.True(t, ok)
}

func TestReconcileCordonState_UncordonsAdminOnly(t *testing.T) {
	ssr := &mockSharedStateRegistry{}
	// Remote has only eth_getLogs now; eth_call was removed remotely.
	ssr.On("LoadCordonStates", mock.Anything, "proj1", "ups1").Return([]data.CordonStateEntry{
		{Method: "eth_getLogs", Reason: "r", CordonedAtMs: 100},
	}, nil)

	u := newTestUpstream(t, ssr)
	// Simulate eth_call having been admin-cordoned on this replica previously.
	u.metricsTracker.CordonAdmin(u, "eth_call", "admin: manual cordon")
	u.adminCordonedMethods.Store("eth_call", struct{}{})
	// Simulate an automatic cordon for eth_getBalance (should NOT be uncordoned by reconcile).
	u.metricsTracker.Cordon(u, "eth_getBalance", "consensus sit-out")

	u.reconcileCordonState(context.Background())

	// Admin cordon for removed method should be cleared.
	assert.False(t, u.metricsTracker.IsCordoned(u, "eth_call"))
	// Automatic cordon must survive reconciliation.
	assert.True(t, u.metricsTracker.IsCordoned(u, "eth_getBalance"))
	// New remote entry applied.
	assert.True(t, u.metricsTracker.IsCordoned(u, "eth_getLogs"))
}

func TestReconcileCordonState_LoadError_IsNoop(t *testing.T) {
	ssr := &mockSharedStateRegistry{}
	ssr.On("LoadCordonStates", mock.Anything, mock.Anything, mock.Anything).
		Return(nil, errors.New("redis down"))

	u := newTestUpstream(t, ssr)
	// Should not panic and should leave state unchanged.
	u.reconcileCordonState(context.Background())
}
