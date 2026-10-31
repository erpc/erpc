package wsupstream

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestAdapterHeadsLiveConcurrentWithResub guards the read path that the
// on-demand fallback-heads gate adds: gateFallbackHeads (package erpc) calls
// networkHandle.primaryHeadsLive, which calls Adapter.HeadsLive() from its own
// goroutine while the adapter's resubscribe loop and SetHeadsWanted mutate
// a.heads. HeadsLive and every write to a.heads.id are serialized by subsMu;
// this test hammers that concurrency so -race fails if that ever regresses.
func TestAdapterHeadsLiveConcurrentWithResub(t *testing.T) {
	compressResubRetry(t)
	server := newNotifyServer(t)
	a, sink := newTestAdapter(t, server.wsURL(t))
	a.headsOnDemand = true
	<-server.newConn
	require.Eventually(t, a.wsClient.IsConnected, 3*time.Second, 10*time.Millisecond)
	require.NoError(t, a.Start(context.Background(), fakeNetworkHandle{}, sink))

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Readers: mimic the gate goroutine polling HeadsLive() in a tight loop.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = a.HeadsLive()
				}
			}
		}()
	}

	// Writer: toggle the on-demand subscription, which subscribes/drops newHeads
	// and so sets and clears a.heads.id (under subsMu).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				a.SetHeadsWanted(context.Background(), true)
				a.SetHeadsWanted(context.Background(), false)
			}
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}
