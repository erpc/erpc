package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/jackc/pgconn"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeConnector is a minimal data.Connector implementation that captures
// Get call counts and returns programmable results. We only need to mock
// Get for the auth strategy tests — every other interface method panics so
// accidental usage is loud.
type fakeConnector struct {
	id        string
	getCalls  atomic.Int64
	getResult func() ([]byte, error) // closure so tests can flip behavior over time
}

func (f *fakeConnector) Id() string { return f.id }

func (f *fakeConnector) Get(ctx context.Context, index, partitionKey, rangeKey string, _ interface{}) ([]byte, error) {
	f.getCalls.Add(1)
	if f.getResult == nil {
		return nil, errors.New("fakeConnector: no getResult configured")
	}
	return f.getResult()
}

func (f *fakeConnector) Set(_ context.Context, _, _ string, _ []byte, _ *time.Duration) error {
	panic("fakeConnector.Set should not be called from auth tests")
}
func (f *fakeConnector) Delete(_ context.Context, _, _ string) error {
	panic("fakeConnector.Delete should not be called from auth tests")
}
func (f *fakeConnector) List(_ context.Context, _ string, _ int, _ string) ([]data.KeyValuePair, string, error) {
	panic("fakeConnector.List should not be called from auth tests")
}
func (f *fakeConnector) Lock(_ context.Context, _ string, _ time.Duration) (data.DistributedLock, error) {
	panic("fakeConnector.Lock should not be called from auth tests")
}
func (f *fakeConnector) WatchCounterInt64(_ context.Context, _ string) (<-chan data.CounterInt64State, func(), error) {
	panic("fakeConnector.WatchCounterInt64 should not be called from auth tests")
}
func (f *fakeConnector) PublishCounterInt64(_ context.Context, _ string, _ data.CounterInt64State) error {
	panic("fakeConnector.PublishCounterInt64 should not be called from auth tests")
}

// newTestStrategyWith builds a DatabaseStrategy wired to a fakeConnector
// and the provided fail-open + retry config. Cache is left nil to keep the
// tests focused on the connector → fail-open code path.
func newTestStrategyWith(t *testing.T, fc *fakeConnector, failOpenEnabled bool) *DatabaseStrategy {
	t.Helper()
	logger := zerolog.Nop()
	cfg := &common.DatabaseStrategyConfig{
		Connector: &common.ConnectorConfig{Id: "test-db", Driver: "postgresql"},
		FailOpen: &common.DatabaseFailOpenConfig{
			Enabled:         failOpenEnabled,
			UserId:          "emergency-failopen",
			RateLimitBudget: "emergency",
		},
	}
	return &DatabaseStrategy{
		logger:    &logger,
		cfg:       cfg,
		connector: fc,
	}
}

// TestClassifyDbError pins down the bounded set of telemetry labels.
//
// The new "db_not_ready" label is the operational signal that distinguishes
// "our PostgreSQLConnector is mid-reconnect — wait and retry" from
// "pgbouncer/postgres is actually unreachable — call ops". Before
// 2026-05-13 both rolled up into "db_connection", which made the reconnect
// cascade look identical to a real outage on the dashboard.
func TestClassifyDbError(t *testing.T) {
	t.Parallel()

	s := &DatabaseStrategy{}

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil",
			err:  nil,
			want: "db_query_error",
		},

		// --- db_not_ready: our own connector signalling mid-reconnect ---
		{
			name: "ErrConnectorNotReady direct",
			err:  data.ErrConnectorNotReady,
			want: "db_not_ready",
		},
		{
			name: "ErrConnectorNotReady wrapped",
			err:  fmt.Errorf("auth get failed: %w", data.ErrConnectorNotReady),
			want: "db_not_ready",
		},

		// --- db_timeout ---
		{
			name: "context deadline exceeded",
			err:  context.DeadlineExceeded,
			want: "db_timeout",
		},
		{
			name: "wrapped deadline exceeded",
			err:  fmt.Errorf("query: %w", context.DeadlineExceeded),
			want: "db_timeout",
		},
		{
			name: "substring: timeout",
			err:  errors.New("operation timeout: server did not respond"),
			want: "db_timeout",
		},

		// --- db_connection: real transport failures ---
		{
			name: "substring: connection refused",
			err:  errors.New("dial tcp: connection refused"),
			want: "db_connection",
		},
		{
			name: "substring: connection reset",
			err:  errors.New("write tcp: connection reset by peer"),
			want: "db_connection",
		},
		{
			name: "substring: broken pipe",
			err:  errors.New("write: broken pipe"),
			want: "db_connection",
		},
		{
			name: "substring: EOF",
			err:  io.EOF,
			want: "db_connection",
		},
		{
			name: "syscall ECONNREFUSED wrapped — error string contains 'connection refused'",
			err:  fmt.Errorf("dial: %w", syscall.ECONNREFUSED),
			want: "db_connection",
		},

		// --- db_query_error: everything else (regression guard) ---
		{
			name: "pg error: too many connections (53300) is NOT db_connection",
			err:  &pgconn.PgError{Code: "53300", Message: "too many connections for role"},
			want: "db_query_error",
		},
		{
			name: "pg error: syntax error",
			err:  &pgconn.PgError{Code: "42601", Message: "syntax error"},
			want: "db_query_error",
		},
		{
			name: "generic error mentioning 'connection' without specific fragment is NOT db_connection",
			err:  errors.New("connection pool acquired in caller code"),
			want: "db_query_error",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := s.classifyDbError(tt.err)
			assert.Equal(t, tt.want, got, "classifyDbError(%v)", tt.err)
		})
	}
}

// TestIsDownSignal verifies the predicate used by Authenticate to decide
// whether a Get failure should flip the connectorDown latch. False positives
// here (flipping for query errors that won't help by fail-open) waste auth
// requests; false negatives leave us in the per-request Error-log path that
// triggered the 2026-05-13 cascade.
func TestIsDownSignal(t *testing.T) {
	t.Parallel()
	s := &DatabaseStrategy{}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"record not found", common.NewErrRecordNotFound("p", "r", "postgresql"), false},
		{"parse error", errors.New("invalid JSON"), false},
		{"pg syntax error 42601", &pgconn.PgError{Code: "42601", Message: "syntax error"}, false},
		{"pg too many connections 53300", &pgconn.PgError{Code: "53300", Message: "too many connections"}, false},

		{"ErrConnectorNotReady", data.ErrConnectorNotReady, true},
		{"wrapped ErrConnectorNotReady", fmt.Errorf("auth: %w", data.ErrConnectorNotReady), true},
		{"context deadline exceeded", context.DeadlineExceeded, true},
		{"io.EOF", io.EOF, true},
		{"connection refused", errors.New("dial tcp: connection refused"), true},
		{"econnrefused wrapped", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, s.isDownSignal(tc.err))
		})
	}
}

// TestMarkConnectorDownUp_Idempotent verifies that repeated calls only
// trigger a single transition (the CompareAndSwap guard works), so log/
// metric volume during a sustained outage stays bounded regardless of
// concurrent request count.
func TestMarkConnectorDownUp_Idempotent(t *testing.T) {
	t.Parallel()
	fc := &fakeConnector{id: "test"}
	s := newTestStrategyWith(t, fc, true)

	assert.False(t, s.connectorDown.Load(), "initial state should be up")

	// Simulate 100 concurrent "DB failed" handlers all racing to mark down.
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.markConnectorDown()
		}()
	}
	wg.Wait()

	assert.True(t, s.connectorDown.Load(), "should be down after concurrent marks")
	tsAfterDown := s.connectorDownSince.Load()
	assert.NotZero(t, tsAfterDown, "downSince should be populated")

	// Another wave of markConnectorDown must not move the timestamp —
	// otherwise the probe interval would slide forward forever during a
	// long outage and we'd never re-attempt the real DB path.
	for i := 0; i < 100; i++ {
		s.markConnectorDown()
	}
	assert.Equal(t, tsAfterDown, s.connectorDownSince.Load(),
		"downSince must not be overwritten while already down")

	// 100 concurrent recoveries — exactly one transition.
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.markConnectorUp()
		}()
	}
	wg.Wait()
	assert.False(t, s.connectorDown.Load(), "should be up after concurrent marks")
}

// TestTryFastFailOpen_RespectsFailOpenConfig verifies that when fail-open
// is not configured, we never fast-path — strict-auth semantics are
// preserved even if connectorDown is flipped by an earlier failure.
func TestTryFastFailOpen_RespectsFailOpenConfig(t *testing.T) {
	t.Parallel()
	fc := &fakeConnector{id: "test"}
	s := newTestStrategyWith(t, fc, false /* failOpenEnabled */)
	s.markConnectorDown()
	assert.Nil(t, s.tryFastFailOpen(),
		"must not fast-path when fail-open is disabled — caller must still run real DB path")
}

// TestTryFastFailOpen_HealthyConnector verifies that with fail-open enabled
// but connector healthy, we return nil (normal DB path).
func TestTryFastFailOpen_HealthyConnector(t *testing.T) {
	t.Parallel()
	fc := &fakeConnector{id: "test"}
	s := newTestStrategyWith(t, fc, true)
	assert.False(t, s.connectorDown.Load())
	assert.Nil(t, s.tryFastFailOpen(),
		"must not fast-path while connector is healthy")
}

// TestTryFastFailOpen_DownProbeOnePerInterval is the core load-shedding
// test. It asserts that across N concurrent callers while connectorDown is
// latched, exactly ONE is elected as the probe (returns nil → real DB
// path) per probe interval; everyone else gets the fast-path emergency
// user. This is what bounds per-request DB load during a sustained
// outage to ~1 query/sec instead of full request rate.
func TestTryFastFailOpen_DownProbeOnePerInterval(t *testing.T) {
	t.Parallel()
	fc := &fakeConnector{id: "test"}
	s := newTestStrategyWith(t, fc, true)

	// Latch down and force the timestamp far in the past so every caller
	// sees the probe window as expired.
	s.markConnectorDown()
	s.connectorDownSince.Store(time.Now().Add(-1 * time.Hour).UnixNano())

	var probes atomic.Int64
	var fastPathed atomic.Int64

	const concurrency = 200
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if u := s.tryFastFailOpen(); u != nil {
				fastPathed.Add(1)
			} else {
				probes.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.Equal(t, int64(1), probes.Load(),
		"exactly one caller must be elected as the probe per interval; got %d", probes.Load())
	assert.Equal(t, int64(concurrency-1), fastPathed.Load(),
		"all other callers must fast-path; got %d", fastPathed.Load())
}

// TestTryFastFailOpen_DownWithinWindow verifies that during the cooldown
// window (downSince fresh), ALL callers fast-path — no probes are elected
// until probeInterval elapses since the down transition.
func TestTryFastFailOpen_DownWithinWindow(t *testing.T) {
	t.Parallel()
	fc := &fakeConnector{id: "test"}
	s := newTestStrategyWith(t, fc, true)

	s.markConnectorDown()
	// downSince is set by markConnectorDown to time.Now(), so we're well
	// inside the probe interval.

	for i := 0; i < 50; i++ {
		u := s.tryFastFailOpen()
		require.NotNil(t, u, "every caller within the probe window must fast-path; iter=%d", i)
		assert.Equal(t, "emergency-failopen", u.Id)
	}
}

// TestGetWithRetries_SkipsRetryOnNotReady verifies the retry loop aborts
// immediately on data.ErrConnectorNotReady. Retrying during a known
// reconnect just burns the auth request's deadline without helping
// recovery — the initializer's auto-retry loop is the only thing that
// fixes the connector.
func TestGetWithRetries_SkipsRetryOnNotReady(t *testing.T) {
	t.Parallel()

	fc := &fakeConnector{
		id: "test",
		getResult: func() ([]byte, error) {
			return nil, data.ErrConnectorNotReady
		},
	}
	logger := zerolog.Nop()
	bb := common.Duration(50 * time.Millisecond)
	s := &DatabaseStrategy{
		logger:    &logger,
		connector: fc,
		cfg: &common.DatabaseStrategyConfig{
			Connector: &common.ConnectorConfig{Id: "test", Driver: "postgresql"},
			Retry: &common.DatabaseRetryConfig{
				MaxAttempts: 5,
				BaseBackoff: bb,
			},
		},
	}

	start := time.Now()
	_, err := s.getWithRetries(context.Background(), data.ConnectorMainIndex, "k", "*")
	elapsed := time.Since(start)

	assert.True(t, errors.Is(err, data.ErrConnectorNotReady),
		"should return ErrConnectorNotReady unchanged, got %v", err)
	assert.Equal(t, int64(1), fc.getCalls.Load(),
		"must only call Get once on ErrConnectorNotReady; got %d", fc.getCalls.Load())
	assert.Less(t, elapsed, 50*time.Millisecond,
		"must not sleep through the retry backoff; took %v", elapsed)
}

// TestGetWithRetries_RetriesOnOtherErrors verifies the no-retry-on-not-ready
// optimization didn't accidentally short-circuit the legitimate retry path
// for other transient errors.
func TestGetWithRetries_RetriesOnOtherErrors(t *testing.T) {
	t.Parallel()

	fc := &fakeConnector{
		id: "test",
		getResult: func() ([]byte, error) {
			return nil, errors.New("connection reset by peer")
		},
	}
	logger := zerolog.Nop()
	bb := common.Duration(1 * time.Millisecond)
	s := &DatabaseStrategy{
		logger:    &logger,
		connector: fc,
		cfg: &common.DatabaseStrategyConfig{
			Connector: &common.ConnectorConfig{Id: "test", Driver: "postgresql"},
			Retry: &common.DatabaseRetryConfig{
				MaxAttempts: 3,
				BaseBackoff: bb,
			},
		},
	}

	_, err := s.getWithRetries(context.Background(), data.ConnectorMainIndex, "k", "*")
	assert.Error(t, err)
	assert.Equal(t, int64(3), fc.getCalls.Load(),
		"must retry up to MaxAttempts for non-not-ready errors")
}

// TestAuthenticate_FastPathDuringOutage is the end-to-end regression guard
// for the 2026-05-13 cascade. Once the connector is observed to be down,
// subsequent requests must serve the emergency user WITHOUT calling Get
// (which is what generated the Error-log fan-out that contended on the
// stdout fd lock).
func TestAuthenticate_FastPathDuringOutage(t *testing.T) {
	t.Parallel()

	fc := &fakeConnector{
		id: "test",
		getResult: func() ([]byte, error) {
			return nil, data.ErrConnectorNotReady
		},
	}
	s := newTestStrategyWith(t, fc, true)

	ap := &AuthPayload{Type: common.AuthTypeSecret, Secret: &SecretPayload{Value: "k1"}}

	// First request: connectorDown is false, so we go through the real
	// path → Get fails with ErrConnectorNotReady → markConnectorDown is
	// called → fail-open user is returned.
	u, err := s.Authenticate(context.Background(), nil, ap)
	require.NoError(t, err)
	require.NotNil(t, u)
	assert.Equal(t, "emergency-failopen", u.Id)
	assert.Equal(t, int64(1), fc.getCalls.Load())
	assert.True(t, s.connectorDown.Load(), "first failure must latch connectorDown")

	// Subsequent requests within the probe interval must fast-path —
	// connector.Get must NOT be invoked.
	apiKeys := []string{"k1", "k2", "k3", "different-key", "another"}
	for _, k := range apiKeys {
		ap.Secret.Value = k
		u, err := s.Authenticate(context.Background(), nil, ap)
		require.NoError(t, err)
		require.NotNil(t, u)
		assert.Equal(t, "emergency-failopen", u.Id)
	}
	assert.Equal(t, int64(1), fc.getCalls.Load(),
		"fast-path must NOT invoke connector.Get for subsequent requests; got %d Get calls (expected 1 from the first request)",
		fc.getCalls.Load())
}

// TestAuthenticate_RecoveryClearsConnectorDown verifies that once the DB
// is healthy again, a successful query clears the latch and subsequent
// requests resume normal flow (no fast-path, real Get for each).
func TestAuthenticate_RecoveryClearsConnectorDown(t *testing.T) {
	t.Parallel()

	var alive atomic.Bool
	fc := &fakeConnector{
		id: "test",
		getResult: func() ([]byte, error) {
			if !alive.Load() {
				return nil, data.ErrConnectorNotReady
			}
			return []byte(`{"userId":"real-user","enabled":true}`), nil
		},
	}
	s := newTestStrategyWith(t, fc, true)
	ap := &AuthPayload{Type: common.AuthTypeSecret, Secret: &SecretPayload{Value: "k1"}}

	// Trip the down latch.
	_, err := s.Authenticate(context.Background(), nil, ap)
	require.NoError(t, err)
	require.True(t, s.connectorDown.Load())

	// Make the connector "recover" and force the probe window expired so the
	// next caller is elected as the probe.
	alive.Store(true)
	s.connectorDownSince.Store(time.Now().Add(-1 * time.Hour).UnixNano())

	// One caller will probe and succeed → markConnectorUp clears the latch.
	u, err := s.Authenticate(context.Background(), nil, ap)
	require.NoError(t, err)
	require.NotNil(t, u)
	assert.Equal(t, "real-user", u.Id, "probe must return the real user, not emergency")
	assert.False(t, s.connectorDown.Load(), "success must clear connectorDown latch")

	// Subsequent requests now hit the DB directly (no fast-path).
	callsBefore := fc.getCalls.Load()
	_, err = s.Authenticate(context.Background(), nil, ap)
	require.NoError(t, err)
	assert.Greater(t, fc.getCalls.Load(), callsBefore,
		"normal flow must invoke connector.Get after recovery")
}
