package clients

import (
	"context"
	"io"
	"net/url"
	"strconv"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/credentials/insecure"
)

// newTestPool builds a pool directly (no server needed — grpc.NewClient dials
// lazily) so the sizing/round-robin behavior can be asserted in isolation.
func newTestPool(t *testing.T, poolSize int) *bdsPool {
	t.Helper()
	logger := zerolog.New(io.Discard)
	p, err := newBdsPool(
		context.Background(),
		&logger,
		"test-project",
		"n/a",
		"dns:///127.0.0.1:50051",
		insecure.NewCredentials(),
		"{}",
		poolSize,
		0, // no expected chainId — identity checks stay disarmed in this test
	)
	require.NoError(t, err)
	t.Cleanup(p.Shutdown)
	return p
}

// newClientWithPoolSize builds a client against a dummy (never-dialed) address.
// NewGrpcBdsClient does not probe on construction, so no server is required to
// assert how many connections the pool opened.
func newClientWithPoolSize(t *testing.T, poolSize int) *GenericGrpcBdsClient {
	t.Helper()
	parsedURL, err := url.Parse("grpc://127.0.0.1:50051")
	require.NoError(t, err)
	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client, err := NewGrpcBdsClient(ctx, &logger, "test-project", nil, parsedURL, poolSize)
	require.NoError(t, err)
	return client.(*GenericGrpcBdsClient)
}

// TestNewBdsPool_HonorsConfiguredSize verifies a configured poolSize maps 1:1
// to the number of connection slots the pool opens.
func TestNewBdsPool_HonorsConfiguredSize(t *testing.T) {
	for _, size := range []int{1, 2, 5, 16, 32} {
		size := size
		t.Run("size="+strconv.Itoa(size), func(t *testing.T) {
			p := newTestPool(t, size)
			require.Equal(t, size, p.Size(),
				"pool should open exactly the configured number of conns")
			require.Len(t, p.conns, size)
		})
	}
}

// TestNewBdsPool_DefaultsWhenUnsetOrInvalid verifies that an unset (0) or
// nonsensical (negative) poolSize falls back to the built-in default so legacy
// configs and bad values both keep the historical behavior.
func TestNewBdsPool_DefaultsWhenUnsetOrInvalid(t *testing.T) {
	for _, size := range []int{0, -1, -100} {
		size := size
		t.Run("size="+strconv.Itoa(size), func(t *testing.T) {
			p := newTestPool(t, size)
			require.Equal(t, bdsPoolSize, p.Size(),
				"non-positive poolSize must fall back to the default")
		})
	}
}

// TestBdsPool_PickRoundRobinsAcrossConfiguredSize verifies Pick() rotates
// evenly across every slot of a custom-sized pool — the blast-radius guarantee
// must scale with the configured size, not just the default.
func TestBdsPool_PickRoundRobinsAcrossConfiguredSize(t *testing.T) {
	const size = 5
	p := newTestPool(t, size)

	seen := make(map[*bdsConn]int)
	for i := 0; i < size*3; i++ {
		seen[p.Pick()]++
	}
	require.Len(t, seen, size, "Pick must rotate through every slot")
	for c, n := range seen {
		require.Equal(t, 3, n, "every slot should be picked exactly 3 times; %p got %d", c, n)
	}
}

// TestBdsPool_SingleConnHasNoIsolation documents the degenerate poolSize=1
// case: every Pick returns the same conn, so a wedge there chokes everyone.
// This is the behavior an operator opts into by setting poolSize: 1.
func TestBdsPool_SingleConnHasNoIsolation(t *testing.T) {
	p := newTestPool(t, 1)
	require.Equal(t, 1, p.Size())

	first := p.Pick()
	for i := 0; i < 5; i++ {
		require.Same(t, first, p.Pick(),
			"a single-conn pool must always return the same conn")
	}
}

// TestNewGrpcBdsClient_ThreadsPoolSize verifies the poolSize argument is
// threaded all the way from the client constructor into the underlying pool.
func TestNewGrpcBdsClient_ThreadsPoolSize(t *testing.T) {
	client := newClientWithPoolSize(t, 7)
	require.Equal(t, 7, client.pool.Size())
}

// TestNewGrpcBdsClient_DefaultPoolSizeWhenZero verifies the client falls back
// to the default pool size when none is configured (poolSize == 0).
func TestNewGrpcBdsClient_DefaultPoolSizeWhenZero(t *testing.T) {
	client := newClientWithPoolSize(t, 0)
	require.Equal(t, bdsPoolSize, client.pool.Size())
}
