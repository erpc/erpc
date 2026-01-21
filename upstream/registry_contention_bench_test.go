package upstream

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
)

func init() { util.ConfigureTestLogger() }

// buildRegistryForBench creates a registry populated with numNetworks × upstreamsPerNetwork upstreams
// and initializes sortedUpstreams entries for provided methods.
func buildRegistryForBench(b *testing.B, numNetworks, upstreamsPerNetwork int, methods []string) (*UpstreamsRegistry, []string) {
	b.Helper()
	appCtx := context.Background()

	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	rlr, _ := NewRateLimitersRegistry(context.Background(), nil, &log.Logger)
	mt := health.NewTracker(&log.Logger, "bench-prj", time.Minute)

	reg := NewUpstreamsRegistry(
		appCtx,
		&log.Logger,
		"bench-prj",
		nil, // upsCfg
		nil, // shared state registry
		rlr,
		vr,
		pr,
		nil, // proxy pool registry
		mt,
		0,   // scoreRefreshInterval
		nil, // onUpstreamRegistered
	)

	networkIds := make([]string, 0, numNetworks)

	for ni := 0; ni < numNetworks; ni++ {
		chainId := int64(100000 + ni)
		networkId := util.EvmNetworkId(chainId)
		networkIds = append(networkIds, networkId)

		for ui := 0; ui < upstreamsPerNetwork; ui++ {
			cfg := &common.UpstreamConfig{
				Id:         fmt.Sprintf("ups-%d-%d", ni, ui),
				Type:       common.UpstreamTypeEvm,
				Endpoint:   "http://rpc1.localhost",
				VendorName: "", // allow guess
				Evm:        &common.EvmUpstreamConfig{ChainId: chainId},
			}
			ups, err := reg.NewUpstream(cfg)
			if err != nil {
				b.Fatalf("failed to create upstream: %v", err)
			}
			// Ensure upstream has networkId set
			ups.SetNetworkConfig(&common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{ChainId: chainId},
				Alias:        networkId,
			})

			// Directly register without bootstrapping external calls
			reg.doRegisterBootstrappedUpstream(ups)
		}
	}

	// Initialize sortedUpstreams entries for "*" and specific methods per network
	for _, nw := range networkIds {
		if _, err := reg.GetSortedUpstreams(appCtx, nw, "*"); err != nil {
			b.Fatalf("init GetSortedUpstreams(*): %v", err)
		}
		for _, m := range methods {
			if _, err := reg.GetSortedUpstreams(appCtx, nw, m); err != nil {
				b.Fatalf("init GetSortedUpstreams(%s): %v", m, err)
			}
		}
	}

	return reg, networkIds
}

func BenchmarkRegistry_ReadOnly(b *testing.B) {
	reg, networks := buildRegistryForBench(b, 8, 64, []string{"eth_getBlockByNumber", "eth_call", "eth_getLogs"})
	ctx := context.Background()
	rand.Seed(42) // deterministic
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		n := len(networks)
		for pb.Next() {
			nw := networks[rand.Intn(n)]
			_ = reg.GetNetworkUpstreams(ctx, nw)
		}
	})
}

func BenchmarkRegistry_ReadUnderRefresh(b *testing.B) {
	reg, networks := buildRegistryForBench(b, 8, 64, []string{"eth_getBlockByNumber", "eth_call", "eth_getLogs"})
	ctx := context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = reg.RefreshUpstreamNetworkMethodScores()
			}
		}
	}()
	rand.Seed(42)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		n := len(networks)
		for pb.Next() {
			nw := networks[rand.Intn(n)]
			_ = reg.GetNetworkUpstreams(ctx, nw)
		}
	})
	close(stop)
	wg.Wait()
}

func BenchmarkRegistry_ReadUnderHealth(b *testing.B) {
	reg, networks := buildRegistryForBench(b, 8, 64, []string{"eth_getBlockByNumber", "eth_call", "eth_getLogs"})
	ctx := context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = reg.GetUpstreamsHealth()
			}
		}
	}()
	rand.Seed(42)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		n := len(networks)
		for pb.Next() {
			nw := networks[rand.Intn(n)]
			_ = reg.GetNetworkUpstreams(ctx, nw)
		}
	})
	close(stop)
	wg.Wait()
}

func BenchmarkRegistry_ReadUnderBoth(b *testing.B) {
	reg, networks := buildRegistryForBench(b, 8, 64, []string{"eth_getBlockByNumber", "eth_call", "eth_getLogs"})
	ctx := context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = reg.RefreshUpstreamNetworkMethodScores()
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = reg.GetUpstreamsHealth()
			}
		}
	}()
	rand.Seed(42)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		n := len(networks)
		for pb.Next() {
			nw := networks[rand.Intn(n)]
			_ = reg.GetNetworkUpstreams(ctx, nw)
		}
	})
	close(stop)
	wg.Wait()
}
