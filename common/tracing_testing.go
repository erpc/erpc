package common

import (
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// SetTracerProviderForTest installs tp as the active tracer provider and enables tracing.
// Intended for unit tests that need to inspect emitted spans without a real OTLP collector.
// Consumes initOnce so that InitializeTracing is a no-op for the rest of the process.
func SetTracerProviderForTest(tp *sdktrace.TracerProvider) {
	otel.SetTracerProvider(tp)
	tracer = otel.Tracer(instrumentationName)
	IsTracingEnabled = true
	IsTracingDetailed = false
	initOnce.Do(func() {})
}
