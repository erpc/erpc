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
}

// Validate runs every enabled, applicable integrity check against the response
// and returns an ErrEndpointContentValidation on the first violation, so the
// existing retry/consensus machinery routes around the offending upstream.
// It returns nil when no applicable check is enabled, when the response is
// empty/null, or when all enabled checks pass.
func Validate(ctx context.Context, in Input) error {
	method := strings.ToLower(in.Method)
	checks := checksFor(method)
	if len(checks) == 0 || in.Response == nil {
		return nil
	}

	// Decide enablement before paying for any decode.
	enabled := enabledChecks(checks, in.Checks)
	if len(enabled) == 0 {
		return nil
	}

	if in.Response.IsObjectNull() || in.Response.IsResultEmptyish() {
		return nil
	}
	jrr, err := in.Response.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return nil
	}
	raw := jrr.GetResultBytes()
	if len(raw) == 0 {
		return nil
	}

	d := newDecoded(method, raw)
	for _, c := range enabled {
		if v := c.Run(ctx, d, in.Checks.For(c.ID)); v != nil {
			return common.NewErrEndpointContentValidation(
				fmt.Errorf("integrity check %q failed: %s", c.ID, v.Reason),
				in.Upstream,
			)
		}
	}
	return nil
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
