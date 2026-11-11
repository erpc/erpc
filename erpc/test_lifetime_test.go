package erpc

import (
	"context"
	"testing"
	"time"
)

// stopWait bounds how long a stopped network's goroutines may take to
// end: each fetch they run ends with the app ctx.
const stopWait = 10 * time.Second

// stopAtEnd waits, when t ends, until no goroutine of network runs, so
// none outlives t: no poller sends through gock while the next test resets
// it. Network's app ctx must have ended by then: a deferred cancel, or a
// t.Cleanup(cancel) registered after this call, so it runs first. A gock
// reset runs after when registered first (t.Cleanup(util.ResetGock)).
func stopAtEnd(t testing.TB, network *Network) {
	t.Helper()
	if network == nil {
		return
	}
	t.Cleanup(func() { waitStopped(t, "network "+network.networkId, network.appCtx, network.wait) })
}

// stopERPCAtEnd is stopAtEnd for an eRPC instance built on ctx: every
// project's networks and upstreams.
func stopERPCAtEnd(t testing.TB, e *ERPC, ctx context.Context) {
	t.Helper()
	if e == nil {
		return
	}
	t.Cleanup(func() { waitStopped(t, "erpc", ctx, e.wait) })
}

// waitStopped runs wait once ctx ended; t fails when ctx still runs at the
// test's end, or when wait is still blocked stopWait later.
func waitStopped(t testing.TB, what string, ctx context.Context, wait func()) {
	t.Helper()
	if ctx.Err() == nil {
		t.Errorf("%s: its app ctx still runs when the test ends; cancel it first", what)
		return
	}
	done := make(chan struct{})
	go func() {
		wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(stopWait):
		t.Errorf("%s: goroutines still run %s after its app ctx ended", what, stopWait)
	}
}
