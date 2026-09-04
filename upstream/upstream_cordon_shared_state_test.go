package upstream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

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
		ProjectId:              "proj1",
		config:                 cfg,
		logger:                 &logger,
		metricsTracker:         tracker,
		sharedStateRegistry:    ssr,
		appCtx:                 context.Background(),
		pendingCordonMutations: make(map[string]*data.CordonStateEntry),
		cordonReconcileCh:      make(chan struct{}, 1),
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

func TestCordonAdmin_SerializesWithOlderReconcile(t *testing.T) {
	ssr := &mockSharedStateRegistry{}
	loadStarted := make(chan struct{})
	releaseLoad := make(chan struct{})
	ssr.On("LoadCordonStates", mock.Anything, "proj1", "ups1").
		Run(func(mock.Arguments) {
			close(loadStarted)
			<-releaseLoad
		}).
		Return([]data.CordonStateEntry{}, nil).
		Once()
	ssr.On("SetCordonState", mock.Anything, "proj1", "ups1", mock.Anything).Return(nil).Once()

	u := newTestUpstream(t, ssr)
	reconcileDone := make(chan struct{})
	go func() {
		u.reconcileCordonState(context.Background())
		close(reconcileDone)
	}()
	<-loadStarted

	cordonDone := make(chan error, 1)
	go func() { cordonDone <- u.CordonAdmin("eth_call", "incident") }()
	select {
	case err := <-cordonDone:
		t.Fatalf("cordon mutation bypassed in-flight reconciliation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseLoad)
	<-reconcileDone
	require.NoError(t, <-cordonDone)
	assert.True(t, u.metricsTracker.IsCordoned(u, "eth_call"))
}

func TestCordonAdmin_PersistenceFailureRemainsPendingUntilRetry(t *testing.T) {
	ssr := &mockSharedStateRegistry{}
	ssr.On("SetCordonState", mock.Anything, "proj1", "ups1", mock.Anything).
		Return(errors.New("shared state unavailable")).Once()
	ssr.On("SetCordonState", mock.Anything, "proj1", "ups1", mock.Anything).
		Return(nil).Once()
	ssr.On("LoadCordonStates", mock.Anything, "proj1", "ups1").
		Return([]data.CordonStateEntry{{Method: "eth_call", Reason: "incident", CordonedAtMs: 1}}, nil).Once()

	u := newTestUpstream(t, ssr)
	require.Error(t, u.CordonAdmin("eth_call", "incident"))
	_, pending := u.pendingCordonMutations["eth_call"]
	require.True(t, pending)

	u.reconcileCordonState(context.Background())

	assert.True(t, u.metricsTracker.IsCordoned(u, "eth_call"))
	assert.Empty(t, u.pendingCordonMutations)
	ssr.AssertExpectations(t)
}

func TestUncordonAdmin_PersistenceFailureRemainsPendingUntilRetry(t *testing.T) {
	ssr := &mockSharedStateRegistry{}
	ssr.On("SetCordonState", mock.Anything, "proj1", "ups1", mock.Anything).Return(nil).Once()
	ssr.On("DeleteCordonState", mock.Anything, "proj1", "ups1", "eth_call").
		Return(errors.New("shared state unavailable")).Once()
	ssr.On("DeleteCordonState", mock.Anything, "proj1", "ups1", "eth_call").Return(nil).Once()
	ssr.On("LoadCordonStates", mock.Anything, "proj1", "ups1").
		Return([]data.CordonStateEntry{}, nil).Once()

	u := newTestUpstream(t, ssr)
	require.NoError(t, u.CordonAdmin("eth_call", "incident"))
	require.Error(t, u.UncordonAdmin("eth_call", "resolved"))
	entry, pending := u.pendingCordonMutations["eth_call"]
	require.True(t, pending)
	assert.Nil(t, entry)

	u.reconcileCordonState(context.Background())

	assert.False(t, u.metricsTracker.IsCordoned(u, "eth_call"))
	assert.Empty(t, u.pendingCordonMutations)
	ssr.AssertExpectations(t)
}

func TestCordonAdmin_RepeatedCallPersistsOriginalTimestamp(t *testing.T) {
	ssr := &mockSharedStateRegistry{}
	timestamps := make([]int64, 0, 2)
	ssr.On("SetCordonState", mock.Anything, "proj1", "ups1", mock.Anything).
		Run(func(args mock.Arguments) {
			timestamps = append(timestamps, args.Get(3).(data.CordonStateEntry).CordonedAtMs)
		}).
		Return(nil).
		Twice()

	u := newTestUpstream(t, ssr)
	require.NoError(t, u.CordonAdmin("eth_call", "first"))
	time.Sleep(2 * time.Millisecond)
	require.NoError(t, u.CordonAdmin("eth_call", "updated"))

	require.Len(t, timestamps, 2)
	assert.Equal(t, timestamps[0], timestamps[1])
}

func TestCordonReconcileInitialDelayWithinFallbackBound(t *testing.T) {
	u := newTestUpstream(t, nil)
	delay := u.cordonReconcileInitialDelay()
	assert.GreaterOrEqual(t, delay, time.Second)
	assert.Less(t, delay, cordonReconcileInterval)
}
