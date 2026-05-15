package legacy

import (
	"fmt"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
)

// Translate inspects (prj, ups, networks) for legacy fields and, if any
// are present, synthesizes a new-shape selectionPolicy.EvalFunc on each
// network. Returns a list of deprecation warnings the caller should log.
//
// Cardinal rule: NEVER mutate the new-shape config in a way that loses
// information the user explicitly set. The translator only fills in
// `selectionPolicy.evalFunc` when:
//
//   - the network had no explicit `selectionPolicy` block; OR
//   - the network had a `selectionPolicy.evalFunction` (legacy field) and
//     no `selectionPolicy.evalFunc` (new field).
//
// In both cases the synthesized source is assigned to `cfg.EvalFunc`;
// SetDefaults will compile it into `cfg.CompiledProgram` and fill the
// remaining unset fields (EvalInterval, EvalTimeout).
func Translate(
	prj WidenedProject,
	upstreams []WidenedUpstream,
	upstreamConfigs []*common.UpstreamConfig,
	networks []WidenedNetwork,
	networkConfigs []*common.NetworkConfig,
) ([]string, error) {
	semantic := hasSemanticLegacy(prj, upstreams, networks)
	inert := hasInertLegacy(prj)
	if !semantic && !inert {
		return nil, nil
	}

	warnings := make([]string, 0, 6)
	if prj.RoutingStrategy != "" {
		warnings = append(warnings, warnRoutingStrategy(prj.RoutingStrategy))
	}
	if prj.ScoreMetricsMode != "" {
		warnings = append(warnings, warnScoreMetricsMode(prj.ScoreMetricsMode))
	}
	if prj.ScoreMetricsWindowSize != 0 {
		warnings = append(warnings, warnInertField("scoreMetricsWindowSize",
			"metrics windowing is owned by health.Tracker; this field has no behavioral effect"))
	}
	if prj.ScoreGranularity != "" {
		warnings = append(warnings, warnInertField("scoreGranularity",
			"replaced by selectionPolicy.evalPerMethod (false=upstream, true=method)"))
	}
	if prj.ScoreRefreshInterval != 0 {
		warnings = append(warnings, warnInertField("scoreRefreshInterval",
			"replaced by selectionPolicy.evalInterval"))
	}
	if prj.ScorePenaltyDecayRate != 0 {
		warnings = append(warnings, warnInertField("scorePenaltyDecayRate",
			"no equivalent; sortByScore uses a fresh metric snapshot every tick"))
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

	// If only inert legacy fields are present, emit warnings but DON'T
	// synthesize an eval — the network's selectionPolicy stays nil so
	// SetDefaults installs the canonical default policy. Operators
	// removing their score-multiplier blocks while leaving a stray
	// `scoreMetricsWindowSize` knob get the FULL default policy
	// (removeCordoned + keepHealthy + preferTag + sortByScore + sticky
	// + probeExcluded), not a minimal score-only stand-in.
	if !semantic {
		return warnings, nil
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
		if nwCfg.SelectionPolicy != nil && strings.TrimSpace(nwCfg.SelectionPolicy.EvalFunc) != "" {
			continue
		}

		evalSrc := synthesizeEval(prj, upstreamConfigs, upstreams, legacyNw)
		if evalSrc == "" {
			continue
		}

		if nwCfg.SelectionPolicy == nil {
			nwCfg.SelectionPolicy = &common.SelectionPolicyConfig{}
		}
		nwCfg.SelectionPolicy.EvalFunc = evalSrc

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

// hasSemanticLegacy returns true if the user wrote ANY legacy field that
// the synthesizer can translate into a meaningful eval. These are the
// fields that ALTER selection behavior:
//
//   - prj.RoutingStrategy            ("round-robin" → rotateBy; "score-based" → sortByScore)
//   - prj.ScoreSwitchHysteresis      (stickyPrimary({hysteresis}))
//   - prj.ScoreMinSwitchInterval     (stickyPrimary({minSwitchInterval}))
//   - upstream.routing.scoreMultipliers / scoreLatencyQuantile
//                                    (per-id weights map handed to sortByScore)
//   - network.selectionPolicy.evalFunction  (wrapped as legacy fn)
//   - network.selectionPolicy.resample*     (appended as probeExcluded)
//
// If none of these are set, the translator must leave selectionPolicy
// nil so the canonical default policy installs unchanged.
func hasSemanticLegacy(prj WidenedProject, ups []WidenedUpstream, nws []WidenedNetwork) bool {
	if prj.RoutingStrategy != "" ||
		prj.ScoreSwitchHysteresis != 0 ||
		prj.ScoreMinSwitchInterval != 0 {
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

// hasInertLegacy returns true if the user wrote ANY project-level legacy
// field that has no behavioral mapping in the new system. The translator
// emits a deprecation warning per inert field but does NOT synthesize an
// eval on their account.
func hasInertLegacy(prj WidenedProject) bool {
	return prj.ScoreGranularity != "" ||
		prj.ScorePenaltyDecayRate != 0 ||
		prj.ScoreMetricsMode != "" ||
		prj.ScoreMetricsWindowSize != 0 ||
		prj.ScoreRefreshInterval != 0
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

// TranslateFromConfig is the LoadConfig hook (assigned to
// common.LegacyTranslateFn). It walks every project's stashed legacy
// fields (captured by the UnmarshalYAML shadows on ProjectConfig /
// UpstreamConfig / SelectionPolicyConfig), runs the existing Translate
// over them, and clears the stashes so the runtime sees a clean
// canonical Config.
func TranslateFromConfig(cfg *common.Config) ([]string, error) {
	if cfg == nil {
		return nil, nil
	}
	var allWarnings []string
	for _, prj := range cfg.Projects {
		if prj == nil {
			continue
		}
		wp := widenedProjectFromConfig(prj)
		wUps := widenedUpstreamsFromConfig(prj.Upstreams)
		wNws := widenedNetworksFromConfig(prj.Networks)

		// Fast path: no legacy fields anywhere → skip. Inert legacy
		// (warning-only) still goes through Translate so the warning
		// fires; only the genuinely-clean case short-circuits.
		if !hasSemanticLegacy(wp, wUps, wNws) && !hasInertLegacy(wp) {
			clearLegacyStashes(prj)
			continue
		}

		warns, err := Translate(wp, wUps, prj.Upstreams, wNws, prj.Networks)
		if err != nil {
			return allWarnings, fmt.Errorf("project %q: %w", prj.Id, err)
		}
		for _, w := range warns {
			allWarnings = append(allWarnings, fmt.Sprintf("project %q: %s", prj.Id, w))
		}
		clearLegacyStashes(prj)
	}
	return allWarnings, nil
}

func widenedProjectFromConfig(prj *common.ProjectConfig) WidenedProject {
	if prj.LegacyProject == nil {
		return WidenedProject{}
	}
	lp := prj.LegacyProject
	return WidenedProject{
		RoutingStrategy:        lp.RoutingStrategy,
		ScoreGranularity:       lp.ScoreGranularity,
		ScorePenaltyDecayRate:  lp.ScorePenaltyDecayRate,
		ScoreSwitchHysteresis:  lp.ScoreSwitchHysteresis,
		ScoreMinSwitchInterval: lp.ScoreMinSwitchInterval,
		ScoreMetricsMode:       lp.ScoreMetricsMode,
		ScoreMetricsWindowSize: lp.ScoreMetricsWindowSize,
		ScoreRefreshInterval:   lp.ScoreRefreshInterval,
	}
}

func widenedUpstreamsFromConfig(ups []*common.UpstreamConfig) []WidenedUpstream {
	out := make([]WidenedUpstream, len(ups))
	for i, u := range ups {
		if u == nil || u.LegacyRouting == nil {
			continue
		}
		out[i] = WidenedUpstream{Routing: routingConfigFromCommon(u.LegacyRouting)}
	}
	return out
}

func widenedNetworksFromConfig(nws []*common.NetworkConfig) []WidenedNetwork {
	out := make([]WidenedNetwork, len(nws))
	for i, n := range nws {
		if n == nil || n.SelectionPolicy == nil || n.SelectionPolicy.LegacySelectionPolicy == nil {
			continue
		}
		lp := n.SelectionPolicy.LegacySelectionPolicy
		out[i] = WidenedNetwork{
			SelectionPolicy: &selectionPolicy{
				// EvalInterval / EvalPerMethod live on the canonical
				// SelectionPolicyConfig; the translator only needs them
				// when the user wrote a legacy block AND no modern eval.
				// Copy them over so synthesized eval keeps the cadence.
				EvalInterval:     n.SelectionPolicy.EvalInterval,
				EvalPerMethod:    n.SelectionPolicy.EvalPerMethod,
				EvalFunction:     lp.EvalFunction,
				ResampleExcluded: lp.ResampleExcluded,
				ResampleInterval: lp.ResampleInterval,
				ResampleCount:    lp.ResampleCount,
			},
		}
	}
	return out
}

func routingConfigFromCommon(lr *common.LegacyUpstreamRouting) *routingConfig {
	rc := &routingConfig{ScoreLatencyQuantile: lr.ScoreLatencyQuantile}
	for _, m := range lr.ScoreMultipliers {
		if m == nil {
			continue
		}
		rc.ScoreMultipliers = append(rc.ScoreMultipliers, &scoreMultiplier{
			Network:         m.Network,
			Method:          m.Method,
			Finality:        m.Finality,
			Overall:         m.Overall,
			ErrorRate:       m.ErrorRate,
			RespLatency:     m.RespLatency,
			TotalRequests:   m.TotalRequests,
			ThrottledRate:   m.ThrottledRate,
			BlockHeadLag:    m.BlockHeadLag,
			FinalizationLag: m.FinalizationLag,
			Misbehaviors:    m.Misbehaviors,
		})
	}
	return rc
}

func clearLegacyStashes(prj *common.ProjectConfig) {
	prj.LegacyProject = nil
	for _, u := range prj.Upstreams {
		if u != nil {
			u.LegacyRouting = nil
		}
	}
	if prj.UpstreamDefaults != nil {
		prj.UpstreamDefaults.LegacyRouting = nil
	}
	for _, n := range prj.Networks {
		if n != nil && n.SelectionPolicy != nil {
			n.SelectionPolicy.LegacySelectionPolicy = nil
		}
	}
}
