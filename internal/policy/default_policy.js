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
    // Latency uses RELATIVE deviation (p85 > 25% above the pool median)
    // rather than an absolute threshold — a 200ms p85 on Ethereum and a
    // 5ms p85 on a fast L2 are both "normal" for their network, but in
    // either case an upstream answering meaningfully slower than its
    // peers IS degraded. Pairing with an absolute floor catches the case
    // where every upstream is slow (median rises, deviation looks tame)
    // but p85 is genuinely catastrophic.
    //
    // We're deliberately aggressive on latency relative to the other
    // health predicates (25% above median vs 50% errorRate or 30%
    // throttle): latency is the user-visible quality signal, every other
    // axis already has retry/hedge layers to absorb spikes. Excluding a
    // marginally-slow upstream costs us one degraded reply at most; NOT
    // excluding one costs us tail latency on every call routed through
    // it. Lower threshold (25% vs the previous 100%) trips roughly 4×
    // more aggressively than the same predicate would on `error_rate` or
    // `throttle_rate` — by design.
    //
    // p85 (instead of p95) catches "consistently slow" upstreams a tier
    // earlier — an upstream whose p85 is degraded probably has its p95
    // degraded too, and tripping at p85 means we eject before the tail
    // poisons consensus / hedge timeouts upstream.
    //
    // The relative-deviation predicate is gated behind a `samplesAbove`
    // guard: under low-traffic startup (handful of state-poller calls,
    // tiny timing variance) the median-of-others is too unstable and one
    // upstream can spuriously land >25% above the other's. Requiring
    // ≥20 samples before tripping keeps the predicate honest. The
    // absolute p85>30s floor stays unguarded because it's a real
    // catastrophic threshold any sample count.
    .excludeIf(errorRateAbove(0.5))
    .excludeIf(throttleRateAbove(0.3))
    .excludeIf(any(all(samplesAbove(20), latencyDeviationPctAbove(85, 25)), latencyAbove(85, 30_000)))
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
