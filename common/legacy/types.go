package legacy

import (
	"github.com/erpc/erpc/common"
)

// scoreMultiplier mirrors the deleted `common.ScoreMultiplierConfig`. We
// re-declare it here so legacy YAML still parses without leaking the type
// into the new-shape config.
type scoreMultiplier struct {
	Network         string                     `yaml:"network,omitempty" json:"network,omitempty"`
	Method          string                     `yaml:"method,omitempty" json:"method,omitempty"`
	Finality        []common.DataFinalityState `yaml:"finality,omitempty" json:"finality,omitempty"`
	Overall         *float64                   `yaml:"overall,omitempty" json:"overall,omitempty"`
	ErrorRate       *float64                   `yaml:"errorRate,omitempty" json:"errorRate,omitempty"`
	RespLatency     *float64                   `yaml:"respLatency,omitempty" json:"respLatency,omitempty"`
	TotalRequests   *float64                   `yaml:"totalRequests,omitempty" json:"totalRequests,omitempty"`
	ThrottledRate   *float64                   `yaml:"throttledRate,omitempty" json:"throttledRate,omitempty"`
	BlockHeadLag    *float64                   `yaml:"blockHeadLag,omitempty" json:"blockHeadLag,omitempty"`
	FinalizationLag *float64                   `yaml:"finalizationLag,omitempty" json:"finalizationLag,omitempty"`
	Misbehaviors    *float64                   `yaml:"misbehaviors,omitempty" json:"misbehaviors,omitempty"`
}

// routingConfig mirrors the deleted `common.RoutingConfig`.
type routingConfig struct {
	ScoreMultipliers     []*scoreMultiplier `yaml:"scoreMultipliers,omitempty" json:"scoreMultipliers,omitempty"`
	ScoreLatencyQuantile float64            `yaml:"scoreLatencyQuantile,omitempty" json:"scoreLatencyQuantile,omitempty"`
}

// selectionPolicy mirrors the deleted legacy `SelectionPolicyConfig`
// shape — `EvalFunction` is the source string, NOT a compiled callable
// (the translator only reads source).
type selectionPolicy struct {
	EvalInterval     common.Duration `yaml:"evalInterval,omitempty" json:"evalInterval,omitempty"`
	EvalFunction     string          `yaml:"evalFunction,omitempty" json:"evalFunction,omitempty"`
	EvalPerMethod    bool            `yaml:"evalPerMethod,omitempty" json:"evalPerMethod,omitempty"`
	ResampleExcluded bool            `yaml:"resampleExcluded,omitempty" json:"resampleExcluded,omitempty"`
	ResampleInterval common.Duration `yaml:"resampleInterval,omitempty" json:"resampleInterval,omitempty"`
	ResampleCount    int             `yaml:"resampleCount,omitempty" json:"resampleCount,omitempty"`
}

// WidenedProject is the projection used by the translator. Legacy fields
// + new fields side-by-side. After Translate, the legacy slots are
// stripped and only the new shape is read by the rest of the codebase.
type WidenedProject struct {
	// Legacy project-level scoring fields. None of these survive after
	// translation; their values flow into the synthesized
	// `selectionPolicy.eval` on every network.
	RoutingStrategy        string          `yaml:"routingStrategy,omitempty" json:"routingStrategy,omitempty"`
	ScoreGranularity       string          `yaml:"scoreGranularity,omitempty" json:"scoreGranularity,omitempty"`
	ScorePenaltyDecayRate  float64         `yaml:"scorePenaltyDecayRate,omitempty" json:"scorePenaltyDecayRate,omitempty"`
	ScoreSwitchHysteresis  float64         `yaml:"scoreSwitchHysteresis,omitempty" json:"scoreSwitchHysteresis,omitempty"`
	ScoreMinSwitchInterval common.Duration `yaml:"scoreMinSwitchInterval,omitempty" json:"scoreMinSwitchInterval,omitempty"`
	ScoreMetricsMode       string          `yaml:"scoreMetricsMode,omitempty" json:"scoreMetricsMode,omitempty"`
	ScoreMetricsWindowSize common.Duration `yaml:"scoreMetricsWindowSize,omitempty" json:"scoreMetricsWindowSize,omitempty"`
	ScoreRefreshInterval   common.Duration `yaml:"scoreRefreshInterval,omitempty" json:"scoreRefreshInterval,omitempty"`
}

// WidenedUpstream pairs the new upstream config with any legacy `routing`
// block the user wrote.
type WidenedUpstream struct {
	Routing *routingConfig `yaml:"routing,omitempty" json:"routing,omitempty"`
}

// WidenedNetwork pairs the new network config with any legacy
// `selectionPolicy` block the user wrote.
type WidenedNetwork struct {
	SelectionPolicy *selectionPolicy `yaml:"selectionPolicy,omitempty" json:"selectionPolicy,omitempty"`
}

// LegacyPolicySnapshot is the test-only constructor input for a legacy
// selection-policy block. Used by translator unit tests to fabricate a
// `WidenedNetwork` without having to round-trip through YAML.
type LegacyPolicySnapshot struct {
	EvalInterval     common.Duration
	EvalFunction     string
	EvalPerMethod    bool
	ResampleExcluded bool
	ResampleInterval common.Duration
	ResampleCount    int
}

// WidenedSelectionPolicyForTest constructs a *selectionPolicy from a
// LegacyPolicySnapshot. Test-only.
func WidenedSelectionPolicyForTest(s LegacyPolicySnapshot) *selectionPolicy {
	return &selectionPolicy{
		EvalInterval:     s.EvalInterval,
		EvalFunction:     s.EvalFunction,
		EvalPerMethod:    s.EvalPerMethod,
		ResampleExcluded: s.ResampleExcluded,
		ResampleInterval: s.ResampleInterval,
		ResampleCount:    s.ResampleCount,
	}
}

// ScoreMultiplierSnapshot is the test-only public projection of the
// legacy `scoreMultiplier` shape. Zero values mean "no override on that
// metric" — pass non-zero numbers for the weights you want emitted.
type ScoreMultiplierSnapshot struct {
	Network         string
	Method          string
	Finality        []common.DataFinalityState
	Overall         float64
	ErrorRate       float64
	RespLatency     float64
	TotalRequests   float64
	ThrottledRate   float64
	BlockHeadLag    float64
	FinalizationLag float64
	Misbehaviors    float64
}

// WidenedUpstreamForTest constructs a WidenedUpstream with the given
// legacy score multipliers + latency quantile. Test-only constructor
// (mirrors WidenedSelectionPolicyForTest). Pass a zero-length slice for
// "no multipliers" — callers usually want at most one entry per upstream.
func WidenedUpstreamForTest(multipliers []ScoreMultiplierSnapshot, latencyQuantile float64) WidenedUpstream {
	if len(multipliers) == 0 && latencyQuantile == 0 {
		return WidenedUpstream{}
	}
	rc := &routingConfig{ScoreLatencyQuantile: latencyQuantile}
	ptr := func(v float64) *float64 {
		if v == 0 {
			return nil
		}
		return &v
	}
	for _, s := range multipliers {
		rc.ScoreMultipliers = append(rc.ScoreMultipliers, &scoreMultiplier{
			Network:         s.Network,
			Method:          s.Method,
			Finality:        s.Finality,
			Overall:         ptr(s.Overall),
			ErrorRate:       ptr(s.ErrorRate),
			RespLatency:     ptr(s.RespLatency),
			TotalRequests:   ptr(s.TotalRequests),
			ThrottledRate:   ptr(s.ThrottledRate),
			BlockHeadLag:    ptr(s.BlockHeadLag),
			FinalizationLag: ptr(s.FinalizationLag),
			Misbehaviors:    ptr(s.Misbehaviors),
		})
	}
	return WidenedUpstream{Routing: rc}
}
