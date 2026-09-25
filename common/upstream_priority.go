package common

import (
	"context"
	"sort"
)

// upstreamPriority returns an upstream's configured routing.priority
// (0 when unset or when the upstream has no config).
func upstreamPriority(u Upstream) int {
	if u == nil {
		return 0
	}
	return u.Config().EffectivePriority()
}

// HasPriorityTiers reports whether ups spans more than one distinct
// routing.priority value. When false, priority ordering is a no-op.
func HasPriorityTiers(ups []Upstream) bool {
	if len(ups) < 2 {
		return false
	}
	first := upstreamPriority(ups[0])
	for _, u := range ups[1:] {
		if upstreamPriority(u) != first {
			return true
		}
	}
	return false
}

// SortUpstreamsByPriority orders ups by ascending routing.priority
// (lower tier first). The sort is stable, so the incoming order (the
// selection policy's score / sticky order) is preserved inside each
// tier. Every tier is kept: nothing is filtered out, so the request can
// fail over from a cheap tier to a more expensive one within the same
// request.
//
// The input is never mutated (the policy engine hands out its shared
// cached slice). When there is at most one distinct tier the input slice
// is returned as-is, so configs without priorities keep their exact
// ordering and pay no allocation.
func SortUpstreamsByPriority(ups []Upstream) []Upstream {
	if !HasPriorityTiers(ups) {
		return ups
	}
	out := make([]Upstream, len(ups))
	copy(out, ups)
	sort.SliceStable(out, func(i, j int) bool {
		return upstreamPriority(out[i]) < upstreamPriority(out[j])
	})
	return out
}

// HedgeLegContextKey marks a context executing a hedge leg (not the
// primary attempt) of a network-level hedge race.
const HedgeLegContextKey ContextKey = "hedgeLeg"

// WithHedgeLeg returns ctx marked as a hedge leg.
func WithHedgeLeg(ctx context.Context) context.Context {
	return context.WithValue(ctx, HedgeLegContextKey, true)
}

// IsHedgeLeg reports whether ctx executes a hedge leg.
func IsHedgeLeg(ctx context.Context) bool {
	v, _ := ctx.Value(HedgeLegContextKey).(bool)
	return v
}

// HedgeTierFilter returns an upstream filter for a hedge leg that keeps
// hedges inside the request's cheapest tier, or nil when ups has no
// priority tiers (hedging behaves exactly as before). A hedge is a
// speculative, parallel extra call. Letting it spill into a more
// expensive tier would silently pay for traffic a healthy cheap tier
// could have served, so only sequential failover (sweep / retry after a
// real failure) is allowed to cross tiers.
func HedgeTierFilter(ups []Upstream) func(Upstream) bool {
	if !HasPriorityTiers(ups) {
		return nil
	}
	lowest := upstreamPriority(ups[0])
	for _, u := range ups[1:] {
		if p := upstreamPriority(u); p < lowest {
			lowest = p
		}
	}
	return func(u Upstream) bool { return upstreamPriority(u) <= lowest }
}
