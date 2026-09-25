# Native Per-Request Upstream Eligibility

**Status:** proposed — implementation plan for a single PR
**Motivation thread:** follow-up to #1008 (consensus vote dedup / minAgreement)

## 1. Problem

The only mechanism to constrain which upstreams may serve a request is the
`use-upstream` directive: a **glob string** matched against upstream ID and
tags **on every selection attempt** inside `NormalizedRequest.NextUpstream`.

Consequences:

- Internal code cannot say "these specific upstream objects are in/out for
  this request" without encoding intent into ID/tag glob strings and paying
  glob evaluation per pick.
- Selection policy, consensus, integrity checks, and future callers each
  need their own ad-hoc filtering (or can't filter at all).
- The glob runs on the hot path for every pick attempt of every request
  that carries the directive.

Wanted: a **reference-based** allow/deny property on the request itself —
`use-upstream` becomes just one producer of it, translated once.

## 2. Design

### 2.1 State

One new field on `NormalizedRequest`, guarded by the existing
`upstreamMutex` (no new locks):

```go
// eligibility constrains which upstreams may serve this request.
// nil maps mean "no constraint". Deny always wins over allow.
type upstreamEligibility struct {
    allow map[Upstream]struct{} // nil => all upstreams allowed
    deny  map[Upstream]struct{} // nil => none denied
}
```

Keyed by interface reference, not ID string: upstream objects are
long-lived singletons per network registry; reference identity is exactly
"the same upstream" with zero ambiguity and zero string matching.

### 2.2 API

All methods nil-receiver-safe and concurrency-safe:

| Method | Semantics |
|---|---|
| `RestrictUpstreams(ups []Upstream)` | Intersects the allow-set. Repeat calls narrow, never widen. Empty slice = allow nothing (explicit). |
| `ExcludeUpstream(u Upstream)` | Adds to deny-set. Idempotent. |
| `EligibleUpstream(u Upstream) bool` | `deny` lookup, then `allow` lookup. The ONLY read path. |
| `resetEligibilityAllow()` (unexported) | Clears allow-set only; used by re-translation (§2.4). Deny survives — see invariant I3. |

### 2.3 Enforcement — single choke point

`NextUpstream` replaces the per-pick `UpstreamMatchesSelector` glob block
with:

```go
if !r.EligibleUpstream(upstream) {
    continue
}
```

No other selection path exists (`NextUpstream` is the sole reservation
point; verified: hedge legs, retries, and consensus slots all route
through it via the shared request object).

### 2.4 `use-upstream` translation

At `SetUpstreams` (the single point where the candidate list binds to the
request, called on every network `Forward`):

1. If `directives.UseUpstream == ""` → clear allow-set (no constraint).
2. Else resolve the matcher against the **incoming** list via the existing
   `UpstreamMatchesSelector` — once — and store matches as the allow-set.

Why here and not at directive-parse time: upstreams are lazy-loaded; the
candidate list can gain members between Forwards. `SetUpstreams` re-runs
per Forward, so translation self-heals against list changes — preserving
today's per-pick semantics exactly (a late-appearing matching upstream
becomes selectable, a directive matching nothing keeps failing with
no-candidates).

The per-pick glob call in `NextUpstream` is deleted. Net hot-path change:
glob-per-pick → map-lookup-per-pick.

## 3. Invariants (each backed by a test)

- **I1 — Directive parity.** For every selector form (`exact-id`, `*` glob,
  `?`, tag selector, `!negation`, comma lists): the set of upstreams
  selectable after translation is byte-identical to today's per-pick
  matching. Table-driven test enumerating all forms against a mixed pool.
- **I2 — Lazy-load parity.** Upstream appears after first Forward →
  next `SetUpstreams` makes it selectable iff the directive matches it.
  Upstream removed from pool → stale allow entry is inert (never returned
  because it is no longer in `upstreamList`; see leak analysis §5).
- **I3 — Deny survives re-translation.** `ExcludeUpstream` entries are
  never cleared by `SetUpstreams`. Allow-set is derived state (from the
  directive); deny-set is imperative state (from callers). Only explicit
  request completion drops it (with the request object itself).
- **I4 — Deny beats allow.** An upstream in both sets is ineligible.
- **I5 — Reservation/eligibility/history independence.** `ConsumedUpstreams`
  (reservation), `ErrorsByUpstream` (history), and eligibility (policy)
  never write to each other. `MarkUpstreamCompleted`'s re-admission
  (retryable/empty freeing) is untouched — an upstream freed for retry is
  still subject to eligibility.
- **I6 — Cancelled hedge neutrality.** The cancel branch of
  `MarkUpstreamCompleted` frees the reservation and must NOT touch
  eligibility (a cancelled leg says nothing about policy).
- **I7 — Exhaustion diagnosability.** When eligibility filters out every
  candidate, the returned `ErrNoUpstreamsLeftToSelect` message
  distinguishes "excluded by eligibility/directive" from "all consumed" —
  operators must be able to tell a typo'd selector from saturation.
- **I8 — Nil/zero cost.** Requests without directive and without caller
  restrictions carry nil maps: zero allocations, one nil-check per pick.

## 4. Backward compatibility

- **Wire surface unchanged:** `X-ERPC-Use-Upstream` header / query param /
  directive JSON parse exactly as today; only the evaluation site moves.
- **No config changes**, no new YAML fields, no TS type changes.
- **Selection order unchanged:** eligibility only filters; round-robin
  cursor, score ordering, quota promotion (`reorderForParticipantQuota`)
  are untouched.
- **Error surface:** same error type on no-candidates; message gains a
  reason suffix (I7). Nothing matches on that message (verified: callers
  match by error code only).
- **`Copy()`/clone paths:** request copies carry eligibility by value-copy
  of the two maps (same rule as `directives` today). Audit every
  `NormalizedRequest` duplication site as part of the PR.

## 5. Security / leak analysis

- **Memory:** maps are keyed by upstream references. Upstream objects are
  process-lifetime registry singletons — holding a reference cannot leak
  or double-free; the maps die with the request. A dynamically REMOVED
  upstream (registry reload) held in a deny-map keeps its (small) struct
  alive only until the request completes: bounded by request lifetime,
  no accumulation.
- **Concurrency:** all mutations under `upstreamMutex`, same lock already
  taken by `NextUpstream`/`MarkUpstreamCompleted` — no new lock ordering,
  no read of the maps outside the lock. Race-detector run over the
  consensus + hedge suites is part of acceptance.
- **Authorization surface:** unchanged. `use-upstream` was already
  client-controllable; translation neither widens what a client can
  express nor bypasses `allowHeaderOverrides`-style gating (directive
  parsing untouched). Internal `RestrictUpstreams`/`ExcludeUpstream` are
  Go-API-only — not reachable from the wire.
- **DoS:** allow/deny sizes are bounded by pool size (translation) and by
  internal callers (deny). No client-controlled unbounded growth: a
  malicious selector string is still a single glob evaluated once per
  Forward instead of per pick — strictly cheaper than today.
- **Fail-closed bias:** empty allow-set (directive matched nothing) fails
  with no-candidates, exactly as today. Deny of every upstream likewise.
  No path where a filtering failure silently widens selection.

## 6. Test matrix (all in one PR)

| Area | Cases |
|---|---|
| Translation parity (I1) | every selector form × match/no-match/partial pools |
| Lazy load (I2) | upstream appears / disappears between Forwards |
| Deny semantics (I3, I4) | deny+allow overlap; deny persists across re-translation; idempotency |
| Independence (I5, I6) | retryable-free then deny; cancelled hedge; empty-result re-admission with and without deny |
| Exhaustion (I7) | directive matching zero; deny-all; mixed consumed+denied |
| Hot path (I8) | benchmark `NextUpstream` before/after: no-directive and directive cases |
| Copy paths | request clone carries eligibility; mutations after clone don't alias |
| Race | `go test -race` on consensus, hedge, network executor suites |
| E2E | live server: `use-upstream` header end-to-end unchanged (happy + no-match) |

## 7. Explicit non-goals

- **No consensus behavior change.** Consensus round-scoped no-re-ask
  ("exclude upstreams that already delivered a final answer") becomes a
  one-line `ExcludeUpstream` call for a FUTURE decision — deliberately not
  flipped here. Rationale: #1008's keep-best dedup currently harvests
  value from duplicate answers (empty→non-empty, stale→fresh upgrades),
  and `maxParticipants > pool` hedge-coverage configs depend on re-asks.
  Gate any change on the duplicate-rate metric
  (`collectedResponses - totalParticipants`).
- No deprecation of `use-upstream` — it is the wire-facing producer of
  this mechanism, permanently.
- No selection-ordering changes (least-picked-first etc. — separate
  discussion; round-robin wrap-around already approximates it).

## 8. Acceptance

- Full test matrix (§6) green, including race runs and the E2E directive
  check.
- `NextUpstream` benchmark shows no regression for the nil case and an
  improvement for the directive case.
- Grep-level proof in the PR description that no caller reads
  `directives.UseUpstream` after `SetUpstreams` except the translation
  site (single enforcement point, no shadow paths).
