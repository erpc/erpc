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
	// Resolver enables cross-source corroboration checks (finality + force-fetch
	// of the canonical block). Nil disables them gracefully.
	Resolver Resolver
	// Reorg maps finality state to the behavior for reorg-sensitive mismatches.
	Reorg ReorgPolicy
}

// Recorded is a reorg-sensitive mismatch that was observed but not rejected
// (the block was unfinalized, so it may be a benign reorg). Callers emit a
// metric/log for it.
type Recorded struct {
	CheckID string
	Reason  string
}

// Result is the outcome of validating a response. Err is non-nil when a check
// hard-failed (the response must be rejected); Recorded lists soft-flagged
// reorg-sensitive mismatches the caller should surface but still serve.
type Result struct {
	Err      error
	Recorded []Recorded
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
	if in.Response.IsObjectNull() || in.Response.IsResultEmptyish() {
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
	d := newDecoded(method, raw)

	var res Result
	for _, c := range enabled {
		cfg := in.Checks.For(c.ID)
		// Resolve the verdict for this check up front. If it would be ignored
		// (invalidBehavior unfinalized: off, or a per-check onFailure: off),
		// skip the check entirely — and with it any force-fetch it would issue.
		behavior := in.verdictFor(ctx, c, cfg, d)
		if behavior == BehaviorIgnore {
			continue
		}

		v := c.Run(ctx, d, cfg)
		if v == nil {
			continue
		}

		if behavior == BehaviorError {
			res.Err = contentValidation(c, v, in.Upstream)
			return res
		}
		// soft-flag: surface the violation but still serve the response.
		res.Recorded = append(res.Recorded, Recorded{CheckID: c.ID, Reason: v.Reason})
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
