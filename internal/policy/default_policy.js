// Default selection policy. Applied when `selectionPolicy.evalFunc` is omitted.
//
// Two-primitive admission control:
//   excludeIf       — drop upstreams matching a predicate this tick.
//   readmitExcluded — cautiously bring back excluded upstreams once their
//                     cooldown has elapsed (this is the anti-flap layer).
// Predicate factories (errorRateAbove, p95DeviationMs, blockNumberLagAbove,
// …) keep each rule self-documenting; combinators (all / any / not) compose
// them into compound conditions. See specs/selection-policy/feature.md §4.
(upstreams, ctx) =>
  upstreams
    .removeCordoned()
    // Health excludes — each rule trips an upstream out for the current
    // tick. Re-admission timing is handled by readmitExcluded at the
    // end of the chain (uniform 90s cooldown regardless of why a given
    // upstream was excluded — adequate for most operational scenarios).
    //
    // Latency uses RELATIVE deviation (p95 > 100% above the pool median)
    // rather than an absolute threshold — a 200ms p95 on Ethereum and a
    // 5ms p95 on a fast L2 are both "normal" for their network, but in
    // either case an upstream answering 4× slower than its peers IS
    // degraded. Pairing with an absolute floor catches the case where
    // every upstream is slow (median rises, deviation looks tame) but
    // p95 is genuinely catastrophic.
    //
    // The relative-deviation predicate is gated behind a `samplesAbove`
    // guard: under low-traffic startup (handful of state-poller calls,
    // tiny timing variance) the median-of-others is too unstable and one
    // upstream can spuriously land >100% above the other's. Requiring
    // ≥20 samples before tripping keeps the predicate honest. The
    // absolute p95>30s floor stays unguarded because it's a real
    // catastrophic threshold any sample count.
    .excludeIf(errorRateAbove(0.5))
    .excludeIf(throttleRateAbove(0.3))
    .excludeIf(any(all(samplesAbove(20), p95DeviationPctAbove(100)), latencyP95Above(30_000)))
    // Block-head lag: trip on either a chain-agnostic block count
    // (16 blocks ≈ 3 min behind tip on Ethereum, ~32s on a 2s chain)
    // OR a wall-clock seconds bound (30s behind tip) — the seconds
    // bound adapts to the chain's actual block-time via the tracker's
    // EMA. The seconds predicate is a no-op until the EMA accumulates
    // ≥3 samples, so the count predicate carries the load at boot.
    .excludeIf(any(blockNumberLagAbove(16), blockSecondsLagAbove(30)))
    // Outage safety net — never return an empty list to the executor.
    // If every upstream fails the health excludes (project-wide outage),
    // fall back to the raw set rather than failing closed.
    .whenEmpty(() => upstreams)
    // Tier split: primary = NOT tier:fallback; fallback when no primary survives.
    .preferTag('!tier:fallback', { minHealthy: 1, fallback: 'tier:fallback' })
    // Order survivors by the BALANCED composite penalty (lower = primary).
    .sortByScore(BALANCED)
    // Sticky primary across ALL finalities — flapping between two
    // similarly-ranked upstreams every tick costs more in connection
    // setup + cache-locality than it saves in marginal ranking
    // accuracy, regardless of whether the request is reorg-tolerant.
    // Hysteresis + minSwitchInterval together keep the primary stable
    // unless a meaningfully better option exists for long enough.
    .stickyPrimary({ hysteresis: 0.30, minSwitchInterval: '30s' })
    // Bring back excluded upstreams whose cooldown elapsed — one at a
    // time at the tail (lowest blast radius). Whether they STAY in is
    // decided by the next tick's excludeIf rules. The 90s default leaves
    // room for an upstream to actually recover before we re-introduce it;
    // tighter `reAdmitAfter` values make the policy probe more aggressively.
    .readmitExcluded({ reAdmitAfter: '90s', maxConcurrent: 2, position: 'tail' })
