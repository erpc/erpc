// Default selection policy. Applied when `selectionPolicy.evalFunc` is omitted.
//
// Two-primitive admission control:
//   excludeIf       — drop upstreams matching a predicate; optionally hold
//                     them out for at least `outFor` (anti-flap memory).
//   readmitExcluded — cautiously bring back excluded upstreams once their
//                     cooldown has elapsed.
// Predicate factories (errorRateAbove, latencyP95Above, blockNumberLagAbove,
// …) keep each rule self-documenting; combinators (all / any / not) compose
// them into compound conditions. See specs/selection-policy/feature.md §4.
(upstreams, ctx) =>
  upstreams
    .removeCordoned()
    // Health excludes — each signal locks the upstream out for its own
    // cooldown. Errors recover faster than block-lag (which usually means
    // the chain is reorging or the node is syncing), so the cooldowns
    // are tuned per-signal rather than shared.
    .excludeIf(errorRateAbove(0.5),       { outFor: '30s', reason: 'errorRate>50%' })
    .excludeIf(throttleRateAbove(0.3),    { outFor: '30s', reason: 'throttled>30%' })
    .excludeIf(latencyP95Above(10_000),   { outFor: '30s', reason: 'p95>10s'       })
    .excludeIf(blockNumberLagAbove(16),   { outFor: '90s', reason: 'blockLag>16'   })
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
