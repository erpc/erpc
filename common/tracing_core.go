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
)

var (
	IsTracingEnabled  bool
	IsTracingDetailed bool

	tracerProvider *sdktrace.TracerProvider
	tracer         trace.Tracer
	initOnce       sync.Once
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

		res, err := resource.New(ctx,
			resource.WithAttributes(
				semconv.ServiceNameKey.String("erpc"),
				semconv.ServiceVersionKey.String(ErpcVersion),
				attribute.String("commit.sha", ErpcCommitSha),
			),
		)
		if err != nil {
			logger.Error().Err(err).Msg("failed to create resource")
			return
		}

		tracerProvider = sdktrace.NewTracerProvider(
			sdktrace.WithSampler(createTracingSampler(cfg)),
			sdktrace.WithBatcher(exporter),
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
			logger.Error().Err(err).Msg("open telemetry export error")
		}))

		tracer = otel.Tracer(instrumentationName)
		IsTracingEnabled = true
		IsTracingDetailed = cfg.Detailed

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

	return otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		secureOption,
	)
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

	return otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(cfg.Endpoint),
		secureOption,
	)
}

func createTracingSampler(cfg *TracingConfig) sdktrace.Sampler {
	if cfg.SampleRate <= 0 {
		return sdktrace.NeverSample()
	}

	if cfg.SampleRate >= 1.0 {
		return sdktrace.AlwaysSample()
	}

	// Use ParentBased sampler to ensure child spans follow parent's sampling decision
	// This prevents orphan spans and "invalid parent span" errors
	return sdktrace.ParentBased(
		sdktrace.TraceIDRatioBased(cfg.SampleRate),
		// These options ensure consistent sampling across the trace
		sdktrace.WithRemoteParentSampled(sdktrace.AlwaysSample()),
		sdktrace.WithRemoteParentNotSampled(sdktrace.NeverSample()),
		sdktrace.WithLocalParentSampled(sdktrace.AlwaysSample()),
		sdktrace.WithLocalParentNotSampled(sdktrace.NeverSample()),
	)
}
