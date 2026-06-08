/**
 * Compile-time tests for `@erpc-cloud/config` selection-policy typings.
 *
 * Run via:
 *   pnpm --filter @erpc-cloud/config test
 *
 * which invokes `tsc --noEmit` against this file. Positive cases must
 * compile clean; negative cases are marked with `@ts-expect-error` so an
 * accidental relaxation of the public types fails the build.
 */

import {
  createConfig,
  type PolicyEvalUpstream,
  type PolicyEvalUpstreamArray,
  type PolicyEvalContext,
  type PolicyEvalPredicate,
  type ScoreWeights,
  type SelectionPolicyEvalFunction,
} from "../index";

/* ───────────────────────── 1. Realistic config compiles ───────────────── */

const _realisticConfig = createConfig({
  projects: [
    {
      id: "main",
      scoreMetricsWindowSize: "10s",
      upstreams: [
        { endpoint: "alchemy://placeholder" },
        { endpoint: "drpc://placeholder", tags: ["tier:fallback"] },
      ],
      networks: [
        {
          architecture: "evm",
          evm: { chainId: 1 },
          selectionPolicy: {
            evalInterval: "1s",
            evalTimeout: "100ms",
            evalScope: "network",
            // Real arrow function — NOT a string. This is the whole point
            // of `.ts` config: every chainable method + predicate factory
            // is type-checked here.
            evalFunc: (upstreams, ctx) =>
              upstreams
                .removeCordoned()
                .excludeIf(errorRateAbove(0.5))
                .excludeIf(throttleRateAbove(0.3))
                .excludeIf(any(latencyDeviationAbove(3), latencyAbove(30_000)))
                .excludeIf(any(blockNumberLagAbove(16), blockSecondsLagAbove(30)))
                .whenEmpty(() => upstreams)
                .preferTag("!tier:fallback", { minHealthy: 1, fallback: "tier:fallback" })
                .sortByScore(PREFER_FASTEST)
                .stickyPrimary({ hysteresis: 0.3, minSwitchInterval: "30s" })
                .probeExcluded({ sampleRate: 1.0, maxConcurrent: 4, timeout: "10s" }),
          },
        },
      ],
    },
  ],
});

/* ───────────────────────── 2. String form still accepted ──────────────── */

const _stringEvalFunc = createConfig({
  projects: [
    {
      id: "main",
      upstreams: [{ endpoint: "alchemy://x" }],
      networks: [
        {
          architecture: "evm",
          evm: { chainId: 1 },
          selectionPolicy: {
            evalInterval: "1s",
            evalFunc: "(upstreams, ctx) => upstreams.sortByScore(PREFER_FASTEST)",
          },
        },
      ],
    },
  ],
});

/* ───────────────────────── 3. Custom predicates compose ───────────────── */

const _customPredicateConfig = createConfig({
  projects: [
    {
      id: "main",
      upstreams: [{ endpoint: "alchemy://x" }],
      networks: [
        {
          architecture: "evm",
          evm: { chainId: 1 },
          selectionPolicy: {
            evalInterval: "1s",
            evalFunc: (upstreams) =>
              upstreams
                // Inline custom predicate — note the optional `reason` arg.
                .excludeIf((u) => u.id.startsWith("legacy-"), "legacy upstream")
                // Predicate combinators compose freely.
                .excludeIf(all(errorRateAbove(0.2), not(samplesBelow(10))))
                .sortByScore(PREFER_FASTEST),
          },
        },
      ],
    },
  ],
});

/* ───────────────────────── 4. Per-upstream weights function ───────────── */

const _weightsByIdConfig: SelectionPolicyEvalFunction = (upstreams) =>
  upstreams.sortByScore((u): ScoreWeights => {
    if (u.id === "hot") return { errorRate: 8, respLatency: 12 };
    if (u.id === "cold") return { errorRate: 8, respLatency: 4 };
    return PREFER_FASTEST;
  });

/* ───────────────────────── 5. ctx fields are typed ────────────────────── */

const _ctxFieldsTyped: SelectionPolicyEvalFunction = (upstreams, ctx) => {
  // Each field has the right type — wrong types below would fail.
  const _network: string = ctx.network;
  const _method: string = ctx.method;
  const _finality: "realtime" | "unfinalized" | "finalized" | "unknown" =
    ctx.finality;
  const _tick: number = ctx.tickCount;
  // previousExcluded / excludedSince fields were dropped from the
  // EvalContext when readmitExcluded was replaced by probeExcluded
  // (the new primitive doesn't need JS-side bookkeeping of
  // excluded-set timestamps — re-admission is driven by tracker
  // metrics, not elapsed time).

  if (ctx.finality === "finalized") {
    // Reorg-tolerant: no sticky needed.
    return upstreams.sortByScore(PREFER_FASTEST);
  }
  return upstreams.sortByScore(PREFER_FASTEST).stickyPrimary();
};

/* ─────────────── 5b. sortByScore base + multiplier modes ──────────────── */

// `base` is optional — defaults to PREFER_FASTEST.
const _bareSort: SelectionPolicyEvalFunction = (upstreams) =>
  upstreams.sortByScore();

// merge / override / off modes + latencyQuantile all type-check.
const _multiplierModes: SelectionPolicyEvalFunction = (upstreams) =>
  upstreams
    .sortByScore(PREFER_FASTEST, { multipliers: "merge" })
    .sortByScore(PREFER_FRESHEST, { multipliers: "override", latencyQuantile: "p95" })
    .sortByScore({ errorRate: 8 }, { multipliers: "off" });

const _badMultiplierMode: SelectionPolicyEvalFunction = (upstreams) =>
  // @ts-expect-error — "sometimes" is not a valid multipliers mode
  upstreams.sortByScore(PREFER_FASTEST, { multipliers: "sometimes" });

// `u.scoreMultipliers` is readable and typed (overall + metric weights).
const _readMultipliers: SelectionPolicyEvalFunction = (upstreams) =>
  upstreams.sortByScore((u) => {
    const _overall: number | undefined = u.scoreMultipliers?.overall;
    const _err: number | undefined = u.scoreMultipliers?.errorRate;
    return u.scoreMultipliers ?? PREFER_FASTEST;
  });

/* ─────────────── 5c. Per-upstream routing.scoreMultipliers config ──────── */

const _scoreMultipliersConfig = createConfig({
  projects: [
    {
      id: "main",
      upstreams: [
        {
          endpoint: "alchemy://x",
          // Object-of-matchers list form: per-method + a catch-all.
          routing: {
            scoreMultipliers: [
              { network: "evm:1", method: "eth_getLogs", respLatency: 25 },
              { method: "*", overall: 1.5 },
            ],
            scoreLatencyQuantile: 0.9,
          },
        },
        {
          endpoint: "drpc://x",
          tags: ["tier:fallback"],
          routing: { scoreMultipliers: [{ errorRate: 12, overall: 0.5 }] },
        },
      ],
      networks: [{ architecture: "evm", evm: { chainId: 1 } }],
    },
  ],
});

/* ───────────────────────── 6. Upstream helpers + metrics ──────────────── */

const _upstreamHelpers: SelectionPolicyEvalFunction = (upstreams) =>
  upstreams.filter((u: PolicyEvalUpstream) => {
    const _hasTag: boolean = u.hasTag("tier:premium");
    const _alias: boolean = u.is("region:us-east");
    const _p95Ms: number = u.metrics.latencyP(95);
    const _lagSec: number = u.metrics.blockHeadLagSeconds;
    return u.metrics.errorRate < 0.5;
  }) as unknown as PolicyEvalUpstreamArray;

/* ─────────────── 6b. includeIf conditions type-check ──────────────────── */

// Conditions are native array methods over the per-upstream predicate
// factories; includeIf consumes them with a tag/id/vendor/type/where
// selector + position.
const _includeIfConditions: SelectionPolicyEvalFunction = (upstreams) =>
  upstreams
    .excludeTag("tier:reserve")
    .includeIf((p) => p.every(blockSecondsLagAbove(30)), { tag: "tier:reserve" })
    .includeIf((p) => p.some(errorRateAbove(0.5)), { id: "reserve-*" })
    .includeIf((p) => p.length < 2, { vendor: "premium" })
    .includeIf((p) => p.filter(blockNumberLagAbove(16)).length >= 2, {
      tag: "tier:reserve",
      position: "tail",
    })
    .includeIf((p) => p.length === 0, { where: { tag: "tier:reserve" } })
    .includeIf(true, { type: "evm" })
    .sortByScore(PREFER_FASTEST);

/* ───────────────────────── 7. Negative cases (must error) ─────────────── */

const _includeIfBadCondition: SelectionPolicyEvalFunction = (upstreams) =>
  // @ts-expect-error — condition must be a boolean or an array→boolean function
  upstreams.includeIf(42, { tag: "tier:reserve" });

const _includeIfMissingSelector: SelectionPolicyEvalFunction = (upstreams) =>
  // @ts-expect-error — includeIf requires an opts selector argument
  upstreams.includeIf((p) => p.length < 2);

createConfig({
  projects: [
    {
      id: "main",
      upstreams: [{ endpoint: "alchemy://x" }],
      networks: [
        {
          architecture: "evm",
          evm: { chainId: 1 },
          selectionPolicy: {
            evalInterval: "1s",
            // @ts-expect-error — number is not a valid evalFunc value
            evalFunc: 42,
          },
        },
      ],
    },
  ],
});

createConfig({
  projects: [
    {
      id: "main",
      upstreams: [{ endpoint: "alchemy://x" }],
      networks: [
        {
          architecture: "evm",
          evm: { chainId: 1 },
          selectionPolicy: {
            evalInterval: "1s",
            evalFunc: (upstreams) =>
              upstreams
                // @ts-expect-error — `errorRateAbove` takes a number, not a string
                .excludeIf(errorRateAbove("oops"))
                .sortByScore(PREFER_FASTEST),
          },
        },
      ],
    },
  ],
});

createConfig({
  projects: [
    {
      id: "main",
      upstreams: [{ endpoint: "alchemy://x" }],
      networks: [
        {
          architecture: "evm",
          evm: { chainId: 1 },
          selectionPolicy: {
            evalInterval: "1s",
            evalFunc: (upstreams) =>
              upstreams
                // @ts-expect-error — `maxConcurrent` must be a number, not a string
                .probeExcluded({ maxConcurrent: "lots" })
                .sortByScore(PREFER_FASTEST),
          },
        },
      ],
    },
  ],
});

createConfig({
  projects: [
    {
      id: "main",
      upstreams: [{ endpoint: "alchemy://x" }],
      networks: [
        {
          architecture: "evm",
          evm: { chainId: 1 },
          selectionPolicy: {
            evalInterval: "1s",
            evalFunc: (upstreams) =>
              upstreams
                // @ts-expect-error — `stickyPrimary.hysteresis` must be a number
                .stickyPrimary({ hysteresis: "0.3" })
                .sortByScore(PREFER_FASTEST),
          },
        },
      ],
    },
  ],
});

createConfig({
  projects: [
    {
      id: "main",
      upstreams: [{ endpoint: "alchemy://x" }],
      networks: [
        {
          architecture: "evm",
          evm: { chainId: 1 },
          selectionPolicy: {
            evalInterval: "1s",
            evalFunc: (upstreams) =>
              upstreams
                // @ts-expect-error — `sortByLatency` only accepts the literal quantile union
                .sortByLatency("p42")
                .sortByScore(PREFER_FASTEST),
          },
        },
      ],
    },
  ],
});

createConfig({
  projects: [
    {
      id: "main",
      upstreams: [{ endpoint: "alchemy://x" }],
      networks: [
        {
          architecture: "evm",
          evm: { chainId: 1 },
          selectionPolicy: {
            evalInterval: "1s",
            evalFunc: (upstreams) =>
              // @ts-expect-error — `nonExistentMethod` is not on PolicyEvalUpstreamArray
              upstreams.nonExistentMethod().sortByScore(PREFER_FASTEST),
          },
        },
      ],
    },
  ],
});

/* ───────────────────────── 8. Standalone predicate value typing ───────── */

const _standalonePredicate: PolicyEvalPredicate = any(
  errorRateAbove(0.5),
  latencyAbove(10_000, 95),
);
// `policyReason` is readable for diagnostics.
const _reason: string | undefined = _standalonePredicate.policyReason;

// Suppress "declared but never read" for the smoke values.
void _realisticConfig;
void _stringEvalFunc;
void _customPredicateConfig;
void _weightsByIdConfig;
void _ctxFieldsTyped;
void _upstreamHelpers;
void _standalonePredicate;
void _reason;
