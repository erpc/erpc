package clients

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

// Hard-coded resilience tunables. Kept inline (not config-driven) until
// there's a real need for per-upstream tuning — flagging this surface
// as config would invite drift / mis-tuning. Declared as var (not
// const) so tests can override without restructuring; production code
// MUST NOT mutate these at runtime.
var (
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

// errBdsHardCapExceeded is the context cause set by SendRequest's
// bdsHardCallTimeout. It MUST be distinct from
// common.ErrDynamicTimeoutExceeded: the upstream/network failsafe executors
// set that shared sentinel as the cause on CALLER contexts
// (context.WithTimeoutCause in upstream_executor.go / network_executor.go),
// and util.BoundedCall surfaces context.Cause on expiry — so matching the
// shared sentinel made every routine failsafe-timeout expiry (e.g. a
// quantile policy with a 200ms floor) look like our hard cap. That fed the
// watchdog, replaced healthy conns, and the fresh-dial latency then blew
// the next calls' budgets too — a self-sustaining churn/warn storm.
var errBdsHardCapExceeded = errors.New("bds hard call timeout exceeded")

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

	// poolMu protects every read/write of p.conns. Pick takes RLock so
	// the hot path stays cheap; replaceConn and Shutdown take Lock when
	// mutating slot pointers. Without this, Pick could race the slot
	// swap in replaceConn — even though pointer writes are atomic on
	// 64-bit, Go's race detector flags it and a future slice resize
	// could turn it into a real bug.
	poolMu sync.RWMutex
	conns  []*bdsConn
	cursor atomic.Uint64

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
	p.poolMu.RLock()
	defer p.poolMu.RUnlock()
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

// replaceConn dials a new conn and atomically swaps it into c's slot,
// then closes the old one. Dialing FIRST means a transient dial
// failure (e.g. DNS hiccup) leaves the existing conn in place rather
// than parking the slot with a permanently-closed *grpc.ClientConn.
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

	// Dial first. If dial fails, leave the old conn in place — it's
	// likely still broken but at least grpc-go can keep retrying
	// through it (with its own reconnect backoff), which is strictly
	// better than parking the slot with a closed conn that's already
	// suppressed from re-replacement by the closedAt dedup.
	replacement, err := p.dial()
	if err != nil {
		p.logger.Error().Err(err).Str("target", p.target).Msg("BDS watchdog: failed to dial replacement; old conn left in place for grpc-go to reconnect")
		return
	}

	// Dial succeeded — commit the swap and close the old conn.
	c.closedAt.Store(time.Now().UnixNano())
	p.logger.Warn().
		Str("target", p.target).
		Str("upstream.id", p.upstreamId).
		Int("slot", slot).
		Msg("BDS watchdog: replacing wedged connection")
	telemetry.MetricGrpcBdsConnReplacementsTotal.WithLabelValues(p.projectId, p.upstreamId).Inc()

	p.conns[slot] = replacement
	if c.conn != nil {
		_ = c.conn.Close()
	}
}

// Shutdown closes every connection in the pool. Idempotent.
// Takes the write lock to serialize with any in-flight replaceConn
// (so we don't close a conn that's just been swapped out).
func (p *bdsPool) Shutdown() {
	p.poolMu.Lock()
	defer p.poolMu.Unlock()
	for _, c := range p.conns {
		if c != nil && c.conn != nil {
			_ = c.conn.Close()
		}
	}
}

// callBounded / callBoundedT are package-local aliases for the shared
// helpers in util/ — kept so the BDS resilience code (and its tests)
// don't need to rename every call site after the helpers moved.
//
// The pattern itself is documented on util.BoundedCall.

func callBounded(ctx context.Context, fn func(context.Context) error) error {
	return util.BoundedCall(ctx, fn)
}

func callBoundedT[T any](ctx context.Context, fn func(context.Context) (T, error)) (T, error) {
	return util.BoundedCallT(ctx, fn)
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
