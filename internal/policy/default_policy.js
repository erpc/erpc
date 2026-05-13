// Default selection policy — applied when a user has not supplied
// `selectionPolicy.eval`. See specs/selection-policy/feature.md §7.
//
// This source is embedded into the binary via internal/policy/default_policy.go
// and overrides `common.DefaultSelectionPolicySource` at engine boot, so any
// SelectionPolicyConfig that left `eval` blank gets this richer behavior
// instead of the bare identity passthrough that common/defaults.go falls
// back to.
(upstreams, ctx) =>
  upstreams
    .sortByScore(BALANCED)
    .preferGroup('default', { minHealthy: 1, fallback: 'fallback' })
    .stickyPrimary({ hysteresis: 0.10, minSwitchInterval: '30s' })
    .probeExcluded({ reAdmitAfter: '5m', maxConcurrent: 1 })
