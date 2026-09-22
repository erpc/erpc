package clients

import (
	"context"
	"net/url"
	"runtime"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/connectivity"
)

const (
	// bdsAbandonedClients is how many clients the leak test creates and then
	// abandons. The production leak was a retried bootstrap task, so the
	// contract under test is that the goroutine residue does NOT scale with
	// this number. 8 is picked so that even a one-goroutine-per-client
	// regression overshoots the residue budget below by more than 2x.
	bdsAbandonedClients = 8

	// bdsAbandonedPoolSize keeps each client small (2 conns) so the test stays
	// quick while still exercising a multi-conn pool — Shutdown has to close
	// every slot, not just the first.
	bdsAbandonedPoolSize = 2

	// bdsResidueBudget is the total goroutine residue tolerated after all
	// bdsAbandonedClients clients have been shut down.
	//
	// Measured on this tree (poolSize 2): ~8 goroutines per client while live
	// — the pool's maintainLoop, the appCtx watcher, and per-conn grpc-go
	// callback serializers / HTTP2 loops — and 0 residue once Shutdown runs.
	// So the two outcomes at 8 clients are +64 (broken) versus +0 (fixed) and
	// the budget only has to separate them. It is deliberately near the floor:
	//   - Shutdown reverted to a no-op          => +64, 21x over budget
	//   - pool closed but the appCtx watcher
	//     left blocked on appCtx.Done()         =>  +8, 2.6x over budget
	// The 3 goroutines of slack absorb whole-suite noise (a sibling test's
	// background loop still winding down, a runtime worker that NumGoroutine
	// stops excluding) without ever admitting a per-client leak, which is the
	// only thing this test exists to catch.
	bdsResidueBudget = 3
)

// newAbandonedBdsClient builds a client the way an aborted bootstrap does:
// against an address nothing listens on. grpc.NewClient dials lazily, so
// construction succeeds with no server while still spawning the pool
// maintainer, the appCtx watcher, and each conn's grpc-go goroutines.
//
// The context is deliberately context.Background() and not a cancellable one:
// cancelling the app context is the OTHER path that releases these goroutines,
// so using it here would let a Shutdown that does nothing still pass.
func newAbandonedBdsClient(t *testing.T) *GenericGrpcBdsClient {
	t.Helper()
	parsedURL, err := url.Parse("http://127.0.0.1:59999")
	require.NoError(t, err)
	logger := zerolog.Nop()
	client, err := NewGrpcBdsClient(context.Background(), &logger, "test-project", nil, parsedURL, bdsAbandonedPoolSize)
	require.NoError(t, err)

	// data/grpc.go can only release an abandoned client through a runtime
	// assertion, cli.(clients.ShutdownableClient): Shutdown is deliberately off
	// GrpcBdsClient so query_pipe_through.go's capability probe keeps matching
	// test doubles. That makes the constructor's DYNAMIC type load-bearing, and
	// no compiler check covers it — wrapping the return value would still
	// satisfy the `var _ ShutdownableClient = (*GenericGrpcBdsClient)(nil)` pin
	// while making the cleanup path's assertion miss, silently restoring the
	// leak. So probe the constructor's product exactly the way that path does.
	_, shutdownable := client.(ShutdownableClient)
	require.True(t, shutdownable,
		"NewGrpcBdsClient returned %T, which does not satisfy ShutdownableClient; "+
			"data/grpc.go's cleanup path would silently skip closing it", client)

	concrete, ok := client.(*GenericGrpcBdsClient)
	require.True(t, ok, "expected *GenericGrpcBdsClient, got %T", client)
	return concrete
}

// settledGoroutineCount waits for runtime.NumGoroutine() to stop moving and
// returns it. gRPC teardown is asynchronous — ClientConn.Close returns before
// the HTTP2 transport loops and callback serializers have run their last
// deferred exit — so a single read races the very exits it is trying to
// observe. Polls to a deadline rather than sleeping a fixed span so a slow
// machine costs correctness nothing and a fast one costs no time.
func settledGoroutineCount() int {
	const (
		interval      = 25 * time.Millisecond
		stableSamples = 4
		timeout       = 5 * time.Second
	)
	until := time.Now().Add(timeout)
	last, stable := -1, 0
	for time.Now().Before(until) {
		runtime.GC()
		time.Sleep(interval)
		n := runtime.NumGoroutine()
		if n != last {
			last, stable = n, 0
			continue
		}
		if stable++; stable == stableSamples {
			return n
		}
	}
	return runtime.NumGoroutine()
}

// TestGrpcBdsClient_ShutdownReleasesGoroutines is the regression test for the
// unbounded goroutine leak: a BDS client that is created and then abandoned
// must give its goroutines back when Shutdown is called. A retried bootstrap
// created one client per attempt and dropped it on the floor, so a residue
// that scales with the client count is the production failure mode (a dev pod
// reached 45.8k goroutines, 76% of them orphaned BDS clients).
//
// Shutdown is reached the way the abandoning caller reaches it: off the
// GrpcBdsClient interface (see newAbandonedBdsClient). It was unexported with
// zero callers, so no caller could close a client even if it wanted to.
func TestGrpcBdsClient_ShutdownReleasesGoroutines(t *testing.T) {
	// Warm-up client, created and closed before the baseline is taken: the
	// first BDS client in a process also brings up grpc-go's and otel's
	// process-wide singletons, whose goroutines never exit. Paying that once
	// here keeps the one-time cost out of the delta, so what the budget
	// measures below is purely per-client.
	newAbandonedBdsClient(t).Shutdown()

	before := settledGoroutineCount()

	live := make([]*GenericGrpcBdsClient, 0, bdsAbandonedClients)
	for range bdsAbandonedClients {
		live = append(live, newAbandonedBdsClient(t))
	}
	peak := settledGoroutineCount()

	// Guard against a vacuous pass. If construction ever stops spawning
	// goroutines, the residue below goes to zero for the wrong reason and this
	// test silently stops defending anything.
	require.GreaterOrEqual(t, peak-before, bdsAbandonedClients,
		"%d live clients should hold at least one goroutine each (count went %d -> %d); "+
			"if construction no longer spawns any, this test can no longer detect the leak",
		bdsAbandonedClients, before, peak)

	for _, c := range live {
		c.Shutdown()
	}
	residue := settledGoroutineCount() - before
	t.Logf("goroutines: baseline %d, %d live clients %d (+%d, %.1f/client), after Shutdown +%d (budget %d)",
		before, bdsAbandonedClients, peak, peak-before,
		float64(peak-before)/float64(bdsAbandonedClients), residue, bdsResidueBudget)

	require.LessOrEqual(t, residue, bdsResidueBudget,
		"Shutdown must release every abandoned client's goroutines: %d clients grew the "+
			"count %d -> %d (+%d) and left +%d behind after Shutdown, over the budget of %d. "+
			"Residue that scales with the client count means Shutdown is not closing the "+
			"pool, or is leaving the appCtx watcher blocked on appCtx.Done().",
		bdsAbandonedClients, before, peak, peak-before, residue, bdsResidueBudget)
}

// TestGrpcBdsClient_ShutdownIsIdempotentAndNilSafe pins the two properties the
// abandoned-client call sites rely on. Those sites are error and early-return
// paths that cannot always prove the client is non-nil or that no one else has
// closed it, and Shutdown closes a channel — so without sync.Once the second
// call panics on a double close, and without the nil guard the first call
// panics on a nil receiver. Both would turn a leak fix into a crash.
//
// Also asserts the state transition Shutdown is responsible for: every conn in
// the pool ends up closed, not just the first slot.
func TestGrpcBdsClient_ShutdownIsIdempotentAndNilSafe(t *testing.T) {
	var absent *GenericGrpcBdsClient
	absent.Shutdown()

	client := newAbandonedBdsClient(t)
	conns := client.pool.conns
	require.Len(t, conns, bdsAbandonedPoolSize)
	for i, c := range conns {
		require.NotEqual(t, connectivity.Shutdown, c.conn.GetState(),
			"conn %d should still be live before Shutdown", i)
	}

	client.Shutdown()
	client.Shutdown()

	for i, c := range conns {
		require.Equal(t, connectivity.Shutdown, c.conn.GetState(),
			"Shutdown must close pooled conn %d", i)
	}
}
