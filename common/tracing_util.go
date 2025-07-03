package common

import (
	"context"
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"go.opentelemetry.io/otel/trace"
)

// noopSpan is a no-op implementation of trace.Span that does nothing when methods are called
type noopSpan struct{ trace.Span }

func (s noopSpan) End(...trace.SpanEndOption)              {}
func (s noopSpan) AddEvent(string, ...trace.EventOption)   {}
func (s noopSpan) IsRecording() bool                       { return false }
func (s noopSpan) SetStatus(codes.Code, string)            {}
func (s noopSpan) SetName(string)                          {}
func (s noopSpan) SetAttributes(...attribute.KeyValue)     {}
func (s noopSpan) RecordError(error, ...trace.EventOption) {}
func (s noopSpan) SpanContext() trace.SpanContext          { return trace.SpanContext{} }
func (s noopSpan) TracerProvider() trace.TracerProvider    { return nil }

// A single instance of noopSpan can be reused
var defaultNoopSpan = noopSpan{nil}

// Create simple spans only for major operations such as external interactions (cache, upstreams, etc) with low-cardinality tags
func StartSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if !IsTracingEnabled {
		return ctx, defaultNoopSpan
	}

	return tracer.Start(ctx, name, opts...)
}

// Create detailed spans for erpc internal operations as well as high-cardinality tags such as request params verbatim
func StartDetailSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if !IsTracingEnabled || !IsTracingDetailed {
		return ctx, defaultNoopSpan
	}

	return tracer.Start(ctx, name, opts...)
}

func ExtractHTTPRequestTraceContext(r *http.Request) context.Context {
	if !IsTracingEnabled {
		return r.Context()
	}

	propagator := propagation.TraceContext{}
	return propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
}

func InjectHTTPResponseTraceContext(ctx context.Context, w http.ResponseWriter) {
	if !IsTracingEnabled {
		return
	}

	propagator := propagation.TraceContext{}
	propagator.Inject(ctx, propagation.HeaderCarrier(w.Header()))
}

func StartHTTPServerSpan(ctx context.Context, r *http.Request) (context.Context, trace.Span) {
	if !IsTracingEnabled {
		return ctx, trace.SpanFromContext(ctx)
	}

	propagator := propagation.TraceContext{}
	ctx = propagator.Extract(ctx, propagation.HeaderCarrier(r.Header))
	var span trace.Span
	ctx, span = StartSpan(ctx, "Http.ReceivedRequest",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			semconv.HTTPMethodKey.String(r.Method),
			semconv.HTTPURLKey.String(r.URL.String()),
			semconv.HTTPSchemeKey.String(r.URL.Scheme),
			semconv.HTTPUserAgentKey.String(r.UserAgent()),
		),
	)

	return ctx, span
}

func EnrichHTTPServerSpan(ctx context.Context, statusCode int, err error) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}

	span.SetAttributes(semconv.HTTPStatusCodeKey.Int(statusCode))

	if err != nil {
		SetTraceSpanError(span, err)
	} else {
		if statusCode >= 400 {
			span.SetStatus(codes.Error, http.StatusText(statusCode))
		} else {
			span.SetStatus(codes.Ok, "")
		}
	}
}

func StartRequestSpan(ctx context.Context, req *NormalizedRequest) context.Context {
	if !IsTracingEnabled {
		return ctx
	}

	method, _ := req.Method()
	ctx, span := tracer.Start(ctx, "Request.Handle",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("request.method", method),
		),
	)
	if IsTracingDetailed {
		span.SetAttributes(
			attribute.String("request.id", fmt.Sprintf("%v", req.ID())),
		)

		// If we have a JSON-RPC request, add more details
		if jrpcReq, err := req.JsonRpcRequest(); err == nil && jrpcReq != nil {
			// Add params as attributes if they're not too large
			if jrpcReq.Params != nil {
				paramsStr, _ := SonicCfg.MarshalToString(jrpcReq.Params)
				span.SetAttributes(attribute.String("request.jsonrpc.params", paramsStr))
			}
		}
	}

	return ctx
}

func EndRequestSpan(ctx context.Context, resp *NormalizedResponse, err interface{}) {
	if !IsTracingEnabled {
		return
	}

	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}

	if err != nil {
		SetTraceSpanError(span, err)
	} else if resp != nil {
		span.SetStatus(codes.Ok, "")
		span.SetAttributes(
			attribute.Bool("cache.hit", resp.FromCache()),
		)
		if ups := resp.Upstream(); ups != nil {
			span.SetAttributes(attribute.String("upstream.id", ups.Id()))
		}
		if IsTracingDetailed {
			if jrpcResp, err := resp.JsonRpcResponse(); err == nil && jrpcResp != nil {
				span.SetAttributes(attribute.Int("response.result_size", len(jrpcResp.Result)))
			}
		}
	}

	span.End()
}

// ForceFlushTraces forces the tracer provider to export all pending spans
// Use sparingly for critical traces that must be delivered
func ForceFlushTraces(ctx context.Context) error {
	if !IsTracingEnabled || tracerProvider == nil {
		return nil
	}

	// ForceFlush exports all ended spans that have not yet been exported
	return tracerProvider.ForceFlush(ctx)
}
