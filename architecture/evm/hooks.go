package evm

import (
	"context"
	"strings"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog/log"
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

	switch strings.ToLower(method) {
	case "eth_call":
		return projectPreForward_eth_call(ctx, network, nq)
	case "eth_chainid":
		return projectPreForward_eth_chainId(ctx, network, nq)
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
	case "eth_chainid":
		return networkPreForward_eth_chainId(ctx, network, upstreams, nq)
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
	case "eth_blocknumber":
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

	switch strings.ToLower(method) {
	case "eth_getlogs":
		return upstreamPreForward_eth_getLogs(ctx, n, u, r)
	case "eth_chainid":
		return upstreamPreForward_eth_chainId(ctx, n, u, r)
	case "trace_filter", "arbtrace_filter":
		return upstreamPreForward_trace_filter(ctx, n, u, r)
	case "eth_queryblocks", "eth_querytransactions", "eth_querylogs", "eth_querytraces", "eth_querytransfers":
		return upstreamPreForward_eth_query(ctx, n, u, r)
	default:
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
	// longer dispute an honest majority). Internal requests (e.g. the
	// corroboration force-fetch) are skipped to avoid recursing into the engine.
	dirs := rq.Directives()
	if integrity.HasChecks(methodLower) && (dirs == nil || !dirs.IsInternal) {
		if cs, policy := resolveIntegrity(n, dirs); len(cs) > 0 {
			view := networkChainView(n)
			input := integrity.Input{
				Method:   methodLower,
				Upstream: u,
				Response: rs,
				Checks:   cs,
				Resolver: newIntegrityResolver(n, u),
				Reorg:    policy,
			}
			if view != nil {
				input.History = view
			}
			// The span wraps Validate so its duration is the integrity overhead and
			// the aux force-fetches (network.Forward) nest under it. Detailed tracing
			// adds the actual mismatch values (verbatim) to pinpoint the bad field.
			vctx, span := common.StartSpan(ctx, "Integrity.Validate",
				trace.WithAttributes(
					attribute.String("integrity.method", methodLower),
					attribute.String("integrity.upstream", u.Id()),
				))
			vStart := time.Now()
			res := integrity.Validate(vctx, input)
			rq.AddIntegrityOverhead(time.Since(vStart))
			annotateIntegritySpan(span, res)
			// Record the rejected/soft-flagged ("original") body on the violating
			// attempt's span, for a by-hand sanity check. Only on a violation here:
			// a recovering pass runs concurrently (hedged) so its IntegrityCaught
			// flag may not be set yet — the "corrected" served body is recorded once
			// at the request level (project.Forward) instead. The IsTracingDetailed
			// gate short-circuits before any body copy, so it's zero-cost when
			// tracing is off.
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
			// Per-check attempts/outcomes (pass/reject/soft_flag/off) — sum = total
			// attempts. Higher volume than the violation counter below.
			for _, oc := range res.Outcomes {
				telemetry.MetricIntegrityCheck.WithLabelValues(
					n.ProjectId(), u.VendorName(), n.Label(), u.Id(), methodLower, oc.CheckID, oc.Outcome,
				).Inc()
			}
			for _, rec := range res.Recorded {
				telemetry.MetricIntegrityViolation.WithLabelValues(
					n.ProjectId(), u.VendorName(), n.Label(), u.Id(), methodLower, rec.CheckID, "soft_flag",
				).Inc()
				log.Warn().Str("check", rec.CheckID).Str("reason", rec.Reason).Str("method", methodLower).
					Msg("integrity: recorded reorg-sensitive mismatch on unfinalized block")
			}
			if res.Err != nil {
				telemetry.MetricIntegrityViolation.WithLabelValues(
					n.ProjectId(), u.VendorName(), n.Label(), u.Id(), methodLower, res.RejectedCheckID, "reject",
				).Inc()
				// Remember we caught a bad response (and which check); project.Forward
				// then counts it as saved (a retry succeeded) or failed (no good
				// response found) — see integrity_saved_total / integrity_failed_total.
				rq.MarkIntegrityCaught(res.RejectedCheckID)
				validationErr = res.Err
				rq.ClearLastValidResponse()
				return rs, validationErr
			}
			// Feed the ChainView with this validated response: block responses
			// populate pin+header; narrow responses (receipts/tx) pin the number→hash
			// for FINALIZED blocks only (tip-thrash safety).
			if isBlockMethod(methodLower) {
				observeBlockView(ctx, view, rs)
			} else if isAnchoredNarrowMethod(methodLower) {
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
	// clear the lastValidResponse so retry/consensus doesn't mistakenly use an invalid response.
	if validationErr != nil && common.HasErrorCode(validationErr, common.ErrCodeEndpointContentValidation) {
		rq.ClearLastValidResponse()
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
		outcome = "soft_flag"
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
		span.AddEvent("integrity.soft_flag", trace.WithAttributes(
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
