# Consensus Fallback Policy

**Status:** proposed — implementation plan for a single PR
**Motivation thread:** follow-up to #1008 (consensus winner-composition quota via `minAgreement`)

## 1. Problem

Mixed-node consensus (#1008) enforces per-tag composition quotas on the winning response group.
A typical deployment requires responses from at least two tagged groups — for example,
`provider:internal ≥ 1` AND `provider:external ≥ 1` — before a result is returned.

Internal nodes can become unhealthy in two ways that both produce `ErrConsensusCompositionDispute`:

- **Absent** — nodes are down, unreachable, or placed into sitout by `punishMisbehavior`.
- **Disputing** — nodes are responding but with wrong data (out-of-sync, stale, compromised).

In both cases the standard policy fails consistently even when other participant groups agree.
Today the only recourse is a **manual failover with security approval** — a slow, expensive
process that trades availability for control.

This feature replaces that manual process with an automatic, configurable fallback that relaxes
per-group composition quotas under controlled conditions. Different services have different
security vs. availability requirements: `fallbackAllowedUsers` gives operators per-service
control over which callers may activate the fallback, so high-security services can opt out
while availability-sensitive ones benefit automatically.

Security approval is centralized at the eRPC config layer. Permitted callers are declared in
`fallbackAllowedUsers` — clients do not need to opt in or pass any special header to use
fallback. The only client-facing directive is opt-out (`X-ERPC-Skip-Consensus-Fallback: true`),
for callers that explicitly require the standard policy to hold. This means security teams
approve fallback access by reviewing eRPC configuration, not by auditing per-client header
usage across services.

## 2. Prerequisite

`minAgreementFallback` is only valid when `requiredParticipants` entries with `minAgreement`
are present. The config validator rejects `minAgreementFallback` on any entry whose owning
`requiredParticipants` block has no `minAgreement`. This fallback feature requires mixed-node
consensus (#1008) to be configured — it relaxes per-group thresholds; it does not replace
the threshold mechanism.

## 3. Design

### 3.1 Config shape

`minAgreementFallback` is added inline on each `requiredParticipants` entry.
`fallbackTrigger`, `fallbackWindow`, `fallbackThreshold`, and `fallbackAllowedUsers` are
added at the top-level `consensus` block.

```yaml
consensus:
  fallbackTrigger: circuit-breaker  # realtime | circuit-breaker
  fallbackWindow: 5m                # circuit-breaker only: rolling window
  fallbackThreshold: 0.8            # circuit-breaker only: dispute rate to trip
  fallbackAllowedUsers:             # omit to allow all authenticated callers
    - "service-a"
    - "service-b"
  requiredParticipants:
    - tag: "provider:internal"
      minParticipants: 1
      minAgreement: 1
      minAgreementFallback: 0       # 0 = group is optional in fallback mode
    - tag: "provider:external"
      minParticipants: 2
      minAgreement: 1
      minAgreementFallback: 2       # ≥2 independent external providers must agree
```

| Field | Type | Default | Description |
|---|---|---|---|
| `minAgreementFallback` | `int` | `minAgreement` | Relaxed per-group agreement quota for fallback evaluation. `0` = group optional. |
| `fallbackTrigger` | `"realtime" \| "circuit-breaker"` | — | Required when any group has `minAgreementFallback`. Pick-one. |
| `fallbackWindow` | `Duration` | TBD | Circuit-breaker: rolling window for dispute-rate tracking. |
| `fallbackThreshold` | `float` | TBD | Circuit-breaker: `ErrConsensusCompositionDispute` rate (`0..1`) over `fallbackWindow` to trip the breaker. |
| `fallbackAllowedUsers` | `[]string` | `nil` (all) | `userId` values permitted to activate fallback. Omit = all authenticated callers. Unauthenticated requests are always blocked. |
| `fallbackProbeRate` | `float` | `0.05` | Circuit-breaker only: fraction of requests (`0..1`) that attempt standard evaluation while the breaker is tripped, to detect recovery. |

### 3.2 Evaluation order

1. **`lowParticipantsBehavior` checked first.** If fewer valid participants responded than
   `agreementThreshold`, the existing low-participants behavior fires and fallback is not
   considered.
2. **Standard policy evaluated.** If it passes, return result with
   `X-ERPC-Consensus-Policy: standard`.
3. **Fallback eligibility check.** Fallback proceeds only when all of the following hold:
   - Standard policy failed with `ErrConsensusCompositionDispute`.
   - Caller's `userId` is non-empty (authenticated) AND (`fallbackAllowedUsers` is empty OR `userId` is in `fallbackAllowedUsers`).
   - `X-ERPC-Skip-Consensus-Fallback: true` was not sent.
   - `fallbackTrigger: circuit-breaker` — the breaker is tripped for this network (see §3.3).
     `fallbackTrigger: realtime` — no additional gate.
4. **Fallback policy evaluated.** Apply `minAgreementFallback` quotas. If satisfied, return
   result with `X-ERPC-Consensus-Policy: fallback`.
5. **Both fail.** Return error. `X-ERPC-Consensus-Policy: standard` (last attempted policy,
   or the policy in effect when fallback was blocked before being attempted).

`eth_sendRawTransaction` is exempt from `minAgreementFallback` enforcement, inheriting the
same exemption already applied to `minAgreement` in #1008.

`X-ERPC-Consensus-Policy` is absent from the response when consensus is bypassed via
`X-ERPC-Skip-Consensus`.

### 3.3 Trigger modes

**`realtime`** — fallback eligibility is re-evaluated on every request independently.
Stateless. No per-network state is maintained. Suitable for testing or simple deployments.

**`circuit-breaker`** — tracks a per-network rolling rate of `ErrConsensusCompositionDispute`
errors against total consensus requests over `fallbackWindow`. Once the rate crosses
`fallbackThreshold`, the network enters fallback mode. While tripped, most requests evaluate
directly against fallback quotas — subject to `fallbackAllowedUsers` and
`X-ERPC-Skip-Consensus-Fallback` as always. A configurable fraction of requests
(`fallbackProbeRate`, default `0.05`) still attempt standard evaluation while tripped; a
standard-policy success from a probe request resets the breaker. Fallback successes do not
count toward reset. Circuit-breaker is the recommended production mode.

### 3.4 Misbehaving upstreams and sitout

When an internal upstream consistently returns wrong data, `punishMisbehavior` applies a
rate-limited penalty and eventually places the upstream into sitout (`sitOutPenalty` duration).
A sitout upstream is excluded from consensus rounds and therefore absent from its tag group.
These consistent disputes — before sitout and after it — contribute to the
`ErrConsensusCompositionDispute` rate that trips the circuit-breaker. Once tripped, external
providers can satisfy fallback quotas alone.

The minimum quorum required from the external group while the internal group disputes or is
absent is controlled by `minAgreementFallback` on the external group entry.

### 3.5 Headers

| Header | Direction | Values | Notes |
|---|---|---|---|
| `X-ERPC-Skip-Consensus-Fallback` | Request | `true` | Disables fallback for this request. Omitted = fallback allowed. Follows the `X-ERPC-Skip-*` directive convention. |
| `X-ERPC-Consensus-Policy` | Response | `standard` \| `fallback` | Present only when consensus is active and a policy was attempted. `standard` when fallback was blocked or not attempted. Absent when `X-ERPC-Skip-Consensus` bypasses consensus entirely. |

`X-ERPC-Skip-Consensus-Fallback` is subject to `allowClientDirectives` gating in the same
way as other `X-ERPC-Skip-*` directives.

## 4. Invariants (each backed by a test)

- **I1 — Standard-only by default.** When no group has `minAgreementFallback` set (or all
  entries default to `minAgreement`), behavior is byte-identical to #1008 with no fallback
  ever attempted.
- **I2 — Dispute-triggered activation.** Fallback evaluates on any `ErrConsensusCompositionDispute`
  from the standard policy, whether the failure is due to absent participants or present-but-disagreeing
  participants. The trigger is the error, not its cause.
- **I3 — Directive suppression.** `X-ERPC-Skip-Consensus-Fallback: true` always prevents
  fallback evaluation, regardless of trigger mode or breaker state. The caller receives
  `X-ERPC-Consensus-Policy: standard` and the standard policy error.
- **I4 — User gate.** Unauthenticated callers (empty `userId`) never activate fallback,
  regardless of `fallbackAllowedUsers`. When the list is set, a caller whose `userId` is not
  in it is also blocked. Omitting `fallbackAllowedUsers` allows all authenticated callers.
- **I5 — `eth_sendRawTransaction` exemption.** The method is exempt from `minAgreementFallback`
  enforcement, identical to the `minAgreement` exemption in #1008. No fallback attempt is made.
- **I6 — `lowParticipantsBehavior` priority.** The low-participant check fires before fallback
  eligibility is considered. A low-participant failure does not trigger or increment the
  circuit-breaker counter.
- **I7 — Circuit-breaker counts composition disputes only.** The failure counter increments
  on `ErrConsensusCompositionDispute` from the standard policy evaluation. It does not
  increment on `lowParticipantsBehavior` errors or infrastructure / timeout errors.
- **I8 — Breaker reset only on standard success via probe.** A fallback-mode success does not
  decrement the counter or advance breaker reset. Only a standard policy success from a probe
  request (sampled at `fallbackProbeRate`) while the breaker is tripped resets it. Without
  probe sampling, the breaker has no path to observe recovery and would remain tripped
  indefinitely.
- **I9 — Header absent on skip.** `X-ERPC-Consensus-Policy` is absent when
  `X-ERPC-Skip-Consensus` bypasses consensus. No header is written.
- **I10 — Prerequisite enforcement.** Config load fails (startup error) when any entry has
  `minAgreementFallback` set but the corresponding `requiredParticipants` block has no
  `minAgreement`, or when `fallbackTrigger` is absent while any entry has `minAgreementFallback`.

## 5. Backward compatibility

- **No config changes for existing deployments.** `minAgreementFallback` defaults to
  `minAgreement` when omitted — fallback evaluation applies standard quotas, producing
  identical behavior to #1008.
- **Wire surface unchanged.** `X-ERPC-Skip-Consensus-Fallback` and `X-ERPC-Consensus-Policy`
  are new headers; no existing header is renamed or removed.
- **Error surface unchanged.** `ErrConsensusCompositionDispute` (HTTP 409) is the error code
  for both standard and fallback failures. No wire-visible error code changes.
- **`punishMisbehavior` unchanged.** The sitout/rate-limiter mechanism is untouched; the
  disputes and sitout that result naturally feed the fallback trigger.

## 6. Open questions

1. **`fallbackWindow` and `fallbackThreshold` defaults.** Values affect breaker sensitivity
   and recovery speed. To be decided based on observed internal-node dispute patterns.
   Candidates: `5m` / `0.8`.

2. **`agreementThreshold` is redundant when `minAgreement` is configured.** When
   `requiredParticipants[].minAgreement` is set, the effective agreement threshold is fully
   determined by `sum(minAgreement)` — specifying both is redundant and a potential source
   of misconfiguration (what is the correct behavior when they conflict?). Proposed change
   (applicable to #1008 and extended here to `minAgreementFallback`):
   - **Without `minAgreement`**: `agreementThreshold` required as today.
   - **With `minAgreement`**: `agreementThreshold` derived as `sum(minAgreement)`; config
     validator rejects an explicit value to prevent silent conflicts.
   - **With `minAgreementFallback`**: effective fallback threshold derived as
     `sum(minAgreementFallback)` — no separate `agreementThresholdFallback` field needed.

   This is a breaking config change for operators who currently set both fields. To be
   resolved in #1008 or as part of this PR.

## 7. Test matrix

| Area | Cases |
|---|---|
| Standard-only (I1) | No `minAgreementFallback` → identical to #1008 in all scenarios |
| Dispute-triggered activation (I2) | Standard fails with composition dispute → fallback evaluates; standard fails with low-participants → fallback blocked |
| Directive suppression (I3) | `X-ERPC-Skip-Consensus-Fallback: true` with realtime and circuit-breaker modes |
| User gate (I4) | Unauthenticated always blocked; allowlist set: callers in/out; allowlist omitted: all authenticated pass |
| `eth_sendRawTransaction` (I5) | Composition dispute on that method → no fallback attempted |
| `lowParticipantsBehavior` priority (I6) | Low-participant threshold not met → fires before fallback; does not increment breaker counter |
| Counter scope (I7) | Composition dispute increments counter; low-participants error does not; timeout error does not |
| Breaker reset (I8) | Probe request standard success while tripped → resets; fallback success while tripped → no reset; no probe path → breaker never resets |
| Header absent on skip (I9) | `X-ERPC-Skip-Consensus` set → no `X-ERPC-Consensus-Policy` header in response |
| Prerequisite validation (I10) | `minAgreementFallback` without `minAgreement` → startup error; `minAgreementFallback` without `fallbackTrigger` → startup error |
| Realtime mode end-to-end | Standard fails composition dispute → fallback succeeds → `X-ERPC-Consensus-Policy: fallback` |
| Circuit-breaker trip | Dispute rate crosses threshold → mode flips; subsequent requests skip standard |
| Circuit-breaker recovery | Standard succeeds while tripped → mode resets |
| Misbehavior → sitout → fallback | `punishMisbehavior` sitout on internal upstream → disputes accumulate → breaker trips → fallback activates |
| Race | `go test -race` on consensus suite with concurrent requests and breaker state transitions |

## 8. Explicit non-goals

- **No new endpoint or routing surface.** Fallback is a policy applied within the existing
  consensus execution path, not a separate proxy tier or duplicate endpoint.
- **No fallback without mixed-node consensus configured.** Fallback is not a stand-alone
  mode; it requires `requiredParticipants` with `minAgreement`.
- **No relaxation of `agreementThreshold`.** The overall agreement count is unchanged;
  only per-group composition quotas are relaxed.
- **No client-controlled fallback threshold.** `fallbackAllowedUsers` is an operator list;
  callers cannot add themselves. The only client directive is opt-out, not opt-in or
  threshold adjustment.

## 9. Acceptance

- Full test matrix (§7) green, including race runs.
- Config-load validation rejects every invalid combination (I10).
- `X-ERPC-Consensus-Policy` header present and correct on every consensus-evaluated response
  in the integration suite.
- Circuit-breaker counter verified to increment on composition disputes and not on
  low-participant or infrastructure errors.
