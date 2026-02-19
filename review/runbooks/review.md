# Runbook: Review

1. Read `AGENTS.md`, `review/checklist.md`.
2. Build context:
   - `scripts/review-repo-map.sh`
   - `scripts/review-impact-map.sh <range>` or `--staged`
   - `scripts/agent-harness/review-load.sh <range>` or `--staged`
3. Inspect diffs/tests first on high-risk paths (`erpc`, `upstream`, `consensus`, `data`, `auth`).
4. File findings by severity with confidence score.
5. Separate low-confidence watchlist.
6. Note missing tests + observability gaps explicitly.
