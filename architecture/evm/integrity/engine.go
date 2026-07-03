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
}

// Recorded is a reorg-sensitive mismatch that was observed but not rejected
// (the block was unfinalized, so it may be a benign reorg). Callers emit a
// metric/log for it.
type Recorded struct {
	CheckID  string
	Reason   string
	Class    FailureClass
	Finality string // "finalized"/"unfinalized"/"unknown" — for the violation metric
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
// against a fresh canonical fetch — a reorg, not corruption; served), "off"
// (disabled for this finality or check).
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
		behavior := in.verdictFor(ctx, c, cfg, d)
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
				if _, confirmed := rc.ReconfirmPin(ctx, v.DisputedPin); confirmed {
					// Skipped counts as cleared too: the adopted pin resolved the
					// dispute and the check has nothing left to verify.
					if v2 := c.Run(ctx, d, cfg); v2 == nil || v2 == Skipped {
						res.Outcomes = append(res.Outcomes, CheckOutcome{c.ID, "reconfirmed"})
						continue
					} else {
						v = v2
					}
				}
			}
		}

		if behavior == BehaviorError {
			res.Outcomes = append(res.Outcomes, CheckOutcome{c.ID, "reject"})
			res.Err = contentValidation(c, v, in.Upstream)
			res.RejectedCheckID = c.ID
			res.RejectedClass = c.Class
			res.Finality = in.finalityOf(ctx, d)
			return res
		}
		// soft-flag: surface the violation but still serve the response.
		res.Outcomes = append(res.Outcomes, CheckOutcome{c.ID, "soft_flag"})
		res.Recorded = append(res.Recorded, Recorded{CheckID: c.ID, Reason: v.Reason, Class: c.Class, Finality: in.finalityOf(ctx, d)})
	}
	return res
}

// verdictFor decides what to do on a violation of check c: a per-check override
// wins; otherwise deterministic checks reject and reorg-sensitive checks defer
// to finality (via the resolver) and the ReorgPolicy.
func (in Input) verdictFor(ctx context.Context, c *Check, cfg CheckConfig, d *Decoded) Behavior {
	if cfg.FailOverride != nil {
		return *cfg.FailOverride
	}
	if c.Class == Deterministic {
		return BehaviorError
	}
	final, known := false, false
	if in.Resolver != nil {
		final, known = in.Resolver.IsFinalized(ctx, d.BlockNumber())
	}
	return in.Reorg.behaviorFor(final, known)
}

// finalityOf returns the target block's finality as an observability label —
// "finalized" / "unfinalized" / "unknown". Separates genuine (finalized /
// deterministic) catches from reorg-prone unfinalized ones in metrics/logs.
// Called only on a reject/record (rare) so the resolver cost is negligible.
func (in Input) finalityOf(ctx context.Context, d *Decoded) string {
	if in.Resolver == nil {
		return "unknown"
	}
	final, known := in.Resolver.IsFinalized(ctx, d.BlockNumber())
	if !known {
		return "unknown"
	}
	if final {
		return "finalized"
	}
	return "unfinalized"
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
