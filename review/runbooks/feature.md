# Runbook: Feature

1. Define contract delta (API/config/runtime behavior).
2. Locate impacted packages via `scripts/review-impact-map.sh`.
3. Add/update tests for new behavior.
4. Implement incrementally; keep diffs reviewable.
5. Run `make agent-gate` (`agent-gate-full` for high-risk changes).
6. Update docs for new capabilities/flags/constraints.
7. Handoff with migration notes + known follow-ups.
