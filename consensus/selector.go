package consensus

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/failsafe"
	policyengine "github.com/erpc/erpc/internal/consensus/policy"
	"github.com/erpc/erpc/telemetry"
)

// resolvePolicy runs the pre-round selector for one request and returns the
// consensusPolicy that governs the round. Every failure mode — an eval throw,
// a non-string return, a timeout, or a returned name that isn't a configured
// policy — falls closed to the inline default policy (the strictest), and is
// attributed to a metric reason so operators can see why a round didn't run
// under the selected policy. An explicit null return or `return "default"`
// chooses the inline default intentionally and is NOT counted as a failure.
// Lack of a free Sobek VM is never a fail-closed trigger (feature §4.6).
func (c *Consensus) resolvePolicy(req *common.NormalizedRequest) *consensusPolicy {
	projectId, networkId := reqLabels(req)

	start := time.Now()
	name, err := c.selector.Evaluate(c.buildEvalContext(req), c.evalTimeout)
	telemetry.MetricConsensusPolicyEvalDuration.
		WithLabelValues(projectId, networkId).
		Observe(time.Since(start).Seconds())

	switch {
	case err != nil:
		switch {
		case errors.Is(err, policyengine.ErrEvalTimeout):
			telemetry.MetricConsensusPolicyEvalFailed.
				WithLabelValues(projectId, networkId, "timeout").Inc()
		default:
			telemetry.MetricConsensusPolicyEvalFailed.
				WithLabelValues(projectId, networkId, "throw").Inc()
		}
		if c.logger != nil {
			c.logger.Warn().Err(err).Str("networkId", networkId).
				Msg("consensus policy selector failed closed to default policy")
		}
		c.markSelected(req, projectId, networkId, defaultPolicyName)
		return c.policy

	case name == "" || name == defaultPolicyName:
		// Operator returned null/undefined or the reserved "default" string:
		// intentional select of the inline fail-closed policy (§3).
		c.markSelected(req, projectId, networkId, defaultPolicyName)
		return c.policy

	default:
		if pol, ok := c.named[name]; ok {
			c.markSelected(req, projectId, networkId, name)
			return pol
		}
		// A name the operator returned that isn't configured. Fail closed.
		telemetry.MetricConsensusPolicyEvalFailed.
			WithLabelValues(projectId, networkId, "unknown_name").Inc()
		if c.logger != nil {
			c.logger.Warn().Str("policy", name).Str("networkId", networkId).
				Msg("consensus policy selector returned an unknown policy name; using default")
		}
		c.markSelected(req, projectId, networkId, defaultPolicyName)
		return c.policy
	}
}

// markSelected records the resolved policy for observability: the selected
// metric and the X-ERPC-Consensus-Policy response header (via ExecState).
func (c *Consensus) markSelected(req *common.NormalizedRequest, projectId, networkId, name string) {
	telemetry.MetricConsensusPolicySelected.
		WithLabelValues(projectId, networkId, name).Inc()
	if req != nil {
		req.ExecState().SetConsensusPolicy(name)
	}
}

// buildEvalContext translates the request, caller identity, and candidate
// upstreams into the plain-value EvalContext the selector engine consumes. The
// engine imports nothing from this package; this is the single bridge, so the
// two sides cannot drift.
func (c *Consensus) buildEvalContext(req *common.NormalizedRequest) policyengine.EvalContext {
	method, _ := req.Method()

	// Prefer the unfiltered network registry for ctx.upstreams so
	// allUnavailable() still sees availability-cordoned internals that
	// removeCordoned already dropped from the dispatch candidate list
	// (feature §3.1 / §6). Fall back to post-selection candidates only when
	// the network list was never set (unit tests / non-network paths). An
	// explicitly empty snapshot must stay empty — do not reintroduce
	// method-ineligible dispatch candidates as healthy selector inputs.
	upsForHealth := req.Upstreams()
	if req.HasNetworkUpstreams() {
		upsForHealth = req.NetworkUpstreams()
	}

	ectx := policyengine.EvalContext{
		Request: policyengine.Request{
			Method:      method,
			Network:     req.NetworkLabel(),
			BlockNumber: blockNumberPtr(req.EvmBlockNumber()),
		},
		Upstreams: toEvalUpstreams(upsForHealth, method),
	}

	// Auth is the strategy name stamped by the auth registry (§4.3). A nil
	// user is a nil ctx.user.
	if u := req.User(); u != nil {
		ectx.User = &policyengine.User{Auth: u.Auth}
	}

	// PunishedOrOperator is filtered from the unfiltered network registry so
	// anyPunished / anyOperatorCordon still see nodes that selection already
	// dropped via removeCordoned. Unset NetworkUpstreams → empty (no pin).
	for _, up := range req.NetworkUpstreams() {
		if up == nil {
			continue
		}
		if !hasPunishmentOrOperator(up, method) {
			continue
		}
		mapped := toEvalUpstreams([]common.Upstream{up}, method)
		if len(mapped) > 0 {
			ectx.PunishedOrOperator = append(ectx.PunishedOrOperator, mapped[0])
		}
	}

	return ectx
}

// toEvalUpstreams maps runtime upstreams into the plain-value snapshots the
// selector engine consumes (tags, cordon classes, block-availability bounds).
func toEvalUpstreams(ups []common.Upstream, method string) []policyengine.Upstream {
	out := make([]policyengine.Upstream, 0, len(ups))
	for _, up := range ups {
		if up == nil {
			continue
		}
		pu := policyengine.Upstream{ID: up.Id()}
		if cfg := up.Config(); cfg != nil {
			pu.Tags = cfg.Tags
		}

		var classes []string
		if tr := up.Tracker(); tr != nil {
			for _, cl := range common.CordonClassesOf(tr, up, method) {
				classes = append(classes, string(cl))
			}
		}
		pu.Health.CordonClasses = classes
		// Cordon wins over breaker state: a cordoned upstream is out of
		// rotation for a typed reason, which allUnavailable() whitelists
		// separately. An open catch-all circuit breaker with no cordon is
		// the unhealthy-but-uncordoned case (down, not yet cordoned).
		switch {
		case len(classes) > 0:
			pu.Health.State = policyengine.StateCordoned
		case circuitBreakerOpen(up):
			pu.Health.State = policyengine.StateUnhealthy
		default:
			pu.Health.State = policyengine.StateHealthy
		}

		// Block availability is EVM-specific; generic upstreams leave the range
		// open (nil), so the eval's canServeBlock guard never short-circuits.
		if ba, ok := up.(interface {
			EvmBlockAvailabilityBounds() (int64, int64)
		}); ok {
			if r := blockRange(ba.EvmBlockAvailabilityBounds()); r != nil {
				pu.BlockAvailability = r
			}
		}

		out = append(out, pu)
	}
	return out
}

// circuitBreakerOpen reports the catch-all breaker used for methods without
// their own failsafe block. Half-open is still serving probes, so only StateOpen
// is the down-but-uncordoned case allUnavailable() documents. Upstreams that
// do not expose a breaker (fakes, non-upstream types) are not unhealthy.
func circuitBreakerOpen(up common.Upstream) bool {
	type circuitBreaker interface {
		CircuitBreakerState() failsafe.State
	}
	b, ok := up.(circuitBreaker)
	return ok && b.CircuitBreakerState() == failsafe.StateOpen
}

// hasPunishmentOrOperator reports whether up currently holds a punishment or
// operator cordon for method (including the wildcard "*" scope the tracker
// unions in). Availability-only cordons return false — they are not a pin.
func hasPunishmentOrOperator(up common.Upstream, method string) bool {
	tr := up.Tracker()
	if tr == nil {
		return false
	}
	for _, cl := range common.CordonClassesOf(tr, up, method) {
		if cl == common.CordonClassPunishment || cl == common.CordonClassOperator {
			return true
		}
	}
	return false
}

// reqLabels derives the {project, network} metric labels from the request,
// mirroring the consensus executor's own label sourcing.
func reqLabels(req *common.NormalizedRequest) (projectId, networkId string) {
	if req == nil {
		return "", ""
	}
	networkId = req.NetworkLabel()
	if net := req.Network(); net != nil {
		projectId = net.ProjectId()
	}
	return projectId, networkId
}

// blockRange converts the upstream's (lower, upper) availability bounds into a
// *policyengine.BlockRange. The fully-open sentinel window (MinInt64/MaxInt64)
// means "no configured range" and maps to nil; each open side maps to a nil
// bound.
func blockRange(lower, upper int64) *policyengine.BlockRange {
	if lower == math.MinInt64 && upper == math.MaxInt64 {
		return nil
	}
	r := &policyengine.BlockRange{}
	if lower != math.MinInt64 {
		l := lower
		r.Lower = &l
	}
	if upper != math.MaxInt64 {
		u := upper
		r.Upper = &u
	}
	return r
}

// blockNumberPtr best-effort converts the request's resolved EVM block number
// (an untyped value: int64, a hex/decimal string, or a float from JSON) into a
// *int64. A missing or unparseable value is nil — the eval's "unknown block"
// case — never an error, since block extraction must not break selection.
func blockNumberPtr(v interface{}) *int64 {
	switch n := v.(type) {
	case nil:
		return nil
	case int64:
		return &n
	case int:
		v := int64(n)
		return &v
	case float64:
		v := int64(n)
		return &v
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return nil
		}
		base := 10
		if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
			base, s = 16, s[2:]
		}
		if parsed, err := strconv.ParseInt(s, base, 64); err == nil {
			return &parsed
		}
	}
	return nil
}
