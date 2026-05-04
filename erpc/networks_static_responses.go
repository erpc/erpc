package erpc

import (
	"context"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
)

// tryServeStaticResponse returns a canned response if the request matches one
// of the network's configured static entries. It echoes the inbound request's
// JSON-RPC id into the response and records a metric on hit. Returns (nil,
// false) when no entry matches or the request cannot be inspected.
func (n *Network) tryServeStaticResponse(
	ctx context.Context,
	lg *zerolog.Logger,
	req *common.NormalizedRequest,
	method string,
) (*common.NormalizedResponse, bool) {
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		lg.Debug().Err(err).Str("method", method).Msg("skipping static response: cannot inspect request")
		return nil, false
	}

	jrq.RLock()
	params := jrq.Params
	jrq.RUnlock()

	match := common.FindStaticResponseMatch(n.cfg.StaticResponses, method, params)
	if match == nil {
		return nil, false
	}

	var rpcErr *common.ErrJsonRpcExceptionExternal
	if match.Response.Error != nil {
		rpcErr = &common.ErrJsonRpcExceptionExternal{
			Code:    match.Response.Error.Code,
			Message: match.Response.Error.Message,
			Data:    match.Response.Error.Data,
		}
	}

	jrr, err := common.NewJsonRpcResponse(req.ID(), match.Response.Result, rpcErr)
	if err != nil {
		lg.Error().Err(err).Str("method", method).Msg("failed to build static response")
		return nil, false
	}

	resp := common.NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(jrr)

	telemetry.CounterHandle(
		telemetry.MetricNetworkStaticResponseServedTotal,
		n.projectId, n.Label(), method,
	).Inc()

	if lg.GetLevel() <= zerolog.DebugLevel {
		lg.Debug().Str("method", method).Msg("served static response (no upstream contacted)")
	}

	return resp, true
}
