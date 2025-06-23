package erpc

import (
	"context"
	"fmt"
	"strconv"

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
				fmt.Sprintf("network:%s", network.networkId),
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
	expectedHash, err := resp.Hash(ctx)
	originalSize, err := resp.Size(ctx)
	if err != nil {
		resp.RUnlock()
		p.Logger.Error().Err(err).Msg("failed to compute hash for original response while executing shadow requests")
		return
	}

	resp.RUnlock()

	// Fire shadow requests concurrently
	for _, ups := range shadowUpstreams {
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

			// Set network reference for completeness (not strictly required for forwarding)
			shadowReq.SetNetwork(origReq.Network())

			// Execute the request against the shadow upstream (bypass exclusion)
			shadowResp, errForward := ups.Forward(shadowCtx, shadowReq, true)
			if errForward != nil {
				telemetry.MetricShadowResponseErrorTotal.WithLabelValues(
					p.Config.Id,
					ups.VendorName(),
					network.networkId,
					ups.Id(),
					method,
					common.ErrorFingerprint(errForward),
				).Inc()
				p.Logger.Debug().Err(errForward).
					Str("component", "shadowTraffic").
					Str("projectId", p.Config.Id).
					Str("networkId", network.networkId).
					Str("upstreamId", ups.Id()).
					Str("method", method).
					Msg("shadow request returned error")
				return
			}

			if shadowResp == nil {
				telemetry.MetricShadowResponseErrorTotal.WithLabelValues(
					p.Config.Id,
					ups.VendorName(),
					network.networkId,
					ups.Id(),
					method,
					"nil_response",
				).Inc()
				p.Logger.Debug().
					Str("component", "shadowTraffic").
					Str("projectId", p.Config.Id).
					Str("networkId", network.networkId).
					Str("upstreamId", ups.Id()).
					Str("method", method).
					Msg("shadow request returned nil response")
				return
			}

			shadowSize, err := shadowResp.Size(shadowCtx)
			if err != nil {
				p.Logger.Error().Err(err).Msg("failed to compute size for shadow response")
				return
			}
			isShadowLarger := shadowSize > originalSize

			shadowHash, errHash := shadowResp.Hash(shadowCtx)
			if errHash != nil {
				telemetry.MetricShadowResponseErrorTotal.WithLabelValues(
					p.Config.Id,
					ups.VendorName(),
					network.networkId,
					ups.Id(),
					method,
					"hash_error",
				).Inc()
				p.Logger.Debug().Err(errHash).
					Str("component", "shadowTraffic").
					Str("projectId", p.Config.Id).
					Str("networkId", network.networkId).
					Str("upstreamId", ups.Id()).
					Str("method", method).
					Msg("failed to compute hash for shadow response")
				return
			}

			if shadowHash == expectedHash {
				telemetry.MetricShadowResponseIdenticalTotal.WithLabelValues(
					p.Config.Id,
					ups.VendorName(),
					network.networkId,
					ups.Id(),
					method,
				).Inc()
				p.Logger.Trace().
					Str("component", "shadowTraffic").
					Str("projectId", p.Config.Id).
					Str("networkId", network.networkId).
					Str("upstreamId", ups.Id()).
					Str("method", method).
					Msg("shadow response identical to primary response")
			} else {
				isEmpty := shadowResp.IsResultEmptyish(shadowCtx)
				telemetry.MetricShadowResponseMismatchTotal.WithLabelValues(
					p.Config.Id,
					ups.VendorName(),
					network.networkId,
					ups.Id(),
					method,
					strconv.FormatBool(isEmpty),
					strconv.FormatBool(isShadowLarger),
				).Inc()
				p.Logger.Error().
					Str("component", "shadowTraffic").
					Str("projectId", p.Config.Id).
					Str("networkId", network.networkId).
					Str("upstreamId", ups.Id()).
					Str("method", method).
					Str("expectedHash", expectedHash).
					Str("shadowHash", shadowHash).
					Object("originalResponse", resp).
					Object("shadowResponse", shadowResp).
					Msg("shadow response hash mismatch")
			}

			shadowResp.Release()
		}()
	}
}
