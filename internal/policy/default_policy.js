// Default selection policy. Applied when `selectionPolicy.evalFunc` is omitted.
//
// Two-primitive admission control:
//   excludeIf       — drop upstreams matching a predicate this tick.
//   readmitExcluded — cautiously bring back excluded upstreams once their
//                     cooldown has elapsed (this is the anti-flap layer).
// Predicate factories (errorRateAbove, latencyP95Above, blockNumberLagAbove,
// …) keep each rule self-documenting; combinators (all / any / not) compose
// them into compound conditions. See specs/selection-policy/feature.md §4.
(upstreams, ctx) =>
  upstreams
    .removeCordoned()
    // Health excludes — each rule trips an upstream out for the current
    // tick. Re-admission timing is handled by readmitExcluded at the
    // end of the chain (uniform 90s cooldown regardless of why a given
    // upstream was excluded — adequate for most operational scenarios).
    .excludeIf(errorRateAbove(0.5),       { reason: 'errorRate>50%' })
    .excludeIf(throttleRateAbove(0.3),    { reason: 'throttled>30%' })
    .excludeIf(latencyP95Above(10_000),   { reason: 'p95>10s'       })
    .excludeIf(blockNumberLagAbove(16),   { reason: 'blockLag>16'   })
    // Outage safety net — never return an empty list to the executor.
    // If every upstream fails the health excludes (project-wide outage),
    // fall back to the raw set rather than failing closed.
    .whenEmpty(() => upstreams)
    // Tier split: primary = NOT tier:fallback; fallback when no primary survives.
    .preferTag('!tier:fallback', { minHealthy: 1, fallback: 'tier:fallback' })
    // Order survivors by the BALANCED composite penalty (lower = primary).
    .sortByScore(BALANCED)
    // Sticky primary ONLY for REALTIME — reorg-tolerant calls skip this.
    .if(ctx.finality === REALTIME,
        u => u.stickyPrimary({ hysteresis: 0.10, minSwitchInterval: '30s' }),
        u => u)
    // Bring back excluded upstreams whose cooldown elapsed — one at a
    // time at the tail (lowest blast radius). Whether they STAY in is
    // decided by the next tick's excludeIf rules. The 90s default leaves
    // room for an upstream to actually recover before we re-introduce it;
    // tighter `reAdmitAfter` values make the policy probe more aggressively.
    .readmitExcluded({ reAdmitAfter: '90s', maxConcurrent: 2, position: 'tail' })
