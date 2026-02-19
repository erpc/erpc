# Runbook: Bugfix

1. Reproduce bug.
2. Add failing regression test first (when feasible).
3. Locate root cause (avoid patch-over symptom).
4. Implement fix with minimal surface area.
5. Run targeted tests + `make agent-gate`.
6. If behavior/config contract changed: update docs (`review/contracts.md`, `review/data-models.md`, config docs).
7. Capture residual risk in handoff.
