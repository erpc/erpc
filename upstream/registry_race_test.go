package upstream

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
)

func init() {
	util.ConfigureTestLogger()
}

// TestRefreshScoresRace_ErasesNewlyRegisteredUpstreams attempts to reveal a lost-update window:
// - a refresh snapshots state
// - a new upstream is registered (and appended to sortedUpstreams)
// - the refresh commits using the stale snapshot and overwrites sortedUpstreams, dropping the new upstream
func TestRefreshScoresRace_ErasesNewlyRegisteredUpstreams(t *testing.T) {
	t.Helper()

	ctx := context.Background()
	lg := log.Logger

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&lg, vr, nil, nil)
	if err != nil {
		t.Fatalf("providers registry: %v", err)
	}
	ppr, err := clients.NewProxyPoolRegistry(nil, &lg)
	if err != nil {
		t.Fatalf("proxy pool registry: %v", err)
	}
	tracker := health.NewTracker(&lg, "test-project", time.Minute)

	reg := NewUpstreamsRegistry(ctx, &lg, "test-project", nil, nil, nil, vr, pr, ppr, tracker, 0, nil, nil)

	mk := func(id string) *Upstream {
		u := &Upstream{
			ProjectId:      "test-project",
			logger:         &lg,
			metricsTracker: tracker,
			config: &common.UpstreamConfig{
				Id:         id,
				Type:       common.UpstreamTypeEvm,
				VendorName: "test-vendor",
				Endpoint:   "http://127.0.0.1",
				Evm:        &common.EvmUpstreamConfig{ChainId: 1},
			},
		}
		u.networkId.Store("net-1")
		u.networkLabel.Store("net-1")
		return u
	}

	// Seed with a baseline upstream to ensure refresh has work to do
	a := mk("a")
	reg.doRegisterBootstrappedUpstream(a)
	if err := reg.RefreshUpstreamNetworkMethodScores(); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}

	// Inflate the number of (network,method) pairs to increase compute time between snapshot and commit
	reg.upstreamsMu.Lock()
	if _, ok := reg.sortedUpstreams["net-1"]; !ok {
		reg.sortedUpstreams["net-1"] = make(map[string][]*Upstream)
	}
	base := reg.networkUpstreams["net-1"]
	cp := make([]*Upstream, len(base))
	copy(cp, base)
	reg.sortedUpstreams["net-1"]["*"] = cp
	for i := 0; i < 250; i++ {
		m := fmt.Sprintf("m-%d", i)
		cpM := make([]*Upstream, len(base))
		copy(cpM, base)
		reg.sortedUpstreams["net-1"][m] = cpM
	}
	reg.upstreamsMu.Unlock()

	// Run refreshes continuously in the background to maximize chance of the window
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				_ = reg.RefreshUpstreamNetworkMethodScores()
			}
		}
	}()
	time.Sleep(5 * time.Millisecond) // let the refresher spin up

	// Repeatedly register new upstreams while the refresher runs; look for an erase event
	deadline := time.Now().Add(2 * time.Second)
	erasedObserved := false

	for i := 0; time.Now().Before(deadline) && !erasedObserved; i++ {
		id := fmt.Sprintf("b-%d", i)
		b := mk(id)
		reg.doRegisterBootstrappedUpstream(b)

		// Phase 1: wait until sortedUpstreams includes the new upstream (next refresh cycle)
		includeDeadline := time.Now().Add(500 * time.Millisecond)
		seenIncluded := false
		for time.Now().Before(includeDeadline) && !seenIncluded {
			reg.upstreamsMu.RLock()
			nw := reg.networkUpstreams["net-1"]
			sorted := reg.sortedUpstreams["net-1"]["*"]
			reg.upstreamsMu.RUnlock()

			hasInNetwork := false
			for _, u := range nw {
				if u != nil && u.Id() == id {
					hasInNetwork = true
					break
				}
			}
			hasInSorted := false
			for _, u := range sorted {
				if u != nil && u.Id() == id {
					hasInSorted = true
					break
				}
			}
			if hasInNetwork && !hasInSorted {
				// not yet included in sorted; keep waiting for inclusion
			}
			if hasInNetwork && hasInSorted {
				seenIncluded = true
				break
			}
			time.Sleep(1 * time.Millisecond)
		}
		if !seenIncluded {
			// Could not observe inclusion; move to next iteration
			continue
		}

		// Phase 2: after inclusion, ensure it never disappears while still present in networkUpstreams
		checkDeadline := time.Now().Add(200 * time.Millisecond)
		for time.Now().Before(checkDeadline) {
			reg.upstreamsMu.RLock()
			nw := reg.networkUpstreams["net-1"]
			sorted := reg.sortedUpstreams["net-1"]["*"]
			reg.upstreamsMu.RUnlock()

			hasInNetwork := false
			for _, u := range nw {
				if u != nil && u.Id() == id {
					hasInNetwork = true
					break
				}
			}
			hasInSorted := false
			for _, u := range sorted {
				if u != nil && u.Id() == id {
					hasInSorted = true
					break
				}
			}
			if hasInNetwork && !hasInSorted {
				erasedObserved = true
				break
			}
			time.Sleep(1 * time.Millisecond)
		}
	}

	close(stop)
	<-done

	if erasedObserved {
		t.Fatalf("observed erased upstream in sortedUpstreams while present in networkUpstreams; indicates refresh commit overwrote concurrent registration")
	}
	t.Skip("race not observed within time window; rerun may catch it")
}
