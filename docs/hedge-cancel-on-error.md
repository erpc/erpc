# Hedge: "Don't cancel on first error" exploration

## Current behavior

- **CancelIf** in the hedge policy cancels other hedges when:
  - This execution returns **any** non-exhaustion error, or
  - This execution returns an **accepted** result (non-empty, or empty but in `emptyResultAccept` and not consensus).
- We explicitly **do not** cancel on `ErrUpstreamsExhausted` / `ErrCodeNoUpstreamsLeftToSelect` so other hedges can still finish.

So: "first to return (result or error) wins" — we cancel as soon as one execution returns, unless that return is exhaustion.

## Why "don't cancel on first error" was not the default

1. **Latency when all upstreams fail**  
   If we only cancelled on success, then when every hedge fails we would wait for the **slowest** execution instead of returning as soon as the first one fails. Example: Alchemy errors in 100ms, QuickNode in 3s → today we return in ~100ms; if we stopped cancelling on error we’d wait ~3s for the same outcome.

2. **Original mental model**  
   With a single execution (no hedge), "first response" is the only response. Adding hedge kept the same idea: "first response (success or failure) wins" to avoid extra wait when the first response is already a failure.

3. **Loop-over-upstreams**  
   Each execution runs a **loop** over upstreams (`maxLoopIterations` = 1 in consensus, `UpstreamsCount()` otherwise). With hedge, executions **share** `ConsumedUpstreams`, so:
   - Execution 1 often gets upstream A, execution 2 gets B.
   - Execution 1 can return after **one** upstream (e.g. error from A, then next `NextUpstream` hits duplicate or exhausted and the loop breaks).
   - So "first return" is often "first error from one upstream", not "tried everything". Letting that first error cancel the other hedge (e.g. QuickNode) is what causes the trace_filter case: Alchemy errors fast, we cancel QuickNode which would have succeeded in 2–3s.

So the loop doesn’t remove the need for "don’t cancel on first error"; it’s what makes the current "cancel on any return" strict — one execution can exit quickly with an error and kill the other.

## Side effects of "don’t cancel on first error"

| Scenario | Current (cancel on any return) | Don’t cancel on error |
|--------|--------------------------------|------------------------|
| First returns **success** | Other hedges cancelled ✓ | Same ✓ |
| First returns **error**, another would succeed | Other hedges cancelled ✗ (e.g. QuickNode discarded) | Other can complete ✓ |
| All return **errors** | Return after first error (low latency) ✓ | Wait for slowest (higher latency) ✗ |
| First returns **client/execution** error (same everywhere) | Other hedges cancelled, we return quickly ✓ | We’d still wait for others for no benefit ✗ |

So a good refinement is: **cancel only on terminal errors**, not on every error.

- **Terminal errors** (cancel others): same on every upstream, no need to wait for more.
  - `IsClientError(err)` (bad request, range exceeded, etc.)
  - `ErrCodeEndpointExecutionException` (e.g. revert)
- **Non-terminal errors** (don’t cancel): method not supported, 5xx, timeout, missing data, etc. — another hedge might succeed.

## Recommended refinement

In `CancelIf`, instead of:

```go
if err != nil {
    return true  // cancel on any error
}
```

use:

```go
if err != nil {
    // Cancel only on terminal errors; let other hedges complete on transient/upstream-specific errors.
    if common.IsClientError(err) || common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
        return true
    }
    return false
}
```

Effects:

- **trace_filter**: Alchemy returns "method not supported" → we don’t cancel → QuickNode can return success (or its own error) → we use the first success or aggregate failures.
- **All fail**: We wait for all hedges to finish, then failsafe/retry sees the errors. Latency is max of hedge durations instead of min; acceptable if we prefer success when any single hedge can succeed.
- **Client/revert**: We still cancel on first terminal error and return quickly.

## Summary

- We didn’t avoid "don’t cancel on first error" because of the loop; the loop is why one execution often returns quickly with one upstream’s error and then we cancel the rest.
- Full "don’t cancel on error" would fix the trace_filter case but worsen latency when every hedge fails.
- **Cancel only on terminal error** (client + execution exception) keeps latency good for deterministic failures and lets other hedges complete when the first failure is upstream-specific (e.g. method not supported).
