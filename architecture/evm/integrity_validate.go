package evm

import (
	"context"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// sourceHeaderCategory labels the integrity metrics of source headers.
const sourceHeaderCategory = "source_header"

// integrityRun is one integrity validation of an upstream response.
type integrityRun struct {
	method      string // lowercase; selects the checks
	category    string // the metrics' method label
	checks      integrity.CheckSet
	policy      integrity.ReorgPolicy
	observeOnly bool
	autoCorrect bool
	params      []any
	view        *ChainTracker
	// held: the response is eRPC's own data (a tracker fetch, a probe, a
	// source header), so checks read only what the tracker holds and never
	// fetch (spec §7).
	held bool
	// header is the response's parsed header when the caller has it.
	header *chainHeader
}

// runIntegrity validates rs and reports what the checks found: per-check
// outcomes, violations, the archive, misbehavior for a deterministic
// reject, and each header check's verdict for the network's suspensions.
// rq is nil for a source header.
func runIntegrity(ctx context.Context, n common.Network, u common.Upstream, rq *common.NormalizedRequest, rs *common.NormalizedResponse, r integrityRun) integrity.Result {
	input := integrity.Input{
		Method:      r.method,
		Upstream:    u,
		Response:    rs,
		Checks:      r.checks,
		Params:      r.params,
		Resolver:    newIntegrityResolver(n, u, r.view, r.held),
		Reorg:       r.policy,
		ObserveOnly: r.observeOnly,
		AutoCorrect: r.autoCorrect,
	}
	if r.view != nil {
		if r.held {
			input.History = heldHistory{r.view}
		} else {
			input.History = r.view
		}
	}
	sus := suspensionsOf(n)
	if sus != nil {
		input.Modes = sus.modeFn
	}
	// The span wraps Validate so its duration is the integrity overhead and
	// the aux force-fetches nest under it. Detailed tracing adds the actual
	// mismatch values (verbatim) to pinpoint the bad field.
	vctx, span := common.StartSpan(ctx, "Integrity.Validate",
		trace.WithAttributes(
			attribute.String("integrity.method", r.category),
			attribute.String("integrity.upstream", u.Id()),
		))
	vStart := time.Now()
	res := integrity.Validate(vctx, input)
	if rq != nil {
		rq.AddIntegrityOverhead(time.Since(vStart))
	}
	annotateIntegritySpan(span, res)
	// Record the rejected/recorded ("original") body on the violating
	// attempt's span, for a by-hand sanity check. The IsTracingDetailed gate
	// short-circuits before any body copy, so it's zero-cost when tracing is
	// off.
	if common.IsTracingDetailed && (res.Err != nil || len(res.Recorded) > 0) {
		if jrr, jerr := rs.JsonRpcResponse(vctx); jerr == nil && jrr != nil {
			body := jrr.GetResultBytes()
			if len(body) > maxTraceResponseBytes {
				body = body[:maxTraceResponseBytes]
				span.SetAttributes(attribute.Bool("integrity.response_truncated", true))
			}
			span.SetAttributes(attribute.String("integrity.response", string(body)))
		}
	}
	span.End()

	if sus != nil && isBlockMethod(r.method) {
		if r.header != nil {
			sus.recordOutcomes(r.header.number, vendorKey(u), res.Outcomes)
		} else if height, ok := responseBlockNumber(ctx, rs); ok {
			sus.recordOutcomes(height, vendorKey(u), res.Outcomes)
		}
	}
	// Per-check attempts/outcomes — sum = total attempts. "pass" means an
	// actual verification ran; "skip" means the check couldn't evaluate.
	for _, oc := range res.Outcomes {
		telemetry.MetricIntegrityCheck.WithLabelValues(
			n.ProjectId(), u.VendorName(), n.Label(), u.Id(), r.category, oc.CheckID, oc.Outcome,
		).Inc()
	}
	for _, rec := range res.Recorded {
		verdict := rec.Verdict
		if verdict == "" {
			verdict = "record_only"
		}
		msg := "integrity: recorded mismatch (served, not rejected)"
		if verdict == "would_reject" {
			// Observe-only: this WOULD have failed the request under
			// enforcement. Logged distinctly so the enforcement-readiness
			// review can find them without untangling routine record-only flags.
			msg = "integrity: observe-only suppressed a rejection (served; enforcement would have failed this request)"
		}
		telemetry.MetricIntegrityViolation.WithLabelValues(
			n.ProjectId(), u.VendorName(), n.Label(), u.Id(), r.category, rec.CheckID, verdict, rec.Finality,
		).Inc()
		log.Warn().Str("project", n.ProjectId()).Str("network", n.Label()).
			Str("upstream", u.Id()).Str("vendor", u.VendorName()).Str("method", r.category).
			Str("check", rec.CheckID).Str("finality", rec.Finality).Str("reason", rec.Reason).
			Msg(msg)
		exportIntegrityCatch(ctx, n, u, rs, r.category, verdict, rec.CheckID, rec.Class.String(), rec.Finality, rec.Reason)
	}
	if res.Err == nil {
		return res
	}
	rejectVerdict := "reject"
	if res.FallbackEligible {
		rejectVerdict = "reject_recoverable"
	}
	telemetry.MetricIntegrityViolation.WithLabelValues(
		n.ProjectId(), u.VendorName(), n.Label(), u.Id(), r.category, res.RejectedCheckID, rejectVerdict, res.Finality,
	).Inc()
	log.Warn().Str("project", n.ProjectId()).Str("network", n.Label()).
		Str("upstream", u.Id()).Str("vendor", u.VendorName()).Str("method", r.category).
		Str("check", res.RejectedCheckID).Str("finality", res.Finality).
		Str("reason", res.Err.Error()).Msg("integrity: rejected response (caught bad data)")
	exportIntegrityCatch(ctx, n, u, rs, r.category, "reject", res.RejectedCheckID, res.RejectedClass.String(), res.Finality, res.Err.Error())
	// A Deterministic reject is PROVABLE corruption from this upstream —
	// feed it into misbehavior scoring so routing learns to avoid a
	// chronically-corrupt node. Reorg-sensitive rejects may be transient
	// races; they don't score.
	if res.RejectedClass == integrity.Deterministic {
		if ht := u.Tracker(); ht != nil {
			ht.RecordUpstreamMisbehavior(u, r.method, rs.Finality(ctx))
		}
	}
	return res
}

// responseBlockNumber is the number of the block a block response carries.
func responseBlockNumber(ctx context.Context, rs *common.NormalizedResponse) (uint64, bool) {
	jrr, err := rs.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return 0, false
	}
	s, err := jrr.PeekStringByPath(ctx, "number")
	if err != nil {
		return 0, false
	}
	n, err := common.HexToInt64(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return uint64(n), true
}

// headerChecks is a network's header checks, compiled from its integrity
// config once per config.
type headerChecks struct {
	cfg         *common.IntegrityConfig
	chainId     int64
	checks      integrity.CheckSet
	policy      integrity.ReorgPolicy
	observeOnly bool
}

// headerChecksOf is n's header checks, compiled again only once n's
// integrity config changed.
func (nt *networkTrackers) headerChecksOf(n common.Network) *headerChecks {
	var cfg *common.IntegrityConfig
	var chainId int64
	if c := n.Config(); c != nil {
		cfg = c.Integrity
		if c.Evm != nil {
			chainId = c.Evm.ChainId
		}
	}
	if hc := nt.checks.Load(); hc != nil && hc.cfg == cfg && hc.chainId == chainId {
		return hc
	}
	cs, policy, observeOnly, _ := resolveIntegrity(n, nil)
	hc := &headerChecks{cfg: cfg, chainId: chainId, checks: integrity.HeaderCheckSet(cs), policy: policy, observeOnly: observeOnly}
	nt.checks.Store(hc)
	return hc
}

// CheckSourceHeader is intake check 1 (spec §5.3): while integrity is on, a
// source header must pass the network's header checks, judged against the
// network-wide tracker's held headers only. A header whose hash the tracker
// holds from an earlier source that passed is not validated again. false:
// drop it; the violation is counted against u.
func CheckSourceHeader(n common.Network, u common.Upstream, sh SourceHeader) bool {
	nt := registeredTrackers(n)
	if nt == nil {
		return true
	}
	hc := nt.headerChecksOf(n)
	if len(hc.checks) == 0 {
		return true
	}
	h := sh.h
	view := nt.suspend.tracker.Load()
	if view != nil {
		if held := view.heldHeader(h.hash); held != nil && held.intakePassed.Load() {
			return true
		}
	}
	ctx := context.Background()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["` + h.parsed.Number + `",false]}`))
	req.SetNetwork(n)
	jrr, err := common.NewJsonRpcResponseFromBytes([]byte("1"), h.raw, nil)
	if err != nil {
		return false
	}
	rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	res := runIntegrity(ctx, n, u, nil, rs, integrityRun{
		method:      integrity.MethodGetBlockByNumber,
		category:    sourceHeaderCategory,
		checks:      hc.checks,
		policy:      hc.policy,
		observeOnly: hc.observeOnly,
		view:        view,
		held:        true,
		header:      h,
	})
	if res.Err != nil {
		return false
	}
	h.intakePassed.Store(true)
	return true
}
