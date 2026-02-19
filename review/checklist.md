# Review Checklist

Use for exhaustive issue harvest. One issue per bullet.

## CRITICAL

- Auth bypass, secrets exposure, unsafe config defaults.
- JSON-RPC breaking behavior or response corruption.
- Consensus/integrity violations (wrong data accepted).
- Cache corruption or invalidation bugs causing stale/incorrect data.

## HIGH

- Retry/timeout/hedge logic regressions.
- Upstream selection policy changes without tests.
- Race conditions, goroutine leaks, context cancel missing.
- Rate limiter regressions or cost blowups.

## MEDIUM

- Performance regressions (allocs, hot path, N+1 upstream calls).
- Observability gaps (missing metrics/logs/tracing).
- Config defaults unclear or inconsistent with templates.

## LOW

- Style/docs nits.

## Output Requirements

- Issue index by severity.
- Confidence score per issue (0/25/50/75/100).
- Separate low-confidence watchlist.
