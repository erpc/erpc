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
            evalPerMethod: false,
            // Real arrow function — NOT a string. This is the whole point
            // of `.ts` config: every chainable method + predicate factory
            // is type-checked here.
            evalFunc: (upstreams, ctx) =>
              upstreams
                .removeCordoned()
                .excludeIf(errorRateAbove(0.5))
                .excludeIf(throttleRateAbove(0.3))
                .excludeIf(any(p95DeviationPctAbove(100), latencyP95Above(30_000)))
                .excludeIf(any(blockNumberLagAbove(16), blockSecondsLagAbove(30)))
                .whenEmpty(() => upstreams)
                .preferTag("!tier:fallback", { minHealthy: 1, fallback: "tier:fallback" })
                .sortByScore(BALANCED)
                .stickyPrimary({ hysteresis: 0.3, minSwitchInterval: "30s" })
                .readmitExcluded({ reAdmitAfter: "90s", maxConcurrent: 2, position: "tail" }),
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
            evalFunc: "(upstreams, ctx) => upstreams.sortByScore(BALANCED)",
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
                .sortByScore(BALANCED),
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
    return BALANCED;
  });

/* ───────────────────────── 5. ctx fields are typed ────────────────────── */

const _ctxFieldsTyped: SelectionPolicyEvalFunction = (upstreams, ctx) => {
  // Each field has the right type — wrong types below would fail.
  const _network: string = ctx.network;
  const _method: string = ctx.method;
  const _finality: "realtime" | "unfinalized" | "finalized" | "unknown" =
    ctx.finality;
  const _tick: number = ctx.tickCount;
  const _excluded: readonly string[] = ctx.previousExcluded;
  const _excludedSince: number | undefined = ctx.excludedSince["some-id"];

  if (ctx.finality === "finalized") {
    // Reorg-tolerant: no sticky needed.
    return upstreams.sortByScore(BALANCED);
  }
  return upstreams.sortByScore(BALANCED).stickyPrimary();
};

/* ───────────────────────── 6. Upstream helpers + metrics ──────────────── */

const _upstreamHelpers: SelectionPolicyEvalFunction = (upstreams) =>
  upstreams.filter((u: PolicyEvalUpstream) => {
    const _hasTag: boolean = u.hasTag("tier:premium");
    const _alias: boolean = u.is("region:us-east");
    const _p95Ms: number = u.metrics.latencyP(95);
    const _lagSec: number = u.metrics.blockHeadLagSeconds;
    return u.metrics.errorRate < 0.5;
  }) as unknown as PolicyEvalUpstreamArray;

/* ───────────────────────── 7. Negative cases (must error) ─────────────── */

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
                .sortByScore(BALANCED),
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
                // @ts-expect-error — `position` accepts only the literal union
                .readmitExcluded({ position: "middle" })
                .sortByScore(BALANCED),
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
                .sortByScore(BALANCED),
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
                .sortByScore(BALANCED),
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
              upstreams.nonExistentMethod().sortByScore(BALANCED),
          },
        },
      ],
    },
  ],
});

/* ───────────────────────── 8. Standalone predicate value typing ───────── */

const _standalonePredicate: PolicyEvalPredicate = any(
  errorRateAbove(0.5),
  latencyP95Above(10_000),
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
