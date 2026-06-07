package common

import (
	"slices"
	"strings"
)

// LaneName derives a short, human-readable name for a GROUP of upstream ids
// (e.g. the set a `use-upstream` selector matches). It is deterministic: the
// same set of ids always yields the same name, regardless of input order, so
// it is safe to use as a cross-pod label.
//
// Algorithm:
//  1. Split each id on '-'. If some token appears in EVERY id, return it —
//     preferring the token that appears earliest in the name when several
//     tokens are shared by all (e.g. {systx-chainstack, systx-quicknode} ->
//     "systx"; {chainstack-systx, quicknode-systx} -> "systx").
//  2. Otherwise (no token common to all ids), return a combo of the first two
//     characters of each id, concatenated, using up to the first 5 ids
//     (e.g. {alchemy-evm, blockpi-base} -> "albl").
func LaneName(ids []string) string {
	if len(ids) == 0 {
		return ""
	}

	// Deterministic order so "earliest in the name" and the combo fallback do
	// not depend on caller ordering.
	sorted := append([]string(nil), ids...)
	slices.Sort(sorted)

	// Per-id token sets (for the "shared by all" test).
	sets := make([]map[string]struct{}, len(sorted))
	for i, id := range sorted {
		s := make(map[string]struct{}, 4)
		for _, t := range strings.Split(id, "-") {
			if t != "" {
				s[t] = struct{}{}
			}
		}
		sets[i] = s
	}

	// (1) Walk the first id's tokens in order; the first one present in EVERY
	// id wins. Because we walk in name order, ties resolve to the earlier token.
	for _, t := range strings.Split(sorted[0], "-") {
		if t == "" {
			continue
		}
		sharedByAll := true
		for _, s := range sets[1:] {
			if _, ok := s[t]; !ok {
				sharedByAll = false
				break
			}
		}
		if sharedByAll {
			return t
		}
	}

	// (2) Combo fallback: first two chars of each id (up to 5), concatenated.
	var b strings.Builder
	for i, id := range sorted {
		if i >= 5 {
			break
		}
		if len(id) >= 2 {
			b.WriteString(id[:2])
		} else {
			b.WriteString(id)
		}
	}
	return b.String()
}
