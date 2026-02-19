# Review + Autonomy Playbook (eRPC)

Repo-aware, exhaustive reviews + autonomous coding loops for Go + TS + config.

## Quickstart

1. Read `AGENTS.md`.
2. Build context:
   - `make agent-context`
   - or `scripts/agent-harness/context.sh --staged`
3. Pick runbook:
   - `review/runbooks/bugfix.md`
   - `review/runbooks/feature.md`
   - `review/runbooks/review.md`
4. Run harness checks: `make agent-check`.
5. Check review load budget: `make agent-review-load`.

## Core Artifacts

- Repo map: `review/repo-map.md`
- Review checklist: `review/checklist.md`
- Contracts: `review/contracts.md`
- Data models: `review/data-models.md`
- Skills/shell guidance: `review/skills-shell-tips.md`
- Maintenance triggers: `review/autonomy.md`

## Mechanical Maintenance

- Refresh generated map: `make agent-refresh`
- Verify harness integrity: `make agent-check`
- Verify skills/shell conventions: `make agent-skills-shell-check`
- Review-load signal: `make agent-review-load`
- PR mergeability signal: `make agent-pr-health`
- Random latent-bug scan: `make agent-random-bug`
- Daily digest output: `make agent-digest`
- Get doc update hints for a diff:
  - `scripts/agent-harness/suggest-doc-updates.sh --staged`
  - `scripts/agent-harness/suggest-doc-updates.sh HEAD~5..HEAD`

## Output Contract

- Issue index by severity.
- Confidence score per issue (0/25/50/75/100).
- Separate low-confidence watchlist.
