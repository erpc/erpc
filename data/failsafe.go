package data

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// isTransportError reports whether err originates from the network/transport
// layer. Retries are reserved for these — application-level errors (server-side
// failures, malformed responses, validation issues) are not retriable, since
// retrying them just burns budget without changing the outcome.
func isTransportError(err error) bool {
	if err == nil {
		return false
	}

	if common.HasErrorCode(err, common.ErrCodeEndpointTransportFailure) {
		return true
	}

	// Cache-specific record states are NOT transport errors.
	if common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
		return false
	}
	if common.HasErrorCode(err, common.ErrCodeRecordExpired) {
		return false
	}

	// Context cancellation is caller-initiated; never a transport signal.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	if errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}

	if s, ok := status.FromError(err); ok {
		switch s.Code() {
		case codes.Unavailable, codes.DeadlineExceeded, codes.Aborted:
			return true
		}
	}

	// Fallback: opaque errors whose string clearly indicates transport-layer
	// failure or a known-transient server condition that retry can resolve.
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "no such host"),
		strings.Contains(msg, "network is unreachable"),
		strings.Contains(msg, "tls handshake"),
		strings.Contains(msg, "i/o timeout"),
		strings.Contains(msg, "operation timed out"),
		strings.Contains(msg, "use of closed network connection"),
		strings.Contains(msg, "client is closed"),
		strings.Contains(msg, "unexpectedly closed"),
		strings.Contains(msg, "goaway"),
		strings.Contains(msg, "clusterdown"),
		strings.Contains(msg, "masterdown"),
		strings.Contains(msg, "tryagain"),
		strings.Contains(msg, "redis is loading"):
		return true
	}

	return false
}

var scopeConnector = common.Scope("connector")

// FailsafeConnector wraps a Connector with retry / hedge / breaker /
// timeout policies driven by *cacheExecutor.
type FailsafeConnector struct {
	wrapped      Connector
	logger       *zerolog.Logger
	getExecutors []*cacheExecutor
	setExecutors []*cacheExecutor
}

var _ Connector = (*FailsafeConnector)(nil)

// NewFailsafeConnector constructs a FailsafeConnector backed by per-direction
// cacheExecutor instances for Get vs Set/Delete operations.
func NewFailsafeConnector(
	logger *zerolog.Logger,
	wrapped Connector,
	getCfgs []*common.FailsafeConfig,
	setCfgs []*common.FailsafeConfig,
) (*FailsafeConnector, error) {
	lg := logger.With().Str("component", "failsafeConnector").Str("connectorId", wrapped.Id()).Logger()

	getExecutors, err := buildCacheExecutors(&lg, wrapped.Id(), getCfgs)
	if err != nil {
		return nil, err
	}
	setExecutors, err := buildCacheExecutors(&lg, wrapped.Id(), setCfgs)
	if err != nil {
		return nil, err
	}

	return &FailsafeConnector{
		wrapped:      wrapped,
		logger:       &lg,
		getExecutors: getExecutors,
		setExecutors: setExecutors,
	}, nil
}

func buildCacheExecutors(logger *zerolog.Logger, connectorId string, cfgs []*common.FailsafeConfig) ([]*cacheExecutor, error) {
	var executors []*cacheExecutor

	for _, fsCfg := range cfgs {
		if fsCfg == nil {
			continue
		}
		ex, err := NewCacheExecutor(fsCfg, logger)
		if err != nil {
			return nil, common.NewErrFailsafeConfiguration(
				err,
				map[string]interface{}{"connectorId": connectorId},
			)
		}
		executors = append(executors, ex)
	}

	// Append a no-op fallback executor so unmatched operations always have one.
	noop, _ := NewCacheExecutor(nil, logger)
	executors = append(executors, noop)

	return executors, nil
}

// pickCacheExecutor selects the best-matching executor using the 4-tier
// priority: method+finality → method → finality → default.
func pickCacheExecutor(executors []*cacheExecutor, ctx context.Context) *cacheExecutor {
	var method string
	var finality common.DataFinalityState

	if r := ctx.Value(common.RequestContextKey); r != nil {
		if req, ok := r.(*common.NormalizedRequest); ok && req != nil {
			method, _ = req.Method()
			finality = req.Finality(ctx)
		}
	}

	for _, fe := range executors {
		if fe.method != "*" && len(fe.finalities) > 0 {
			matched, _ := common.WildcardMatch(fe.method, method)
			if matched && slices.Contains(fe.finalities, finality) {
				return fe
			}
		}
	}
	for _, fe := range executors {
		if fe.method != "*" && len(fe.finalities) == 0 {
			matched, _ := common.WildcardMatch(fe.method, method)
			if matched {
				return fe
			}
		}
	}
	for _, fe := range executors {
		if fe.method == "*" && len(fe.finalities) > 0 {
			if slices.Contains(fe.finalities, finality) {
				return fe
			}
		}
	}
	for _, fe := range executors {
		if fe.method == "*" && len(fe.finalities) == 0 {
			return fe
		}
	}
	return nil
}

// ----- Connector interface implementation -----

func (f *FailsafeConnector) Id() string {
	return f.wrapped.Id()
}

func (f *FailsafeConnector) Get(ctx context.Context, index, partitionKey, rangeKey string, metadata interface{}) ([]byte, error) {
	fe := pickCacheExecutor(f.getExecutors, ctx)
	if fe == nil {
		return f.wrapped.Get(ctx, index, partitionKey, rangeKey, metadata)
	}

	ctx, span := common.StartDetailSpan(ctx, "ConnectorFailsafe.Get",
		trace.WithAttributes(
			attribute.String("connector.id", f.wrapped.Id()),
			attribute.String("connector.operation", "get"),
			attribute.String("connector.partition_key", partitionKey),
			attribute.String("connector.range_key", rangeKey),
			attribute.String("failsafe.match_method", fe.method),
		),
	)
	defer span.End()

	result, err := fe.RunBytes(ctx, func(ctx context.Context) ([]byte, error) {
		return f.wrapped.Get(ctx, index, partitionKey, rangeKey, metadata)
	})
	if err != nil {
		common.SetTraceSpanError(span, err)
		span.SetAttributes(attribute.String("error.summary", common.ErrorSummary(err)))
		return nil, err
	}
	span.SetAttributes(attribute.Int("result.bytes", len(result)))
	return result, nil
}

func (f *FailsafeConnector) Set(ctx context.Context, partitionKey, rangeKey string, value []byte, ttl *time.Duration) error {
	fe := pickCacheExecutor(f.setExecutors, ctx)
	if fe == nil {
		return f.wrapped.Set(ctx, partitionKey, rangeKey, value, ttl)
	}

	ctx, span := common.StartDetailSpan(ctx, "ConnectorFailsafe.Set",
		trace.WithAttributes(
			attribute.String("connector.id", f.wrapped.Id()),
			attribute.String("connector.operation", "set"),
			attribute.String("connector.partition_key", partitionKey),
			attribute.String("connector.range_key", rangeKey),
			attribute.String("failsafe.match_method", fe.method),
			attribute.Int("value.bytes", len(value)),
		),
	)
	defer span.End()

	if ttl != nil {
		span.SetAttributes(attribute.Int64("ttl.ms", ttl.Milliseconds()))
	}

	err := fe.RunVoid(ctx, func(ctx context.Context) error {
		return f.wrapped.Set(ctx, partitionKey, rangeKey, value, ttl)
	})
	if err != nil {
		common.SetTraceSpanError(span, err)
		span.SetAttributes(attribute.String("error.summary", common.ErrorSummary(err)))
		return err
	}
	return nil
}

func (f *FailsafeConnector) Delete(ctx context.Context, partitionKey, rangeKey string) error {
	fe := pickCacheExecutor(f.setExecutors, ctx)
	if fe == nil {
		return f.wrapped.Delete(ctx, partitionKey, rangeKey)
	}

	ctx, span := common.StartDetailSpan(ctx, "ConnectorFailsafe.Delete",
		trace.WithAttributes(
			attribute.String("connector.id", f.wrapped.Id()),
			attribute.String("connector.operation", "delete"),
			attribute.String("connector.partition_key", partitionKey),
			attribute.String("connector.range_key", rangeKey),
			attribute.String("failsafe.match_method", fe.method),
		),
	)
	defer span.End()

	err := fe.RunVoid(ctx, func(ctx context.Context) error {
		return f.wrapped.Delete(ctx, partitionKey, rangeKey)
	})
	if err != nil {
		common.SetTraceSpanError(span, err)
		span.SetAttributes(attribute.String("error.summary", common.ErrorSummary(err)))
		return err
	}
	return nil
}

func (f *FailsafeConnector) List(ctx context.Context, index string, limit int, paginationToken string) ([]KeyValuePair, string, error) {
	return f.wrapped.List(ctx, index, limit, paginationToken)
}

func (f *FailsafeConnector) Lock(ctx context.Context, key string, ttl time.Duration) (DistributedLock, error) {
	return f.wrapped.Lock(ctx, key, ttl)
}

func (f *FailsafeConnector) WatchCounterInt64(ctx context.Context, key string) (<-chan CounterInt64State, func(), error) {
	return f.wrapped.WatchCounterInt64(ctx, key)
}

func (f *FailsafeConnector) PublishCounterInt64(ctx context.Context, key string, value CounterInt64State) error {
	return f.wrapped.PublishCounterInt64(ctx, key, value)
}
