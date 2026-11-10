# Custom Consensus Policies Engine — Specification

**Status**: Proposal (for core maintainer review)
**Issue**: [#1088](https://github.com/erpc/erpc/issues/1088)
**Direction**: [@aramalipoor](https://github.com/erpc/erpc/issues/1088#issuecomment-5492667906), accepted in [PR #18](https://github.com/andreclaro/erpc/pull/18#discussion_r4082158555)
**Last revised**: 2026-09-29

Companion: [plan.md](./plan.md) (phased delivery).

> Design contract for named consensus policies + a pre-round JS selector.
> Semantics below incorporate lessons from an internal prototype used to validate
> the design (waiver floors, round-complete waivers, hedge empty votes, PreferNonEmpty
> ties, config repetition). This document proposes behavior for upstream — it is
> not a changelog of a fork.

---

## TLDR

Operators define named consensus policies in config, and a small JS function
picks one per request before the round starts. The function only returns a
policy name — it never grades responses — and any error, timeout, or unknown
name falls back to the strictest policy, the inline default.

- **Named policies.** The inline `consensus:` block stays the fail-closed
 default; `consensus.policies. ` adds relaxed grades (e.g. `fallback`,
 `waive-missing`). `extends: default` reuses the shared
 hygiene, so each policy only states what changes.
- **Pre-round JS selector.** `consensus.customPolicy.evalFunction` runs before
 fan-out and returns a policy name. It sees the request, the caller, and
 upstream health — never round outcomes.
- **Caller identity comes from auth, unchanged.** The auth strategy that
 authenticates the caller stamps its `name` on the user; the eval compares it
 with plain string checks. Auth learns nothing about consensus.
- **Fail closed everywhere.** Eval error, timeout, unknown name, punished or
 admin-cordoned upstreams → the default policy. Never a more permissive one.
- **Zero migration.** A config without `policies` behaves exactly as today.

---

## 1. Purpose

The **Custom Consensus Policies Engine** lets operators define **named declarative
consensus policies** and a **pre-round freeform JS selector** (`customPolicy`) that
picks which policy governs each request. Which caller may use which grade is
decided inside the eval, by the **name of the auth strategy** that authenticated
them — auth itself stays consensus-free.

After v1, an operator can:

- Run strict internal+external consensus for production callers, and define
 relaxed named policies for trusted strategies — without code changes.
- Automatically fall back to external-only consensus when internal nodes are
 unhealthy (out of sync, down, unreachable) and not punished for consensus
 misbehaviour — but only for callers on a strategy that allows it.
- Serve historical / pruned data only for callers on a strategy that allows
 it: `waive-missing` keeps the default composition but forgives pruned
 internals — MissingData errors, and empty results proven outside retention.
 Everyone else stays on the strict default and disputes.

Freeform JS decides **which** declarative policy to run — never **how** the round
is graded. The consensus executor and its `ConsensusPolicyConfig` contract stay
unchanged. Open-ended operator logic (caller identity, upstream health, block
availability) resolves into one bounded interface — a named
`ConsensusPolicyConfig`. This mirrors the selection-policy pattern
(`internal/policy`): freeform JS at the edge, declarative result into the core.

### Goals

- `consensus.policies. `: named map of `ConsensusPolicyConfig`, with
 one-level `extends` to reuse a base config.
- `consensus.customPolicy.evalFunction`: JS evaluated **per request, before the
 round**, returning a policy name.
- Eval context: request (method, blockNumber, network), user (auth strategy
 name), upstreams (tags, health, blockAvailability).
- Caller-gated automatic fallback when internals are unavailable for
 availability reasons (not punishment).
- Caller-gated historical serving via two integrity grades: the default (no
 waiver) and `waive-missing` (waivers for pruned/MissingData and
 empty-outside-retention).
- Observability: chosen policy name on every served round (header + metric).
- Zero migration: today's inline `consensus:` block becomes the default policy.

### Non-goals

- No post-round JS grading. `agreeing` / `punished` / `valueGroups` are **not**
 exposed to JS.
- No mid-round policy switching. A round runs under the policy selected at
 start (fail-visible, not fail-silent).
- No separate policy conditioning outside the eval. Caller→policy lives in
 `customPolicy.evalFunction` via `ctx.user.auth`; auth modules learn nothing
 about consensus.
- No inline policy object return in v1 — eval returns a **name** only.
- No decision cache in v1 — add only if measured eval cost forces it.
- No silent auto-inherit of unset fields from the inline default onto named
 policies (that would wrongly pull `requiredParticipants` onto `fallback` /
 `waive-missing`). Inheritance is always explicit via `extends` (§2).
- #1069-style bounded-deviation value logic stays executor-side (separate
 change).

---

## 2. Configuration

```yaml
consensus:
  # Inline block = fail-closed default and shared hygiene base.
  # Must be the strictest grade.
  maxParticipants: 3
  agreementThreshold: 2
  disputeBehavior: returnError
  ignoreFields: { ... }
  requiredParticipants:
    - { tag: "type:internal", minParticipants: 1, minAgreement: 1 }
    - { tag: "type:external", minParticipants: 2, minAgreement: 2 }

  policies:                          # each value = ConsensusPolicyConfig + optional extends
    waive-missing:                   # default composition; waivers for pruned internals
      extends: default
      requiredParticipants:
        - { tag: "type:internal", minParticipants: 1, minAgreement: 1,
            waiveAgreementOnMissingData: true,
            waiveAgreementOnEmptyOutsideRetention: true,
            retentionBlocks: 10000 }
        - { tag: "type:external", minParticipants: 2, minAgreement: 2 }  # never waived
    fallback:                        # internals unhealthy (not punished); healthy pool is the externals
      # Use only when internals are out of sync / down / unreachable.
      # If internals are absent from the round because they were punished for
      # consensus misbehaviour, stay on default — never select fallback.
      extends: default
      requiredParticipants: []       # explicit clear — no tag quotas when 1P is down

  customPolicy:                      # optional; omitted = default policy for all
    evalTimeout: 50ms                # hard cap; fail closed to default on timeout
    evalFunction: |
      (ctx) => {
        const s = ctx.user?.auth
        const internals = ctx.upstreams.withTag("type:internal")

        // consensus-waive always uses waive-missing
        if (s === "consensus-waive") return "waive-missing"

        // Fallback logic:
        if (internals.anyPunished()) return "default"
        if (internals.allUnavailable() && s === "consensus-fallback") {
          return "fallback"
        }

        return "default"
      }

auth:
  strategies:
    - type: jwt
      name: consensus-waive          # may serve waive-missing
      jwt: { ... }
    - type: jwt
      name: consensus-fallback       # may serve fallback
      jwt: { ... }
    - type: secret
      name: consensus-default        # matches no grade check — always stays on default
```

Two integrity grades for data serving (plus `fallback`):

| Policy | Composition | Waivers (internals) | When selected |
|---|---|---|---|
| inline / `"default"` | internal + external | None | Fail-closed; callers on strategies without a grade; eval `return "default"` / `null` |
| `waive-missing` | same as default | MissingData + empty-outside-retention | `consensus-waive` strategy |

| Field | Type | Description |
|---|---|---|
| `policies` | `map[string]ConsensusPolicyConfig` | Named policies. Each value is a consensus config (same fields as today's inline block) plus optional `extends`. |
| `policies..extends` | `string` | Optional. One-level, config-only merge from another named policy or `default` (= the inline block). Child fields win; explicit `requiredParticipants: []` clears quotas. Resolved at load, so the runtime policy is always fully resolved. |
| `customPolicy.evalFunction` | `string` | JS function. Signature `(ctx) => string \| null`. If omitted, the default/inline policy applies to every request. |
| `customPolicy.evalTimeout` | `Duration` | Hard wall-clock cap on each eval; default `50ms` (`common/defaults.go`). Fail closed to default on timeout (VM discarded). |
| `requiredParticipants[].waiveAgreementOnMissingData` | `bool` | Opt-in, default `false`. Put on `waive-missing`, not on the fail-closed default. See §7. |
| `requiredParticipants[].waiveAgreementOnEmptyOutsideRetention` | `bool` | Opt-in, default `false`. Waives the quota when ≥ `minAgreement` matching participants returned **empty** and the block the client's request names is proven outside retention. Requires `retentionBlocks > 0`. See §7. |
| `requiredParticipants[].retentionBlocks` | `int64` | Default `0`. Retained depth behind chain head for the chain-head fallback proof (`head − requestedBlock > retentionBlocks`). Must be `> 0` when the empty-waiver is set. |

**Backward compatibility:** an inline `consensus:` block with no `policies` map
is one anonymous default policy — zero operator migration. All existing fields
(`agreementThreshold`, `requiredParticipants`, `punishMisbehavior`, …) keep
working. This design **reimplements** the `X-ERPC-Consensus-Policy` header and
`consensus_policy` metric (not dependent on #1041). Ordered YAML
`acceptancePolicies` is superseded.

**Default policy must be the strictest.** The **inline `consensus:` block itself**
is the default policy — the round falls back to it whenever the eval returns
null/unknown, throws, or times out. Operators must configure it as the most
restrictive policy. The reserved label `"default"`
(`common.ConsensusDefaultPolicyName`) is what the metric/header report for that
fall-closed round, so `policies["default"]` is **rejected at load** (no second
config entry under that name). Eval may still **`return "default"`** as an
intentional select of the inline policy (§3). Lack of a free Sobek VM is
**not** a fail-closed trigger — the pool refills / grows so eval still runs
(§4.6). A named policy may **not** itself define `customPolicy` / `policies`
(selection is top-level only) — also a load-time error.

**`extends` merge rule (load-time).** Resolve the base — one level only: a
named policy, or `default` for the inline block; unknown base or a cycle is a
config error. Copy the base, overlay the child's set fields (explicit
`requiredParticipants: []` clears quotas), then run `SetDefaults()` on the
result. There is no inheritance without `extends`: a policy that omits it
starts from **stock** defaults, not the inline block — operators who want the
shared hygiene say `extends: default` explicitly.

---

## 3. Eval interface

```js
(ctx) => string | null
// ctx.request   { method, blockNumber: number|null, network }
// ctx.user      { auth: string } | null   // name of the auth strategy that
//                                         // authenticated the caller (§4.3);
//                                         // "" when the strategy is unnamed
// ctx.upstreams [{ id, tags, health, blockAvailability: {lower, upper} | null }]
//   health = { state: "healthy" | "unhealthy" | "cordoned",
//              cordonClasses: ("punishment"|"availability"|"operator")[],
//              cordonReason?: string }   // diagnostic only — never matched
//   An upstream can hold several cordons at once (§4.5), so the class is a
//   SET. Empty ⟺ not cordoned. `state == "cordoned"` if the set is non-empty,
//   and takes precedence over "unhealthy".
//
// return "name"     → run policies[name]
//                     (unknown name → fail closed to default + warn log)
// return "default"  → inline default (intentional; NOT unknown_name)
// return null       → inline default (also intentional)
```

The eval never sees round outcomes. It answers one question: *under which rule
set should this request's round run?*

**`return "default"` vs `null`.** Both select the inline fail-closed policy and
emit header/metric `default`. Prefer the string form in operator evals for
uniform vocabulary alongside `"fallback"` / `"waive-missing"`. Resolving the
string `"default"` must **not** count as `unknown_name` fail-closed. Still
forbid defining `policies["default"]` as a map entry.

### 3.1 Stdlib (v1, chainable — mirrors selection policy)

Grow only when forced by observed configs:

| Helper | Behavior |
|---|---|
| `upstreams.withTag(t)` | Filter by tag |
| `upstreams.healthy()` | Keep only `state == "healthy"` — cordoned upstreams of any class are excluded (cordon = out of rotation) |
| `upstreams.anyPunished()` | True if any **network** upstream holds a `punishment` cordon — **including nodes selection already dropped** (`removeCordoned`). The eval ctx must carry a separate scan list for punished/operator nodes; otherwise a dropped punished node would be invisible and could not pin the request to default. Not "ever punished" |
| `upstreams.anyOperatorCordon()` | True if any network upstream holds an `operator` (admin) cordon (same dropped-node scan) — deliberate human action, undifferentiated between maintenance and distrust |
| `upstreams.allUnavailable()` | True if the set is **non-empty** and every member is either unhealthy-but-uncordoned or holds `availability` cordons and nothing else. **False on an empty set** — a tag matching no upstream is a config error, not an outage. Whitelist of availability only: `punishment`, `operator`, and any future cordon class make it false |

`ctx.user?.auth` needs no helper — it is a plain string compare against the
strategy names the operator configured.

---

## 4. Architecture

```
internal/consensus/policy/   ← new standalone engine: sobek pool (compile once,
                               borrow VM per eval), stdlib, no executor imports
consensus/executor.go        ← resolve policy BEFORE the round; round code untouched
auth/ , health/              ← supply ctx.user and upstream health/availability
```

### 4.1 Request flow

1. Auth resolves the user and stamps the strategy name (**new plumbing** —
 see §4.3).
2. Build `EvalContext` (request, user, upstream refs, network).
3. `selector.Evaluate(ctx)` bounded by `evalTimeout` → policy name.
4. Resolve name → `ConsensusPolicyConfig` (unknown/empty/`"default"` → inline).
5. Existing executor runs under that config — untouched code path.
6. Emit `X-ERPC-Consensus-Policy: ` header + `consensus_policy` metric
 label.

Eval failure, timeout, or unknown name fails **closed** to the default policy
(§2) with an error/warn log. (`"default"` is not unknown.)

### 4.2 Latency

Eval runs once per request **before** fan-out and does not extend consensus wait
windows. Sobek bytecode on a pre-warmed VM is tens–low hundreds of µs —
negligible vs upstream RTT. A decision cache is **not** in v1; add only if
profiles force it.

### 4.3 Auth plumbing (new)

Auth stays consensus-free: the eval only needs to know **which strategy**
authenticated the caller. `common.User` today carries `Id`, `RateLimitBudget`,
`AllowClientDirectives` — nothing identifying the strategy. The plumbing
mirrors how `AllowClientDirectives` is already attached:

- Add optional `name` to `AuthStrategyConfig`.
- Add `Auth string` to `common.User`.
- Every strategy (`jwt`, `secret`, `network`, `database`, `siwe`) stamps its
 configured `name` onto `User.Auth` when it authenticates a request; unnamed
 strategies stamp `""`.
- The name is **never** accepted from client-controlled headers, query params,
 or body fields — it comes only from the strategy config that verified the
 caller (same guarantee as `AllowClientDirectives`).

The eval then gates grades with plain string checks (`ctx.user?.auth ===
"consensus-fallback"`). Operators grant a grade by issuing credentials under a
named strategy; consensus never parses tokens, and auth never learns about
policies. This is a hard dependency of UC2 (caller-gated fallback) and
caller-gated historical access.

### 4.4 Sitout state ownership

Today `punishMisbehavior` sitout lives in a private executor map
(`misbehavingUpstreamsSitoutTimer`) plus `upstream.Cordon("*", "misbehaving in
consensus")`. The selector package must not import `consensus/`, so sitout
state moves to **`health.Tracker`**:

- Executor records sitout via `upstream.Cordon("*", "misbehaving in
 consensus", CordonClassPunishment)` and the matching
 `upstream.Uncordon("*", "end of consensus penalty", CordonClassPunishment)`
 on timer expiry. The claim is idempotent: a second
 punishment for the same upstream while already in sitout is a no-op (the
 timer is not reset).
- The misbehavior **rate limiter** also moves to `health.Tracker` (keyed by
 upstream ID) so that reaching a sitout is global across policies. The
 misbehavior **exporter** stays per-policy — it is a reporting sink, not a
 correctness signal. Because one destination can now back several policies,
 load-time validation rejects two policies sharing an S3
 `misbehaviorsDestination.path` unless each `filePattern` contains both
 `{timestampMs}` and `{policy}`; without them two exporters can resolve the
 same key (even in the same millisecond) and an S3 PUT overwrites rather
 than appends.
- **`cordonClass` is a required parameter on `Cordon` / `Uncordon`, and the
 tracker holds cordon state per class** (§4.5). The selector reads the class
 set — never the reason string.
- `anyPunished()` is true if any upstream that would otherwise be eligible
 holds a `punishment` cordon; `anyOperatorCordon()` is the matching helper
 for the `operator` class. The reference eval uses the first to gate
 `fallback` (§2 / §6); the second is available for operators who want admin
 cordons to pin requests to default.

This keeps the zero-import rule for `internal/consensus/policy/` and gives
both executor and selector a shared, race-safe source of truth.

### 4.5 Cordon classes

Not every cordon is punishment, and not every non-punishment cordon is an
availability failure. `Cordon` takes a **typed class** alongside the
free-text reason; the selector matches on the class, never the reason string
(otherwise the eval would couple to the message text of every
cordon-producing call site):

| `cordonClass` | Set by | Meaning | Fallback |
|---|---|---|---|
| `punishment` | Consensus executor misbehavior sitout (`consensus/executor.go`) | Punished for misbehavior | **Blocks** fallback |
| `availability` | EVM state poller chain-identity mismatch (`architecture/evm/evm_state_poller.go`); SVM state poller lag/unhealthy (`architecture/svm/svm_state_poller.go`) | Automatic availability failure | **Allows** fallback |
| `operator` | Admin API (`erpc/admin.go`) | Deliberate operator cordon | **Blocks** fallback (fail closed) |

The class is required at the call site, so there is no runtime "unknown"
row: a cordon class added in the future blocks fallback until the selector
explicitly maps it to `availability` — fail closed by construction.

**Why `operator` fails closed.** An admin cordon is undifferentiated: the
same call serves planned maintenance and "take this node out, I do not trust
it". The API cannot tell them apart, so the class is read as the second one.
An operator who wants authorized callers to keep getting `fallback` during
maintenance changes the policy config, which is an explicit and audited act,
rather than relying on a cordon to imply it.

#### Cordon state is held per class

An upstream can be cordoned by several sources at once, and today they share
one flag: `health.Tracker` stores a single `Cordoned` bool plus one
`LastCordonedReason` per `(upstream, method)` (`health/tracker.go`), and
`Uncordon` clears it without checking who set it. The three automatic
sources above all use method `"*"`, as does the admin API whenever the
operator passes no method, so a single scalar class would be last-writer-wins
on one shared entry:

- A state poller cordoning an already-punished upstream would overwrite
 `punishment` with `availability`, and `allUnavailable()` would then permit
 the downgrade while the sit-out is still running.
- The SVM poller's recovery `Uncordon` (`architecture/svm/svm_state_poller.go`)
 would clear a live consensus punishment cordon outright, and the sit-out
 timer's `Uncordon` would likewise clear a poller's live availability cordon.

So the tracker holds **one flag per class** — a bitmask over the closed class
enum on the existing `(upstream, method)` entry, replacing the single bool.
`IsCordoned` is "any bit set". `Uncordon(class)` clears one bit and leaves
the others. `CordonedAtMs` is stamped on the empty→non-empty transition and
cleared on the return to empty, so duration accounting is unchanged. This is
order-independent by construction: `allUnavailable()` is false while any
`punishment` bit is held, whatever arrived last.

**Method scope.** Cordons are keyed by method, and `IsCordoned` matches the
exact method or `"*"`. The three automatic sources use `"*"`; the admin API
passes an operator-supplied method, so an upstream can hold `operator` on one
method and `availability` on `"*"`. The eval sees the union of both scopes for
the request's method, which is why `health.cordonClasses` is a set.

The eval ctx exposes `health` as a structured value:
`{ state, cordonClasses, cordonReason? }` (§3). `cordonReason` is diagnostic
only, and `state` is `"cordoned"` whenever the class set is non-empty — a
cordoned upstream never reports `"unhealthy"`, so a failing health check
cannot mask a punishment cordon. The reference fallback predicate is
`allUnavailable()` (§3.1).

### 4.6 Sobek pool

The selector runs operator `evalFunction` JS in **Sobek** (Go JS runtime, same
family as selection policy). To keep per-request cost negligible vs upstream RTT
and avoid skipping eval under burst, the engine owns a **pool of pre-warmed
VMs** that **refills in advance** and **grows on demand** — never fail-closed
for lack of a free VM (`pool_exhausted` is not a contract outcome).

- **Pre-warm:** start with **8** VMs at load (same order of magnitude as
 selection policy). Compile the operator program once; each VM is sandboxed
 (below).
- **Low-water refill (in advance):** when idle/free VMs drop to **≤ 2**,
 asynchronously build more until idle is back near the pre-warm target (8),
 so the next requests still borrow a warm VM instead of paying cold-start on
 the hot path. Refill is best-effort background work — not a request blocker.
- **Grow on demand:** if the pool is nevertheless empty at acquire time
 (stampede faster than refill), **synchronously create one VM**, run the eval,
 and release it into the pool. Do **not** return `pool_exhausted`, do **not**
 bypass to default for capacity, do **not** skip `evalFunction`. Optional hard
 `maxSize` may cap total VMs for memory; if set and hit, **wait briefly for a
 free VM** rather than bypassing eval — integrity of the selected named policy
 beats silent default under load.
- Memory per VM: small (empty Sobek runtime + stdlib); steady-state bounded by
 pre-warm + refill target; peak tracks concurrent evals up to any configured
 max.
- `evalTimeout` caps wall-clock per eval; a runaway eval poisons only the
 borrowed VM, which is discarded (rebuilt async, same as selection-policy
 heal). A plain throw leaves the VM clean and it returns to the pool.
- **Sandboxed runtime.** Each VM is a bare `sobek.New()` exposing only the
 `EvalContext` + stdlib — **no `env` / `process.env` / `console`**, unlike the
 shared `common.NewRuntime()`. `evalFunction` is operator-writable config, so
 it never gets host-process env access. The one-time evaluation of the
 operator function expression at pool build is itself bounded by
 `evalTimeout`, so a runaway expression fails config load loudly instead of
 hanging startup.

---

## 5. Decision cache (future work)

Not in v1. The eval is measured in microseconds against upstream RTT in
milliseconds; a cache adds dependency-tracking complexity for no forced
benefit. If profiles later show eval cost matters, the design is:

- Auto-derive cache keys from accessed ctx paths (getter tracking), with
 operator-declared `cacheKeys` as fallback.
- Health/availability reads fold into a health-tracker generation counter for
 O(1) invalidation.
- `blockNumber` buckets by `blockAvailability` boundaries; cardinality guard
 disables caching for exploding keys.
- Cache stores policy **name** only; errors never cached.

---

## 6. Automatic fallback (caller-gated)

When internals are **unhealthy** (out of sync, down, unreachable) or
availability-cordoned — and **not** punished for consensus misbehaviour — the
eval may select `fallback` for callers on the strategy that allows it: a plain
consensus over whoever is healthy. No tag quotas needed, because the healthy
pool *is* the externals — config shape in §2 (`extends: default` +
`requiredParticipants: []`). If internals are missing from the round only
because they were punished, stay on the default — never select `fallback`.

Eval (pre-round) — full reference eval in §2. The fallback gate:
`internals.anyPunished()` pins the request to default; otherwise
`internals.allUnavailable()` selects `fallback` for callers on the
`consensus-fallback` strategy.

The eval is where the operator defines the required behavior — the gates and
their order above are the reference, not a mandate. An operator who wants an
active sit-out or admin cordon **anywhere in the network** to block the
downgrade adds `ctx.upstreams.anyPunished()` / `anyOperatorCordon()` checks
first. Callers whose strategy does not allow `fallback` stay on default and
dispute — never silently downgraded.

| Rule | Behavior |
|---|---|
| Availability vs punishment | Cordons carry a typed class (§4.5). The reference eval checks `anyPunished()` on internals before selecting fallback; `allUnavailable()` then admits only availability failures (§3.1) — `punishment`, `operator`, and future cordon classes block the downgrade by construction. |
| In-flight failure | No mid-round switch. That round disputes under default; the tracker records it; the *next* request's eval picks `fallback`. |
| Recovery | Internals healthy again → eval returns default. Automatic both ways. Header `X-ERPC-Consensus-Policy: fallback` makes the degraded grade explicit. |

---

## 7. Serving historical data

Goal: recent → internal+external under default; pruned / historical data only
for callers on a strategy that allows it — via `waive-missing`, which keeps
the default composition but forgives pruned internals. Everyone else disputes
when internals miss.

### 7.1 Policy split (primary) + waivers (safety net)

Two serving grades (§2 table): the fail-closed default for everyone, and
`waive-missing` for the `consensus-waive` strategy — same mixed composition,
but the waivers below may release the internal quota post-round.

Layers that make those grades work:

1. **Block-availability guard (does the historical work).** Internals
 configure `blockAvailability`; the guard short-circuits out-of-range
 requests with `ErrEndpointMissingData` before any provider call. A
 known-old block therefore arrives at analysis as a clean internal
 abstention — exactly what the waivers below release. No separate
 external-only policy is needed.
2. **MissingData waiver.**
 `requiredParticipants[].waiveAgreementOnMissingData`: in
 `enforceWinnerComposition` a failing `minAgreement` quota is waived when
 **at least `minAgreement`** distinct tag-matching participants returned
 `ErrEndpointMissingData` — the normalized, terminal, non-misbehavior edge
 classification — **and no matching participant returned data**. Sibling
 **transport / infra errors do not block** the count; a matching value vote
 holds the quota (that is disagreement, not abstention).

 The floor is **≥ `minAgreement`**, not "every matching participant
 abstained". Releasing the quota must be symmetric with meeting it; a
 unanimous rule over-commits beyond the quota contract and blocks archive
 majorities when a sibling returns a transport error.

 The waiver is **round-complete**: it reports no waivers while responses can
 still arrive. Because composition disputes deliberately do **not**
 short-circuit while `hasRemaining`, a round with an unanswered required
 slot defers the waiver. Wait-cap cancel alone must **not** seal: a result
 may already be buffered when the timer fires, or the upstream call may
 ignore cancellation, so sealing a partial bag could waive while omitted
 tagged data still arrives. Stay fail-closed until every slot has produced
 a terminal acknowledgement. Mid-round "unproven" empty-waiver deferral is
 **debug-only** (do not spam Info / metrics while the round is incomplete).

 Load-time validation: a policy using any waiver is rejected unless at least
 one entry is never-waivable **and** carries `minAgreement > 0`.

3. **Empty-waiver with block proof.**
   Null-shaped pruning is ambiguous at the source, and a block number inside
   an answer is the answering upstreams' own claim: two upstreams that agree on
   a fabricated tx can also agree on a fabricated old block (issue 1363). So
   the proof reads only the block the client's request names.
   `waiveAgreementOnEmptyOutsideRetention` fires only when **≥ `minAgreement`**
   distinct matching participants returned empty, **no** matching participant
   returned non-empty data, **and** the requested block is proven outside
   retention. Proof prefers each abstainer's finite `blockAvailability` lower
   bound (`requestedBlock < lower`); else chain-head depth
   (`head − requestedBlock > retentionBlocks`), where `head` is trusted from
   the network served tip **only** under majority served-tip mode
   (`evm.servedTip.enabledFor: latest`), otherwise the min of ≥ 2 corroborated
   participant poller latests. Default max-mode tip is **not** trusted. A
   request that names no concrete block (hash lookups, tags) → no proof →
   dispute.

 The synthesized default mixed-node tie (a lone non-empty archive group tying
 an equally-sized empty internal group at `agreementThreshold`) produces an
 `ErrConsensusDispute` with no backing group, which the waiver path would
 never see. A `promoteAbstentionWinner` (or equivalent) rescue under the same
 guards is required so that case can still consult the empty-waiver.

4. **Hedge × consensus-slot empty vote.** For lookup methods, hedge recovery
 normally rejects an emptyish `{"result": null}` so it can keep racing. On a
 **consensus slot**, that empty **is the slot's vote** — the empty-waiver
 keys on it. Replacing it with another upstream or `n/a` exhausted hides the
 tagged empty from analysis and the waiver never fires. Consensus-slot hedge
 must keep the empty as the vote.

5. **PreferNonEmpty vs PreferLarger.** The
 `acceptMostCommonValidResult` + PreferNonEmpty rule must gate on **counts**
 (any empty/error group ≥ threshold and any non-empty, only while empty/error
 is at least tied with the leading non-empty) — not solely on
 `getBestByCount()`. Map-iteration order otherwise flakes on equal-count ties,
 and an ungated PreferNonEmpty would override PreferLarger when a non-empty
 group already leads by count.

Strategy-gating is the default recommendation when archive cost or blast radius
matters: never put the waiver on the fail-closed default. If historical
serving is open to all, put the waiver on the default (or drop the strategy
check from the eval) with zero extra JS.

### 7.2 Edge-case matrix

| Scenario | Outcome |
|----------|---------|
| Old block, `blockAvailability` correct | Guard short-circuit → MissingData. **`waive-missing`**: waiver → external 2-agreement. **default**: composition dispute |
| Old block, `blockAvailability` stale/wrong | Provider error normalized to MissingData → same split by selected policy |
| Pruned tx, node returns MissingData error | `waive-missing` (strategy) → waiver; other callers on default → dispute |
| Pruned block, node returns `null` | Empty-waiver serves when ≥ `minAgreement` matching nulls + requested block proven outside retention; recent null (inside retention/bounds) or hash lookup → dispute |
| Data never existed (all null / all MissingData) | `null` served (empty ≥ threshold) / agreed MissingData error — correct |
| Internal + external both return MissingData, one external returns the value | **Composition dispute** — a matching participant that returned data holds the quota; waiver does not fire |
| Just-mined tx not yet on internal (internal null) | Dispute → retry succeeds |
| Internal wrong value (misbehavior) | Quota holds → composition dispute + `punishMisbehavior` |
| Internal outage (infra error ≠ MissingData) | Quota holds → dispute for other callers; the fallback strategy gets `fallback` on the next request (§6) |
| Internal punished/sitout (misbehavior) | Eval stays on default → hard dispute — never falls through to `fallback` |
| External punished while internals down | Reference eval still selects `fallback` for the fallback strategy — the punishment gate scans internals; pin network-wide by checking `ctx.upstreams.anyPunished()` first in your own eval |
| Admin cordons an internal (any reason) | Operator cordon is not availability → `allUnavailable()` false → no downgrade (fail closed by construction) |
| Externals disagree (one archive, one not) | No group ≥ 2 → dispute. Operator pins archive externals via tags |
| `maxParticipants: 4`, one required slot never answers | Wait-cap cancels stragglers, **seals** collection, re-runs winner — waiver may fire; without seal the round disputes forever waiting |
| Hedge on consensus slot gets internal `null` | Keep empty as the slot vote so the empty-waiver can see it |

### 7.3 Requirements

- **R1** waiver fields on `requiredParticipants[]` (opt-in, default off) —
 `waiveAgreementOnMissingData`, `waiveAgreementOnEmptyOutsideRetention` +
 `retentionBlocks`; semantics per
 §7.1.2–7.1.3, localized to `enforceWinnerComposition` /
 `promoteAbstentionWinner`.
- **R2** `blockAvailability` on every internal upstream, so the guard
 classifies known-old requests as MissingData (§7.1.1).
- **R3** archive externals tagged, `minAgreement: 2`, never waivable.
- **R4** blockNumber extraction for eval ctx.
- **R5** observability: `erpc_consensus_composition_waived_total{project,network,tag,reason}`
 (`reason`: `missing_data` / `empty_outside_retention`) + log on every waiver
 fire; `erpc_consensus_composition_waiver_unproven_total{project,network,reason}`
 for fail-closed empty-waiver holds; policy name header/metric as §8.
- **R6** tests: one per matrix row; fallthrough cases (nil user, unknown
 policy name, unlisted method, `return "default"`) first.
- **R7** punished/sitout and operator cordons distinct from unhealthy /
 availability in eval ctx; the reference eval refuses fallback when any
 **internal** is punished, and `allUnavailable()` fails closed on operator
 and future cordon classes.
- **R8** typed `cordonClass`; per-class tracker flags; `allUnavailable()`
 semantics per §3.1.
- **R9** load-time validation: at least one never-waivable entry with
 `minAgreement > 0`.
- **R10** S3 misbehavior path uniqueness / `{timestampMs}`+`{policy}` (§4.4).
- **R11** hedge consensus-slot empty-keep (§7.1.4).
- **R12** PreferNonEmpty count-gated ties (§7.1.5).

---

## 8. Observability

Metric names are `erpc_`-prefixed; all carry `project` + `network` labels.

| Signal | Meaning |
|---|---|
| `X-ERPC-Consensus-Policy` response header | Chosen policy name; **output-only** (never read from the request), emitted only when a selector is configured |
| `erpc_consensus_policy_selected_total{consensus_policy}` | Named policy chosen per served round (`default` = inline / `return "default"` / null / fail-closed) |
| `erpc_consensus_composition_waived_total{tag,reason}` | Waiver fires (`missing_data` / `empty_outside_retention`) |
| `erpc_consensus_composition_waiver_unproven_total{reason}` | Empty-waiver held for missing proof (`no_request_block` / `no_avail_bound` / `no_head`); mid-round deferral is **debug-only**, not counted |
| `erpc_consensus_policy_eval_duration_seconds` | Eval latency histogram |
| `erpc_consensus_policy_eval_failed_total{reason}` | Eval **ran** and failed (`timeout`, `throw`, `unknown_name`) — not for intentional `"default"` |
| Warn log on unknown policy name | Config typo signal |
| Warn log on eval failure/timeout | Fail-closed fallback |

No `pool_exhausted` / eval-bypass metric: the Sobek pool must refill or grow so
every request with a configured selector still runs `evalFunction` (§4.6).

**Alerting:** page on sustained non-default policy selection (e.g. `fallback`
> 5 min) or a step change in `consensus_composition_waived_total`. These are
security signals, not only availability signals.

**Metric cardinality:** `consensus_policy` label cardinality is bounded by the
number of named policies (expected ≤ 10 per network). No per-request unbounded
labels.

---

## 9. Security

- JS is operator-supplied config, same trust level as selection-policy eval.
- Auth data flows **into** the eval (`ctx.user`) — caller-aware decisions are
 the point. No policy conditioning outside the eval.
- Fallback must not fire while any **internal** is punished (R7), and
 `allUnavailable()` fails closed on operator-cordoned and future-class
 nodes (R8).
- `allUnavailable()` is false on an empty set, so a typo'd tag cannot select
 `fallback`.
- Empty-waiver must not fire without block proof outside retention —
 closes the correlated-externals-outvote-internal hole for recent data.
- Strategy-name gating turns credentials into a **correctness control**, not
 only a rate limit. A token issued under a grade strategy can select a weaker
 consensus grade; the `X-ERPC-Consensus-Policy` header lets callers detect a
 degraded grade. Prove the name stamping end-to-end before relying on it in
 production grades (see §13).

---

## 10. Rollout, rollback, and reload

- **Enablement:** per-network config. Add `policies` + `customPolicy` to one
 network first; others keep the inline default.
- **Rollback:** remove `customPolicy` (or fix `evalFunction`) and reload
 config. If config reload is not available, restart the process — recovery
 time is bounded by pod restart.
- **Reload:** on config change, recompile the Sobek program and rebuild the
 VM pool. In-flight requests continue under the policy selected at their
 start; new requests use the new program. If recompilation fails, keep the
 previous program and log the error.
- **Dry-run / validation:** `erpc config validate` (or a dedicated CLI) smoke-
 compiles `evalFunction` and runs it against synthetic contexts before
 deploy. Optional later: dump resolved policies after `extends` (§12.1).

---

## 11. Open questions answered

1. **Why freeform JS instead of a declarative selector?** The operator need is
 an open-ended set (caller, health, block range, method, future dimensions).
 Freeform JS is the weakest commitment that handles observed and unseen
 cases. Selection-policy precedent already pays the Sobek cost.
2. **Why a second Sobek engine instead of reusing `internal/policy`?** Different
 lifecycles (per-request name vs tick-based ordered list). A small standalone
 package is the weaker commitment.
3. **If the waivers landed alone, how much of #1088 is solved?**
 Historical serving (UC3) is solved. Caller-gated fallback (UC2) and custom
 operator policies still need the selector.
4. **Measured eval cost?** Design assumes tens–low hundreds of µs on a
 pre-warmed VM. Phase 1 benchmark must confirm; if p99 ≪ 1ms, decision cache
 stays unbuilt (§5).
5. **Why ≥ `minAgreement` abstentions, not unanimous?** Symmetric with the
 quota floor; a sibling transport error must not block an archive majority;
 a matching data / non-empty vote still holds the quota.

---

## 12. Nice-to-haves / follow-ons

Not part of the core contract. Useful DX improvements from operating
multi-grade maps:

### 12.1 Resolved-policy dump (medium)

`erpc config validate --dump-policies` (or equivalent) prints post-`extends`
configs so operators can see that `fallback` kept live `ignoreFields` / prefer /
punish. Without this, sparse overlays stay opaque.

### 12.2 Selector simulate (nice-to-have)

Dry-run “this synthetic request → which policy name” for CI / config review,
without running a consensus round.

### 12.3 Explicitly out of scope here

- Multi-hop / mixin `extends` graphs (v1 `extends` is one level — §2)
- Silent inherit from inline without `extends`
- Post-round JS grading / exposing `valueGroups`
- Decision cache (still §5 — only if measured cost forces it)
- External-only `historical` grade for known-old blocks — deferred: the
 waivers cover pruning today (§7); revisit only if they prove insufficient

---

## 13. Open topics

Items that affect production caller-gating, but are **not** locked into the v1
contract yet:

1. **End-to-end proof of strategy-name-gated grades.** Selector health /
 block-range / cordon predicates can be validated without auth; live
 strategy-gated `fallback` / `waive-missing` still needs a
 battle-test plan — name stamping across all five strategies, then eval
 gating.

2. **Receipt / method `ignoreFields` gaps across vendors.** Separate from this
 engine, but multi-grade maps amplify disputes when archives disagree on
 receipt hashes. `extends: default` helps share one hygiene list; which
 fields belong in the shared list remains operator/chain-specific.
