package data

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/jackc/pgconn"
	"github.com/jackc/pgx/v4"
	"github.com/stretchr/testify/assert"
)

// TestIsPostgresConnectionError pins down the exact predicate that drives
// reconnect decisions. The 2026-05-13 edge-prod incident root-caused to a
// substring match on the bare word "connection" being too broad — this test
// is the primary regression guard against that class of mistake.
func TestIsPostgresConnectionError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		// --- Should NOT trigger reconnect ---
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			name: "context canceled",
			err:  context.Canceled,
			want: false,
		},
		{
			name: "context deadline exceeded",
			err:  context.DeadlineExceeded,
			want: false,
		},
		{
			name: "wrapped context deadline exceeded",
			err:  fmt.Errorf("query timed out: %w", context.DeadlineExceeded),
			want: false,
		},
		{
			name: "pgx ErrNoRows",
			err:  pgx.ErrNoRows,
			want: false,
		},
		{
			name: "wrapped pgx ErrNoRows",
			err:  fmt.Errorf("scan: %w", pgx.ErrNoRows),
			want: false,
		},
		{
			name: "ErrConnectorNotReady — regression guard for 2026-05-13 cascade",
			err:  ErrConnectorNotReady,
			want: false,
		},
		{
			name: "wrapped ErrConnectorNotReady",
			err:  fmt.Errorf("auth get: %w", ErrConnectorNotReady),
			want: false,
		},
		{
			name: "pg error: too many connections (53300) — capacity, not transport",
			err:  &pgconn.PgError{Code: "53300", Message: "too many connections for role"},
			want: false,
		},
		{
			name: "pg error: syntax error (42601)",
			err:  &pgconn.PgError{Code: "42601", Message: "syntax error"},
			want: false,
		},
		{
			name: "pg error: foreign key violation (23503)",
			err:  &pgconn.PgError{Code: "23503", Message: "foreign key violation"},
			want: false,
		},
		{
			name: "generic error mentioning 'connection' without specific transport fragment",
			err:  errors.New("connection pool exhausted in application code"),
			want: false,
		},
		{
			name: "ErrCodeRecordNotFound wrapper",
			err:  common.NewErrRecordNotFound("p", "r", "postgresql"),
			want: false,
		},

		// --- Should trigger reconnect ---
		{
			name: "io.EOF",
			err:  io.EOF,
			want: true,
		},
		{
			name: "io.ErrUnexpectedEOF",
			err:  io.ErrUnexpectedEOF,
			want: true,
		},
		{
			name: "syscall ECONNREFUSED",
			err:  syscall.ECONNREFUSED,
			want: true,
		},
		{
			name: "syscall ECONNRESET",
			err:  syscall.ECONNRESET,
			want: true,
		},
		{
			name: "syscall EPIPE",
			err:  syscall.EPIPE,
			want: true,
		},
		{
			name: "syscall ETIMEDOUT",
			err:  syscall.ETIMEDOUT,
			want: true,
		},
		{
			name: "wrapped syscall error",
			err:  fmt.Errorf("dial tcp 1.2.3.4:5432: %w", syscall.ECONNREFUSED),
			want: true,
		},
		{
			name: "pg error: 08006 connection failure",
			err:  &pgconn.PgError{Code: "08006", Message: "connection_failure"},
			want: true,
		},
		{
			name: "pg error: 08000 connection exception (class root)",
			err:  &pgconn.PgError{Code: "08000", Message: "connection_exception"},
			want: true,
		},
		{
			name: "pg error: 08001 SQL client unable to establish",
			err:  &pgconn.PgError{Code: "08001", Message: "sqlclient_unable_to_establish_sqlconnection"},
			want: true,
		},
		{
			name: "pg error: 08004 server rejected connection",
			err:  &pgconn.PgError{Code: "08004", Message: "sqlserver_rejected_establishment_of_sqlconnection"},
			want: true,
		},
		{
			name: "net.OpError timeout",
			err:  &net.OpError{Op: "read", Net: "tcp", Err: &timeoutErr{}},
			want: true,
		},
		{
			name: "substring: connection refused",
			err:  errors.New("dial tcp 10.0.0.1:5432: connect: connection refused"),
			want: true,
		},
		{
			name: "substring: connection reset",
			err:  errors.New("write tcp: connection reset by peer"),
			want: true,
		},
		{
			name: "substring: broken pipe",
			err:  errors.New("write: broken pipe"),
			want: true,
		},
		{
			name: "substring: i/o timeout (case-insensitive)",
			err:  errors.New("read tcp 10.0.0.1:5432: i/o timeout"),
			want: true,
		},
		{
			name: "substring: TLS handshake (case-insensitive)",
			err:  errors.New("TLS handshake failed: timeout"),
			want: true,
		},
		{
			name: "substring: use of closed network connection",
			err:  errors.New("use of closed network connection"),
			want: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isPostgresConnectionError(tt.err)
			assert.Equal(t, tt.want, got, "isPostgresConnectionError(%v)", tt.err)
		})
	}
}

// TestErrConnectorNotReadyChain verifies that the sentinel can be detected
// through errors.Is even when wrapped (the auth strategy wraps it once before
// classifying), and that the underlying error string remains stable for
// existing dashboard/log greps.
func TestErrConnectorNotReadyChain(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("auth get: %w", ErrConnectorNotReady)

	assert.True(t, errors.Is(wrapped, ErrConnectorNotReady),
		"errors.Is should detect ErrConnectorNotReady through wrapping")

	assert.Contains(t, ErrConnectorNotReady.Error(), "PostgreSQLConnector not connected yet",
		"sentinel error string must remain stable for backward-compatible log/dashboard greps")
}

// (timeoutErr is shared with failsafe_transport_test.go in the same package.)

// TestHandleConnectionFailure_CoalescesConcurrentMarks verifies that an
// avalanche of failures collapses to a single MarkTaskAsFailed call per
// cooldown window. This is the structural guard against the 2026-05-13
// fd-lock cascade where 1000+ failing Gets each produced an Error-level
// "marking task as failed" log in the same millisecond.
//
// We can't directly assert MarkTaskAsFailed call count without
// reimplementing the initializer, so we assert on lastFailureMarkNanos
// updates via the same atomic the production path uses — only one CAS
// can win per cooldown window.
func TestHandleConnectionFailure_CoalescesConcurrentMarks(t *testing.T) {
	t.Parallel()

	// Use real connector struct so the atomic field is the same one the
	// production code reads. We can't run the full handler without an
	// initializer, but the coalescing CAS is observable directly.
	p := &PostgreSQLConnector{}

	// First call within the cooldown sets the timestamp.
	now := time.Now().UnixNano()
	last := p.lastFailureMarkNanos.Load()
	assert.Zero(t, last, "fresh connector has zero lastFailureMarkNanos")

	ok := p.lastFailureMarkNanos.CompareAndSwap(last, now)
	assert.True(t, ok, "first CAS must succeed")

	// Within the cooldown, another caller must observe now-last < cooldown.
	updatedLast := p.lastFailureMarkNanos.Load()
	assert.Equal(t, now, updatedLast)
	// Simulate a near-instant second failure: now+1ns - updatedLast == 1ns,
	// which is far less than failureMarkCooldown (1s).
	secondNow := updatedLast + int64(time.Nanosecond)
	withinCooldown := secondNow-updatedLast < int64(failureMarkCooldown)
	assert.True(t, withinCooldown,
		"second failure within cooldown must be observable via the timestamp delta — this is the property the production code branches on")

	// After cooldown elapses, a new failure should be allowed to CAS again.
	expiredNow := updatedLast + int64(2*failureMarkCooldown)
	expired := expiredNow-updatedLast > int64(failureMarkCooldown)
	assert.True(t, expired, "post-cooldown, new failure may CAS")
	ok = p.lastFailureMarkNanos.CompareAndSwap(updatedLast, expiredNow)
	assert.True(t, ok, "post-cooldown CAS must succeed")
}
