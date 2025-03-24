package tracing

import (
	"context"
	"fmt"
	"sync"

	"github.com/go-logr/zerologr"
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
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc/credentials"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

const (
	SpanContextKey      common.ContextKey = "trace_span"
	instrumentationName string            = "github.com/erpc/erpc"
)

var (
	tracerProvider *sdktrace.TracerProvider
	tracer         trace.Tracer
	initOnce       sync.Once
	isEnabled      bool
)

func Initialize(ctx context.Context, logger *zerolog.Logger, cfg *common.TracingConfig) error {
	var err error

	initOnce.Do(func() {
		if cfg == nil || !cfg.Enabled {
			logger.Info().Msg("OpenTelemetry tracing is disabled")
			isEnabled = false
			return
		}

		logger.Info().Str("endpoint", cfg.Endpoint).Str("protocol", string(cfg.Protocol)).Msg("initializing OpenTelemetry tracing")

		var exporter sdktrace.SpanExporter

		switch cfg.Protocol {
		case common.TracingProtocolGrpc:
			exporter, err = createGRPCExporter(ctx, cfg)
		case common.TracingProtocolHttp:
			exporter, err = createHTTPExporter(ctx, cfg)
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
				semconv.ServiceVersionKey.String(common.ErpcVersion),
				attribute.String("commit.sha", common.ErpcCommitSha),
			),
		)
		if err != nil {
			logger.Error().Err(err).Msg("failed to create resource")
			return
		}

		tracerProvider = sdktrace.NewTracerProvider(
			sdktrace.WithSampler(createSampler(cfg)),
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

		tracer = otel.Tracer(instrumentationName)
		isEnabled = true

		logger.Info().Msg("OpenTelemetry tracing initialized successfully")
	})

	return err
}

func Shutdown(ctx context.Context) error {
	if tracerProvider == nil {
		return nil
	}
	return tracerProvider.Shutdown(ctx)
}

func IsEnabled() bool {
	return isEnabled
}

func Tracer() trace.Tracer {
	if !isEnabled {
		return noop.NewTracerProvider().Tracer("noop")
	}
	return tracer
}

func StartSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if !isEnabled {
		return ctx, trace.SpanFromContext(ctx)
	}

	return tracer.Start(ctx, name, opts...)
}

func SpanFromContext(ctx context.Context) trace.Span {
	return trace.SpanFromContext(ctx)
}

func SetError(span trace.Span, err any) {
	if err, ok := err.(error); ok {
		var errStr string
		if stdErr, ok := err.(common.StandardError); ok {
			errStr, _ = common.SonicCfg.MarshalToString(stdErr)
			if errStr == "" {
				errStr = stdErr.Base().Error()
			}
			span.SetAttributes(
				attribute.String("error.chain", stdErr.CodeChain()),
			)
			span.RecordError(fmt.Errorf("%s", errStr))
			span.SetStatus(codes.Error, string(stdErr.Base().Code))
		} else {
			span.RecordError(err)
			span.SetStatus(codes.Error, common.ErrorSummary(err))
		}
	}
}

func createGRPCExporter(ctx context.Context, cfg *common.TracingConfig) (*otlptrace.Exporter, error) {
	secureOption := otlptracegrpc.WithInsecure()
	if cfg.TLS != nil && cfg.TLS.Enabled {
		tlsConfig, err := common.CreateTLSConfig(cfg.TLS)
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

func createHTTPExporter(ctx context.Context, cfg *common.TracingConfig) (*otlptrace.Exporter, error) {
	secureOption := otlptracehttp.WithInsecure()
	if cfg.TLS != nil && cfg.TLS.Enabled {
		tlsConfig, err := common.CreateTLSConfig(cfg.TLS)
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

func createSampler(cfg *common.TracingConfig) sdktrace.Sampler {
	if cfg.SampleRate <= 0 {
		return sdktrace.NeverSample()
	}

	if cfg.SampleRate >= 1.0 {
		return sdktrace.AlwaysSample()
	}

	return sdktrace.TraceIDRatioBased(cfg.SampleRate)
}
