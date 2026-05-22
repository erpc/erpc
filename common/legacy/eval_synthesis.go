package legacy

import (
	"fmt"
	"strings"
)

// wrapLegacyEvalFunction takes a legacy `selectionPolicy.evalFunction`
// source (which had signature `(upstreams, method) => upstream[]`) and
// produces a new-shape eval (signature `(upstreams, ctx) => upstream[]`)
// that preserves behavior.
//
// Legacy semantics: the upstreams arg passed to the user's eval-function
// was the ALREADY-SCORED list (the scoring loop ran first; the eval-
// function filtered/reordered on top). We reproduce that by sorting with
// `sortByScore(PREFER_FASTEST)` before invoking the legacy fn. Any
// per-upstream `routing.scoreMultipliers` are first-class config now, so
// they are attached to each upstream as `u.scoreMultipliers` at runtime
// and merged in by `sortByScore` automatically — no static weights map
// needs to be emitted here.
//
// `.probeExcluded()` is appended at the end if the user enabled
// `resampleExcluded`.
func wrapLegacyEvalFunction(sp *selectionPolicy, latencyQuantile string) string {
	var b strings.Builder
	b.WriteString("(upstreams, ctx) => {\n")
	b.WriteString("  const __legacyFn = ")
	b.WriteString(strings.TrimSpace(sp.EvalFunction))
	b.WriteString(";\n")
	b.WriteString(fmt.Sprintf("  const __sorted = upstreams.sortByScore(PREFER_FASTEST%s);\n", scoreOpts(latencyQuantile)))
	b.WriteString("  let result = __legacyFn(__sorted, ctx.method);\n")

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
// the user's legacy ScoreSwitchHysteresis + ScoreMinSwitchInterval and
// (when set) scoreLatencyQuantile. Per-upstream `scoreMultipliers` are
// first-class config and flow through `u.scoreMultipliers` at runtime —
// `sortByScore(PREFER_FASTEST)` merges them automatically — so this
// synthesizer no longer emits any per-id weights map.
func synthesizeScoreBasedEval(prj WidenedProject, nw WidenedNetwork, latencyQuantile string) string {
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
	b.WriteString(fmt.Sprintf("  let result = upstreams.sortByScore(PREFER_FASTEST%s);\n", scoreOpts(latencyQuantile)))
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

// scoreOpts renders the `sortByScore` second-argument when a latency
// quantile override is present, e.g. `, { latencyQuantile: 'p90' }`.
// Returns "" for the common no-override case so the emitted call stays
// `sortByScore(PREFER_FASTEST)`.
func scoreOpts(latencyQuantile string) string {
	if latencyQuantile == "" {
		return ""
	}
	return fmt.Sprintf(", { latencyQuantile: '%s' }", latencyQuantile)
}

// firstLatencyQuantile returns the first non-zero `routing.scoreLatencyQuantile`
// across the upstreams, mapped to a `pNN` label sortByScore understands.
// Legacy scoring used a single quantile; per-upstream divergence isn't
// expressible as one sortByScore opt, so the first wins.
func firstLatencyQuantile(legacyUps []WidenedUpstream) string {
	for _, u := range legacyUps {
		if u.Routing != nil && u.Routing.ScoreLatencyQuantile > 0 {
			return latencyQuantileLabel(u.Routing.ScoreLatencyQuantile)
		}
	}
	return ""
}

// latencyQuantileLabel snaps a 0..1 quantile to the nearest precomputed
// bucket label. Mirrors the snapping in internal/policy/eval.go's
// `latencyP`, so the synthesized opt lines up with the runtime metric.
func latencyQuantileLabel(q float64) string {
	switch {
	case q <= 0:
		return ""
	case q <= 0.50:
		return "p50"
	case q <= 0.70:
		return "p70"
	case q <= 0.90:
		return "p90"
	case q <= 0.95:
		return "p95"
	default:
		return "p99"
	}
}
