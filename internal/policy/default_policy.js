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
    // Latency uses RELATIVE deviation — "trip if p70 is more than 3×
    // the fastest peer's p70". This adapts to the network's normal: a
    // 200 ms p70 is healthy on Ethereum and a 5 ms p70 is healthy on a
    // fast L2; both pools share the same predicate because the
    // comparison is to the achievable best in THIS pool. Pairing with
    // an absolute floor catches the case where every upstream is slow
    // and the deviation looks tame in relative terms.
    //
    // We're deliberately aggressive on latency relative to the other
    // health predicates: latency is the user-visible quality signal
    // every other axis (errors, throttle) already has retry / hedge /
    // consensus to absorb. Excluding a marginally-slow upstream costs
    // us one degraded reply at most; NOT excluding one poisons tail
    // latency on every call routed through it. So the latency
    // threshold is tighter than the other rules — 3× vs the much
    // looser "50% error rate / 30% throttle rate" bounds.
    //
    // p70 is the default quantile (no explicit arg) so the exclusion
    // axis aligns with the rank axis — `sortByScore(PREFER_FASTEST)`
    // ranks upstreams by p70 too. Same metric drives both decisions.
    //
    // The relative-deviation predicate is gated behind a `samplesAbove`
    // guard: under low-traffic startup (handful of state-poller calls,
    // tiny timing variance) the fastest-of-others is too unstable and
    // one upstream can spuriously land 3× above another. Requiring
    // ≥20 samples before tripping keeps the predicate honest. The
    // absolute p70>30s floor stays unguarded because it's a real
    // catastrophic threshold at any sample count.
    .excludeIf(errorRateAbove(0.5))
    .excludeIf(throttleRateAbove(0.3))
    .excludeIf(any(all(samplesAbove(20), latencyDeviationAbove(3)), latencyAbove(30_000)))
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
    // Order survivors by latency — among the upstreams that PASS the
    // excludeIf chain, "answers fastest" is the operator's strongest
    // user-visible quality signal. Switch to PREFER_FRESHEST per
    // network if you serve realtime reads that can't tolerate any
    // stale-head, or PREFER_LEAST_ERRORS if you're on a write path
    // and a 5xx costs more than a slow response.
    .sortByScore(PREFER_FASTEST)
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
