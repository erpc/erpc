package erpc

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
)

// headPollLeaseRewireInterval bounds how long a dynamically added upstream
// polls unconditionally before it honors the lease. Re-applying is an
// in-memory pointer store per upstream, so the loop is cheap.
const headPollLeaseRewireInterval = time.Second

// headPollLeases maps *Network to its running lease (kept outside Network so
// the feature stays self-contained; removed when the app context ends).
var headPollLeases sync.Map

// HeadPollLease returns the network's head polling lease, or nil when
// leased polling is not enabled.
func (n *Network) HeadPollLease() *data.HeadPollLease {
	if v, ok := headPollLeases.Load(n); ok {
		return v.(*data.HeadPollLease)
	}
	return nil
}

// initHeadPollLease enables evm.headPolling.mode=lease for one network: a
// single replica per (cluster, project, network, trust fingerprint) polls
// upstream latest/finalized heads while others reuse the shared per-upstream
// counters, polling themselves whenever those go stale. No-op unless opted in.
func (nr *NetworksRegistry) initHeadPollLease(n *Network, cfg *common.NetworkConfig) error {
	if cfg == nil || cfg.Evm == nil || cfg.Evm.HeadPolling == nil || cfg.Evm.HeadPolling.Mode != common.HeadPollingModeLease {
		return nil
	}
	hp := cfg.Evm.HeadPolling
	if n.upstreamsRegistry == nil {
		return fmt.Errorf("head poll lease: upstreams registry unavailable")
	}
	ssr, ok := n.upstreamsRegistry.SharedStateRegistry().(interface {
		Connector() data.Connector
		ClusterKey() string
	})
	if !ok {
		return fmt.Errorf("head poll lease: shared state connector unavailable")
	}

	// Replicas with different upstream/trust configuration must never
	// suppress each other's polling, so the key carries a config digest.
	nr.project.cfgMu.RLock()
	fp, err := headCacheFingerprint(nr.project.Config, cfg)
	nr.project.cfgMu.RUnlock()
	if err != nil {
		return fmt.Errorf("head poll lease fingerprint: %w", err)
	}
	key := fmt.Sprintf("%s/headPollLease/%s/%s/%s", ssr.ClusterKey(), n.projectId, n.networkId, fp[:16])

	project, label := n.projectId, n.Label()
	lg := n.logger.With().Str("component", "headPollLease").Logger()
	lease := data.NewHeadPollLease(ssr.Connector(), key, hp.LeaseTtl.Duration(), &lg, func(held bool) {
		v := 0.0
		if held {
			v = 1
		}
		telemetry.MetricHeadPollLeaseHeld.WithLabelValues(project, label).Set(v)
	})
	onSkip := func() {
		telemetry.CounterHandle(telemetry.MetricHeadPollSkippedTotal, project, label).Inc()
	}
	staleAfter := hp.StaleAfter.Duration()

	ctx := nr.appCtx
	headPollLeases.Store(n, lease)
	lease.Start(ctx)
	go func() {
		t := time.NewTicker(headPollLeaseRewireInterval)
		defer t.Stop()
		for {
			n.applyHeadPollLease(ctx, lease, staleAfter, onSkip)
			select {
			case <-ctx.Done():
				lease.Stop()
				headPollLeases.Delete(n)
				telemetry.MetricHeadPollLeaseHeld.DeleteLabelValues(project, label)
				return
			case <-t.C:
			}
		}
	}()
	lg.Info().Dur("leaseTtl", hp.LeaseTtl.Duration()).Dur("staleAfter", staleAfter).Msg("head polling lease enabled")
	return nil
}

// applyHeadPollLease installs the lease on every current upstream poller that
// honors it, including upstreams registered after the network was prepared.
func (n *Network) applyHeadPollLease(ctx context.Context, lease common.HeadPollLease, staleAfter time.Duration, onSkip func()) int {
	wired := 0
	for _, up := range n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId) {
		sp := up.EvmStatePoller()
		if sp == nil || sp.IsObjectNull() {
			continue
		}
		if la, ok := sp.(common.HeadPollLeaseAware); ok {
			la.SetHeadPollLease(lease, staleAfter, onSkip)
			wired++
		}
	}
	return wired
}
