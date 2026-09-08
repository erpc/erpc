package upstream

import (
	"context"
	"fmt"
	"time"

	"github.com/erpc/erpc/common"
)

// operatorCordonSyncInterval bounds how long a replica can lag the persisted
// operator cordon set. It matches the default selection-policy evalInterval,
// already the documented delay for a cordon to affect routing.
const operatorCordonSyncInterval = 15 * time.Second

// CordonAdmin persists an operator cordon and applies the resulting snapshot
// locally. Nothing changes on this replica unless the write succeeded, so a
// pod never holds a cordon its peers will not see.
func (u *UpstreamsRegistry) CordonAdmin(ctx context.Context, upstreamId, method, reason string) error {
	snap, err := u.cordons.Set(ctx, u.prjId, upstreamId, method, reason)
	if err != nil {
		return fmt.Errorf("failed to persist cordon: %w", err)
	}
	u.applyOperatorCordons(snap)
	return nil
}

// UncordonAdmin removes the operator cordon on (upstreamId, method) for the
// whole fleet and, on this replica, also lifts any automatic cordon on the
// same cell: the operator call is the override for a detector verdict.
func (u *UpstreamsRegistry) UncordonAdmin(ctx context.Context, upstreamId, method, reason string) error {
	snap, err := u.cordons.Delete(ctx, u.prjId, upstreamId, method)
	if err != nil {
		return fmt.Errorf("failed to remove cordon: %w", err)
	}
	u.applyOperatorCordons(snap)
	if ups := u.upstreamById(upstreamId); ups != nil {
		ups.Uncordon(method, reason)
	}
	return nil
}

// OperatorCordons is the snapshot this replica currently routes by.
func (u *UpstreamsRegistry) OperatorCordons() *common.CordonSnapshot {
	return u.metricsTracker.OperatorCordons()
}

func (u *UpstreamsRegistry) applyOperatorCordons(snap *common.CordonSnapshot) {
	u.metricsTracker.SetOperatorCordons(snap, func(id string) common.Upstream {
		if ups := u.upstreamById(id); ups != nil {
			return ups
		}
		return nil
	})
}

func (u *UpstreamsRegistry) upstreamById(id string) *Upstream {
	u.upstreamsMu.RLock()
	defer u.upstreamsMu.RUnlock()
	for _, ups := range u.allUpstreams {
		if ups.Id() == id {
			return ups
		}
	}
	return nil
}

// syncOperatorCordons fetches the persisted snapshot once; the tracker keeps
// it only if it is newer than what it holds, so a slow fetch racing a local
// write can never roll that write back.
func (u *UpstreamsRegistry) syncOperatorCordons() {
	timeout := 3 * time.Second
	if u.sharedStateRegistry != nil {
		timeout = u.sharedStateRegistry.GetFallbackTimeout()
	}
	ctx, cancel := context.WithTimeout(u.appCtx, timeout)
	defer cancel()
	snap, err := u.cordons.Get(ctx, u.prjId)
	if err != nil {
		u.logger.Warn().Err(err).Msg("failed to load operator cordons from shared state; keeping current snapshot")
		return
	}
	u.applyOperatorCordons(snap)
}

func (u *UpstreamsRegistry) runOperatorCordonSync() {
	ticker := time.NewTicker(operatorCordonSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-u.appCtx.Done():
			return
		case <-ticker.C:
			u.syncOperatorCordons()
		}
	}
}
