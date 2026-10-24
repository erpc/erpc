package evm

import (
	"context"
	"strings"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// maxTraceResponseBytes caps the validated-response body recorded on integrity
// spans in detailed tracing mode (for by-hand sanity checks). Larger bodies are
// truncated — re-fetch by block number for the full payload.
const maxTraceResponseBytes = 128 * 1024

// HandleProjectPreForward is the early pre-forward hook executed at project layer
// before cache and before upstream selection. Use this for transformations that
// affect cache hash or short-circuit results without upstream context.
func HandleProjectPreForward(ctx context.Context, network common.Network, nq *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	ctx, span := common.StartDetailSpan(ctx, "Project.PreForwardHook")
	defer span.End()

	method, err := nq.Method()
	if err != nil {
		return false, nil, err
	}

	if handled, resp, err := projectPreForward_localAnswer(ctx, network, nq, method); handled {
		return true, resp, err
	}

	switch strings.ToLower(method) {
	case "eth_getlogs":
		return projectPreForward_eth_getLogs(ctx, network, nq)
	case "trace_filter", "arbtrace_filter":
		return projectPreForward_trace_filter(ctx, network, nq)
	default:
		return false, nil, nil
	}
}

// HandleNetworkPreForward is executed after upstream selection for upstream-aware logic.
// Pass the selected upstreams to allow computing effective thresholds, availability, etc.
func HandleNetworkPreForward(ctx context.Context, network common.Network, upstreams []common.Upstream, nq *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	ctx, span := common.StartDetailSpan(ctx, "Network.PreForwardHook")
	defer span.End()

	method, err := nq.Method()
	if err != nil {
		return false, nil, err
	}

	switch strings.ToLower(method) {
	case "eth_getlogs":
		return networkPreForward_eth_getLogs(ctx, network, upstreams, nq)
	case "eth_blocknumber", "erigon_blocknumber":
		return networkPreForward_eth_blockNumber(ctx, network, nq)
	case "eth_getblockbynumber":
		return networkPreForward_eth_getBlockByNumber(ctx, network, nq)
	case "trace_filter", "arbtrace_filter":
		return networkPreForward_trace_filter(ctx, network, upstreams, nq)
	default:
		return false, nil, nil
	}
}

// HandleNetworkPostForward checks if the request matches a known EVM method customization on network level,
// and returns a custom response if it applies. Otherwise returns the response/error as is.
func HandleNetworkPostForward(ctx context.Context, network common.Network, nq *common.NormalizedRequest, nr *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "Network.PostForwardHook")
	defer span.End()

	method, err := nq.Method()
	if err != nil {
		return nr, err
	}

	switch strings.ToLower(method) {
	case "eth_blocknumber", "erigon_blocknumber":
		return networkPostForward_eth_blockNumber(ctx, network, nq, nr, re)
	case "eth_getblockbynumber":
		return networkPostForward_eth_getBlockByNumber(ctx, network, nq, nr, re)
	case "eth_getlogs":
		return networkPostForward_eth_getLogs(ctx, network, nq, nr, re)
	case "eth_sendrawtransaction":
		return networkPostForward_eth_sendRawTransaction(ctx, network, nq, nr, re)
	case "trace_filter", "arbtrace_filter":
		return networkPostForward_trace_filter(ctx, network, nq, nr, re)
	default:
		return nr, re
	}
}

func HandleUpstreamPreForward(ctx context.Context, n common.Network, u common.Upstream, r *common.NormalizedRequest, skipCacheRead bool) (handled bool, resp *common.NormalizedResponse, err error) {
	ctx, span := common.StartDetailSpan(ctx, "Upstream.PreForwardHook")
	defer span.End()

	method, err := r.Method()
	if err != nil {
		return false, nil, err
	}

	methodLower := strings.ToLower(method)
	switch methodLower {
	case "eth_getlogs":
		return upstreamPreForward_eth_getLogs(ctx, n, u, r)
	case "eth_call":
		return upstreamPreForward_eth_call(ctx, n, u, r)
	case "trace_filter", "arbtrace_filter":
		return upstreamPreForward_trace_filter(ctx, n, u, r)
	default:
		// Deliberately NO per-request gate on the integrity state prober's
		// findings here. The prober publishes evidence — proven-head telemetry
		// and, for a sustained streak of wrong-height answers, upstream
		// misbehavior on the health tracker (see noteDisproved) — and exclusion
		// decisions belong to the operator's selection policy, which already
		// reads that ledger (misbehaviorRateAbove). A hook-level refusal keyed
		// to the proven head was tried and misfired structurally: the proven
		// head advances at probe cadence, so on a chain whose block time is
		// shorter than that cadence it trails every honest upstream's claimed
		// head and the network's own advertised tip becomes unroutable.
		return false, nil, nil
	}
}

func HandleUpstreamPostForward(ctx context.Context, n common.Network, u common.Upstream, rq *common.NormalizedRequest, rs *common.NormalizedResponse, re error, skipCacheRead bool) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "Upstream.PostForwardHook")
	defer span.End()

	method, err := rq.Method()
	if err != nil {
		return rs, err
	}

	methodLower := strings.ToLower(method)

	// Special case: eth_sendRawTransaction needs to handle errors for idempotency
	if methodLower == "eth_sendrawtransaction" {
		return upstreamPostForward_eth_sendRawTransaction(ctx, n, u, rq, rs, re)
	}
	if methodLower == "eth_call" {
		upstreamPostForward_eth_call(u, rq, rs, re)
	}

	// For all other methods, skip validation if there's already an error
	if re != nil {
		return rs, re
	}

	var validationErr error

	// Data-integrity checks. Opt-in: the network's integrity config selects the
	// checks (empty config → nothing runs). The engine runs them over a single
	// decode and converts a violation into a content-validation error so
	// retry/consensus route around the upstream (consensus already excludes
	// errors from preferLargerResponses, so a corrupt-but-larger response can no
	// longer dispute an honest majority). eRPC's own requests (tracker
	// fetches, probes, polls) are checked too, against held data only: a
	// check never fetches recursively, and never judges them against the
	// pins they are there to move. They hunt no replacement. Tracker
	// fetches feed no answer back to the tracker: its scheduler hands them
	// over itself.
	dirs := rq.Directives()
	if integrity.HasChecks(methodLower) {
		if cs, policy, observeOnly, autoCorrect := resolveIntegrity(n, dirs); len(cs) > 0 {
			held := dirs != nil && (dirs.IsInternal || dirs.UpstreamPolicies != common.UpstreamPoliciesConfigured)
			tracked := dirs != nil && dirs.IsInternal
			// Integrity state + corroboration are scoped to the node GROUP the request
			// was pinned to (use-upstream selector), reusing erpc's served-tip grouping
			// so a receipt from one group is only checked against same-group
			// nodes. No selector → network-wide.
			selector := ""
			if dirs != nil {
				selector = dirs.UseUpstream
			}
			view := ChainTrackerOf(ctx, n, selector)
			// Request-aware checks (eth_getLogs filter reproduction) need the
			// original params; read-only access under the request's RLock.
			var params []any
			if jrq, jerr := rq.JsonRpcRequest(ctx); jerr == nil && jrq != nil {
				jrq.RLockWithTrace(ctx)
				params = jrq.Params
				jrq.RUnlock()
			}
			res := runIntegrity(ctx, n, u, rq, rs, integrityRun{
				method:      methodLower,
				category:    methodLower,
				checks:      cs,
				policy:      policy,
				observeOnly: observeOnly,
				autoCorrect: autoCorrect && !held,
				params:      params,
				view:        view,
				held:        held,
			})
			if res.Err != nil {
				if res.FallbackEligible {
					// A recordOnly verdict escalated by autoCorrectWhenPossible:
					// stash the flagged original so project.Forward can serve it
					// if every alternative upstream is exhausted.
					rq.SetIntegrityFallbackResponse(rs, res.RejectedCheckID, res.Finality, res.RejectedReason)
				}
				// project.Forward counts it as saved (a retry succeeded) or
				// failed (no good response found).
				rq.MarkIntegrityCaught(res.RejectedCheckID, res.Finality)
				// Mark first (SetLastValidResponse refuses marked responses — an
				// in-flight hedge could otherwise re-store this body), then drop
				// the LVR only if it IS this response (an unconditional clear
				// could lose a concurrent VALID response from another attempt).
				rs.MarkIntegrityRejected()
				rq.ClearLastValidResponseIf(rs)
				return rs, res.Err
			}
			// Hand the validated response to the tracker: block responses are
			// held as answers; narrow responses (receipts/tx) give anchors for
			// FINALIZED blocks only (tip-thrash safety).
			if !tracked && isBlockMethod(methodLower) {
				observeBlockView(ctx, view, u, rs)
			} else if !tracked && isAnchoredNarrowMethod(methodLower) {
				observeNarrowView(ctx, view, u, rs)
			}
		}
	}

	// Check if this method should have empty results marked as errors
	shouldMarkEmpty := isMethodInMarkEmptyList(n, methodLower)

	// Method-specific post-forward hooks with directive-based validation
	switch methodLower {
	case "eth_getlogs":
		rs, validationErr = upstreamPostForward_eth_getLogs(ctx, n, u, rq, rs, re)

	case "trace_filter", "arbtrace_filter":
		rs, validationErr = upstreamPostForward_trace_filter(ctx, n, u, rq, rs, re)

	default:
		// For other methods, only apply the mark empty check if configured
		if shouldMarkEmpty {
			rs, validationErr = upstreamPostForward_markUnexpectedEmpty(ctx, u, rq, rs, re)
		}
	}

	// If validation failed due to content validation error (e.g. bloom inconsistency),
	// mark the offending response and drop it from lastValidResponse so
	// retry/consensus doesn't mistakenly use (or a hedge re-store) an invalid
	// response — identity-checked so a concurrent valid response isn't lost.
	if validationErr != nil && common.HasErrorCode(validationErr, common.ErrCodeEndpointContentValidation) {
		rs.MarkIntegrityRejected()
		rq.ClearLastValidResponseIf(rs)
	}

	if validationErr != nil {
		return rs, validationErr
	}

	return rs, re
}

// isMethodInMarkEmptyList checks if the given method is in the network's configured
// list of methods that should have empty results marked as errors.
func isMethodInMarkEmptyList(n common.Network, methodLower string) bool {
	if n == nil || n.Config() == nil || n.Config().Evm == nil {
		return false
	}

	methods := n.Config().Evm.MarkEmptyAsErrorMethods
	if methods == nil {
		return false
	}

	for _, m := range methods {
		if strings.ToLower(m) == methodLower {
			return true
		}
	}
	return false
}

// annotateIntegritySpan records the integrity validation outcome on the span.
// Simple mode: the outcome, how many checks were evaluated, and the rejecting
// check. Detailed mode additionally records, verbatim and WITHOUT redaction, the
// reason of every violation (the actual vs expected values) plus each check's
// outcome — enough to pinpoint exactly which field was wrong/corrupt/missing.
func annotateIntegritySpan(span trace.Span, res integrity.Result) {
	outcome := "pass"
	if res.Err != nil {
		outcome = "reject"
	} else if len(res.Recorded) > 0 {
		outcome = "record_only"
	}
	span.SetAttributes(
		attribute.Int("integrity.checks", len(res.Outcomes)),
		attribute.String("integrity.outcome", outcome),
	)
	if res.RejectedCheckID != "" {
		span.SetAttributes(attribute.String("integrity.rejected_check", res.RejectedCheckID))
	}
	if !common.IsTracingDetailed {
		return
	}
	for _, rec := range res.Recorded {
		span.AddEvent("integrity.record_only", trace.WithAttributes(
			attribute.String("check", rec.CheckID),
			attribute.String("reason", rec.Reason),
		))
	}
	if res.Err != nil {
		span.AddEvent("integrity.reject", trace.WithAttributes(
			attribute.String("check", res.RejectedCheckID),
			attribute.String("reason", res.Err.Error()),
		))
	}
	for _, oc := range res.Outcomes {
		span.SetAttributes(attribute.String("integrity.check."+oc.CheckID, oc.Outcome))
	}
}
