package failsafe

import (
	"context"
	"time"
)

// HedgeHooks lets the caller observe hedge events. OnFire fires when an
// extra hedge attempt is spawned — useful for metric increments.
type HedgeHooks struct {
	OnFire func(fireIdx int, delay time.Duration)
}

// hedgeResult is the value posted to the result channel by every
// participating goroutine. The receive loop selects exactly one winner
// based on the keep predicate; every other non-zero result is passed
// to the release callback.
type hedgeResult[R any] struct {
	r   R
	err error
}

// RunHedged runs `inner` in parallel up to maxHedges+1 times, racing for
// the first acceptable result. The first attempt fires immediately; each
// subsequent attempt fires after delayFn(idx) (idx starts at 1).
//
// Semantics:
//
//   - keep(r, err) decides whether a returned (r, err) is the winner.
//     When keep returns false, the result is treated as "not good
//     enough" and the race continues with the remaining hedges.
//
//   - release(r) is called once per non-kept R that was actually
//     produced. Pass nil when R has no cleanup (e.g. []byte). The
//     winner is NEVER released — the caller owns it.
//
//   - Sibling cancellation: once a winner is selected, ctx is canceled
//     for all in-flight hedges. Goroutines that complete after the
//     winner detect siblingCtx.Done() and release their results.
//
//   - Goroutine safety: every goroutine writes exactly once to the
//     result channel (cap = maxHedges+1, so sends never block).
func RunHedged[R any](
	parentCtx context.Context,
	maxHedges int,
	delayFn func(idx int) time.Duration,
	inner func(ctx context.Context) (R, error),
	keep func(r R, err error) bool,
	release func(R),
	hooks HedgeHooks,
) (R, error) {
	var zero R
	if maxHedges < 0 {
		maxHedges = 0
	}

	siblingCtx, cancelAll := context.WithCancel(parentCtx)
	defer cancelAll()

	resultCh := make(chan hedgeResult[R], maxHedges+1)
	fired := 0

	fire := func(idx int) {
		fired++
		if hooks.OnFire != nil && idx > 0 {
			// Best-effort: delayFn was already evaluated above; we don't
			// re-evaluate here. The hook just reports "hedge idx fired".
			hooks.OnFire(idx, 0)
		}
		go func() {
			r, err := inner(siblingCtx)
			select {
			case resultCh <- hedgeResult[R]{r: r, err: err}:
			default:
				// Channel full (shouldn't happen given cap); release the result
				// to avoid leaking.
				if release != nil {
					var zr R
					if any(r) != any(zr) {
						release(r)
					}
				}
			}
		}()
	}

	// Primary attempt fires immediately.
	fire(0)

	var pending int = 1
	var winner hedgeResult[R]
	var winnerSet bool
	var lastResult hedgeResult[R]
	var lastResultSet bool

	// Schedule next hedge timer if we have hedges left.
	var hedgeTimer *time.Timer
	resetHedgeTimer := func() {
		if hedgeTimer != nil {
			hedgeTimer.Stop()
			hedgeTimer = nil
		}
		if fired-1 >= maxHedges {
			return
		}
		nextIdx := fired
		d := time.Duration(0)
		if delayFn != nil {
			d = delayFn(nextIdx)
		}
		if d < 0 {
			d = 0
		}
		hedgeTimer = time.NewTimer(d)
	}
	resetHedgeTimer()

	getHedgeC := func() <-chan time.Time {
		if hedgeTimer == nil {
			return nil
		}
		return hedgeTimer.C
	}

	// Continue until all in-flight are done AND no more hedges can fire.
	// Without the hedgeTimer guard, a fast non-kept primary would exit
	// the loop before a scheduled hedge has a chance to spawn its
	// recovery attempt — losing the whole point of hedging.
	for pending > 0 || hedgeTimer != nil {
		select {
		case <-parentCtx.Done():
			if hedgeTimer != nil {
				hedgeTimer.Stop()
			}
			cancelAll()
			// Drain pending goroutines, releasing their results.
			for pending > 0 {
				res := <-resultCh
				pending--
				if release != nil {
					var zr R
					if any(res.r) != any(zr) {
						release(res.r)
					}
				}
			}
			return zero, parentCtx.Err()

		case <-getHedgeC():
			hedgeTimer = nil
			// Hedge fires per its scheduled delay regardless of whether
			// a primary has already returned with an error. The race
			// continues if no winner was kept — operators want
			// recovery attempts even after a transient primary failure.
			fire(fired)
			pending++
			resetHedgeTimer()

		case res := <-resultCh:
			pending--
			if winnerSet {
				// We already have a winner — this is a late arrival.
				if release != nil {
					var zr R
					if any(res.r) != any(zr) {
						release(res.r)
					}
				}
				continue
			}
			if keep == nil || keep(res.r, res.err) {
				winner = res
				winnerSet = true
				cancelAll()
				if hedgeTimer != nil {
					hedgeTimer.Stop()
					hedgeTimer = nil
				}
				// Continue draining; remaining goroutines will be released.
				continue
			}
			// Not a winner — remember it so we can return it later if
			// every sibling also ends up not-kept. Don't release here;
			// the receiver of the final return value owns release.
			lastResult = res
			lastResultSet = true
			// If we have no more hedges to fire and no in-flight, return the
			// last non-kept result with its error.
			if pending == 0 && (hedgeTimer == nil) {
				return res.r, res.err
			}
		}
	}

	if winnerSet {
		return winner.r, winner.err
	}
	if lastResultSet {
		return lastResult.r, lastResult.err
	}
	return zero, nil
}
