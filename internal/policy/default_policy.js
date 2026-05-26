(upstreams, ctx) =>
  upstreams
    .removeCordoned()
    // Errors / throttle: drop upstreams that are clearly broken, gated
    // on samplesAbove(10) so a single failed call can't evict a fresh-
    // pod upstream. Thresholds are loose — failsafe (retry/hedge/
    // consensus) already absorbs occasional failures.
    .excludeIf(all(samplesAbove(10), errorRateAbove(0.7)))
    .excludeIf(all(samplesAbove(10), throttleRateAbove(0.4)))
    // Latency: drop if p70 is >3× the fastest peer's p70 (gated on
    // samplesAbove(20) so the relative comparison is meaningful) OR
    // catastrophically slow (>30s). p70 matches the rank axis below.
    .excludeIf(any(all(samplesAbove(20), latencyDeviationAbove(3)), latencyAbove(30_000)))
    // Block-head lag: drop if behind tip by ≥16 blocks or ≥30s.
    .excludeIf(any(blockNumberLagAbove(16), blockSecondsLagAbove(30)))
    // Outage safety net: if everyone failed the health excludes, fall
    // back to the raw set rather than failing closed.
    .whenEmpty(() => upstreams)
    // Tier split: prefer non-fallback; fall back to tier:fallback if no
    // primary survives.
    .preferTag('!tier:fallback', { minHealthy: 1, fallback: 'tier:fallback' })
    // Rank survivors by p70 latency.
    .sortByScore(PREFER_FASTEST)
    // Hold the primary stable across ticks unless a meaningfully better
    // option exists for ≥30s.
    .stickyPrimary({ hysteresis: 0.30, minSwitchInterval: '30s' })
    // Re-admit excluded upstreams once cooled down — at the tail, one
    // at a time, lowest blast radius.
    .readmitExcluded({ reAdmitAfter: '90s', maxConcurrent: 2, position: 'tail' })
