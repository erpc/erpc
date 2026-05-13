# Legacy config translator — golden file fixtures

This directory holds golden-file pairs for the legacy → new config
translator built in **Phase 12** of `specs/selection-policy/plan.md`.

The translator is the ONE place in the codebase that knows about the
old `routingStrategy`, `scoreMultipliers`, `scoreGranularity`,
`selectionPolicy.evalFunction`, `resampleExcluded`, and the
`ROUTING_POLICY_*` env-var conventions. It runs during config
unmarshal: legacy YAML lands in `WidenedConfig`, the translator
synthesizes equivalent new-shape YAML (one `selectionPolicy.eval`
per network using the new stdlib), and downstream code only ever sees
the new shape.

## File pairs

Each scenario is two files:

- `NN-name.legacy.yaml` — what a user wrote with legacy syntax. This is
  the input to the translator.
- `NN-name.expected.yaml` — what the translator MUST emit. Comments in
  this file explain why a given new-style construct was chosen.

The translator's `Translate(*WidenedConfig) (warnings, error)` function
takes the legacy YAML, returns the new-shape `Config` whose
`yaml.Marshal` form should equal the `.expected.yaml` byte-for-byte
(after standard formatting).

## Acceptance criteria for Phase 12

The translator passes when:

1. Every fixture pair round-trips: `Translate(unmarshal(legacy.yaml))`
   marshals to bytes equal to `expected.yaml` (whitespace-normalized).
2. Each `.legacy.yaml` also passes through the END-TO-END safety net:
   load it, boot the engine, drive the metrics noted in the file's
   front-matter comment, assert the SAME observable ordering as
   `erpc/selection_safety_net_test.go` captures against the legacy
   code today.
3. No `legacy.*` symbol is referenced anywhere outside this directory,
   `common/legacy/`, and `cmd/erpc/migrate.go`. (Grep audit from
   Phase 9.11.)

## Scenario catalog

| # | Scenario | Legacy feature(s) | Notes |
|---|---|---|---|
| 01 | round-robin | `routingStrategy: round-robin` | Synthesizes `return upstreams.rotateBy(ctx.tickCount)` |
| 02 | score-based-basic | `routingStrategy: score-based` (defaults) | Synthesizes default `sortByScore(BALANCED)` |
| 03 | score-multipliers-per-method | `upstream.routing.scoreMultipliers` with method patterns | Per-upstream weight function passed to `sortByScore` |
| 04 | score-multipliers-per-network | `upstream.routing.scoreMultipliers` with `network: evm:1` | Function selects weights based on `ctx.network` |
| 05 | score-multipliers-per-finality | `upstream.routing.scoreMultipliers` with `finality: [finalized]` | Function selects weights based on `ctx.finality` |
| 06 | legacy-eval-function | `selectionPolicy.evalFunction` (custom JS) | Wrapped into `eval: const __legacyFn = (...) => {...}; return __legacyFn(upstreams, ctx.method);` |
| 07 | resample-excluded | `selectionPolicy.resampleExcluded: true` + `resampleInterval` | Appends `.probeExcluded({ reAdmitAfter: M, maxConcurrent: 1, longestFirst: true })` to the synthesized chain |
| 08 | routing-policy-env-vars | implicit (default policy reads `ROUTING_POLICY_*`) | Synthesized eval reads `process.env.ROUTING_POLICY_*` with same defaults |
| 09 | sticky-primary-tuned | `scoreSwitchHysteresis`, `scoreMinSwitchInterval` | Baked into `sortByScore(...).stickyPrimary({ hysteresis, minSwitchInterval })` |
| 10 | kitchen-sink | All of the above in one config | End-to-end stress |

Each scenario MUST come with:

1. A frontmatter YAML comment block listing the **driving events**
   (metric injections + initial conditions) and the **assertion**
   (expected selection order at observation point T).
2. A copy of that driving sequence in
   `erpc/selection_safety_net_test.go` so the same scenario can be
   exercised against both legacy and new code paths.

## Deprecation warnings

Each translation should also be asserted to emit a specific deprecation
warning (see plan §12.6). The translator returns `[]string` of warnings;
tests assert the slice's contents.

## What is NOT in this directory

- Tests for the new (post-translator) engine: those live in
  `internal/policy/stdlib/*_test.go` and `internal/policy/engine_test.go`.
- The legacy production code: see Phase 1 deletion list in `plan.md`.
- TypeScript type tests: see Phase 8.
