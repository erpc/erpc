package upstream

import (
	"context"
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

// Ensures GetSortedUpstreams does not panic when the wildcard map is not yet initialized.
// Reproduces the scenario:
//  1. Registry has at least one upstream in networkUpstreams
//  2. sortedUpstreams["*"] has not been created yet
//  3. First call to GetSortedUpstreams must not panic and should return the network upstreams
func TestGetSortedUpstreams_WildcardMapInitialization_NoPanic(t *testing.T) {
	t.Parallel()
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

	reg := NewUpstreamsRegistry(ctx, &lg, "test-project", nil, nil, nil, vr, pr, ppr, tracker, 0, nil)

	// Create a minimal real upstream (no HTTP) and register it
	u := &Upstream{
		ProjectId:      "test-project",
		logger:         &lg,
		metricsTracker: tracker,
		config: &common.UpstreamConfig{
			Id:         "u1",
			Type:       common.UpstreamTypeEvm,
			VendorName: "test-vendor",
			Endpoint:   "http://127.0.0.1",
			Evm:        &common.EvmUpstreamConfig{ChainId: 1},
		},
	}
	u.networkId.Store("net-1")
	u.networkLabel.Store("net-1")
	reg.doRegisterBootstrappedUpstream(u)

	// Guard against panic
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("GetSortedUpstreams panicked: %v", r)
		}
	}()

	// First call should not panic and should return the network upstreams
	ups, err := reg.GetSortedUpstreams(ctx, "net-1", "eth_getBalance")
	if err != nil {
		t.Fatalf("GetSortedUpstreams returned error: %v", err)
	}
	if len(ups) == 0 {
		t.Fatalf("expected at least one upstream, got 0")
	}
}
