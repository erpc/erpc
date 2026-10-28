// Package policy is the standalone pre-round consensus-policy selector engine.
//
// A request's consensus round runs under one named ConsensusPolicyConfig. This
// package answers a single question before the round starts: given the caller,
// the candidate upstreams, and the request, which named policy should govern
// this round? It never sees round outcomes and never grades a round — that
// stays in the declarative config and the consensus executor.
//
// The package deliberately imports nothing from consensus/, health/, or auth/.
// The caller (the executor wiring, a later phase) builds an EvalContext from
// those subsystems and hands it in as plain values; the vocabulary those
// callers populate — cordon classes, health states — is owned here so the two
// sides cannot drift. This mirrors internal/policy (the selection-policy
// engine): freeform operator JS at the edge, a bounded declarative result
// (here, a policy name) into the core.
//
// Every failure mode fails closed. An empty/null eval result, a throw, or an
// eval that overruns evalTimeout resolves to the empty name with a typed
// error, so the caller can attribute the reason and fall back to the
// strictest default policy. Lack of a free Sobek VM is never a fail-closed
// trigger — the pool refills / grows so every configured selector still runs
// (feature §4.6).
//
// The eval runs in a bare VM (newSandboxRuntime) that exposes only the
// EvalContext and the stdlib — no env, process.env, or console. customPolicy
// is operator-writable config, so the eval never gets host-process env-var
// read access the way the shared common.NewRuntime() would grant.
package policy

import "errors"

// Cordon classes. An upstream can hold several at once (the eval sees a set),
// so these name the members of that set. The selector matches on the class,
// never on the free-text cordon reason — the reason is diagnostic only.
const (
	CordonPunishment   = "punishment"
	CordonAvailability = "availability"
	CordonOperator     = "operator"
)

// Health states exposed to the eval. A non-empty cordon set is reported as
// StateCordoned and takes precedence over StateUnhealthy, so a failing health
// check on a cordoned upstream can never mask its cordon class.
const (
	StateHealthy   = "healthy"
	StateUnhealthy = "unhealthy"
	StateCordoned  = "cordoned"
)

var (
	// ErrEvalTimeout is returned when an eval overruns evalTimeout. The
	// borrowed VM is interrupted and discarded (never returned to the pool).
	ErrEvalTimeout = errors.New("consensus policy: eval timed out")

	// ErrEvalThrew wraps a JS throw or a non-string return from the eval.
	ErrEvalThrew = errors.New("consensus policy: eval failed")
	// ErrEvalNull marks a null/undefined return (e.g. a missing `return`):
	// fail-closed, counted and logged, never a silent default pick.
	ErrEvalNull = errors.New("consensus policy: eval returned null")
)

// Request is the request-shaped half of the eval context. BlockNumber is nil
// for methods that do not carry a numeric block (tx-hash / block-hash lookups),
// which the reference eval treats as the "unknown block" case.
type Request struct {
	Method      string
	BlockNumber *int64
	Network     string
}

// User carries the verified caller identity for the selector. Auth is the
// name of the auth strategy that authenticated the caller (feature §4.3) —
// stamped only by the auth registry from strategy config, never from a
// client-controlled header. Empty string when the strategy is unnamed. A
// nil *User is a nil ctx.user in JS.
type User struct {
	Auth string
}

// Health is an upstream's health as the eval sees it. CordonClasses is a set
// (see the package doc); an empty set means not cordoned. CordonReason is
// diagnostic only and is never matched by the stdlib.
type Health struct {
	State         string
	CordonClasses []string
	CordonReason  string
}

// BlockRange is an upstream's served-block window, lower/upper inclusive. A nil
// bound is open on that side. A nil *BlockRange means no configured range, so
// the availability guard does not short-circuit and the upstream is treated as
// able to serve any block (canServeBlock is true).
type BlockRange struct {
	Lower *int64
	Upper *int64
}

// Upstream is one candidate upstream for the round.
type Upstream struct {
	ID                string
	Tags              []string
	Health            Health
	BlockAvailability *BlockRange
}

// EvalContext is the full argument passed to the operator eval as ctx.
type EvalContext struct {
	Request Request
	User    *User
	// Upstreams is the health-visible set the eval filters over (withTag /
	// canServeBlock / allUnavailable). When the bridge has the unfiltered
	// network registry it prefers that list so availability-cordoned nodes
	// dropped from dispatch candidates remain visible; otherwise it is the
	// post-selection candidate list.
	Upstreams []Upstream
	// PunishedOrOperator is the network's current punishment ∪ operator
	// cordons, including nodes already dropped from dispatch by selection.
	// anyPunished / anyOperatorCordon scan this list. Empty when unset.
	PunishedOrOperator []Upstream
}

// toJS renders the context as plain values sobek exposes as native JS objects
// and arrays. []string becomes []any so sobek builds a real Array (with the
// prototype methods the stdlib uses); nil pointers become JS null.
func (c EvalContext) toJS() map[string]any {
	req := map[string]any{"method": c.Request.Method, "network": c.Request.Network, "blockNumber": nil}
	if c.Request.BlockNumber != nil {
		req["blockNumber"] = *c.Request.BlockNumber
	}

	var user any
	if c.User != nil {
		user = map[string]any{"auth": c.User.Auth}
	}

	return map[string]any{
		"request":            req,
		"user":               user,
		"upstreams":          upsToJS(c.Upstreams),
		"punishedOrOperator": upsToJS(c.PunishedOrOperator),
	}
}

func upsToJS(ups []Upstream) []any {
	out := make([]any, len(ups))
	for i, u := range ups {
		var ba any
		if u.BlockAvailability != nil {
			ba = map[string]any{"lower": int64OrNil(u.BlockAvailability.Lower), "upper": int64OrNil(u.BlockAvailability.Upper)}
		}
		out[i] = map[string]any{
			"id":   u.ID,
			"tags": strAny(u.Tags),
			"health": map[string]any{
				"state":         u.Health.State,
				"cordonClasses": strAny(u.Health.CordonClasses),
				"cordonReason":  u.Health.CordonReason,
			},
			"blockAvailability": ba,
		}
	}
	return out
}

func strAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func int64OrNil(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}
