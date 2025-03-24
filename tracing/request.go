package tracing

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/erpc/erpc/common"
)

func StartRequestSpan(ctx context.Context, req *common.NormalizedRequest) context.Context {
	if !isEnabled {
		return ctx
	}

	method, _ := req.Method()
	ctx, span := tracer.Start(ctx, "Request.Handle",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("request.method", method),
			attribute.String("request.id", fmt.Sprintf("%v", req.ID())),
		),
	)

	// If we have a JSON-RPC request, add more details
	if jrpcReq, err := req.JsonRpcRequest(); err == nil && jrpcReq != nil {
		// Add params as attributes if they're not too large
		if jrpcReq.Params != nil {
			paramsStr, _ := common.SonicCfg.MarshalToString(jrpcReq.Params)
			span.SetAttributes(attribute.String("jsonrpc.params", paramsStr))
		}
	}

	return ctx
}

func EndRequestSpan(ctx context.Context, resp *common.NormalizedResponse, err interface{}) {
	if !isEnabled {
		return
	}

	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}

	if err != nil {
		SetError(span, err)
	} else if resp != nil {
		span.SetStatus(codes.Ok, "")
		span.SetAttributes(
			attribute.Bool("cache.hit", resp.FromCache()),
		)
		if ups := resp.Upstream(); ups != nil {
			span.SetAttributes(attribute.String("upstream.id", ups.Config().Id))
		}
		if jrpcResp, err := resp.JsonRpcResponse(); err == nil && jrpcResp != nil {
			span.SetAttributes(attribute.Int("result_size", len(jrpcResp.Result)))
		}
	}

	span.End()
}
