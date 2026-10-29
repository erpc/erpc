package policy

import (
	"testing"
	"time"
)

// evalName compiles a one-liner eval that returns a string derived from a
// helper result, so each stdlib helper can be asserted in isolation.
func evalName(t *testing.T, js string, ctx EvalContext) string {
	t.Helper()
	s := mustCompile(t, js)
	name, err := s.Evaluate(ctx, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return name
}

func TestStdlib_WithTag(t *testing.T) {
	ctx := EvalContext{Upstreams: []Upstream{
		up("a", []string{"type:internal", "region:us"}, StateHealthy, nil, nil),
		up("b", []string{"type:external"}, StateHealthy, nil, nil),
		up("c", []string{"type:internal"}, StateHealthy, nil, nil),
	}}
	got := evalName(t, `(ctx) => String(ctx.upstreams.withTag("type:internal").length)`, ctx)
	if got != "2" {
		t.Fatalf("withTag length: got %q, want 2", got)
	}
	got = evalName(t, `(ctx) => String(ctx.upstreams.withTag("region:us").length)`, ctx)
	if got != "1" {
		t.Fatalf("withTag chained: got %q, want 1", got)
	}
}

func TestStdlib_Healthy(t *testing.T) {
	ctx := EvalContext{Upstreams: []Upstream{
		up("a", nil, StateHealthy, nil, nil),
		up("b", nil, StateUnhealthy, nil, nil),
		up("c", nil, StateCordoned, []string{CordonAvailability}, nil),
	}}
	// Only the healthy one; unhealthy and cordoned (any class) are excluded.
	got := evalName(t, `(ctx) => String(ctx.upstreams.healthy().length)`, ctx)
	if got != "1" {
		t.Fatalf("healthy length: got %q, want 1", got)
	}
}

func TestStdlib_AnyPunishedAndOperator(t *testing.T) {
	// Helpers scan PunishedOrOperator, not candidate Upstreams — a punished
	// node dropped by removeCordoned must still pin.
	base := func(classes []string) EvalContext {
		return EvalContext{
			Upstreams: []Upstream{
				up("a", nil, StateHealthy, nil, nil),
			},
			PunishedOrOperator: []Upstream{
				up("b", nil, StateCordoned, classes, nil),
			},
		}
	}
	if got := evalName(t, `(ctx) => ctx.upstreams.anyPunished() ? "y" : "n"`, base([]string{CordonPunishment})); got != "y" {
		t.Fatalf("anyPunished punishment: got %q, want y", got)
	}
	if got := evalName(t, `(ctx) => ctx.upstreams.anyPunished() ? "y" : "n"`, base([]string{CordonAvailability})); got != "n" {
		t.Fatalf("anyPunished availability-only: got %q, want n", got)
	}
	if got := evalName(t, `(ctx) => ctx.upstreams.anyOperatorCordon() ? "y" : "n"`, base([]string{CordonOperator})); got != "y" {
		t.Fatalf("anyOperatorCordon: got %q, want y", got)
	}
	if got := evalName(t, `(ctx) => ctx.upstreams.anyOperatorCordon() ? "y" : "n"`, base([]string{CordonPunishment})); got != "n" {
		t.Fatalf("anyOperatorCordon punishment-only: got %q, want n", got)
	}
	// Unset PunishedOrOperator → helpers stay false even if candidates hold
	// a punishment class (do not silently fall back to Upstreams).
	inCandidatesOnly := EvalContext{Upstreams: []Upstream{
		up("b", nil, StateCordoned, []string{CordonPunishment}, nil),
	}}
	if got := evalName(t, `(ctx) => ctx.upstreams.anyPunished() ? "y" : "n"`, inCandidatesOnly); got != "n" {
		t.Fatalf("anyPunished with unset PunishedOrOperator: got %q, want n", got)
	}
}

// Feature §7.2: "External punished while internals down → reference eval still
// selects fallback — the punishment gate scans internals." withTag must narrow
// the pin list so internals.anyPunished() does not see an external sitout.
func TestStdlib_AnyPunished_TagScoped(t *testing.T) {
	ctx := EvalContext{
		Upstreams: []Upstream{
			up("internal", []string{"type:internal"}, StateCordoned, []string{CordonAvailability}, nil),
			up("external", []string{"type:external"}, StateHealthy, nil, nil),
		},
		PunishedOrOperator: []Upstream{
			up("external", []string{"type:external"}, StateCordoned, []string{CordonPunishment}, nil),
		},
	}
	if got := evalName(t, `(ctx) => ctx.upstreams.withTag("type:internal").anyPunished() ? "y" : "n"`, ctx); got != "n" {
		t.Fatalf("internals.anyPunished with only external punished: got %q, want n", got)
	}
	if got := evalName(t, `(ctx) => ctx.upstreams.anyPunished() ? "y" : "n"`, ctx); got != "y" {
		t.Fatalf("network-wide anyPunished: got %q, want y", got)
	}
	// Internal punished (including selection-dropped) still pins via withTag.
	ctxDropped := EvalContext{
		Upstreams: []Upstream{
			up("external", []string{"type:external"}, StateHealthy, nil, nil),
		},
		PunishedOrOperator: []Upstream{
			up("internal", []string{"type:internal"}, StateCordoned, []string{CordonPunishment}, nil),
		},
	}
	if got := evalName(t, `(ctx) => ctx.upstreams.withTag("type:internal").anyPunished() ? "y" : "n"`, ctxDropped); got != "y" {
		t.Fatalf("internals.anyPunished with dropped internal punished: got %q, want y", got)
	}
}

func TestStdlib_AllUnavailable(t *testing.T) {
	yn := `(ctx) => ctx.upstreams.allUnavailable() ? "y" : "n"`

	cases := []struct {
		name string
		ups  []Upstream
		want string
	}{
		{"empty set is false", nil, "n"},
		{"all unhealthy-uncordoned", []Upstream{
			up("a", nil, StateUnhealthy, nil, nil),
			up("b", nil, StateUnhealthy, nil, nil),
		}, "y"},
		{"all availability-cordoned", []Upstream{
			up("a", nil, StateCordoned, []string{CordonAvailability}, nil),
		}, "y"},
		{"one healthy makes it false", []Upstream{
			up("a", nil, StateUnhealthy, nil, nil),
			up("b", nil, StateHealthy, nil, nil),
		}, "n"},
		{"punishment cordon makes it false", []Upstream{
			up("a", nil, StateCordoned, []string{CordonPunishment}, nil),
		}, "n"},
		{"availability plus punishment makes it false", []Upstream{
			up("a", nil, StateCordoned, []string{CordonAvailability, CordonPunishment}, nil),
		}, "n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := evalName(t, yn, EvalContext{Upstreams: tc.ups}); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// AllUnavailable must be order-independent: punish, then availability-cordon
// the same node, and it stays false; drop the availability class and the
// punishment still holds it false.
func TestStdlib_AllUnavailable_OrderIndependent(t *testing.T) {
	yn := `(ctx) => ctx.upstreams.allUnavailable() ? "y" : "n"`
	both := EvalContext{Upstreams: []Upstream{
		up("a", nil, StateCordoned, []string{CordonPunishment, CordonAvailability}, nil),
	}}
	if got := evalName(t, yn, both); got != "n" {
		t.Fatalf("punishment+availability: got %q, want n", got)
	}
	punishOnly := EvalContext{Upstreams: []Upstream{
		up("a", nil, StateCordoned, []string{CordonPunishment}, nil),
	}}
	if got := evalName(t, yn, punishOnly); got != "n" {
		t.Fatalf("punishment only: got %q, want n", got)
	}
}

func TestStdlib_CanServeBlock(t *testing.T) {
	length := `(ctx) => String(ctx.upstreams.canServeBlock(ctx.request.blockNumber).length)`
	// Range [1000, 2000]: serves 1500, not 500, not 2500.
	ranged := []Upstream{up("a", nil, StateHealthy, nil, &BlockRange{Lower: i64(1000), Upper: i64(2000)})}
	if got := evalName(t, length, EvalContext{Request: Request{BlockNumber: i64(1500)}, Upstreams: ranged}); got != "1" {
		t.Fatalf("in-range: got %q, want 1", got)
	}
	if got := evalName(t, length, EvalContext{Request: Request{BlockNumber: i64(500)}, Upstreams: ranged}); got != "0" {
		t.Fatalf("below range: got %q, want 0", got)
	}
	if got := evalName(t, length, EvalContext{Request: Request{BlockNumber: i64(2500)}, Upstreams: ranged}); got != "0" {
		t.Fatalf("above range: got %q, want 0", got)
	}
	// No range configured → guard doesn't short-circuit → can serve.
	noRange := []Upstream{up("a", nil, StateHealthy, nil, nil)}
	if got := evalName(t, length, EvalContext{Request: Request{BlockNumber: i64(1)}, Upstreams: noRange}); got != "1" {
		t.Fatalf("no range: got %q, want 1", got)
	}
	// Open upper bound: lower 1000, upper nil serves anything >= 1000.
	openUpper := []Upstream{up("a", nil, StateHealthy, nil, &BlockRange{Lower: i64(1000), Upper: nil})}
	if got := evalName(t, length, EvalContext{Request: Request{BlockNumber: i64(9_000_000)}, Upstreams: openUpper}); got != "1" {
		t.Fatalf("open upper: got %q, want 1", got)
	}
	// Unknown block (nil → JS null): no positive evidence the range rules it
	// out, so the ranged upstream is kept rather than dropped by null-to-0
	// coercion. A [0, x] range would otherwise spuriously "serve" block 0.
	zeroLower := []Upstream{up("a", nil, StateHealthy, nil, &BlockRange{Lower: i64(0), Upper: i64(2000)})}
	if got := evalName(t, length, EvalContext{Request: Request{BlockNumber: nil}, Upstreams: zeroLower}); got != "1" {
		t.Fatalf("null block, ranged upstream: got %q, want 1", got)
	}
}

func TestStdlib_Auth(t *testing.T) {
	ctx := EvalContext{User: &User{Auth: "consensus-dev"}}
	if got := evalName(t, `(ctx) => ctx.user?.auth === "consensus-dev" ? "y" : "n"`, ctx); got != "y" {
		t.Fatalf("auth present: got %q, want y", got)
	}
	if got := evalName(t, `(ctx) => ctx.user?.auth === "admin" ? "y" : "n"`, ctx); got != "n" {
		t.Fatalf("auth absent: got %q, want n", got)
	}
	// nil user → ctx.user is null; optional chaining yields undefined.
	if got := evalName(t, `(ctx) => ctx.user?.auth === "consensus-dev" ? "y" : "n"`, EvalContext{}); got != "n" {
		t.Fatalf("auth nil user: got %q, want n", got)
	}
}
