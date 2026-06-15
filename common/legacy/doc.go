// Package legacy is the ONE place in the codebase that knows about
// pre-rewrite eRPC selection-policy config (`routingStrategy`,
// `scoreMultipliers`, `selectionPolicy.evalFunction`, `resampleExcluded`,
// the `ROUTING_POLICY_*` env-var defaults, etc.).
//
// It runs during top-level config unmarshal: legacy YAML lands in
// `WidenedConfig`, `Translate` synthesizes the equivalent new-shape
// `selectionPolicy.eval`, and downstream code sees the new shape only.
//
// At the next major release this package is deleted; legacy YAML stops
// loading. Until then, the only operator-visible effect of legacy config
// is a one-time deprecation warning per project at startup.
//
// See specs/selection-policy/plan.md §12 for the full mapping table.
package legacy
