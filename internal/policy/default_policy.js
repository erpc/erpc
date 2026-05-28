(upstreams, ctx) =>
  upstreams
    .removeCordoned()
    // Errors / throttle: drop upstreams that are clearly broken, gated
    // on samplesAbove(10) so a single failed call can't evict a fresh-
    // pod upstream. Thresholds are loose — failsafe (retry/hedge/
    // consensus) already absorbs occasional failures.
    .excludeIf(all(samplesAbove(10), errorRateAbove(0.7)))
    .excludeIf(all(samplesAbove(10), throttleRateAbove(0.4)))
    // Latency: only exclude when the upstream is BOTH absolutely slow
    // AND consistently slower than peers across most of what it
    // serves. Two-stage check:
    //
    //   1. `latencyAbove(3000)` — absolute-latency floor. Drops the
    //      deviation predicate entirely for upstreams below 3s p70.
    //      Sub-3s upstreams stay in rotation regardless of how they
    //      compare to a faster peer; `sortByScore(PREFER_FASTEST)` puts
    //      them later in the ordered list and hedge catches the slack
    //      on the request path. The 3s threshold is the "user-visible
    //      pain" boundary — anything below is recoverable via hedge,
    //      anything above starts hitting tail-latency SLOs.
    //
    //   2. `latencyDeviationAbove(3, majority)` — only AFTER passing
    //      the absolute floor, also check the upstream is consistently
    //      slower than peers. `mode: 'majority'` (more than half of the
    //      per-method comparisons exceed 3×) keeps the predicate robust
    //      against per-tick spikes on a single rare method that
    //      would otherwise push a geomean over the threshold. The
    //      lower multiplier (3 vs the older 10) is intentional — once
    //      we've established the upstream is absolutely slow, a 3×
    //      ratio is a strong "consistently behind peers" signal.
    //
    // Outer `latencyAbove(10_000)` is the catastrophic safety net
    // (>10s absolute, unconditional exclusion regardless of peers or
    // sample count). Lower than the previous 30s — at this PR the
    // operator-stated intent is "3-4s+ is broken", so 10s is well
    // past any reasonable user tolerance.
    //
    // Outer `samplesAbove(20)` gates on aggregate counts so the
    // predicate doesn't even run on cold-start pods.
    .excludeIf(any(all(samplesAbove(20), latencyAbove(3000), latencyDeviationAbove(3, { mode: 'majority' })), latencyAbove(10_000)))
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
