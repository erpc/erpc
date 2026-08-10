package evm

import (
	"context"
	"fmt"
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

	methodLower := strings.ToLower(method)
	switch methodLower {
	case "eth_getlogs":
		return upstreamPreForward_eth_getLogs(ctx, n, u, r)
	case "eth_chainid":
		return upstreamPreForward_eth_chainId(ctx, n, u, r)
	case "trace_filter", "arbtrace_filter":
		return upstreamPreForward_trace_filter(ctx, n, u, r)
	case "eth_queryblocks", "eth_querytransactions", "eth_querylogs", "eth_querytraces", "eth_querytransfers":
		return upstreamPreForward_eth_query(ctx, n, u, r)
	default:
		// The state-proven boundary protects a CLASS (methods answered from the
		// state trie at a block), not a list copied to this call site. The class
		// is defined once, in common.IsEvmStateQueryMethod, so a method joining
		// it cannot be gated in one place and forgotten in another.
		if common.IsEvmStateQueryMethod(methodLower) {
			return upstreamPreForward_stateBoundary(ctx, n, u, r, method)
		}
		return false, nil, nil
	}
}

// upstreamPreForward_stateBoundary refuses to send a state query to an
// upstream whose STATE-PROVEN head does not cover the requested block. Nodes
// sometimes answer state methods from older state while reporting a current
// head; the proven head (advanced only by verified probes — see the state
// prober) is the boundary that cannot silently outrun reality.
//
// Inert unless the state prober is running for this network, so the default
// behavior is byte-identical with the feature off.
func upstreamPreForward_stateBoundary(ctx context.Context, n common.Network, u common.Upstream, r *common.NormalizedRequest, method string) (bool, *common.NormalizedResponse, error) {
	prober := stateProberFor(n.Id())
	if prober == nil {
		return false, nil, nil
	}
	// The probes themselves are internal state calls aimed at upstreams whose
	// boundary has NOT advanced yet — gating them would deadlock the proving.
	if dirs := r.Directives(); dirs != nil && dirs.IsInternal {
		return false, nil, nil
	}
	_, blockNumber, err := ExtractBlockReferenceFromRequest(ctx, r)
	if err != nil || blockNumber <= 0 {
		// Tags (latest/pending), unparseable refs, and requests whose height the
		// method config cannot locate are not gated here: the proven boundary is
		// a statement about a CONCRETE height, so with no height there is nothing
		// to compare and the request goes out as it does today.
		return false, nil, nil
	}
	eu, ok := u.(common.EvmUpstream)
	if !ok {
		return false, nil, nil
	}
	available, err := eu.EvmAssertBlockAvailability(ctx, method, common.AvailbilityConfidenceStateProven, false, blockNumber)
	if err != nil {
		return false, nil, nil
	}
	var proven int64
	if r, ok := u.(common.EvmStateProvenReader); ok {
		proven = r.EvmStateProvenBlock()
	}
	if available {
		// The claimed-head fallback (proven == 0) exists so that upstreams which
		// cannot be probed keep serving. It must not shelter an upstream that
		// ANSWERS the probe and executes at the wrong height — that node has not
		// failed to prove itself, it has proven the opposite, and letting it
		// serve is the silent-wrong-data case this boundary was built for.
		if proven > 0 || !prober.disproved(u.Id()) {
			return false, nil, nil
		}
		// Divert only when someone else can actually answer. A disproved
		// upstream that is the only one able to serve the height still serves;
		// excluding the last resort trades wrong data for an outage.
		if !prober.aSiblingCanServe(ctx, u.Id(), blockNumber) {
			return false, nil, nil
		}
		prober.count(u, "boundary", "diverted")
		return true, nil, &common.ErrUpstreamBlockUnavailable{
			BaseError: common.BaseError{
				Code: common.ErrCodeUpstreamBlockUnavailable,
				Message: fmt.Sprintf(
					"state for block %d is DISPROVEN on this node (it answers pinned calls at the wrong height) for %s",
					blockNumber, method),
				Details: map[string]interface{}{
					"upstreamId":  u.Id(),
					"blockNumber": blockNumber,
					"disproven":   true,
				},
			},
		}
	}
	return true, nil, &common.ErrUpstreamBlockUnavailable{
		BaseError: common.BaseError{
			Code:    common.ErrCodeUpstreamBlockUnavailable,
			Message: fmt.Sprintf("state for block %d is not yet PROVEN on this node (stateProvenBlock: %d) for %s", blockNumber, proven, method),
			Details: map[string]interface{}{
				"upstreamId":       u.Id(),
				"blockNumber":      blockNumber,
				"stateProvenBlock": proven,
			},
		},
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
		if cs, policy, observeOnly := resolveIntegrity(n, dirs); len(cs) > 0 {
			// Integrity state + corroboration are scoped to the node GROUP the request
			// was pinned to (use-upstream selector), reusing erpc's served-tip grouping
			// so a receipt from one group is only checked against same-group
			// nodes. No selector → network-wide.
			selector := ""
			if dirs != nil {
				selector = dirs.UseUpstream
			}
			view := groupChainView(ctx, n, selector)
			input := integrity.Input{
				Method:      methodLower,
				Upstream:    u,
				Response:    rs,
				Checks:      cs,
				Resolver:    newIntegrityResolver(ctx, n, u, selector),
				Reorg:       policy,
				ObserveOnly: observeOnly,
			}
			if view != nil {
				input.History = view
			}
			// Request-aware checks (eth_getLogs filter reproduction) need the
			// original params; read-only access under the request's RLock.
			if jrq, jerr := rq.JsonRpcRequest(ctx); jerr == nil && jrq != nil {
				jrq.RLockWithTrace(ctx)
				input.Params = jrq.Params
				jrq.RUnlock()
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
			// Per-check attempts/outcomes (pass/skip/reject/soft_flag/reconfirmed/
			// off) — sum = total attempts. "pass" means an actual verification ran;
			// "skip" means the check couldn't evaluate (cold cache, missing wiring).
			// Higher volume than the violation counter below.
			for _, oc := range res.Outcomes {
				telemetry.MetricIntegrityCheck.WithLabelValues(
					n.ProjectId(), u.VendorName(), n.Label(), u.Id(), methodLower, oc.CheckID, oc.Outcome,
				).Inc()
			}
			for _, rec := range res.Recorded {
				verdict := rec.Verdict
				if verdict == "" {
					verdict = "soft_flag"
				}
				msg := "integrity: recorded reorg-sensitive mismatch (served, not rejected)"
				if verdict == "would_reject" {
					// Observe-only: this WOULD have failed the request under
					// enforcement. Logged distinctly so the enforcement-readiness
					// review can find them without untangling routine soft-flags.
					msg = "integrity: observe-only suppressed a rejection (served; enforcement would have failed this request)"
				}
				telemetry.MetricIntegrityViolation.WithLabelValues(
					n.ProjectId(), u.VendorName(), n.Label(), u.Id(), methodLower, rec.CheckID, verdict, rec.Finality,
				).Inc()
				log.Warn().Str("project", n.ProjectId()).Str("network", n.Label()).
					Str("upstream", u.Id()).Str("vendor", u.VendorName()).Str("method", methodLower).
					Str("check", rec.CheckID).Str("finality", rec.Finality).Str("reason", rec.Reason).
					Msg(msg)
				exportIntegrityCatch(ctx, n, u, rs, methodLower, verdict, rec.CheckID, rec.Class.String(), rec.Finality, rec.Reason)
			}
			if res.Err != nil {
				telemetry.MetricIntegrityViolation.WithLabelValues(
					n.ProjectId(), u.VendorName(), n.Label(), u.Id(), methodLower, res.RejectedCheckID, "reject", res.Finality,
				).Inc()
				// Remember we caught a bad response (and which check); project.Forward
				// then counts it as saved (a retry succeeded) or failed (no good
				// response found) — see integrity_saved_total / integrity_failed_total.
				log.Warn().Str("project", n.ProjectId()).Str("network", n.Label()).
					Str("upstream", u.Id()).Str("vendor", u.VendorName()).Str("method", methodLower).
					Str("check", res.RejectedCheckID).Str("finality", res.Finality).
					Str("reason", res.Err.Error()).Msg("integrity: rejected response (caught bad data)")
				rq.MarkIntegrityCaught(res.RejectedCheckID, res.Finality)
				exportIntegrityCatch(ctx, n, u, rs, methodLower, "reject", res.RejectedCheckID, res.RejectedClass.String(), res.Finality, res.Err.Error())
				// A Deterministic reject is PROVABLE corruption from this upstream —
				// feed it into misbehavior scoring so routing learns to avoid a
				// chronically-corrupt node (an upstream once served 158k corrupt
				// blocks while keeping a clean score, because content validation
				// happens after the per-attempt outcome is classified).
				// Reorg-sensitive rejects may be transient races; they don't score.
				if res.RejectedClass == integrity.Deterministic {
					if ht := u.Tracker(); ht != nil {
						ht.RecordUpstreamMisbehavior(u, methodLower, rs.Finality(ctx))
					}
				}
				validationErr = res.Err
				// Mark first (SetLastValidResponse refuses marked responses — an
				// in-flight hedge could otherwise re-store this body), then drop
				// the LVR only if it IS this response (an unconditional clear
				// could lose a concurrent VALID response from another attempt).
				rs.MarkIntegrityRejected()
				rq.ClearLastValidResponseIf(rs)
				return rs, validationErr
			}
			// Feed the ChainView with this validated response: block responses
			// populate pin+header; narrow responses (receipts/tx) pin the number→hash
			// for FINALIZED blocks only (tip-thrash safety).
			if isBlockMethod(methodLower) {
				observeBlockView(ctx, view, rs, methodLower)
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
