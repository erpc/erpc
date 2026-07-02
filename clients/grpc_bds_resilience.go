package clients

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
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
	"google.golang.org/grpc/connectivity"
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

	// bdsPoolSize is the DEFAULT number of independent grpc.ClientConn
	// instances kept per upstream, used when the connector/upstream config
	// leaves poolSize unset. Round-robin across them so a single wedged conn
	// only chokes ~1/N of in-flight callers. Override per connector/upstream
	// via the `poolSize` config knob (see newBdsPool / GrpcConnectorConfig).
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

	// bdsConnMaxAge bounds how long a pooled connection may live before the
	// maintainer proactively re-dials it (each conn gets ±20% jitter so the
	// pool never recycles in lockstep). Re-dialing re-resolves DNS, so a
	// connection pinned to an address that no longer backs the record —
	// backend gone, or the address reused by ANOTHER chain's server — cannot
	// outlive this bound. Correctness does not depend on it (per-request
	// chainId assertions and the identity verification below catch
	// cross-wires directly); this is the freshness / load-balancing hygiene
	// layer.
	bdsConnMaxAge = 5 * time.Minute

	// bdsMaintainInterval is how often the pool maintainer wakes to verify
	// the chain identity behind each connection and enforce bdsConnMaxAge.
	bdsMaintainInterval = 60 * time.Second

	// bdsVerifyTimeout bounds a single ChainId verification probe.
	bdsVerifyTimeout = 5 * time.Second

	// bdsAgeRecycleLinger delays closing an age-recycled conn so its in-flight
	// RPCs can finish; new picks already go to the replacement. Mismatch
	// recycles close immediately — a wrong-chain conn must not serve one more
	// call than necessary.
	bdsAgeRecycleLinger = 30 * time.Second
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

	// dialedAt/maxAge drive the maintainer's age-based recycling; maxAge is
	// per-conn jittered at dial time (0 = age recycling disabled).
	dialedAt time.Time
	maxAge   time.Duration

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

	// appCtx bounds the maintainer goroutine and verification probes;
	// expectedChainId (0 = unknown) is what those probes assert against —
	// atomic because the cache connector arms it after probing the server.
	appCtx          context.Context
	expectedChainId atomic.Uint64
	stopCh          chan struct{}
	stopOnce        sync.Once
}

// newBdsPool builds a round-robin pool of poolSize independent connections.
// A poolSize <= 0 means "unconfigured" and falls back to bdsPoolSize, so an
// absent/zero/negative config value preserves the historical default.
// expectedChainId (0 = unknown) arms chain-identity verification: a proven
// mismatch at construction fails the pool, and a background maintainer keeps
// re-verifying every connection and recycles those past their max age.
func newBdsPool(
	appCtx context.Context,
	logger *zerolog.Logger,
	projectId, upstreamId, target string,
	creds credentials.TransportCredentials,
	serviceConfig string,
	poolSize int,
	expectedChainId uint64,
) (*bdsPool, error) {
	if poolSize <= 0 {
		poolSize = bdsPoolSize
	}
	if appCtx == nil {
		appCtx = context.Background()
	}
	p := &bdsPool{
		target:        target,
		creds:         creds,
		serviceConfig: serviceConfig,
		conns:         make([]*bdsConn, poolSize),
		projectId:     projectId,
		upstreamId:    upstreamId,
		logger:        logger,
		appCtx:        appCtx,
		stopCh:        make(chan struct{}),
	}
	p.expectedChainId.Store(expectedChainId)
	for i := 0; i < poolSize; i++ {
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
	// Chain-identity gate at construction: probe one fresh conn (round_robin
	// spreads subsequent traffic over the same resolved backends). A PROVEN
	// mismatch fails the pool — never bootstrap onto a cross-wired endpoint.
	// Transient probe errors keep the historical lazy-dial behavior; the
	// maintainer re-checks every tick and every request carries its own
	// chainId assertion regardless.
	if expectedChainId > 0 {
		if ok, detected, err := p.verifyConn(appCtx, p.conns[0]); err == nil && !ok {
			p.Shutdown()
			return nil, fmt.Errorf("BDS endpoint %s answers for chainId %d but this client expects chainId %d (cross-wired endpoint)", target, detected, expectedChainId)
		}
	}
	go p.maintainLoop()
	return p, nil
}

// verifyConn probes the server behind c with the ChainId RPC and compares the
// answer to the pool's expected chainId. Returns (matched, detected, err);
// err != nil means the identity could not be determined (transport failure) —
// matched is meaningless in that case.
func (p *bdsPool) verifyConn(ctx context.Context, c *bdsConn) (bool, uint64, error) {
	expected := p.expectedChainId.Load()
	if expected == 0 || c == nil || c.rpcClient == nil {
		return true, 0, nil
	}
	vctx, cancel := context.WithTimeout(ctx, bdsVerifyTimeout)
	defer cancel()
	resp, err := c.rpcClient.ChainId(vctx, &evm.ChainIdRequest{})
	if err != nil {
		return false, 0, err
	}
	if resp.ChainId != expected {
		return false, resp.ChainId, nil
	}
	return true, resp.ChainId, nil
}

// maintainLoop periodically re-verifies the chain identity behind every
// pooled connection and recycles connections past their (jittered) max age.
// Identity mismatches recycle immediately; age recycles are capped at one per
// tick so the pool never churns in bulk. Exits on Shutdown / app shutdown.
func (p *bdsPool) maintainLoop() {
	ticker := time.NewTicker(bdsMaintainInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-p.appCtx.Done():
			return
		case <-ticker.C:
		}

		p.poolMu.RLock()
		conns := append([]*bdsConn(nil), p.conns...)
		p.poolMu.RUnlock()

		agedRecycled := false
		for _, c := range conns {
			if c == nil {
				continue
			}
			// A conn quarantined by a previous mismatch (closed in place
			// because the replacement dial failed) errors on verification;
			// retry replacing it every tick until a good conn lands.
			if c.conn != nil && c.conn.GetState() == connectivity.Shutdown {
				p.recycleConn(c, "closed")
				continue
			}
			if ok, detected, err := p.verifyConn(p.appCtx, c); err == nil && !ok {
				p.logger.Error().
					Str("target", p.target).
					Str("upstream.id", p.upstreamId).
					Uint64("expectedChainId", p.expectedChainId.Load()).
					Uint64("detectedChainId", detected).
					Msg("BDS maintainer: connection answers for a DIFFERENT chain (cross-wired endpoint) — quarantining immediately")
				// Quarantine FIRST: closing the conn fails every in-flight and
				// future RPC on it instantly (surfacing through the normal
				// request-error paths), so nothing more can be served by the
				// wrong chain even by a server that doesn't validate chainId.
				// The replacement below then restores capacity.
				_ = c.conn.Close()
				p.recycleConn(c, "chainid_mismatch")
				continue
			}
			if !agedRecycled && c.maxAge > 0 && time.Since(c.dialedAt) > c.maxAge {
				p.recycleConn(c, "age")
				agedRecycled = true
			}
		}
	}
}

// recycleConn dials a verified replacement for c and swaps it into c's slot.
// reason is `age`, `chainid_mismatch` or `closed`. On age recycles the old
// conn closes after a linger (lets in-flight RPCs finish; new picks already go
// to the replacement); on mismatch/closed the old conn is already closed (or
// closes immediately) — a proven wrong-chain conn must not serve another call.
func (p *bdsPool) recycleConn(c *bdsConn, reason string) {
	replacement, err := p.dial()
	if err != nil {
		p.logger.Warn().Err(err).Str("target", p.target).Str("reason", reason).Msg("BDS maintainer: failed to dial replacement; will retry next tick")
		return
	}
	if ok, detected, verr := p.verifyConn(p.appCtx, replacement); verr == nil && !ok {
		_ = replacement.conn.Close()
		p.logger.Error().
			Str("target", p.target).
			Str("upstream.id", p.upstreamId).
			Uint64("expectedChainId", p.expectedChainId.Load()).
			Uint64("detectedChainId", detected).
			Msg("BDS maintainer: replacement answers for a different chain (cross-wired endpoint); will retry next tick")
		return
	}
	slot, swapped := p.swapInReplacement(c, replacement)
	if !swapped {
		_ = replacement.conn.Close()
		return
	}
	p.logger.Info().
		Str("target", p.target).
		Str("upstream.id", p.upstreamId).
		Int("slot", slot).
		Str("reason", reason).
		Msg("BDS maintainer: recycled pool connection")
	if c.conn != nil {
		if reason == "age" {
			old := c.conn
			time.AfterFunc(bdsAgeRecycleLinger, func() { _ = old.Close() })
		} else {
			_ = c.conn.Close()
		}
	}
}

// swapInReplacement installs replacement in c's slot if c is still pooled and
// outside the dedup window. Returns (slot, false) without installing when the
// swap cannot be committed — the caller must close the replacement then.
func (p *bdsPool) swapInReplacement(c, replacement *bdsConn) (int, bool) {
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
		return -1, false
	}
	if last := c.closedAt.Load(); last > 0 && time.Since(time.Unix(0, last)) < bdsReplacementDedupWindow {
		return -1, false
	}
	c.closedAt.Store(time.Now().UnixNano())
	p.conns[slot] = replacement
	return slot, true
}

// Size returns the number of connection slots in the pool.
func (p *bdsPool) Size() int {
	p.poolMu.RLock()
	defer p.poolMu.RUnlock()
	return len(p.conns)
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
	maxAge := time.Duration(0)
	if bdsConnMaxAge > 0 {
		// ±20% jitter so pool conns never expire in lockstep.
		maxAge = time.Duration(float64(bdsConnMaxAge) * (0.8 + 0.4*rand.Float64())) // #nosec G404 -- recycle-spread jitter, not security-sensitive
	}
	return &bdsConn{
		conn:        conn,
		rpcClient:   evm.NewRPCQueryServiceClient(conn),
		queryClient: evm.NewQueryServiceClient(conn),
		dialedAt:    time.Now(),
		maxAge:      maxAge,
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
// Dial and identity verification happen OUTSIDE the pool lock — only the
// slot swap itself takes it — so Pick() is never blocked behind a probe.
func (p *bdsPool) replaceConn(c *bdsConn) {
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

	// Never install a replacement that PROVABLY serves another chain
	// (transient verification errors fall through — same lazy semantics as
	// the original dial; the maintainer re-checks periodically).
	if ok, detected, verr := p.verifyConn(p.appCtx, replacement); verr == nil && !ok {
		_ = replacement.conn.Close()
		p.logger.Error().
			Str("target", p.target).
			Str("upstream.id", p.upstreamId).
			Uint64("expectedChainId", p.expectedChainId.Load()).
			Uint64("detectedChainId", detected).
			Msg("BDS watchdog: replacement answers for a different chain (cross-wired endpoint); keeping old conn")
		return
	}

	slot, swapped := p.swapInReplacement(c, replacement)
	if !swapped {
		_ = replacement.conn.Close()
		return
	}

	p.logger.Warn().
		Str("target", p.target).
		Str("upstream.id", p.upstreamId).
		Int("slot", slot).
		Msg("BDS watchdog: replacing wedged connection")
	telemetry.MetricGrpcBdsConnReplacementsTotal.WithLabelValues(p.projectId, p.upstreamId).Inc()

	if c.conn != nil {
		_ = c.conn.Close()
	}
}

// Shutdown stops the maintainer and closes every connection in the pool.
// Idempotent. Takes the write lock to serialize with any in-flight
// replaceConn (so we don't close a conn that's just been swapped out).
func (p *bdsPool) Shutdown() {
	p.stopOnce.Do(func() { close(p.stopCh) })
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
