package common

import (
	"context"
	"fmt"
	"sync"

	"github.com/go-logr/zerologr"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/credentials"
)

const (
	SpanContextKey      ContextKey = "trace_span"
	instrumentationName string     = "github.com/erpc/erpc"

	// ForceTraceHeader is the HTTP header to force tracing for a request (bypasses sampling)
	ForceTraceHeader = "X-ERPC-Force-Trace"
	// ForceTraceQueryParam is the query parameter to force tracing for a request
	ForceTraceQueryParam = "force-trace"
	// forceTraceAttributeKey is the internal attribute key used by the sampler
	forceTraceAttributeKey = "erpc.force_trace"
	// forceTraceNetworkKey is the context key for network-based force-tracing
	forceTraceNetworkKey ContextKey = "force_trace_network"
)

var (
	IsTracingEnabled  bool
	IsTracingDetailed bool

	tracerProvider *sdktrace.TracerProvider
	tracer         trace.Tracer
	initOnce       sync.Once

	// forceTraceMatchers holds the compiled matchers for force-tracing
	forceTraceMatchers []*ForceTraceMatcher
)

func InitializeTracing(ctx context.Context, logger *zerolog.Logger, cfg *TracingConfig) error {
	var err error

	initOnce.Do(func() {
		if cfg == nil || !cfg.Enabled {
			logger.Info().Msg("OpenTelemetry tracing is disabled")
			IsTracingEnabled = false
			IsTracingDetailed = false
			return
		}

		logger.Info().
			Str("endpoint", cfg.Endpoint).
			Str("protocol", string(cfg.Protocol)).
			Str("serviceName", cfg.ServiceName).
			Float64("sampleRate", cfg.SampleRate).
			Bool("detailed", cfg.Detailed).
			Msg("initializing OpenTelemetry tracing")

		var exporter sdktrace.SpanExporter

		switch cfg.Protocol {
		case TracingProtocolGrpc:
			exporter, err = createTracingGRPCExporter(ctx, cfg)
		case TracingProtocolHttp:
			exporter, err = createTracingHTTPExporter(ctx, cfg)
		default:
			err = fmt.Errorf("unsupported tracing protocol: %s", cfg.Protocol)
		}

		if err != nil {
			logger.Error().Err(err).Msg("failed to create span exporter")
			return
		}

		// Build resource attributes
		attrs := []attribute.KeyValue{
			semconv.ServiceNameKey.String(cfg.ServiceName),
			semconv.ServiceVersionKey.String(ErpcVersion),
			attribute.String("commit.sha", ErpcCommitSha),
		}

		// Add custom resource attributes from config (env vars already expanded)
		for key, value := range cfg.ResourceAttributes {
			if value != "" {
				attrs = append(attrs, attribute.String(key, value))
				logger.Debug().Str("key", key).Str("value", value).Msg("adding custom resource attribute")
			}
		}

		res, err := resource.New(ctx,
			resource.WithAttributes(attrs...),
		)
		if err != nil {
			logger.Error().Err(err).Msg("failed to create resource")
			return
		}

		// Create batch span processor with configured limits to prevent memory leaks.
		// When the queue is full, new spans are dropped (not blocked).
		batchOpts := []sdktrace.BatchSpanProcessorOption{
			sdktrace.WithMaxQueueSize(cfg.MaxQueueSize),
			sdktrace.WithMaxExportBatchSize(cfg.MaxExportBatchSize),
			sdktrace.WithBatchTimeout(cfg.BatchTimeout.Duration()),
			sdktrace.WithExportTimeout(cfg.ExportTimeout.Duration()),
		}

		logger.Info().
			Int("maxQueueSize", cfg.MaxQueueSize).
			Int("maxExportBatchSize", cfg.MaxExportBatchSize).
			Dur("batchTimeout", cfg.BatchTimeout.Duration()).
			Dur("exportTimeout", cfg.ExportTimeout.Duration()).
			Msg("configuring tracing batch processor")

		tracerProvider = sdktrace.NewTracerProvider(
			sdktrace.WithSampler(createTracingSampler(cfg)),
			sdktrace.WithBatcher(exporter, batchOpts...),
			sdktrace.WithResource(res),
		)
		otel.SetTracerProvider(tracerProvider)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		))

		if logger.GetLevel() <= zerolog.DebugLevel {
			otel.SetLogger(zerologr.New(logger))
			logger.Info().Msg("OpenTelemetry debug logging enabled")
		}

		// Create an error handler for logging export failures
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
			logger.Trace().Err(err).Msg("open telemetry export error")
		}))

		tracer = otel.Tracer(instrumentationName)
		IsTracingEnabled = true
		IsTracingDetailed = cfg.Detailed

		// Store force-trace matchers
		forceTraceMatchers = cfg.ForceTraceMatchers

		if len(forceTraceMatchers) > 0 {
			for i, m := range forceTraceMatchers {
				logger.Info().
					Int("index", i).
					Str("network", m.Network).
					Str("method", m.Method).
					Msg("force-trace matcher configured")
			}
		}

		logger.Info().Msg("OpenTelemetry tracing initialized successfully")
	})

	return err
}

func ShutdownTracing(ctx context.Context) error {
	if tracerProvider == nil {
		return nil
	}
	return tracerProvider.Shutdown(ctx)
}

func SpanFromContext(ctx context.Context) trace.Span {
	return trace.SpanFromContext(ctx)
}

func SetTraceSpanError(span trace.Span, err any) {
	if span == nil || !span.IsRecording() {
		return
	}
	if err, ok := err.(error); ok {
		var errStr string
		if stdErr, ok := err.(StandardError); ok {
			errStr, _ = SonicCfg.MarshalToString(stdErr)
			if errStr == "" {
				errStr = stdErr.Base().Error()
			}
			span.SetAttributes(
				attribute.String("error.code", stdErr.CodeChain()),
			)
			span.RecordError(fmt.Errorf("%s", errStr))
			span.SetStatus(codes.Error, string(stdErr.Base().Code))
		} else {
			span.RecordError(err)
			span.SetStatus(codes.Error, ErrorSummary(err))
		}
	}
}

func createTracingGRPCExporter(ctx context.Context, cfg *TracingConfig) (*otlptrace.Exporter, error) {
	secureOption := otlptracegrpc.WithInsecure()
	if cfg.TLS != nil && cfg.TLS.Enabled {
		tlsConfig, err := CreateTLSConfig(cfg.TLS)
		if err != nil {
			return nil, err
		}
		secureOption = otlptracegrpc.WithTLSCredentials(credentials.NewTLS(tlsConfig))
	}

	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		secureOption,
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlptracegrpc.WithHeaders(cfg.Headers))
	}

	return otlptracegrpc.New(ctx, opts...)
}

func createTracingHTTPExporter(ctx context.Context, cfg *TracingConfig) (*otlptrace.Exporter, error) {
	secureOption := otlptracehttp.WithInsecure()
	if cfg.TLS != nil && cfg.TLS.Enabled {
		tlsConfig, err := CreateTLSConfig(cfg.TLS)
		if err != nil {
			return nil, err
		}
		secureOption = otlptracehttp.WithTLSClientConfig(tlsConfig)
	}

	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(cfg.Endpoint),
		secureOption,
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlptracehttp.WithHeaders(cfg.Headers))
	}

	return otlptracehttp.New(ctx, opts...)
}

func createTracingSampler(cfg *TracingConfig) sdktrace.Sampler {
	var baseSampler sdktrace.Sampler

	if cfg.SampleRate <= 0 {
		baseSampler = sdktrace.NeverSample()
	} else if cfg.SampleRate >= 1.0 {
		baseSampler = sdktrace.AlwaysSample()
	} else {
		// Use ParentBased sampler to ensure child spans follow parent's sampling decision
		// This prevents orphan spans and "invalid parent span" errors
		baseSampler = sdktrace.ParentBased(
			sdktrace.TraceIDRatioBased(cfg.SampleRate),
			// These options ensure consistent sampling across the trace
			sdktrace.WithRemoteParentSampled(sdktrace.AlwaysSample()),
			sdktrace.WithRemoteParentNotSampled(sdktrace.NeverSample()),
			sdktrace.WithLocalParentSampled(sdktrace.AlwaysSample()),
			sdktrace.WithLocalParentNotSampled(sdktrace.NeverSample()),
		)
	}

	// Wrap with force-trace sampler to allow bypassing sampling via header/query param
	return &forceTraceSampler{delegate: baseSampler}
}

// forceTraceSampler wraps another sampler and forces sampling when the force-trace attribute is present
type forceTraceSampler struct {
	delegate sdktrace.Sampler
}

func (s *forceTraceSampler) ShouldSample(params sdktrace.SamplingParameters) sdktrace.SamplingResult {
	// Check if force-trace attribute is set
	for _, attr := range params.Attributes {
		if string(attr.Key) == forceTraceAttributeKey && attr.Value.AsBool() {
			return sdktrace.SamplingResult{
				Decision:   sdktrace.RecordAndSample,
				Tracestate: trace.SpanContextFromContext(params.ParentContext).TraceState(),
			}
		}
	}

	return s.delegate.ShouldSample(params)
}

func (s *forceTraceSampler) Description() string {
	return "ForceTraceSampler{" + s.delegate.Description() + "}"
}

// ShouldForceTrace checks if the given network and method match any force-trace matcher.
// Returns true and the matching reason if a matcher matches.
// Network format is "architecture:chainId", e.g., "evm:1", "evm:137".
// Method is the JSON-RPC method name, e.g., "eth_call", "eth_getBalance".
func ShouldForceTrace(network, method string) (bool, string) {
	if len(forceTraceMatchers) == 0 {
		return false, ""
	}

	for _, matcher := range forceTraceMatchers {
		if matchesForceTraceMatcher(matcher, network, method) {
			// Build reason string
			reason := "matcher"
			if matcher.Network != "" && matcher.Method != "" {
				reason = "network:" + network + ",method:" + method
			} else if matcher.Network != "" {
				reason = "network:" + network
			} else if matcher.Method != "" {
				reason = "method:" + method
			}
			return true, reason
		}
	}

	return false, ""
}

// matchesForceTraceMatcher checks if network and method match a single matcher.
// If both network and method are specified, both must match (AND).
// If only one is specified, only that field is checked.
// Patterns support wildcards (*) and OR (|) via WildcardMatch.
func matchesForceTraceMatcher(matcher *ForceTraceMatcher, network, method string) bool {
	if matcher == nil {
		return false
	}

	// If neither is specified, no match
	if matcher.Network == "" && matcher.Method == "" {
		return false
	}

	networkMatches := true
	methodMatches := true

	// Check network pattern if specified (WildcardMatch handles | and *)
	if matcher.Network != "" {
		networkMatches, _ = WildcardMatch(matcher.Network, network)
	}

	// Check method pattern if specified (WildcardMatch handles | and *)
	if matcher.Method != "" {
		methodMatches, _ = WildcardMatch(matcher.Method, method)
	}

	return networkMatches && methodMatches
}

// SetForceTraceNetwork stores the network in context for force-trace matching in child spans.
// Call this after URL path parsing to enable network-based force-tracing for child spans.
// Returns the updated context with network stored (always stores if non-empty, matching happens later).
func SetForceTraceNetwork(ctx context.Context, network string) context.Context {
	if !IsTracingEnabled || network == "" {
		return ctx
	}
	return context.WithValue(ctx, forceTraceNetworkKey, network)
}

// GetForceTraceNetwork returns the network stored in context for force-tracing, if any.
func GetForceTraceNetwork(ctx context.Context) string {
	if v := ctx.Value(forceTraceNetworkKey); v != nil {
		return v.(string)
	}
	return ""
}
