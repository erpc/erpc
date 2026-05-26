package clients

import (
	"context"
	"fmt"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

// Hard-coded resilience constants. Kept inline (not config-driven) until
// there's a real need for per-upstream tuning — flagging this surface as
// config would invite drift / mis-tuning.
const (
	// bdsHardCallTimeout bounds the worst case for a single SendRequest.
	// Big enough for the slowest legitimate eth_getLogs queries observed
	// at the p99 (low single-digit seconds) with 2-3x headroom; small
	// enough that a wedged stream doesn't pile up callers.
	bdsHardCallTimeout = 20 * time.Second

	// bdsPoolSize is the number of independent grpc.ClientConn instances
	// kept per upstream. Round-robin across them so a single wedged conn
	// only chokes ~1/N of in-flight callers.
	bdsPoolSize = 3

	// bdsStuckCallThreshold / bdsStuckCallWindow drive the per-conn
	// watchdog. K bounded-wait timeouts within W on the same conn ⇒
	// force-close that conn and let grpc-go lazily reconnect. Tuned to
	// react quickly without false-positives from occasional slow queries.
	bdsStuckCallThreshold = 3
	bdsStuckCallWindow    = 60 * time.Second

	// bdsReplacementDedupWindow stops two simultaneous threshold-breaches
	// from double-closing the same slot in quick succession.
	bdsReplacementDedupWindow = 5 * time.Second
)

// bdsConn wraps a single grpc.ClientConn with per-connection stuck-call
// tracking. The pool has N of these; when one wedges only its slot is
// replaced.
type bdsConn struct {
	conn        *grpc.ClientConn
	rpcClient   evm.RPCQueryServiceClient
	queryClient evm.QueryServiceClient

	stuckMu    sync.Mutex
	stuckTimes []time.Time
	closedAt   atomic.Int64
}

// bdsPool is the round-robin connection pool + stuck-call watchdog for
// one BDS client.
type bdsPool struct {
	target        string
	creds         credentials.TransportCredentials
	serviceConfig string

	conns  []*bdsConn
	cursor atomic.Uint64
	poolMu sync.Mutex // serializes conn replacement

	projectId  string
	upstreamId string
	logger     *zerolog.Logger
}

func newBdsPool(
	logger *zerolog.Logger,
	projectId, upstreamId, target string,
	creds credentials.TransportCredentials,
	serviceConfig string,
) (*bdsPool, error) {
	p := &bdsPool{
		target:        target,
		creds:         creds,
		serviceConfig: serviceConfig,
		conns:         make([]*bdsConn, bdsPoolSize),
		projectId:     projectId,
		upstreamId:    upstreamId,
		logger:        logger,
	}
	for i := 0; i < bdsPoolSize; i++ {
		c, err := p.dial()
		if err != nil {
			for _, prev := range p.conns[:i] {
				if prev != nil && prev.conn != nil {
					_ = prev.conn.Close()
				}
			}
			return nil, err
		}
		p.conns[i] = c
	}
	return p, nil
}

func (p *bdsPool) dial() (*bdsConn, error) {
	conn, err := grpc.NewClient(p.target,
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithTransportCredentials(p.creds),
		grpc.WithChainUnaryInterceptor(grpcResponseMetadataInterceptor()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(100*1024*1024),
			grpc.MaxCallSendMsgSize(100*1024*1024),
		),
		grpc.WithDefaultServiceConfig(p.serviceConfig),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             5 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.WithConnectParams(grpc.ConnectParams{
			MinConnectTimeout: 3 * time.Second,
			Backoff: backoff.Config{
				BaseDelay:  100 * time.Millisecond,
				Multiplier: 1.5,
				Jitter:     0.2,
				MaxDelay:   1 * time.Second,
			},
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to dial gRPC server at %s: %w", p.target, err)
	}
	return &bdsConn{
		conn:        conn,
		rpcClient:   evm.NewRPCQueryServiceClient(conn),
		queryClient: evm.NewQueryServiceClient(conn),
	}, nil
}

// Pick returns the next pool slot in round-robin order.
func (p *bdsPool) Pick() *bdsConn {
	if len(p.conns) == 0 {
		return nil
	}
	i := int(p.cursor.Add(1)-1) % len(p.conns)
	return p.conns[i]
}

// OnBoundedTimeout records a bounded-wait timeout on c and force-closes
// the connection if the rolling-window threshold is exceeded. Closing
// the conn wakes ALL leaked goroutines blocked in Recv/Send on it —
// that's the only portable way to free them after callBounded abandoned
// the call.
func (p *bdsPool) OnBoundedTimeout(c *bdsConn, method string) {
	telemetry.MetricGrpcBdsHardTimeoutTotal.WithLabelValues(p.projectId, p.upstreamId, method).Inc()
	if p.recordStuck(c) {
		p.replaceConn(c)
	}
}

// recordStuck appends a timestamp to the conn's rolling window and
// returns true if the count is now at/over the configured threshold.
func (p *bdsPool) recordStuck(c *bdsConn) bool {
	now := time.Now()
	cutoff := now.Add(-bdsStuckCallWindow)

	c.stuckMu.Lock()
	defer c.stuckMu.Unlock()
	trimmed := c.stuckTimes[:0]
	for _, t := range c.stuckTimes {
		if t.After(cutoff) {
			trimmed = append(trimmed, t)
		}
	}
	trimmed = append(trimmed, now)
	c.stuckTimes = trimmed
	return len(c.stuckTimes) >= bdsStuckCallThreshold
}

// replaceConn force-closes c and dials a new conn into c's slot.
// Skipped if the slot was replaced within bdsReplacementDedupWindow.
func (p *bdsPool) replaceConn(c *bdsConn) {
	p.poolMu.Lock()
	defer p.poolMu.Unlock()

	slot := -1
	for i, existing := range p.conns {
		if existing == c {
			slot = i
			break
		}
	}
	if slot < 0 {
		return
	}
	if last := c.closedAt.Load(); last > 0 && time.Since(time.Unix(0, last)) < bdsReplacementDedupWindow {
		return
	}
	c.closedAt.Store(time.Now().UnixNano())

	p.logger.Warn().
		Str("target", p.target).
		Str("upstream.id", p.upstreamId).
		Int("slot", slot).
		Msg("BDS watchdog: force-closing wedged connection")
	telemetry.MetricGrpcBdsConnReplacementsTotal.WithLabelValues(p.projectId, p.upstreamId).Inc()

	if c.conn != nil {
		_ = c.conn.Close()
	}
	replacement, err := p.dial()
	if err != nil {
		p.logger.Error().Err(err).Str("target", p.target).Msg("BDS watchdog: failed to dial replacement; slot left closed")
		return
	}
	p.conns[slot] = replacement
}

// Shutdown closes every connection in the pool. Idempotent.
func (p *bdsPool) Shutdown() {
	p.poolMu.Lock()
	defer p.poolMu.Unlock()
	for _, c := range p.conns {
		if c != nil && c.conn != nil {
			_ = c.conn.Close()
		}
	}
}

// callBounded runs fn in a goroutine and waits up to ctx.Done() for it
// to complete. If ctx fires first, returns ctx.Err() and lets the
// goroutine "leak" — grpc-go cleans it up when (a) it eventually honors
// the context, or (b) the watchdog closes the conn.
//
// Without this, grpc-go's clientStream.RecvMsg may not wake on context
// cancel when the H2 stream is wedged at the transport layer (TCP-alive
// but stream-stuck). The select guarantees the CALLER returns within
// ctx's deadline regardless of grpc-go's internal state.
func callBounded(ctx context.Context, fn func(context.Context) error) error {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				done <- fmt.Errorf("BDS goroutine panic: %v\n%s", rec, debug.Stack())
			}
		}()
		done <- fn(ctx)
	}()
	select {
	case err := <-done:
		// If ctx fired simultaneously, the gRPC layer may have race-won
		// with a Canceled status — but the proximate cause is still our
		// deadline. Surface that so the caller treats it as a timeout.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// callBoundedT is the typed wrapper for unary gRPC calls. Sends the
// concrete return through a channel so the caller and the (possibly
// abandoned) inner goroutine never race on a shared variable.
func callBoundedT[T any](ctx context.Context, fn func(context.Context) (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				done <- result{err: fmt.Errorf("BDS goroutine panic: %v\n%s", rec, debug.Stack())}
			}
		}()
		v, err := fn(ctx)
		done <- result{v: v, err: err}
	}()
	select {
	case r := <-done:
		if ctx.Err() != nil {
			var zero T
			return zero, ctx.Err()
		}
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// pickTargetForBDS extracts the host:port + TLS choice from an upstream URL.
func pickTargetForBDS(parsedUrl *url.URL) (target string, useTLS bool) {
	target = parsedUrl.Host
	if parsedUrl.Port() == "" {
		target = fmt.Sprintf("%s:50051", parsedUrl.Hostname())
	}
	target = fmt.Sprintf("dns:///%s", target)

	if portNum, err := strconv.Atoi(parsedUrl.Port()); err == nil && portNum == 443 {
		useTLS = true
	} else if strings.HasPrefix(parsedUrl.Scheme, "grpcs") || strings.Contains(parsedUrl.Scheme, "tls") {
		useTLS = true
	}
	return target, useTLS
}
