package legacy

import (
	"fmt"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
)

// Translate inspects (prj, ups, networks) for legacy fields and, if any
// are present, synthesizes a new-shape selectionPolicy.Eval on each
// network. Returns a list of deprecation warnings the caller should log.
//
// Cardinal rule: NEVER mutate the new-shape config in a way that loses
// information the user explicitly set. The translator only fills in
// `selectionPolicy.eval` when:
//
//   - the network had no explicit `selectionPolicy` block; OR
//   - the network had a `selectionPolicy.evalFunction` (legacy field) and
//     no `selectionPolicy.eval` (new field).
//
// In both cases the synthesized eval is compiled and assigned to
// `cfg.Eval` + `cfg.CompiledProgram`. SetDefaults can still fill in any
// remaining unset fields (EvalInterval, EvalTimeout, DecisionHistory).
func Translate(
	prj WidenedProject,
	upstreams []WidenedUpstream,
	upstreamConfigs []*common.UpstreamConfig,
	networks []WidenedNetwork,
	networkConfigs []*common.NetworkConfig,
) ([]string, error) {
	if !hasAnyLegacy(prj, upstreams, networks) {
		return nil, nil
	}

	warnings := make([]string, 0, 4)
	if prj.RoutingStrategy != "" {
		warnings = append(warnings, warnRoutingStrategy(prj.RoutingStrategy))
	}
	if prj.ScoreMetricsMode != "" {
		warnings = append(warnings, warnScoreMetricsMode(prj.ScoreMetricsMode))
	}
	hasLegacyMul := false
	for _, u := range upstreams {
		if u.Routing != nil && len(u.Routing.ScoreMultipliers) > 0 {
			hasLegacyMul = true
			break
		}
	}
	if hasLegacyMul {
		warnings = append(warnings, warnScoreMultipliers())
	}

	for i := range networkConfigs {
		nwCfg := networkConfigs[i]
		var legacyNw WidenedNetwork
		if i < len(networks) {
			legacyNw = networks[i]
		}

		// Decide whether to synthesize. If the user already wrote
		// `selectionPolicy.eval` (new field), we leave it alone — even if
		// they ALSO wrote legacy fields, the new eval takes precedence.
		if nwCfg.SelectionPolicy != nil && strings.TrimSpace(nwCfg.SelectionPolicy.Eval) != "" {
			continue
		}

		evalSrc := synthesizeEval(prj, upstreamConfigs, upstreams, legacyNw)
		if evalSrc == "" {
			continue
		}

		if nwCfg.SelectionPolicy == nil {
			nwCfg.SelectionPolicy = &common.SelectionPolicyConfig{}
		}
		nwCfg.SelectionPolicy.Eval = evalSrc

		// Honor legacy evalInterval/evalPerMethod if the user wrote a
		// legacy selectionPolicy block.
		if legacyNw.SelectionPolicy != nil {
			if legacyNw.SelectionPolicy.EvalInterval > 0 && nwCfg.SelectionPolicy.EvalInterval == 0 {
				nwCfg.SelectionPolicy.EvalInterval = legacyNw.SelectionPolicy.EvalInterval
			}
			if legacyNw.SelectionPolicy.EvalPerMethod {
				nwCfg.SelectionPolicy.EvalPerMethod = true
			}
		}
	}

	return warnings, nil
}

func hasAnyLegacy(prj WidenedProject, ups []WidenedUpstream, nws []WidenedNetwork) bool {
	if prj.RoutingStrategy != "" || prj.ScoreGranularity != "" ||
		prj.ScorePenaltyDecayRate != 0 || prj.ScoreSwitchHysteresis != 0 ||
		prj.ScoreMinSwitchInterval != 0 || prj.ScoreMetricsMode != "" ||
		prj.ScoreMetricsWindowSize != 0 || prj.ScoreRefreshInterval != 0 {
		return true
	}
	for _, u := range ups {
		if u.Routing != nil && (len(u.Routing.ScoreMultipliers) > 0 || u.Routing.ScoreLatencyQuantile != 0) {
			return true
		}
	}
	for _, n := range nws {
		if n.SelectionPolicy != nil && (n.SelectionPolicy.EvalFunction != "" ||
			n.SelectionPolicy.ResampleExcluded || n.SelectionPolicy.ResampleInterval > 0 ||
			n.SelectionPolicy.ResampleCount > 0) {
			return true
		}
	}
	return false
}

// synthesizeEval emits a JS eval source string equivalent to the legacy
// fields. Branches by the strongest signal first:
//
//   - explicit legacy `evalFunction` → wrapped through new shape;
//   - `routingStrategy: round-robin` → rotateBy(ctx.tickCount);
//   - else → sortByScore + stickyPrimary + probeExcluded with the
//     project's legacy tuning baked in.
//
// Per-upstream `scoreMultipliers` are always gathered up-front and
// flow into whichever branch wins (the legacy eval-function wrapper
// uses them to pre-sort; the score-based branch hands them directly
// to sortByScore). This keeps the translation lossless when a user
// had BOTH evalFunction AND scoreMultipliers set.
func synthesizeEval(
	prj WidenedProject,
	ups []*common.UpstreamConfig,
	legacyUps []WidenedUpstream,
	nw WidenedNetwork,
) string {
	mulByID, defaultMul := collectMultipliers(ups, legacyUps)
	if nw.SelectionPolicy != nil && strings.TrimSpace(nw.SelectionPolicy.EvalFunction) != "" {
		return wrapLegacyEvalFunction(nw.SelectionPolicy, mulByID, defaultMul)
	}
	if strings.EqualFold(prj.RoutingStrategy, "round-robin") {
		// `rotateBy` rotates each tick — uses ctx.tickCount.
		return `(upstreams, ctx) => upstreams.rotateBy(ctx.tickCount)`
	}
	// Score-based default — bake in any project-level hysteresis +
	// min-switch interval, and emit per-upstream weights for legacy
	// scoreMultipliers when present.
	return synthesizeScoreBasedEval(prj, mulByID, defaultMul, nw)
}

// formatDuration returns "30s" for time.Duration(30s). Used to inline
// legacy duration values into the synthesized JS.
func formatDuration(d common.Duration) string {
	if d <= 0 {
		return "0"
	}
	return fmt.Sprintf("'%s'", time.Duration(d).String())
}
