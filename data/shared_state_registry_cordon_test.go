package data

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func init() { util.ConfigureTestLogger() }

func TestSetCordonState_PersistsEntry(t *testing.T) {
	registry, connector, ctx := setupTest("cluster1")

	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil)
	connector.On("Lock", mock.Anything, mock.Anything, mock.Anything).Return(lock, nil).Maybe()

	// no existing map
	connector.On("Get", mock.Anything, ConnectorMainIndex, "cluster1/cordon-map/proj1/ups1", "methods", mock.Anything).
		Return(nil, common.NewErrRecordNotFound("cluster1/cordon-map/proj1/ups1", "methods", "mock")).Once()

	var captured []byte
	connector.On("Set", mock.Anything, "cluster1/cordon-map/proj1/ups1", "methods", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { captured = args.Get(3).([]byte) }).
		Return(nil)

	// bump notify: Get returns not-found (counter not yet set)
	connector.On("Get", mock.Anything, ConnectorMainIndex, mock.MatchedBy(func(s string) bool {
		return s != "cluster1/cordon-map/proj1/ups1"
	}), mock.Anything, mock.Anything).Return(nil, common.NewErrRecordNotFound("", "", "mock")).Maybe()
	connector.On("WatchCounterInt64", mock.Anything, mock.Anything).Return(nil, nil, errors.New("no-op")).Maybe()
	connector.On("Set", mock.Anything, mock.MatchedBy(func(s string) bool {
		return s != "cluster1/cordon-map/proj1/ups1"
	}), mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	connector.On("PublishCounterInt64", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	entry := CordonStateEntry{Method: "eth_call", Reason: "test", CordonedAtMs: 1000}
	err := registry.SetCordonState(ctx, "proj1", "ups1", entry)
	assert.NoError(t, err)

	var m map[string]CordonStateEntry
	assert.NoError(t, json.Unmarshal(captured, &m))
	assert.Equal(t, entry, m["eth_call"])
}

func TestSetCordonState_LockFailure_ReturnsError(t *testing.T) {
	registry, connector, ctx := setupTest("cluster1")
	connector.On("Lock", mock.Anything, mock.Anything, mock.Anything).Return(nil, errors.New("redis down"))

	err := registry.SetCordonState(ctx, "proj1", "ups1", CordonStateEntry{Method: "eth_call"})
	assert.ErrorContains(t, err, "failed to acquire cordon lock")
}

func TestDeleteCordonState_RemovesEntry(t *testing.T) {
	registry, connector, ctx := setupTest("cluster1")

	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil)
	connector.On("Lock", mock.Anything, mock.Anything, mock.Anything).Return(lock, nil).Maybe()

	existing := map[string]CordonStateEntry{
		"eth_call":    {Method: "eth_call", Reason: "r1", CordonedAtMs: 500},
		"eth_getLogs": {Method: "eth_getLogs", Reason: "r2", CordonedAtMs: 600},
	}
	raw, _ := json.Marshal(existing)
	connector.On("Get", mock.Anything, ConnectorMainIndex, "cluster1/cordon-map/proj1/ups1", "methods", mock.Anything).
		Return(raw, nil).Once()

	var captured []byte
	connector.On("Set", mock.Anything, "cluster1/cordon-map/proj1/ups1", "methods", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { captured = args.Get(3).([]byte) }).
		Return(nil)

	connector.On("WatchCounterInt64", mock.Anything, mock.Anything).Return(nil, nil, errors.New("no-op")).Maybe()
	connector.On("Set", mock.Anything, mock.MatchedBy(func(s string) bool {
		return s != "cluster1/cordon-map/proj1/ups1"
	}), mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	connector.On("PublishCounterInt64", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	connector.On("Get", mock.Anything, ConnectorMainIndex, mock.MatchedBy(func(s string) bool {
		return s != "cluster1/cordon-map/proj1/ups1"
	}), mock.Anything, mock.Anything).Return(nil, common.NewErrRecordNotFound("", "", "mock")).Maybe()

	err := registry.DeleteCordonState(ctx, "proj1", "ups1", "eth_call")
	assert.NoError(t, err)

	var m map[string]CordonStateEntry
	assert.NoError(t, json.Unmarshal(captured, &m))
	assert.NotContains(t, m, "eth_call")
	assert.Contains(t, m, "eth_getLogs")
}

func TestDeleteCordonState_LastEntry_DeletesKey(t *testing.T) {
	registry, connector, ctx := setupTest("cluster1")

	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil)
	connector.On("Lock", mock.Anything, mock.Anything, mock.Anything).Return(lock, nil).Maybe()

	existing := map[string]CordonStateEntry{
		"eth_call": {Method: "eth_call", Reason: "r1", CordonedAtMs: 500},
	}
	raw, _ := json.Marshal(existing)
	connector.On("Get", mock.Anything, ConnectorMainIndex, "cluster1/cordon-map/proj1/ups1", "methods", mock.Anything).
		Return(raw, nil).Once()
	connector.On("Delete", mock.Anything, "cluster1/cordon-map/proj1/ups1", "methods").Return(nil)

	connector.On("WatchCounterInt64", mock.Anything, mock.Anything).Return(nil, nil, errors.New("no-op")).Maybe()
	connector.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	connector.On("PublishCounterInt64", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	connector.On("Get", mock.Anything, ConnectorMainIndex, mock.MatchedBy(func(s string) bool {
		return s != "cluster1/cordon-map/proj1/ups1"
	}), mock.Anything, mock.Anything).Return(nil, common.NewErrRecordNotFound("", "", "mock")).Maybe()

	err := registry.DeleteCordonState(ctx, "proj1", "ups1", "eth_call")
	assert.NoError(t, err)
	connector.AssertCalled(t, "Delete", mock.Anything, "cluster1/cordon-map/proj1/ups1", "methods")
}

func TestLoadCordonStates_ReturnsEntries(t *testing.T) {
	registry, connector, ctx := setupTest("cluster1")

	existing := map[string]CordonStateEntry{
		"eth_call":    {Method: "eth_call", Reason: "r1", CordonedAtMs: 100},
		"eth_getLogs": {Method: "eth_getLogs", Reason: "r2", CordonedAtMs: 200},
	}
	raw, _ := json.Marshal(existing)
	connector.On("Get", mock.Anything, ConnectorMainIndex, "cluster1/cordon-map/proj1/ups1", "methods", mock.Anything).
		Return(raw, nil)

	entries, err := registry.LoadCordonStates(ctx, "proj1", "ups1")
	assert.NoError(t, err)
	assert.Len(t, entries, 2)
}

func TestLoadCordonStates_NotFound_ReturnsEmpty(t *testing.T) {
	registry, connector, ctx := setupTest("cluster1")
	connector.On("Get", mock.Anything, ConnectorMainIndex, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, common.NewErrRecordNotFound("", "", "mock"))

	entries, err := registry.LoadCordonStates(context.Background(), "proj1", "ups1")
	assert.NoError(t, err)
	assert.Empty(t, entries)
	_ = ctx
}

func TestLoadCordonStates_UnmarshalError_ReturnsError(t *testing.T) {
	registry, connector, _ := setupTest("cluster1")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	connector.On("Get", mock.Anything, ConnectorMainIndex, mock.Anything, mock.Anything, mock.Anything).
		Return([]byte("not-json"), nil)

	_, err := registry.LoadCordonStates(ctx, "proj1", "ups1")
	assert.ErrorContains(t, err, "cordon map unmarshal failed")
}

func TestSetCordonState_MergesWithExisting(t *testing.T) {
	registry, connector, ctx := setupTest("cluster1")

	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil)
	connector.On("Lock", mock.Anything, mock.Anything, mock.Anything).Return(lock, nil).Maybe()

	existing := map[string]CordonStateEntry{
		"eth_getLogs": {Method: "eth_getLogs", Reason: "existing", CordonedAtMs: 50},
	}
	raw, _ := json.Marshal(existing)
	connector.On("Get", mock.Anything, ConnectorMainIndex, "cluster1/cordon-map/proj1/ups1", "methods", mock.Anything).
		Return(raw, nil).Once()

	var captured []byte
	connector.On("Set", mock.Anything, "cluster1/cordon-map/proj1/ups1", "methods", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { captured = args.Get(3).([]byte) }).
		Return(nil)

	connector.On("WatchCounterInt64", mock.Anything, mock.Anything).Return(nil, nil, errors.New("no-op")).Maybe()
	connector.On("Set", mock.Anything, mock.MatchedBy(func(s string) bool {
		return s != "cluster1/cordon-map/proj1/ups1"
	}), mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	connector.On("PublishCounterInt64", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	connector.On("Get", mock.Anything, ConnectorMainIndex, mock.MatchedBy(func(s string) bool {
		return s != "cluster1/cordon-map/proj1/ups1"
	}), mock.Anything, mock.Anything).Return(nil, common.NewErrRecordNotFound("", "", "mock")).Maybe()

	entry := CordonStateEntry{Method: "eth_call", Reason: "new", CordonedAtMs: 999}
	err := registry.SetCordonState(ctx, "proj1", "ups1", entry)
	assert.NoError(t, err)

	var m map[string]CordonStateEntry
	assert.NoError(t, json.Unmarshal(captured, &m))
	assert.Contains(t, m, "eth_getLogs")
	assert.Contains(t, m, "eth_call")
}

func TestCordonStateKeysEncodeIdentitySegments(t *testing.T) {
	registry, _, _ := setupTest("cluster1")

	assert.NotEqual(t,
		registry.cordonMapKey("a/b", "c"),
		registry.cordonMapKey("a", "b/c"),
		"project and upstream path separators must not collide",
	)
	assert.Equal(t, "cluster1/cordon-map/a%2Fb/c", registry.cordonMapKey("a/b", "c"))
	assert.Equal(t, "cordon-notify/a/b%2Fc", registry.cordonNotifyKey("a", "b/c"))
	assert.Equal(t, "cluster1/cordon-lock/a%2Fb/c", registry.cordonLockKey("a/b", "c"))
}

func TestLoadCordonStates_NullPayloadReturnsEmpty(t *testing.T) {
	registry, connector, ctx := setupTest("cluster1")
	connector.On("Get", mock.Anything, ConnectorMainIndex, mock.Anything, "methods", mock.Anything).
		Return([]byte("null"), nil)

	entries, err := registry.LoadCordonStates(ctx, "proj1", "ups1")
	assert.NoError(t, err)
	assert.Empty(t, entries)
}
