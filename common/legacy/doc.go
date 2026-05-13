// Package legacy is the ONE place in the codebase that knows about
// pre-rewrite eRPC selection-policy config (`routingStrategy`,
// `scoreMultipliers`, `selectionPolicy.evalFunction`, `resampleExcluded`,
// the `ROUTING_POLICY_*` env-var defaults, etc.).
//
// It runs during top-level config unmarshal: legacy YAML lands in
// `WidenedConfig`, `Translate` synthesizes the equivalent new-shape
// `selectionPolicy.eval`, and downstream code sees the new shape only.
//
// When the deprecation cycle is over (two minor releases), this package
// is deleted; legacy YAML stops loading.
//
// See specs/selection-policy/plan.md §12 for the full mapping table.
package legacy
