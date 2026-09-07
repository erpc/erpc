package upstream

import (
	"context"
	"fmt"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
)

// cordonSyncInterval bounds how long a replica can lag the persisted operator
// cordon set. It matches the default selection-policy evalInterval, which is
// already the documented propagation delay for a cordon to affect routing.
const cordonSyncInterval = 15 * time.Second

func (u *UpstreamsRegistry) cordonStore() data.CordonStore {
	if u.sharedStateRegistry == nil {
		return nil
	}
	return u.sharedStateRegistry.Cordons()
}

// CordonAdmin records an operator cordon. When shared state is remote the
// entry is persisted first and applied locally only after the write succeeds:
// a failed write returns the error and changes nothing, so this pod never
// holds a cordon its peers will not, and the sync loop never has to undo it.
func (u *UpstreamsRegistry) CordonAdmin(ctx context.Context, ups *Upstream, method, reason string) error {
	u.cordonMu.Lock()
	defer u.cordonMu.Unlock()

	entry := common.CordonEntry{Reason: reason, CordonedAtMs: time.Now().UnixMilli()}
	if prev, ok := u.metricsTracker.CordonsOwnedBy(ups, health.CordonOwnerAdmin)[method]; ok {
		entry.CordonedAtMs = prev.CordonedAtMs
	}
	if store := u.cordonStore(); store != nil {
		if err := store.Set(ctx, u.prjId, ups.Id(), method, entry); err != nil {
			return fmt.Errorf("failed to persist cordon to shared state: %w", err)
		}
	}
	u.metricsTracker.Cordon(ups, method, health.CordonOwnerAdmin, entry)
	return nil
}

// UncordonAdmin lifts every owner's cordon on (ups, method): the operator
// call is an override, so it also clears automatic cordons that have no
// self-healing path (e.g. a chain-identity mismatch). Only the admin entry
// exists remotely, so only that is deleted there.
func (u *UpstreamsRegistry) UncordonAdmin(ctx context.Context, ups *Upstream, method string) error {
	u.cordonMu.Lock()
	defer u.cordonMu.Unlock()

	if store := u.cordonStore(); store != nil {
		if err := store.Delete(ctx, u.prjId, ups.Id(), method); err != nil {
			return fmt.Errorf("failed to delete cordon from shared state: %w", err)
		}
	}
	u.metricsTracker.Uncordon(ups, method, "*")
	return nil
}

// runCordonSync polls the persisted cordon set until appCtx ends. Polling one
// key per project is cheaper and simpler than a notification channel and
// bounds staleness at cordonSyncInterval regardless of pub/sub health.
func (u *UpstreamsRegistry) runCordonSync() {
	ticker := time.NewTicker(cordonSyncInterval)
	defer ticker.Stop()
	for {
		u.syncCordons()
		select {
		case <-u.appCtx.Done():
			return
		case <-ticker.C:
		}
	}
}

// syncCordons makes the local admin-owned cordons equal the persisted set:
// entries present remotely are applied (idempotent; a same-owner re-cordon
// keeps its original start), local admin cordons absent remotely are lifted.
// Automatic cordons are never touched.
func (u *UpstreamsRegistry) syncCordons() {
	store := u.cordonStore()
	if store == nil {
		return
	}
	u.cordonMu.Lock()
	defer u.cordonMu.Unlock()

	ctx, cancel := context.WithTimeout(u.appCtx, u.sharedStateRegistry.GetFallbackTimeout())
	defer cancel()
	remote, err := store.Get(ctx, u.prjId)
	if err != nil {
		u.logger.Warn().Err(err).Msg("failed to load cordons from shared state; keeping local state")
		return
	}
	for _, ups := range u.GetAllUpstreams() {
		want := remote[ups.Id()]
		have := u.metricsTracker.CordonsOwnedBy(ups, health.CordonOwnerAdmin)
		for method, entry := range want {
			if have[method] != entry {
				u.metricsTracker.Cordon(ups, method, health.CordonOwnerAdmin, entry)
			}
		}
		for method := range have {
			if _, ok := want[method]; !ok {
				u.metricsTracker.Uncordon(ups, method, health.CordonOwnerAdmin)
			}
		}
	}
}
