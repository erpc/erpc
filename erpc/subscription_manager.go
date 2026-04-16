package erpc

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/adapters/wsclient"
	"github.com/erpc/erpc/indexer/adapters/wsupstream"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
)

// JSON-RPC subscription methods. Kept in erpc/ rather than the indexer
// because they are JSON-RPC-specific — a Kafka or gRPC egress deals in
// SubType only, never in RPC method strings.
const (
	MethodEthSubscribe   = "eth_subscribe"
	MethodEthUnsubscribe = "eth_unsubscribe"
)

// Subscription type aliases for convenient use in the erpc package.
// The canonical definitions live in the indexer package.
const (
	SubTypeNewHeads               = indexer.SubTypeNewHeads
	SubTypeLogs                   = indexer.SubTypeLogs
	SubTypeNewPendingTransactions = indexer.SubTypeNewPendingTransactions
)

const (
	// unsubscribeTimeout is the deadline for best-effort upstream
	// unsubscribe calls during connection cleanup.
	unsubscribeTimeout = 5 * time.Second
)

// SubscriptionManager is the client-facing egress layer. It owns
// per-connection *wsclient.Adapter instances, lazily registers networks +
// ingresses with the indexer the first time a client subscribes on a
// given network, and translates the public eth_subscribe / eth_unsubscribe
// surface into indexer calls.
type SubscriptionManager struct {
	logger *zerolog.Logger
	idx    *indexer.Indexer

	// conns maps connId -> *connEntry. One egress adapter per live WS
	// connection.
	conns sync.Map

	// bySubID maps a client-facing subscription ID to the record needed
	// to route Unsubscribe/Cleanup without walking every connection.
	bySubID sync.Map // clientSubId -> *subRecord

	// networks tracks which networkIds have been bootstrapped with
	// ingresses so we don't double-register on every Subscribe call.
	networks sync.Map // networkId -> struct{}

	// bootstrapMu serialises bootstrapNetwork; the indexer's
	// RegisterNetwork is idempotent but ingress creation (WS connects on
	// upstreams) is not, so we avoid duplicate adapters.
	bootstrapMu sync.Mutex
}

// connEntry is the per-connection bookkeeping: the egress adapter and
// the indexer detach handle.
type connEntry struct {
	adapter *wsclient.Adapter
	detach  func()
}

// subRecord is the per-subscription record kept for Unsubscribe /
// CleanupConnection routing. We don't persist these in the adapter
// because the adapter can't know the original subType (it holds
// EventKind, which is a lossy projection of subType for filters).
type subRecord struct {
	clientSubID string
	connID      string
	networkID   string
	subType     string
	kind        indexer.EventKind
	filterHash  string
}

// NewSubscriptionManager creates a client-facing SubscriptionManager
// backed by the given indexer.
func NewSubscriptionManager(logger *zerolog.Logger, idx *indexer.Indexer) *SubscriptionManager {
	return &SubscriptionManager{
		logger: logger,
		idx:    idx,
	}
}

// IsSubscriptionMethod returns true when the JSON-RPC method targets the
// subscription surface (eth_subscribe or eth_unsubscribe).
func IsSubscriptionMethod(method string) bool {
	return method == MethodEthSubscribe || method == MethodEthUnsubscribe
}

// IsSubscribeMethod returns true when the method is eth_subscribe.
func IsSubscribeMethod(method string) bool {
	return method == MethodEthSubscribe
}

// Subscribe handles an eth_subscribe request from a client WS connection.
// Generates a client-facing subscription ID, ensures the corresponding
// upstream subscription exists (via the indexer's EnsureFilter), and
// registers the client with the connection's egress adapter.
func (sm *SubscriptionManager) Subscribe(
	ctx context.Context,
	wsc *WsConnection,
	nq *common.NormalizedRequest,
	project *PreparedProject,
	networkId string,
) (*common.NormalizedResponse, error) {
	start := time.Now()
	method := MethodEthSubscribe
	lg := sm.logger.With().Str("connId", wsc.id).Str("networkId", networkId).Logger()

	nw, err := project.GetNetwork(ctx, networkId)
	if err != nil {
		return nil, err
	}
	nq.SetNetwork(nw)

	// Bootstrap the network if this is the first touch. Returns
	// ErrNoWsUpstreamAvailable if the network has no WS-capable
	// upstreams configured.
	if err := sm.bootstrapNetwork(ctx, nw); err != nil {
		return nil, err
	}

	// Per-connection subscription limit. Get/create the egress adapter
	// up-front — we need it for the limit check and for the subsequent
	// AddSubscription call anyway.
	conn := sm.getOrCreateConn(wsc)
	maxSubs := wsc.server.serverCfg.WebSocket.MaxSubscriptionsPerConnection
	if conn.adapter.Count() >= maxSubs {
		return nil, common.NewErrSubscriptionLimitExceeded(maxSubs)
	}

	if err := sm.acquireRateLimits(ctx, project, nw, nq); err != nil {
		return nil, err
	}

	reqFinality := nq.Finality(ctx)
	telemetry.CounterHandle(telemetry.MetricNetworkRequestsReceived,
		project.Config.Id, nw.Label(), method,
		reqFinality.String(), nq.UserId(), nq.AgentName(),
	).Inc()

	jrReq, err := nq.JsonRpcRequest()
	if err != nil {
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}

	subType := indexer.ExtractSubscriptionType(jrReq.Params)
	clientSubID, err := indexer.GenerateClientSubID()
	if err != nil {
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, fmt.Errorf("failed to generate subscription ID: %w", err))
		return nil, fmt.Errorf("failed to generate subscription ID: %w", err)
	}

	kind, filterHash, err := sm.resolveSubscription(ctx, networkId, subType, jrReq.Params)
	if err != nil {
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}

	conn.adapter.AddSubscription(clientSubID, networkId, kind, filterHash)
	sm.bySubID.Store(clientSubID, &subRecord{
		clientSubID: clientSubID,
		connID:      wsc.id,
		networkID:   networkId,
		subType:     subType,
		kind:        kind,
		filterHash:  filterHash,
	})

	lg.Info().
		Str("clientSubId", clientSubID).
		Str("subType", subType).
		Msg("subscription established")

	telemetry.CounterHandle(telemetry.MetricNetworkSuccessfulRequests,
		project.Config.Id, nw.Label(), "proxy", "proxy",
		method, "1", reqFinality.String(), "false", nq.UserId(), nq.AgentName(),
	).Inc()
	telemetry.ObserverHandle(telemetry.MetricNetworkRequestDuration,
		project.Config.Id, nw.Label(), "proxy", "proxy",
		method, reqFinality.String(), nq.UserId(),
	).Observe(time.Since(start).Seconds())

	return sm.buildSubscribeResponse(nq, jrReq, clientSubID), nil
}

// Unsubscribe handles an eth_unsubscribe request.
func (sm *SubscriptionManager) Unsubscribe(
	ctx context.Context,
	wsc *WsConnection,
	nq *common.NormalizedRequest,
	project *PreparedProject,
	networkId string,
) (*common.NormalizedResponse, error) {
	start := time.Now()
	method := MethodEthUnsubscribe
	lg := sm.logger.With().Str("connId", wsc.id).Str("networkId", networkId).Logger()

	nw, err := project.GetNetwork(ctx, networkId)
	if err != nil {
		return nil, err
	}
	nq.SetNetwork(nw)

	if err := sm.acquireRateLimits(ctx, project, nw, nq); err != nil {
		return nil, err
	}

	reqFinality := nq.Finality(ctx)
	telemetry.CounterHandle(telemetry.MetricNetworkRequestsReceived,
		project.Config.Id, nw.Label(), method,
		reqFinality.String(), nq.UserId(), nq.AgentName(),
	).Inc()

	jrReq, err := nq.JsonRpcRequest()
	if err != nil {
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}

	clientSubID, err := indexer.ExtractClientSubID(jrReq.Params)
	if err != nil {
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}

	recRaw, ok := sm.bySubID.LoadAndDelete(clientSubID)
	if !ok {
		err := common.NewErrSubscriptionNotFound(clientSubID)
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}
	rec := recRaw.(*subRecord)

	if connRaw, ok := sm.conns.Load(rec.connID); ok {
		connRaw.(*connEntry).adapter.RemoveSubscription(clientSubID)
	}
	if rec.kind != indexer.KindNewHead && rec.filterHash != "" {
		sm.idx.ReleaseFilter(ctx, rec.networkID, rec.subType, rec.filterHash)
	}

	lg.Info().Str("clientSubId", clientSubID).Str("subType", rec.subType).Msg("subscription removed")

	telemetry.CounterHandle(telemetry.MetricNetworkSuccessfulRequests,
		project.Config.Id, nw.Label(), "proxy", "proxy",
		method, "1", reqFinality.String(), "false", nq.UserId(), nq.AgentName(),
	).Inc()
	telemetry.ObserverHandle(telemetry.MetricNetworkRequestDuration,
		project.Config.Id, nw.Label(), "proxy", "proxy",
		method, reqFinality.String(), nq.UserId(),
	).Observe(time.Since(start).Seconds())

	resp := common.NewNormalizedResponse().WithRequest(nq)
	jrr := &common.JsonRpcResponse{}
	_ = jrr.SetID(jrReq.ID)
	jrr.SetResult([]byte("true"))
	resp.WithJsonRpcResponse(jrr)
	return resp, nil
}

// CleanupConnection is invoked on WS disconnect. It walks the adapter's
// active subscriptions, releases each filter refcount in the indexer,
// drains the adapter, and detaches it from the indexer's egress set.
func (sm *SubscriptionManager) CleanupConnection(wsc *WsConnection, _ *PreparedProject) {
	lg := sm.logger.With().Str("connId", wsc.id).Logger()

	connRaw, ok := sm.conns.LoadAndDelete(wsc.id)
	if !ok {
		return
	}
	conn := connRaw.(*connEntry)

	for _, sub := range conn.adapter.Subscriptions() {
		sm.bySubID.Delete(sub.ClientSubID)
		if sub.Kind != indexer.KindNewHead && sub.FilterHash != "" {
			ctx, cancel := context.WithTimeout(context.Background(), unsubscribeTimeout)
			sm.idx.ReleaseFilter(ctx, sub.NetworkID, subTypeFor(sub.Kind), sub.FilterHash)
			cancel()
		}
	}
	conn.adapter.Drain()
	conn.detach()

	lg.Debug().Msg("cleaned up all subscriptions for connection")
}

// --- internals --------------------------------------------------------

// buildWsAdapterOptions resolves network-level toggles that the wsupstream
// adapter needs into a flat Options struct. Returns nil when no override
// applies so the adapter keeps its defaults.
func buildWsAdapterOptions(cfg *common.NetworkConfig) *wsupstream.Options {
	if cfg == nil || cfg.Evm == nil {
		return nil
	}
	opts := &wsupstream.Options{}
	set := false
	if cfg.Evm.StripSubscribeFromBlockZero != nil && *cfg.Evm.StripSubscribeFromBlockZero {
		opts.StripSubscribeFromBlockZero = true
		set = true
	}
	if !set {
		return nil
	}
	return opts
}

// bootstrapNetwork registers the network with the indexer and attaches a
// wsupstream.Adapter for each WS upstream on the network. Idempotent per
// networkId — subsequent calls are no-ops.
func (sm *SubscriptionManager) bootstrapNetwork(ctx context.Context, nw *Network) error {
	networkID := nw.networkId
	if _, ok := sm.networks.Load(networkID); ok {
		return nil
	}
	sm.bootstrapMu.Lock()
	defer sm.bootstrapMu.Unlock()
	if _, ok := sm.networks.Load(networkID); ok {
		return nil
	}

	wsUpstreams := nw.upstreamsRegistry.GetWsUpstreams(ctx, networkID)
	if len(wsUpstreams) == 0 {
		return common.NewErrNoWsUpstreamAvailable(networkID)
	}

	sm.idx.RegisterNetwork(&networkHandle{nw: nw})
	adapterOpts := buildWsAdapterOptions(nw.cfg)
	for _, up := range wsUpstreams {
		adapter := wsupstream.New(up, networkID, sm.logger, adapterOpts)
		if adapter == nil {
			continue
		}
		if err := sm.idx.AddIngress(ctx, networkID, adapter); err != nil {
			sm.logger.Warn().Err(err).Str("upstreamId", up.Id()).
				Msg("failed to register upstream ingress with indexer")
		}
	}
	sm.networks.Store(networkID, struct{}{})
	return nil
}

// resolveSubscription validates the subType and, for filter subs,
// translates params into a filterHash via the indexer. Returns
// (kind, filterHash) for subsequent AddSubscription.
func (sm *SubscriptionManager) resolveSubscription(ctx context.Context, networkID, subType string, params []interface{}) (indexer.EventKind, string, error) {
	switch subType {
	case indexer.SubTypeNewHeads:
		return indexer.KindNewHead, "", nil
	case indexer.SubTypeLogs, indexer.SubTypeNewPendingTransactions:
		hash, err := sm.idx.EnsureFilter(ctx, networkID, subType, params)
		if err != nil {
			return 0, "", err
		}
		kind := indexer.KindLog
		if subType == indexer.SubTypeNewPendingTransactions {
			kind = indexer.KindPendingTx
		}
		return kind, hash, nil
	default:
		return 0, "", fmt.Errorf("unsupported subscription type: %s", subType)
	}
}

// getOrCreateConn returns the egress adapter for a WsConnection,
// attaching a new one to the indexer if this is the first subscription
// on that connection.
func (sm *SubscriptionManager) getOrCreateConn(wsc *WsConnection) *connEntry {
	if existing, ok := sm.conns.Load(wsc.id); ok {
		return existing.(*connEntry)
	}
	adapter := wsclient.New(wsc.id, wsc, sm.logger)
	detach := sm.idx.Attach(adapter)
	entry := &connEntry{adapter: adapter, detach: detach}
	if existing, loaded := sm.conns.LoadOrStore(wsc.id, entry); loaded {
		// Lost the race with another Subscribe — drop our adapter.
		detach()
		adapter.Drain()
		return existing.(*connEntry)
	}
	return entry
}

// acquireRateLimits acquires rate-limit permits at both project and
// network level.
func (sm *SubscriptionManager) acquireRateLimits(
	ctx context.Context,
	project *PreparedProject,
	nw *Network,
	nq *common.NormalizedRequest,
) error {
	if err := project.AcquireRateLimitPermit(ctx, nq); err != nil {
		return err
	}
	return nw.acquireRateLimitPermit(ctx, nq)
}

// buildSubscribeResponse constructs the JSON-RPC response carrying the
// client-facing subscription ID.
func (sm *SubscriptionManager) buildSubscribeResponse(
	nq *common.NormalizedRequest,
	jrReq *common.JsonRpcRequest,
	clientSubID string,
) *common.NormalizedResponse {
	resp := common.NewNormalizedResponse().WithRequest(nq)
	jrr := &common.JsonRpcResponse{}
	_ = jrr.SetID(jrReq.ID)
	jrr.SetResult([]byte(fmt.Sprintf(`"%s"`, clientSubID)))
	resp.WithJsonRpcResponse(jrr)
	return resp
}

// recordFailureMetrics emits the failure-path metrics when a subscribe
// or unsubscribe request fails before reaching the indexer.
func (sm *SubscriptionManager) recordFailureMetrics(
	project *PreparedProject,
	nw *Network,
	method string,
	finality common.DataFinalityState,
	start time.Time,
	nq *common.NormalizedRequest,
	err error,
) {
	telemetry.CounterHandle(telemetry.MetricNetworkFailedRequests,
		project.Config.Id, nw.Label(), method,
		"0", // no upstream attempts for client-facing subscription failures
		common.ErrorFingerprint(err),
		string(common.ClassifySeverity(err)),
		finality.String(),
		nq.UserId(),
		nq.AgentName(),
	).Inc()
	telemetry.ObserverHandle(telemetry.MetricNetworkRequestDuration,
		project.Config.Id, nw.Label(), "<error>", "<error>",
		method, finality.String(), nq.UserId(),
	).Observe(time.Since(start).Seconds())
}

// subTypeFor is the inverse of the kind-to-subType mapping done in
// resolveSubscription. Used by CleanupConnection where we've only got
// the adapter's Kind in hand.
func subTypeFor(kind indexer.EventKind) string {
	switch kind {
	case indexer.KindLog:
		return indexer.SubTypeLogs
	case indexer.KindPendingTx:
		return indexer.SubTypeNewPendingTransactions
	}
	return ""
}

// internalReqIdCounter generates unique JSON-RPC IDs for internal
// requests (subscribe/unsubscribe). Kept here so erpc-level callers
// still have access to a uniform counter; the wsupstream adapter
// maintains its own counter since it can't import erpc.
var internalReqIdCounter atomic.Int64

// internalReqIdOffset keeps internal IDs out of the range clients
// typically use (small incrementing integers).
const internalReqIdOffset = 900_000_000

// buildJsonRpcBody marshals a JSON-RPC request with the given method and
// params. Retained for non-subscription JSON-RPC send paths that still
// live inside erpc/.
func buildJsonRpcBody(method string, params interface{}) ([]byte, error) {
	id := internalReqIdCounter.Add(1) + internalReqIdOffset
	return common.SonicCfg.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
}

// --- NetworkHandle ----------------------------------------------------

// networkHandle adapts *Network to indexer.NetworkHandle. Lives here
// (rather than in indexer/) because it touches Network internals; the
// indexer package deliberately doesn't know about erpc.
type networkHandle struct {
	nw *Network
}

func (h *networkHandle) Id() string { return h.nw.networkId }

func (h *networkHandle) FinalityDepth() int64 {
	if h.nw.cfg != nil && h.nw.cfg.Evm != nil {
		return h.nw.cfg.Evm.FallbackFinalityDepth
	}
	return 0
}

// SuggestLatestBlock routes a per-source block observation to the
// upstream's state poller. sourceId is the ingress adapter's Name(),
// which for wsupstream.Adapter is "ws:<upstreamId>".
func (h *networkHandle) SuggestLatestBlock(sourceId string, blockNumber int64) {
	const prefix = "ws:"
	if !strings.HasPrefix(sourceId, prefix) {
		return
	}
	upstreamID := sourceId[len(prefix):]
	for _, u := range h.nw.upstreamsRegistry.GetNetworkUpstreams(context.Background(), h.nw.networkId) {
		if u.Id() != upstreamID {
			continue
		}
		poller := u.EvmStatePoller()
		if poller == nil || poller.IsObjectNull() {
			return
		}
		poller.SuggestLatestBlock(blockNumber)
		return
	}
}

// Interface checks: fail the build if either contract drifts.
var (
	_ wsclient.NotificationWriter = (*WsConnection)(nil)
	_ indexer.NetworkHandle       = (*networkHandle)(nil)
)

