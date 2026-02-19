# Autonomous Harness

Goal: high-success autonomous coding loops with low supervision.

## Artifact Graph

- `AGENTS.md`: entry rules, safety rails, command contract.
- `review/README.md`: harness index + execution path.
- `review/repo-map.md`: generated repo topology snapshot.
- `review/skills-shell-tips.md`: skill-routing + shell-operation guardrails.
- `review/contracts.md`: user-facing/API/config invariants.
- `review/data-models.md`: state/caches/storage invariants.
- `review/checklist.md`: severity rubric + output format.
- `review/runbooks/*.md`: bugfix/feature/review procedures.
- `scripts/agent-harness/*.sh`: mechanical checks + maintenance.
- `.github/workflows/agent-automations.yml`: scheduled autonomous loops.
- `artifacts/agent/`: boundary for generated agent runtime artifacts.

## Trigger Matrix

- Changes in `erpc/`, `architecture/`, `clients/`, `upstream/`, `consensus/`:
  - confirm `review/contracts.md` + `review/checklist.md` still cover behavior.
- Changes in `data/`, `scylla/`:
  - confirm `review/data-models.md` invariants still accurate.
- Changes in `erpc.dist.yaml`, `erpc.dist.ts`, `erpc.yaml`, `prometheus.yaml`, `docker-compose.yml`, `kube/`:
  - confirm config/ops contract docs still accurate.
- Changes in workflow/runtime (`.github/workflows/`, `Makefile`, `scripts/`):
  - confirm command docs in `AGENTS.md` + `review/README.md` still accurate.
- New/moved top-level packages:
  - refresh `review/repo-map.md`.

## Maintenance Cadence

Per PR:
1. `scripts/agent-harness/context.sh --staged`
2. `scripts/agent-harness/suggest-doc-updates.sh --staged`
3. `scripts/agent-harness/review-load.sh --staged`
4. `scripts/agent-harness/skills-shell-check.sh`
5. `scripts/agent-harness/check.sh`

Weekly (or after major refactors):
1. `scripts/agent-harness/update-repo-map.sh`
2. `scripts/agent-harness/check.sh`
3. `scripts/agent-harness/suggest-doc-updates.sh --strict HEAD~20..HEAD`
4. `scripts/agent-harness/random-bug-scan.sh --count 2`

Continuous automation:
- `scripts/agent-harness/pr-health.sh` on schedule for mergeability drift.
- `scripts/agent-harness/merged-digest.sh` daily for contribution visibility.

## Merge Policy

- Harness checks must pass.
- Bugfixes: regression test when feasible.
- If a contract changed, update docs in same PR.
