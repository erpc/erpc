package integrity

import (
	"context"
	"fmt"
	"strings"

	"github.com/erpc/erpc/common"
)

// Input is everything Validate needs to check one upstream response.
type Input struct {
	Method   string
	Upstream common.Upstream
	Response *common.NormalizedResponse
	Checks   CheckSet
	// Params are the originating request's JSON-RPC params, for checks that must
	// reproduce request semantics (e.g. the eth_getLogs filter). Optional.
	Params []any
	// Resolver enables cross-source corroboration checks (finality + force-fetch
	// of the canonical block). Nil disables them gracefully.
	Resolver Resolver
	// History enables cross-block continuity checks (parent-hash linkage, hash
	// stability) by remembering number→hash from observed blocks. Nil disables them.
	History History
	// Reorg maps finality state to the behavior for reorg-sensitive mismatches.
	Reorg ReorgPolicy
	// ObserveOnly suppresses every rejection: checks run and violations are
	// recorded as "would_reject", but the response is always served. Absolute —
	// it outranks a per-check onFailure and covers Deterministic checks too.
	ObserveOnly bool
}

// Recorded is a reorg-sensitive mismatch that was observed but not rejected
// (the block was unfinalized, so it may be a benign reorg). Callers emit a
// metric/log for it.
type Recorded struct {
	CheckID  string
	Reason   string
	Class    FailureClass
	Finality string // "finalized"/"unfinalized"/"unknown" — for the violation metric
	// Verdict is the label this record carries in the violation metric/log:
	// "soft_flag" (a reorg-sensitive mismatch served by policy) or
	// "would_reject" (observe-only suppressed a real rejection). Distinguishing
	// them is the point of observe mode — one is routine, the other is the
	// enforcement cost estimate.
	Verdict string
}

// Result is the outcome of validating a response. Err is non-nil when a check
// hard-failed (the response must be rejected) — RejectedCheckID names that
// check, for metrics. Recorded lists soft-flagged reorg-sensitive mismatches the
// caller should surface but still serve. Outcomes lists EVERY check that was
// evaluated and what happened, for the per-check attempts/outcomes metric.
type Result struct {
	Err             error
	RejectedCheckID string
	// RejectedClass is the failing check's FailureClass (meaningful only when
	// Err != nil). Deterministic = provable corruption — callers may feed it
	// into upstream health/misbehavior scoring; ReorgSensitive may still be a
	// transient race, so it should not damage a score.
	RejectedClass FailureClass
	Finality      string // finality of the rejected block ("finalized"/"unfinalized"/"unknown"); "" if no reject
	Recorded      []Recorded
	Outcomes      []CheckOutcome
}

// CheckOutcome records what one check evaluation did. Outcome is one of:
// "pass" (an actual verification ran and found no violation), "skip" (the
// check could not evaluate this response — missing wiring, cold cache, data
// not fully modeled; see Skipped), "reject" (failed, response rejected),
// "soft_flag" (reorg-sensitive mismatch recorded but served), "reconfirmed"
// (a pin-anchored mismatch cleared once the stale pin was re-confirmed
// against a fresh canonical fetch — a reorg, not corruption; served),
// "would_reject" (observe-only mode suppressed a rejection and served the
// response anyway — the count is the client-facing cost enforcement would
// incur), "off" (disabled for this finality or check).
type CheckOutcome struct {
	CheckID string
	Outcome string
}

// DefaultReorgPolicy is the safe default: a mismatch on finalized data is a
// rejection; on unfinalized data it is recorded (it might be a reorg).
func DefaultReorgPolicy() ReorgPolicy {
	return ReorgPolicy{Finalized: BehaviorError, Unfinalized: BehaviorRecord}
}

// Validate runs every enabled, applicable integrity check against the response.
// Deterministic violations reject immediately; reorg-sensitive violations are
// resolved against finality via the ReorgPolicy. It returns early on the first
// hard failure.
func Validate(ctx context.Context, in Input) Result {
	method := strings.ToLower(in.Method)
	checks := checksFor(method)
	if len(checks) == 0 || in.Response == nil {
		return Result{}
	}
	enabled := enabledChecks(checks, in.Checks)
	if len(enabled) == 0 {
		return Result{}
	}
	// Emptyish responses ("[]"/null) usually mean "nothing there / not yet
	// available" (retryEmpty's territory), so most checks never see them. But
	// for eth_getLogs an empty result IS the worst corruption shape — every
	// log silently dropped — so checks that opt in (AllowEmptyish) still run;
	// everything else keeps today's short-circuit.
	emptyish := in.Response.IsObjectNull() || in.Response.IsResultEmptyish()
	if emptyish && !anyAllowsEmptyish(enabled) {
		return Result{}
	}
	jrr, err := in.Response.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return Result{}
	}
	raw := jrr.GetResultBytes()
	if len(raw) == 0 {
		return Result{}
	}

	ctx = withResolver(ctx, in.Resolver)
	ctx = withHistory(ctx, in.History)
	d := newDecoded(method, raw)
	d.reqParams = in.Params

	var res Result
	// One finality observation for this whole response: every check here judges
	// the same block, and the verdict and its metric label must not come from
	// two separate reads of a moving finalized head (see finalityOnce).
	fin := &finalityOnce{}
	for _, c := range enabled {
		// On an emptyish response only the opted-in checks run; the rest get no
		// outcome at all (exactly as if the response had short-circuited).
		if emptyish && !c.AllowEmptyish {
			continue
		}
		cfg := in.Checks.For(c.ID)
		// Resolve the verdict for this check up front. If it would be ignored
		// (invalidBehavior unfinalized: off, or a per-check onFailure: off),
		// skip the check entirely — and with it any force-fetch it would issue.
		behavior := in.verdictFor(ctx, c, cfg, d, fin)
		if behavior == BehaviorIgnore {
			res.Outcomes = append(res.Outcomes, CheckOutcome{c.ID, "off"})
			continue
		}

		v := c.Run(ctx, d, cfg)
		if v == nil {
			res.Outcomes = append(res.Outcomes, CheckOutcome{c.ID, "pass"})
			continue
		}
		if v == Skipped {
			res.Outcomes = append(res.Outcomes, CheckOutcome{c.ID, "skip"})
			continue
		}

		// Corroborate-before-verdict: a reorg-sensitive violation anchored to a
		// CACHED pin may only mean the pin is stale (routine reorg) — acting on
		// it would flag/reject every honest new-fork response, and since the
		// pin adopts only from passing responses, it would never recover
		// (self-inflicted outage). Re-confirm the disputed pin against a fresh
		// canonical fetch (singleflighted + cooldown-bounded by the History
		// impl) and re-run the check: the reconfirm adopts the current fork, so
		// a reorg clears ("reconfirmed") while a mismatch that survives the
		// fresh pin is genuine and proceeds to the verdict.
		if v.DisputedPin > 0 && c.Class == ReorgSensitive {
			if rc, ok := in.History.(PinReconfirmer); ok {
				switch _, status := rc.ReconfirmPin(ctx, v.DisputedPin); status {
				case PinFresh:
					// Skipped counts as cleared too: the adopted pin resolved the
					// dispute and the check has nothing left to verify.
					if v2 := c.Run(ctx, d, cfg); v2 == nil || v2 == Skipped {
						res.Outcomes = append(res.Outcomes, CheckOutcome{c.ID, "reconfirmed"})
						continue
					} else {
						v = v2
					}
				case PinRateLimited:
					// The pin could not be re-checked right now, so this violation
					// rests on CACHED state we have no fresh evidence for. Hard
					// rejection here is the self-block failure itself: while the
					// rate limit holds, every honest response mismatches the stale
					// pin, none can be served, and the pin never adopts the real
					// fork. Degrade to soft-flag so the mismatch is still recorded
					// and surfaced, without erroring a response that is probably
					// correct. A genuine problem outlives the rate limit and gets
					// a fresh verdict on the next request.
					if behavior == BehaviorError {
						behavior = BehaviorRecord
					}
				}
			}
		}

		if behavior == BehaviorError {
			// Observe-only: never let a verdict touch the response. The violation
			// is still recorded in full (metric + forensic log + archive) under a
			// distinct outcome, so "would_reject" counts exactly what enforcing on
			// this network would have cost clients. Applied HERE rather than in
			// verdictFor so it covers every path that can reach a rejection —
			// including Deterministic checks, which ignore invalidBehavior, and any
			// check added by a later release.
			if in.ObserveOnly {
				res.Outcomes = append(res.Outcomes, CheckOutcome{c.ID, "would_reject"})
				res.Recorded = append(res.Recorded, Recorded{
					CheckID: c.ID, Reason: v.Reason, Class: c.Class,
					Finality: fin.label(ctx, in, d), Verdict: "would_reject",
				})
				continue
			}
			res.Outcomes = append(res.Outcomes, CheckOutcome{c.ID, "reject"})
			res.Err = contentValidation(c, v, in.Upstream)
			res.RejectedCheckID = c.ID
			res.RejectedClass = c.Class
			res.Finality = fin.label(ctx, in, d)
			return res
		}
		// soft-flag: surface the violation but still serve the response.
		res.Outcomes = append(res.Outcomes, CheckOutcome{c.ID, "soft_flag"})
		res.Recorded = append(res.Recorded, Recorded{CheckID: c.ID, Reason: v.Reason, Class: c.Class, Finality: fin.label(ctx, in, d), Verdict: "soft_flag"})
	}
	return res
}

// finalityOnce caches the response block's finality for one Validate call.
//
// It MUST be resolved once and reused. The underlying source is the serving
// upstream's effective finalized head, which moves — and during a reorg it
// rolls back. Reading it separately for the verdict and for the metric label
// let a single violation be judged "finalized → reject" by the strict policy
// and then reported as unfinalized (observed live on mainnet: one
// parentHashLinkage reject labelled unfinalized alongside five soft-flags of
// the same check at the same height). Verdict and label must describe the same
// observation, or the metric misattributes which policy actually fired.
type finalityOnce struct {
	done  bool
	final bool
	known bool
}

// resolve reads finality once and memoizes it (including the "no resolver"
// case, which is legitimately unknown).
func (f *finalityOnce) resolve(ctx context.Context, in Input, d *Decoded) (bool, bool) {
	if !f.done {
		f.done = true
		if in.Resolver != nil {
			f.final, f.known = in.Resolver.IsFinalized(ctx, d.BlockNumber())
		}
	}
	return f.final, f.known
}

// label is the observability value — "finalized" / "unfinalized" / "unknown".
// It resolves on demand so a deterministic check (whose verdict never consults
// finality) is still labelled, and reuses the memoized value so a
// reorg-sensitive verdict and its label can never disagree.
func (f *finalityOnce) label(ctx context.Context, in Input, d *Decoded) string {
	final, known := f.resolve(ctx, in, d)
	if !known {
		return "unknown"
	}
	if final {
		return "finalized"
	}
	return "unfinalized"
}

// verdictFor decides what to do on a violation of check c: a per-check override
// wins; otherwise deterministic checks reject and reorg-sensitive checks defer
// to finality (via the resolver) and the ReorgPolicy.
func (in Input) verdictFor(ctx context.Context, c *Check, cfg CheckConfig, d *Decoded, fin *finalityOnce) Behavior {
	if cfg.FailOverride != nil {
		return *cfg.FailOverride
	}
	if c.Class == Deterministic {
		return BehaviorError
	}
	return in.Reorg.behaviorFor(fin.resolve(ctx, in, d))
}

func contentValidation(c *Check, v *Violation, u common.Upstream) error {
	return common.NewErrEndpointContentValidation(
		fmt.Errorf("integrity check %q failed: %s", c.ID, v.Reason), u,
	)
}

// HasChecks reports whether any check is registered for a method, so callers
// can cheaply skip the engine (and building a CheckSet) for unrelated methods.
func HasChecks(method string) bool {
	return len(checksFor(strings.ToLower(method))) > 0
}

func enabledChecks(checks []*Check, cs CheckSet) []*Check {
	out := checks[:0:0]
	for _, c := range checks {
		if cs.For(c.ID).Enabled {
			out = append(out, c)
		}
	}
	return out
}

func anyAllowsEmptyish(checks []*Check) bool {
	for _, c := range checks {
		if c.AllowEmptyish {
			return true
		}
	}
	return false
}

// --- resolver propagation via context (avoids a signature change on Check.Run) ---

type resolverKey struct{}

func withResolver(ctx context.Context, r Resolver) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, resolverKey{}, r)
}

func resolverFrom(ctx context.Context) Resolver {
	r, _ := ctx.Value(resolverKey{}).(Resolver)
	return r
}
