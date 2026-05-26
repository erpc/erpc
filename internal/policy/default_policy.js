(upstreams, ctx) =>
  upstreams
    .removeCordoned()
    // Errors / throttle: drop upstreams that are clearly broken, gated
    // on samplesAbove(10) so a single failed call can't evict a fresh-
    // pod upstream. Thresholds are loose — failsafe (retry/hedge/
    // consensus) already absorbs occasional failures.
    .excludeIf(all(samplesAbove(10), errorRateAbove(0.7)))
    .excludeIf(all(samplesAbove(10), throttleRateAbove(0.4)))
    // Latency: drop if the geomean of per-method ratios (this
    // upstream's p70 / fastest peer's p70 PER METHOD, exponentially
    // damped by absolute latency at dampingMs=30) exceeds 10× —
    // OR catastrophically slow (>30s absolute). The 10× threshold
    // tolerates real cloud-vendor variance: an upstream serving
    // ~70-150ms while peers serve ~10-30ms isn't broken, it's just
    // a different vendor tier. A vendor at 2s+ is broken and trips.
    // Outer samplesAbove(20) gates on aggregate counts so the
    // predicate doesn't even run on cold-start pods.
    .excludeIf(any(all(samplesAbove(20), latencyDeviationAbove(10)), latencyAbove(30_000)))
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
    // Shadow-mirror sampled real traffic to currently-excluded
    // upstreams in the background so they accumulate fresh tracker
    // samples without touching real user traffic. sampleRate=0.1
    // bounds the per-excluded-upstream probe RPS on high-RPS
    // networks; minSamples=10 ensures even low-traffic networks
    // probe enough to clear the chain's `samplesAbove(N)` gates on
    // re-admission. maxConcurrent=4 is the absolute concurrent cap
    // per upstream. Per-upstream opt-out via `routing.probe: off`.
    .probeExcluded({ sampleRate: 0.1, minSamples: 10, minSamplesWindow: '60s', maxConcurrent: 4, timeout: '10s' })
