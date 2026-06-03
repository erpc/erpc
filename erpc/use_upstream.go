package erpc

import "github.com/erpc/erpc/common"

// filterUpstreamsByUseUpstream restricts a request's candidate upstream pool to
// those whose id matches the `use-upstream` directive.
//
// This is applied once, where the request's upstream set is established, so the
// directive is honored uniformly by every downstream consumer — the consensus
// participant fan-out (and its maxParticipants count), the sequential sweep, and
// the stateful-method guard. NextUpstream still enforces the directive per-slot;
// filtering the pool up front keeps the request's upstream set consistent with
// what will actually be dispatched.
//
// Semantics:
//   - Empty pattern or empty pool is a no-op (returns ups unchanged).
//   - A pattern that matches nothing leaves the pool untouched, preserving the
//     existing no-upstreams handling/error path rather than emptying the pool.
//   - Order is preserved.
func filterUpstreamsByUseUpstream(ups []common.Upstream, pattern string) []common.Upstream {
	if pattern == "" || len(ups) == 0 {
		return ups
	}
	filtered := make([]common.Upstream, 0, len(ups))
	for _, u := range ups {
		if match, err := common.WildcardMatch(pattern, u.Id()); err == nil && match {
			filtered = append(filtered, u)
		}
	}
	if len(filtered) == 0 {
		return ups
	}
	return filtered
}
