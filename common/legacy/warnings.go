package legacy

import "fmt"

// Warning message templates. Each anchors at the migration guide
// (docs/pages/migration/selection-policy.mdx) so operators can find the
// full context.
const migrationDoc = "https://docs.erpc.cloud/migration/selection-policy"

func warnRoutingStrategy(strategy string) string {
	return fmt.Sprintf(
		"[deprecated config] routingStrategy=%q is deprecated; translated to selectionPolicy.eval. See %s#routing-strategy",
		strategy, migrationDoc,
	)
}

func warnScoreMultipliers() string {
	return fmt.Sprintf(
		"[deprecated config] upstream.routing.scoreMultipliers is deprecated; translated to a per-upstream weights function passed to sortByScore. See %s#score-multipliers",
		migrationDoc,
	)
}

func warnScoreMetricsMode(mode string) string {
	return fmt.Sprintf(
		"[deprecated config] scoreMetricsMode=%q is no longer used; the new erpc_selection_* metrics have fixed cardinality. See %s#score-metrics-mode",
		mode, migrationDoc,
	)
}

// WarnLegacySelectionPolicy emits a warning when a network used the
// legacy selectionPolicy.evalFunction field.
func WarnLegacySelectionPolicy() string {
	return fmt.Sprintf(
		"[deprecated config] selectionPolicy.evalFunction is deprecated; wrapped into the new selectionPolicy.eval shape. Migrate manually for clarity (the new chainable stdlib is more expressive). See %s#eval-function",
		migrationDoc,
	)
}

// WarnResampleExcluded emits a warning when a network used resampleExcluded.
func WarnResampleExcluded() string {
	return fmt.Sprintf(
		"[deprecated config] selectionPolicy.resampleExcluded is deprecated; translated to .probeExcluded() (deterministic time-based re-admission). See %s#resample-excluded",
		migrationDoc,
	)
}

// WarnRoutingPolicyEnvVars emits a warning when the implicit
// `ROUTING_POLICY_*` env-var fallback policy is in play.
func WarnRoutingPolicyEnvVars() string {
	return fmt.Sprintf(
		"[deprecated config] ROUTING_POLICY_* env vars are deprecated; values were inlined into the synthesized selectionPolicy.eval. See %s#routing-policy-env-vars",
		migrationDoc,
	)
}
