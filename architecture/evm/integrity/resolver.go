package integrity

import "context"

// Resolver lets a check reach beyond the single response under validation: ask a
// block's finality and force-fetch the canonical block aggregate to corroborate
// against. It is implemented outside this package (by the evm layer over
// network.Forward + the state poller) and injected via Input, so the integrity
// package stays decoupled from the network stack (dependency inversion).
type Resolver interface {
	// IsFinalized reports whether a block number is finalized (immutable).
	// ok=false means finality could not be determined — the engine treats that
	// as unfinalized (conservative: never hard-fail on data we can't confirm is
	// final).
	IsFinalized(ctx context.Context, blockNumber int64) (final bool, ok bool)

	// CanonicalReceipts force-fetches the receipts for a block via the trusted
	// network path (cache-backed, inheriting whatever failsafe — e.g. consensus
	// — the network is configured with). Returns ok=false when unavailable, in
	// which case a corroboration check no-ops rather than failing.
	CanonicalReceipts(ctx context.Context, blockRef string) (receipts []Receipt, ok bool)
}

// Behavior is what to do when a reorg-sensitive check finds a mismatch.
type Behavior int

const (
	// BehaviorError — reject the response (content-validation error). Default
	// for finalized data, where a mismatch is genuine corruption.
	BehaviorError Behavior = iota
	// BehaviorRecord — emit a metric/log but still serve the response. Default
	// for unfinalized data, where a mismatch may be a benign reorg.
	BehaviorRecord
	// BehaviorIgnore — do nothing.
	BehaviorIgnore
)

// ReorgPolicy maps finality state to the mismatch behavior for reorg-sensitive
// checks. The zero value is the safe default (finalized => error, unfinalized
// => record).
type ReorgPolicy struct {
	Finalized   Behavior
	Unfinalized Behavior
}

// behaviorFor returns the configured behavior for a (final, known) finality.
func (p ReorgPolicy) behaviorFor(final, known bool) Behavior {
	if final && known {
		return p.Finalized
	}
	// Unknown finality is treated as unfinalized.
	return p.Unfinalized
}
