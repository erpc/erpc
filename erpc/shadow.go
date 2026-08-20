/* #nosec G404 */
package erpc

import (
	"context"
	"fmt"
	"strconv"

	"math/rand/v2"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
)

func (p *PreparedProject) executeShadowRequests(ctx context.Context, network *Network, shadowUpstreams []*upstream.Upstream, resp *common.NormalizedResponse) {
	defer func() {
		if r := recover(); r != nil {
			p.Logger.Error().Msgf("panic while executing shadow requests: %v", r)
			telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
				"shadow-upstreams",
				fmt.Sprintf("network:%s", network.Label()),
				common.ErrorFingerprint(r),
			).Inc()
		}
	}()
	if resp == nil || len(shadowUpstreams) == 0 {
		return
	}

	resp.RLockWithTrace(ctx)

	// Derive the original request from the response
	origReq := resp.Request()
	if origReq == nil {
		resp.RUnlock()
		return
	}

	method, _ := origReq.Method()

	// Compute the expected hash of the original upstream response once
	originalSize, err := resp.Size(ctx)
	if err != nil {
		resp.RUnlock()
		p.Logger.Error().Err(err).Msg("failed to compute hash for original response while executing shadow requests")
		return
	}

	resp.RUnlock()

	// Fire shadow requests concurrently
	for _, ups := range shadowUpstreams {
		allowed, err := ups.ShouldHandleMethod(method)
		if err != nil {
			p.Logger.Error().Err(err).Msg("failed to check if method is allowed for shadow upstream")
			continue
		}
		if !allowed {
			p.Logger.Debug().Str("method", method).Str("upstreamId", ups.Id()).Msg("method not allowed for shadow upstream")
			continue
		}
		// Block availability, same question the real routing path asks.
		//
		// Shadow used to mirror EVERY sampled request regardless of whether
		// the upstream could possibly hold the block, so a recent-only
		// upstream — `maxAvailableRecentBlocks`, or an explicit
		// `blockAvailability` bound — was still sent archive-depth traffic
		// it can only refuse. Observed on a recent-window replica: requests
		// for 2022 blocks against a node whose window starts millions of
		// blocks later, at several per second, every one of them a
		// guaranteed error that then reads as a shadow "mismatch".
		//
		// That is noise in both directions: it burns the shadow upstream's
		// capacity on work it cannot do, and it pollutes the comparison the
		// shadow exists to produce.
		//
		// Fails OPEN, exactly like the real path: a tag (`latest`), an
		// unparseable ref, a non-EVM upstream or an errored assertion all
		// mirror as before. Only a CONCRETE height the upstream states it
		// does not have is skipped.
		blockNumber := int64(0)
		if _, bn, err := evm.ExtractBlockReferenceFromRequest(ctx, origReq); err == nil && bn > 0 {
			blockNumber = bn
		} else if bn, ok := resp.EvmBlockNumber().(int64); ok && bn > 0 {
			// BLOCK-HASH selectors carry no number in the request, so the
			// extraction above yields nothing and the availability gate
			// used to fail open — mirroring tip-follow traffic (indexers
			// select by hash for reorg safety, at the chain head) to
			// replicas that trail the head by seconds and cannot have that
			// block YET. Measured on a live mirror: ~23/s of guaranteed
			// "unknown hash" errors, the largest error class the shadow
			// stream produced, invisible to request-side extraction.
			//
			// The PRIMARY has already answered by the time shadow runs, so
			// its response knows the resolved block number — use it. Same
			// fail-open shape as above: only a concrete height the
			// upstream states it does not have is skipped.
			blockNumber = bn
		}
		if blockNumber > 0 {
			available, err := ups.EvmAssertBlockAvailability(ctx, method, common.AvailbilityConfidenceBlockHead, false, blockNumber)
			if err == nil && !available {
				p.Logger.Debug().
					Str("method", method).
					Str("upstreamId", ups.Id()).
					Int64("blockNumber", blockNumber).
					Msg("shadow request skipped: block outside the upstream's available range")
				continue
			}
		}
		// Apply sample rate: skip this shadow upstream based on configured probability
		sampleRate := 1.0
		if ups.Config().Shadow.SampleRate != nil {
			sampleRate = *ups.Config().Shadow.SampleRate
		}
		if sampleRate < 1.0 && rand.Float64() >= sampleRate {
			p.Logger.Debug().
				Str("method", method).
				Str("upstreamId", ups.Id()).
				Float64("sampleRate", sampleRate).
				Msg("shadow request skipped due to sampling")
			continue
		}

		ups := ups // capture loop variable
		go func() {
			ctx, cancel := context.WithCancel(p.networksRegistry.appCtx)
			defer cancel()

			shadowCtx, span := common.StartDetailSpan(ctx, "Project.executeShadowRequest")
			defer span.End()

			// Build a safe copy of the original request so that shadow requests do not race on shared state
			var shadowReq *common.NormalizedRequest
			if body := origReq.Body(); body != nil {
				// Copy the bytes to avoid accidental mutations
				cpy := append([]byte(nil), body...)
				shadowReq = common.NewNormalizedRequest(cpy)
			} else {
				jrq, errReq := origReq.JsonRpcRequest(shadowCtx)
				if errReq != nil {
					p.Logger.Error().Err(errReq).Msg("failed to clone json-rpc request for shadow upstream")
					return
				}
				bodyBytes, errMarshal := common.SonicCfg.Marshal(jrq)
				if errMarshal != nil {
					p.Logger.Error().Err(errMarshal).Msg("failed to marshal cloned json-rpc request for shadow upstream")
					return
				}
				shadowReq = common.NewNormalizedRequest(bodyBytes)
				// Pre-populate the parsed request so Forward() does not need to unmarshal again
				_, _ = shadowReq.JsonRpcRequest(shadowCtx)
			}

			// Copy directives so behaviour is consistent
			if dirs := origReq.Directives(); dirs != nil {
				shadowReq.SetDirectives(dirs.Clone())
			}

			// Copy HTTP context (headers, query parameters, user) for proper metrics tracking
			shadowReq.CopyHttpContextFrom(origReq)

			// Set network reference for completeness (not strictly required for forwarding)
			shadowReq.SetNetwork(origReq.Network())

			// Execute the request against the shadow upstream (do bypass exclusion because we have to enforce method exclusion locally here - to ignore the shadow flag checking)
			shadowResp, errForward := ups.Forward(shadowCtx, shadowReq, true, false)
			// An EXECUTION EXCEPTION is an answer, not a failure.
			//
			// A revert or an EVM halt (out of gas, invalid opcode,
			// insufficient funds) means the shadow upstream ran the call
			// and reached a verdict. Scoring that as a shadow ERROR
			// conflates "this upstream is broken" with "this contract
			// reverts", and the second is a correct answer every upstream
			// agrees on. Measured 2026-08-20: an upstream whose real
			// failure rate was 0.01% read as 7% because its halts were
			// counted here.
			//
			// So it is COMPARED instead. The primary's own JSON-RPC
			// envelope carries its error, so the two verdicts can be put
			// side by side: both reverted is identical, one reverting and
			// the other returning is a genuine mismatch worth seeing.
			if errForward != nil && common.HasErrorCode(errForward, common.ErrCodeEndpointExecutionException) {
				primaryReverted := false
				if jrr, jerr := resp.JsonRpcResponse(ctx); jerr == nil && jrr != nil && jrr.Error != nil {
					primaryReverted = true
				}
				if primaryReverted {
					telemetry.MetricShadowResponseIdenticalTotal.WithLabelValues(
						p.Config.Id,
						ups.VendorName(),
						network.Label(),
						ups.Id(),
						method,
					).Inc()
					p.Logger.Trace().
						Str("component", "shadowTraffic").
						Str("networkId", network.Id()).
						Str("upstreamId", ups.Id()).
						Str("method", method).
						Msg("shadow and primary both reached an execution verdict")
				} else {
					telemetry.MetricShadowResponseMismatchTotal.WithLabelValues(
						p.Config.Id,
						ups.VendorName(),
						network.Label(),
						ups.Id(),
						method,
						"n/a",
						"false",
						"false",
					).Inc()
					p.Logger.Warn().Err(errForward).
						Str("component", "shadowTraffic").
						Str("networkId", network.Id()).
						Str("upstreamId", ups.Id()).
						Str("method", method).
						Object("request", shadowReq).
						Msg("shadow reverted but primary returned a result")
				}
				return
			}
			if errForward != nil {
				telemetry.MetricShadowResponseErrorTotal.WithLabelValues(
					p.Config.Id,
					ups.VendorName(),
					network.Label(),
					ups.Id(),
					method,
					common.ErrorFingerprint(errForward),
				).Inc()
				p.Logger.Debug().Err(errForward).
					Str("component", "shadowTraffic").
					Str("networkId", network.Id()).
					Str("upstreamId", ups.Id()).
					Str("method", method).
					Object("request", shadowReq).
					Object("response", shadowResp).
					Msg("shadow request returned error")
				return
			}

			if shadowResp == nil {
				telemetry.MetricShadowResponseErrorTotal.WithLabelValues(
					p.Config.Id,
					ups.VendorName(),
					network.Label(),
					ups.Id(),
					method,
					"nil_response",
				).Inc()
				p.Logger.Debug().
					Str("component", "shadowTraffic").
					Str("networkId", network.Id()).
					Str("upstreamId", ups.Id()).
					Str("method", method).
					Object("request", shadowReq).
					Object("response", shadowResp).
					Msg("shadow request returned nil response")
				return
			}

			shadowSize, err := shadowResp.Size(shadowCtx)
			if err != nil {
				p.Logger.Error().Err(err).Msg("failed to compute size for shadow response")
				return
			}
			isShadowLarger := shadowSize > originalSize

			// Check if this shadow upstream has ignore fields configured for this method
			var ignoreFields []string
			if ups.Config().Shadow.IgnoreFields != nil {
				if fields, ok := ups.Config().Shadow.IgnoreFields[method]; ok {
					ignoreFields = fields
				}
			}

			// Calculate hashes, using ignored fields if configured
			var shadowHash string
			var expectedHash string
			var errHash error
			if len(ignoreFields) > 0 {
				// Recalculate both hashes with ignored fields for fair comparison
				expectedHash, err = resp.HashWithIgnoredFields(ignoreFields, ctx)
				if err != nil {
					p.Logger.Error().Err(err).Msg("failed to compute hash with ignored fields for original response")
				}
				shadowHash, errHash = shadowResp.HashWithIgnoredFields(ignoreFields, shadowCtx)
			} else {
				expectedHash, err = resp.Hash(ctx)
				if err != nil {
					p.Logger.Error().Err(err).Msg("failed to compute hash for original response")
				}
				shadowHash, errHash = shadowResp.Hash(shadowCtx)
			}

			if errHash != nil {
				telemetry.MetricShadowResponseErrorTotal.WithLabelValues(
					p.Config.Id,
					ups.VendorName(),
					network.Label(),
					ups.Id(),
					method,
					"hash_error",
				).Inc()
				p.Logger.Debug().Err(errHash).
					Str("component", "shadowTraffic").
					Str("networkId", network.Id()).
					Str("upstreamId", ups.Id()).
					Str("method", method).
					Object("request", shadowReq).
					Object("response", shadowResp).
					Msg("failed to compute hash for shadow response")
				return
			}

			isShadowEmpty := shadowResp.IsResultEmptyish(shadowCtx)
			isOriginalEmpty := resp.IsResultEmptyish(ctx)

			if shadowHash == expectedHash || (isShadowEmpty && isOriginalEmpty) {
				telemetry.MetricShadowResponseIdenticalTotal.WithLabelValues(
					p.Config.Id,
					ups.VendorName(),
					network.Label(),
					ups.Id(),
					method,
				).Inc()
				p.Logger.Trace().
					Str("component", "shadowTraffic").
					Str("networkId", network.Id()).
					Str("upstreamId", ups.Id()).
					Str("method", method).
					Object("request", shadowReq).
					Object("response", shadowResp).
					Msg("shadow response identical to primary response")
			} else {
				finality := "n/a"
				if shadowResp != nil {
					finality = shadowResp.Finality(shadowCtx).String()
				}
				telemetry.MetricShadowResponseMismatchTotal.WithLabelValues(
					p.Config.Id,
					ups.VendorName(),
					network.Label(),
					ups.Id(),
					method,
					finality,
					strconv.FormatBool(isShadowEmpty),
					strconv.FormatBool(isShadowLarger),
				).Inc()
				p.Logger.Error().
					Str("component", "shadowTraffic").
					Str("projectId", p.Config.Id).
					Str("networkId", network.Id()).
					Str("upstreamId", ups.Id()).
					Str("method", method).
					Str("expectedHash", expectedHash).
					Str("shadowHash", shadowHash).
					Strs("ignoredFields", ignoreFields).
					Object("request", shadowReq).
					Object("originalResponse", resp).
					Object("shadowResponse", shadowResp).
					Msg("shadow response hash mismatch")
			}

			shadowResp.Release()
		}()
	}
}
