// Default selection policy. Applied when `selectionPolicy.eval` is omitted.
// See specs/selection-policy/feature.md §7.
//
// Designed to prevent two common failure classes: an upstream that
// becomes slow drags the whole pool's latency percentiles down (and
// stalls consensus); an upstream that starts erroring keeps receiving
// traffic until its score crosses some ranking threshold. This default
// FILTERS aggressively before scoring, so a degraded upstream is OUT,
// not just LOWER-RANKED.
//
//   1. Drop failsafe-cordoned upstreams (circuit-breaker explicitly marked bad).
//
//   2. keepHealthy — composite health filter:
//        - errorRate    > 0.5  → out  (more than half-erroring is broken)
//        - blockHeadLag > 16   → out  (~1 min behind tip on Ethereum, ~32s
//                                       on a 2s chain — clearly degraded)
//        - p95 latency  > 10s  → out  (catches a slow vendor before its
//                                       blended percentile poisons the pool)
//        - throttledRate > 0.3 → out  (vendor is rate-limiting us; reroute
//                                       before quota burns)
//      A single composite step keeps the four signals tunable together.
//
//   3. Safety net: if the filter wiped everything (project-wide outage),
//      serve from the raw set rather than fail closed.
//
//   4. Tier: primary = `group !== 'fallback'`, fallback = `group === 'fallback'`.
//      Long-standing eRPC convention; one config line to add a fallback upstream.
//
//   5. Sort by the BALANCED composite (errorRate, latency, throttling, lag,
//      misbehaviors). Filtering happened in step 2 — this step orders the
//      survivors.
//
//   6. Sticky primary ONLY for REALTIME requests. Finalized/unfinalized
//      requests are reorg-tolerant and don't benefit from primary stability;
//      stickiness on those just holds a degrading primary during incidents.
//
//   7. Probe excluded — re-admit one upstream every 90s (was 5m). Typical
//      transient-blip windows (rate-limit reset, regional networking dip)
//      recover in 60-90s; 5m hold-outs are excessive.
(upstreams, ctx) =>
  upstreams
    .removeCordoned()
    .keepHealthy({
      maxErrorRate:     0.5,
      maxBlockHeadLag:  16,
      maxP95Ms:         10_000,
      maxThrottledRate: 0.3,
    })
    .whenEmpty(() => upstreams)
    .preferGroup('!fallback', { minHealthy: 1, fallback: 'fallback' })
    .sortByScore(BALANCED)
    .if(ctx.finality === REALTIME,
        u => u.stickyPrimary({ hysteresis: 0.10, minSwitchInterval: '30s' }),
        u => u)
    .probeExcluded({ reAdmitAfter: '90s', maxConcurrent: 2 })
