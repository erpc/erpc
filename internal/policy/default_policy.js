// Default selection policy. Applied when `selectionPolicy.eval` is omitted.
// See specs/selection-policy/feature.md §7.
//
// Designed to be conservative and chain-agnostic:
//
//   1. Drop only what's clearly broken regardless of chain — failsafe-cordoned
//      upstreams, and upstreams whose error rate is >80% over the metric
//      window. Lag and latency vary too much per chain (10 blocks ≈ 120s on
//      Ethereum vs ≈ 2.5s on Arbitrum) and feed into scoring instead.
//
//   2. Safety net: if filters wipe everything (e.g. project-wide outage),
//      serve from the original set rather than fail closed. Better a flaky
//      response than no response.
//
//   3. Tier: primary = `group !== 'fallback'`, fallback = `group ==='fallback'`.
//      This matches the long-standing eRPC convention so existing configs
//      keep working without setting `group: 'default'` on every upstream.
//
//   4. Sort by the BALANCED composite (errorRate, latency, throttling, lag,
//      misbehaviors).
//
//   5. Sticky primary with 10% hysteresis + 30s cooldown so a marginal lead
//      doesn't flap upstreams on every tick.
//
//   6. Deterministically re-admit one excluded upstream every 5 minutes so
//      recovered upstreams get traffic again and their metrics refresh.
(upstreams, ctx) =>
  upstreams
    .removeCordoned()
    .removeByErrorRate(0.8)
    .whenEmpty(() => upstreams)
    .preferGroup('!fallback', { minHealthy: 1, fallback: 'fallback' })
    .sortByScore(BALANCED)
    .stickyPrimary({ hysteresis: 0.10, minSwitchInterval: '30s' })
    .probeExcluded({ reAdmitAfter: '5m', maxConcurrent: 1 })
