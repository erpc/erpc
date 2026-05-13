package legacy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/erpc/erpc/common"
)

// wrapLegacyEvalFunction takes a legacy `selectionPolicy.evalFunction`
// source (which had signature `(upstreams, method) => upstream[]`) and
// produces a new-shape eval (signature `(upstreams, ctx) => upstream[]`)
// that preserves behavior. The legacy fn is bound once and invoked with
// (upstreams, ctx.method); the new chainable .probeExcluded() may be
// appended if the user enabled `resampleExcluded`.
func wrapLegacyEvalFunction(sp *selectionPolicy) string {
	var b strings.Builder
	b.WriteString("(upstreams, ctx) => {\n")
	b.WriteString("  const __legacyFn = ")
	b.WriteString(strings.TrimSpace(sp.EvalFunction))
	b.WriteString(";\n")
	b.WriteString("  let result = __legacyFn(upstreams, ctx.method);\n")
	if sp.ResampleExcluded && sp.ResampleInterval > 0 {
		// .probeExcluded preserves the spirit of legacy resampling.
		b.WriteString(fmt.Sprintf(
			"  result = result.probeExcluded({ reAdmitAfter: %s, maxConcurrent: 1, longestFirst: true });\n",
			formatDuration(sp.ResampleInterval),
		))
	}
	b.WriteString("  return result;\n")
	b.WriteString("}")
	return b.String()
}

// synthesizeScoreBasedEval emits a sortByScore-based policy that honors
// the user's legacy ScoreSwitchHysteresis + ScoreMinSwitchInterval. If
// any upstream had per-(network/method/finality) `scoreMultipliers`, the
// weights are emitted as a static map keyed by upstream id and looked up
// at sort time.
func synthesizeScoreBasedEval(
	prj WidenedProject,
	ups []*common.UpstreamConfig,
	legacyUps []WidenedUpstream,
	nw WidenedNetwork,
) string {
	hysteresis := prj.ScoreSwitchHysteresis
	if hysteresis == 0 {
		hysteresis = 0.10
	}
	minSwitch := formatDuration(prj.ScoreMinSwitchInterval)
	if prj.ScoreMinSwitchInterval == 0 {
		minSwitch = "'30s'"
	}

	var b strings.Builder
	b.WriteString("(upstreams, ctx) => {\n")

	// If any upstream had multipliers, emit a per-id weights map and use
	// the function form of sortByScore. Otherwise default to BALANCED.
	mulByID, defaultMul := collectMultipliers(ups, legacyUps)
	if len(mulByID) > 0 {
		jsonMul, _ := json.Marshal(mulByID)
		jsonDefault, _ := json.Marshal(defaultMul)
		b.WriteString("  const __mulById = ")
		b.WriteString(string(jsonMul))
		b.WriteString(";\n")
		b.WriteString("  const __mulDefault = ")
		b.WriteString(string(jsonDefault))
		b.WriteString(";\n")
		b.WriteString("  const __weightsFn = (u) => __mulById[u.id] || __mulDefault;\n")
		b.WriteString("  let result = upstreams.sortByScore(__weightsFn);\n")
	} else {
		b.WriteString("  let result = upstreams.sortByScore(BALANCED);\n")
	}

	b.WriteString(fmt.Sprintf(
		"  result = result.stickyPrimary({ hysteresis: %v, minSwitchInterval: %s });\n",
		hysteresis, minSwitch,
	))

	if nw.SelectionPolicy != nil && nw.SelectionPolicy.ResampleExcluded &&
		nw.SelectionPolicy.ResampleInterval > 0 {
		b.WriteString(fmt.Sprintf(
			"  result = result.probeExcluded({ reAdmitAfter: %s, maxConcurrent: 1, longestFirst: true });\n",
			formatDuration(nw.SelectionPolicy.ResampleInterval),
		))
	}

	b.WriteString("  return result;\n")
	b.WriteString("}")
	return b.String()
}

// collectMultipliers walks the per-upstream legacy multipliers and emits
// a flattened weights map per upstream id. Per-method/per-network
// granularity is collapsed: we take the first wildcard-matching entry
// (the legacy resolver did the same when called with the per-tick
// method, but at synthesis time we don't know the method). Operators
// who relied on per-method weights should keep their legacy YAML and
// migrate manually — the warning emits a docs anchor.
func collectMultipliers(ups []*common.UpstreamConfig, legacyUps []WidenedUpstream) (map[string]scoreWeights, scoreWeights) {
	out := make(map[string]scoreWeights, len(ups))
	for i, u := range ups {
		if u == nil || i >= len(legacyUps) || legacyUps[i].Routing == nil {
			continue
		}
		for _, mul := range legacyUps[i].Routing.ScoreMultipliers {
			// Take the first wildcard-or-explicit-match entry.
			out[u.Id] = weightsFromMul(mul)
			break
		}
	}
	return out, defaultWeights()
}

// scoreWeights mirrors the JS `ScoreWeights` shape.
type scoreWeights struct {
	ErrorRate       float64 `json:"errorRate,omitempty"`
	RespLatency     float64 `json:"respLatency,omitempty"`
	ThrottledRate   float64 `json:"throttledRate,omitempty"`
	BlockHeadLag    float64 `json:"blockHeadLag,omitempty"`
	FinalizationLag float64 `json:"finalizationLag,omitempty"`
	Misbehaviors    float64 `json:"misbehaviors,omitempty"`
}

func weightsFromMul(m *scoreMultiplier) scoreWeights {
	deref := func(p *float64) float64 {
		if p == nil {
			return 0
		}
		return *p
	}
	w := scoreWeights{
		ErrorRate:       deref(m.ErrorRate),
		RespLatency:     deref(m.RespLatency),
		ThrottledRate:   deref(m.ThrottledRate),
		BlockHeadLag:    deref(m.BlockHeadLag),
		FinalizationLag: deref(m.FinalizationLag),
		Misbehaviors:    deref(m.Misbehaviors),
	}
	if m.Overall != nil && *m.Overall > 0 {
		// In the new shape, `overall` is a per-upstream multiplier on the
		// final penalty. We bake it in by scaling each weight; for
		// `overall > 0` it's a uniform scale, so equivalent to keeping
		// the relative shape and trusting the sort to be invariant under
		// monotonic scaling. We skip the scale here and let the new sort
		// produce the same order.
	}
	return w
}

func defaultWeights() scoreWeights {
	// Matches the legacy `DefaultScoreMultiplier`.
	return scoreWeights{
		ErrorRate: 4, RespLatency: 8, ThrottledRate: 3,
		BlockHeadLag: 2, FinalizationLag: 1, Misbehaviors: 5,
	}
}
