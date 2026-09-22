package integrity

import (
	"context"
	"fmt"
	"sort"

	"github.com/erpc/erpc/common"
)

// eth_getLogs is the one method with no intrinsic commitment: a response is a
// filtered SUBSET across a range, so there is no root to recompute and a
// silently-dropped log passes every self-consistency check. These two checks
// close that gap (see specs/data-integrity/getlogs-receipt-crosscheck.md):
//
//   - getLogsFilterSanity (deterministic, zero deps): every returned log must
//     actually satisfy the request's filter and range — catches fabricated,
//     mixed-in, or out-of-range logs from the request+response alone.
//   - getLogsCompleteness (cross-source): per-block comparison of the response
//     against the ChainView's cached canonical receipts (hash-anchored,
//     cache-only — zero forced fetches) — catches dropped, extra, duplicated,
//     or content-corrupted logs; plus finalized absent-block detection
//     (pin-anchored, so the corroborate-before-verdict reconfirm applies).
//
// getLogsCompleteness also sees fully-empty `[]` responses (AllowEmptyish):
// an empty result over a range whose cached canonical receipts contain
// filter-matching logs on finalized, pinned blocks is the everything-dropped
// corruption — previously invisible (the engine's emptyish gate short-circuited
// it, leaving only retryEmpty as a guard).

// absentBlockScanCap bounds the absent-block sweep; the ChainView pin window
// (default 32) is the real bound, this is a guard for misconfigured windows.
const absentBlockScanCap = 512

func init() {
	register(&Check{
		ID: "getLogsFilterSanity", Family: FamilyStructural, Class: Deterministic,
		Methods: []string{MethodGetLogs},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			f := d.LogsFilter()
			if f == nil {
				return Skipped // couldn't reproduce the filter — never reject
			}
			from, to, concrete := f.ConcreteRange()
			for i := range d.Logs() {
				l := &d.Logs()[i]
				if l.Removed {
					continue // reorg-removed marker, not a filter member
				}
				if !f.Matches(l) {
					return failf("log %s (block %s, logIndex %s) does not match the request filter", l.TransactionHash, l.BlockNumber, l.LogIndex)
				}
				if f.BlockHash != "" && l.BlockHash != "" && !eqHex(l.BlockHash, f.BlockHash) {
					return failf("log %s belongs to block %s but the filter requested blockHash %s", l.TransactionHash, l.BlockHash, f.BlockHash)
				}
				if concrete && l.BlockNumber != "" {
					if n, err := common.HexToInt64(l.BlockNumber); err == nil && (n < from || n > to) {
						return failf("log %s at block %d is outside the requested range [%d, %d]", l.TransactionHash, n, from, to)
					}
				}
			}
			return nil
		},
	})

	register(&Check{
		ID: "getLogsCompleteness", Family: FamilyStructural, Class: ReorgSensitive,
		Methods: []string{MethodGetLogs},
		// An empty [] response is the everything-dropped shape — the absent-block
		// sweep below is exactly the check for it, so opt into emptyish responses
		// (the engine short-circuits them for every other check).
		AllowEmptyish: true,
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			f := d.LogsFilter()
			if f == nil {
				return Skipped
			}
			hist := historyFrom(ctx)
			cache, hasCache := hist.(ReceiptCache)
			if !hasCache {
				return Skipped // no receipts cache wired — opportunistic check no-ops
			}

			// This check is opportunistic (cache-only), so "pass" is earned: it is
			// reported only when at least one block was actually compared against
			// canonical receipts. A run where every block was cold is a "skip".
			verified := 0
			done := func() *Violation {
				if verified == 0 {
					return Skipped
				}
				return nil
			}

			// Group the response's logs by the block they claim, keyed by the
			// normalized hash so every comparison below is hash-anchored (immutable
			// content — a reorg cannot explain a mismatch against the same hash).
			// Cache lookups use the ORIGINAL hash string (cache keys are stored raw).
			byHash := map[string][]*Log{}
			origHash := map[string]string{}
			for i := range d.Logs() {
				l := &d.Logs()[i]
				if l.Removed || l.BlockHash == "" {
					continue
				}
				h := normHex(l.BlockHash)
				byHash[h] = append(byHash[h], l)
				if _, ok := origHash[h]; !ok {
					origHash[h] = l.BlockHash
				}
			}

			// Per-block comparison for blocks PRESENT in the response whose
			// canonical receipts are already cached (zero fetch cost).
			for h, got := range byHash {
				expected, ok := cache.CachedReceipts(origHash[h])
				if !ok || len(expected) == 0 {
					continue // not warm — skip this block
				}
				switch v := compareBlockLogs(f, h, expected, got); v {
				case nil:
					verified++
				case Skipped:
					// this block's data wasn't comparable — not a verification
				default:
					return v
				}
			}

			// Absent-block detection: a block in the requested range whose cached
			// canonical receipts contain filter-matching logs, yet the response has
			// NONE for it, means the upstream dropped that block's logs entirely.
			// FINALIZED blocks only (immutable set) and pin-anchored — the violation
			// disputes the pin, so a stale pin is re-confirmed before the verdict.
			from, to, concrete := f.ConcreteRange()
			if !concrete || to-from >= absentBlockScanCap {
				return done()
			}
			resolver := resolverFrom(ctx)
			if resolver == nil {
				return done()
			}
			for n := from; n <= to; n++ {
				pin, known := hist.HashAt(n)
				if !known {
					continue
				}
				if _, present := byHash[normHex(pin)]; present {
					continue // the response covered this block; compared above
				}
				if final, ok := resolver.IsFinalized(ctx, n); !ok || !final {
					continue
				}
				expected, ok := cache.CachedReceipts(pin)
				if !ok {
					continue
				}
				missing := 0
				for i := range expected {
					for j := range expected[i].Logs {
						if !expected[i].Logs[j].Removed && f.Matches(&expected[i].Logs[j]) {
							missing++
						}
					}
				}
				if missing > 0 {
					return failf("response has no logs for finalized block %d (%s) but its canonical receipts contain %d filter-matching logs", n, pin, missing).disputes(n)
				}
				verified++ // an absent-block sweep against warm canonical data IS a verification
			}
			return done()
		},
	})
}

// compareBlockLogs cross-references one block's response logs against the
// block's cached canonical receipts (both hash-anchored to the same immutable
// block hash): the filter applied to the canonical logs must yield exactly the
// response's set, with identical content per logIndex. Returns Skipped when
// the data is not comparable (malformed indexes) — never rejects on our own data.
func compareBlockLogs(f *LogsFilter, blockHash string, canonical []Receipt, got []*Log) *Violation {
	expected := map[int64]*Log{}
	for i := range canonical {
		for j := range canonical[i].Logs {
			l := &canonical[i].Logs[j]
			if l.Removed || !f.Matches(l) {
				continue
			}
			idx, err := common.HexToInt64(l.LogIndex)
			if err != nil {
				return Skipped // canonical cache not comparable
			}
			expected[idx] = l
		}
	}

	seen := map[int64]bool{}
	for _, l := range got {
		idx, err := common.HexToInt64(l.LogIndex)
		if err != nil {
			return Skipped // shape checks own malformed indexes
		}
		if seen[idx] {
			return failf("block %s: logIndex %d appears more than once in the response", blockHash, idx)
		}
		seen[idx] = true
		want, ok := expected[idx]
		if !ok {
			return failf("block %s: response log at logIndex %d is not among the block's %d filter-matching canonical logs", blockHash, idx, len(expected))
		}
		if !eqHex(l.Address, want.Address) || !eqHex(l.Data, want.Data) || !eqHex(l.TransactionHash, want.TransactionHash) || len(l.Topics) != len(want.Topics) {
			return failf("block %s: response log at logIndex %d differs from the canonical receipt's log", blockHash, idx)
		}
		for t := range l.Topics {
			if !eqHex(l.Topics[t], want.Topics[t]) {
				return failf("block %s: response log at logIndex %d topic %d differs from canonical", blockHash, idx, t)
			}
		}
	}

	if len(seen) < len(expected) {
		miss := make([]int64, 0, len(expected)-len(seen))
		for idx := range expected {
			if !seen[idx] {
				miss = append(miss, idx)
			}
		}
		sort.Slice(miss, func(i, j int) bool { return miss[i] < miss[j] })
		return failf("block %s: response is missing %d filter-matching canonical logs (logIndexes %s)", blockHash, len(miss), fmtIndexes(miss))
	}
	return nil
}

func fmtIndexes(idx []int64) string {
	const max = 8
	s := ""
	for i, v := range idx {
		if i == max {
			return fmt.Sprintf("%s, … +%d more", s, len(idx)-max)
		}
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("%d", v)
	}
	return s
}
