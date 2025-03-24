package tracing

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"go.opentelemetry.io/otel/trace"
)

func ExtractHTTPRequestTraceContext(r *http.Request) context.Context {
	if !isEnabled {
		return r.Context()
	}

	propagator := propagation.TraceContext{}
	return propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
}

func InjectHTTPResponseTraceContext(ctx context.Context, w http.ResponseWriter) {
	if !isEnabled {
		return
	}

	propagator := propagation.TraceContext{}
	propagator.Inject(ctx, propagation.HeaderCarrier(w.Header()))
}

func StartHTTPServerSpan(ctx context.Context, r *http.Request) (context.Context, trace.Span) {
	if !isEnabled {
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
		SetError(span, err)
	} else {
		if statusCode >= 400 {
			span.SetStatus(codes.Error, http.StatusText(statusCode))
		} else {
			span.SetStatus(codes.Ok, "")
		}
	}
}
