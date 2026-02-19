# AGENTS.md

Codex + review agents. eRPC repo (Go + TS + infra/config).

## Autonomous Harness Protocol

- Start: read this file + `review/README.md`.
- Build context: `scripts/agent-harness/context.sh` (`--staged` or `<range>` optional).
- Pick runbook:
  - `review/runbooks/bugfix.md`
  - `review/runbooks/feature.md`
  - `review/runbooks/review.md`
- Keep docs current when code/config/contracts shift. Use `review/autonomy.md`.
- Before handoff:
  - `make agent-check`
  - `make agent-review-load`
  - `make agent-gate` (or `make agent-gate-full`)

## Harness Artifacts

- Entry + guardrails: `AGENTS.md`
- Harness index: `review/README.md`
- Skills+shell guidance: `review/skills-shell-tips.md`
- Repo map: `review/repo-map.md` (generated)
- Contract surfaces: `review/contracts.md`
- Stateful/data invariants: `review/data-models.md`
- Severity rubric: `review/checklist.md`
- Maintenance triggers: `review/autonomy.md`
- Mechanical scripts: `scripts/agent-harness/*.sh`
- Artifact boundary: `artifacts/agent/`
- Scheduled loops: `.github/workflows/agent-automations.yml`

## Review Depth Protocol (Repo-Aware)

- Read `review/README.md` + `review/checklist.md` first.
- Run `scripts/review-repo-map.sh` for current repo map.
- Run `scripts/review-impact-map.sh <range>` or `--staged` for impact map.
- Findings: issue index by severity + confidence scores + low-confidence watchlist.

## Severity

- **CRITICAL**: Security, data loss, breaking API behavior, auth bypass.
- **HIGH**: Reliability, correctness, consensus integrity, cache corruption.
- **MEDIUM**: Performance, observability gaps, config drift.
- **LOW**: Style/docs nits.

## Review Focus (eRPC-specific)

- JSON-RPC correctness + error normalization.
- Re-org aware cache correctness + invalidation.
- Consensus policy + integrity checks.
- Upstream selection, retries, timeouts, hedging, circuit breakers.
- Rate limiting + cost control.
- Auth flows (basic/JWT/SIWE), secret handling.
- Telemetry/metrics/logging (Prometheus, tracing).
- Config schema changes (`erpc.yaml`, `erpc.dist.*`).

## Canonical Commands

- Context: `make agent-context`
- Refresh map: `make agent-refresh`
- Harness checks: `make agent-check`
- Direct check script: `scripts/agent-harness/check.sh`
- Skills+shell check: `scripts/agent-harness/skills-shell-check.sh`
- Review bottleneck budget: `make agent-review-load`
- Direct review-load script: `scripts/agent-harness/review-load.sh`
- PR mergeability scan: `make agent-pr-health`
- Random latent-bug sweep: `make agent-random-bug`
- Contribution digest: `make agent-digest`
- Artifact boundary path: `artifacts/agent/`
- Full autonomous gate:
  - `make agent-gate` (build + fast tests)
  - `make agent-gate-full` (build + full tests)
