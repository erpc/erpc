// Package failsafe provides eRPC's resilience primitives: hedge,
// circuit breaker, and retry-backoff math. These primitives compose
// inside scope-specific executors (upstream / network / cache).
//
// The package is intentionally narrow:
//
//   - Breaker is a policy-free state machine. Callers decide eligibility
//     and classification (eligible? success / failure / ignore?) at the
//     integration site as plain Go functions. The breaker only owns the
//     state machine and metric ring buffer.
//
//   - RunHedged[R] is a generic goroutine fan-out helper. The caller
//     supplies the inner function, keep predicate, release callback,
//     and delay function. The helper guarantees: at most one winner,
//     all losers are released, and no goroutine leaks past return.
//
//   - ComputeBackoff and SleepCtx are pure-math helpers used by retry
//     loops. The retry loop itself lives in each scope's executor.
package failsafe
