package erpc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/architecture/evm/logfilter"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/adapters/wsclient"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

const (
	MethodEthSubscribe   = "eth_subscribe"
	MethodEthUnsubscribe = "eth_unsubscribe"
)

// unsubscribeTimeout bounds best-effort unsubscribes during connection
// cleanup.
const unsubscribeTimeout = 5 * time.Second

// SubscriptionManager serves eth_subscribe and eth_unsubscribe on client
// WebSocket connections. newHeads and logs subscriptions are taps on the
// network-wide chain tracker; newPendingTransactions go through the
// indexer, whose upstream ingresses each network's source reconciler
// registers.
type SubscriptionManager struct {
	logger *zerolog.Logger
	idx    *indexer.Indexer
	heads  newHeadsPayloads

	// connMu guards conns and every WsConnection.subsClosed, so no adapter
	// is created for a connection after CleanupConnection.
	connMu sync.Mutex
	conns  map[string]*connEntry // connId -> entry

	// activeSubs counts client subscriptions on this instance for
	// server.webSocket.maxSubscriptions.
	activeSubs atomic.Int64
}

type connEntry struct {
	adapter *wsclient.Adapter
	detach  func()
}

func NewSubscriptionManager(logger *zerolog.Logger, idx *indexer.Indexer) *SubscriptionManager {
	return &SubscriptionManager{
		logger: logger,
		idx:    idx,
		conns:  make(map[string]*connEntry),
	}
}

// IsSubscriptionMethod reports whether method is eth_subscribe or
// eth_unsubscribe.
func IsSubscriptionMethod(method string) bool {
	return method == MethodEthSubscribe || method == MethodEthUnsubscribe
}

func IsSubscribeMethod(method string) bool {
	return method == MethodEthSubscribe
}

// Subscribe handles eth_subscribe: it ensures the upstream subscription
// exists and registers a new client subscription on the connection. The
// subscription writes nothing until the caller calls open, once the
// response carrying its id is written.
func (sm *SubscriptionManager) Subscribe(
	ctx context.Context,
	wsc *WsConnection,
	nq *common.NormalizedRequest,
	project *PreparedProject,
	networkId string,
) (resp *common.NormalizedResponse, open func(), err error) {
	start := time.Now()
	method := MethodEthSubscribe
	lg := sm.logger.With().Str("connId", wsc.id).Str("networkId", networkId).Logger()

	nw, err := project.GetNetwork(ctx, networkId)
	if err != nil {
		return nil, nil, err
	}
	nq.SetNetwork(nw)

	conn := sm.getOrCreateConn(wsc, nw.Label())
	if conn == nil {
		return nil, nil, wsclient.ErrClosed
	}
	// Fail fast before touching upstream filters; AddSubscription below
	// enforces the limit atomically.
	maxSubs := wsc.server.serverCfg.WebSocket.MaxSubscriptionsPerConnection
	if conn.adapter.Count() >= maxSubs {
		return nil, nil, common.NewErrSubscriptionLimitExceeded(maxSubs)
	}
	maxTotal := wsc.server.serverCfg.WebSocket.MaxSubscriptions
	if maxTotal > 0 && sm.activeSubs.Load() >= int64(maxTotal) {
		telemetry.CounterHandle(telemetry.MetricWebsocketRejectedTotal, project.Config.Id, wsRejectSubGlobal).Inc()
		return nil, nil, common.NewErrInstanceSubscriptionLimitExceeded(maxTotal)
	}

	if err := sm.acquireRateLimits(ctx, project, nw, nq); err != nil {
		return nil, nil, err
	}

	reqFinality := nq.Finality(ctx)
	telemetry.CounterHandle(telemetry.MetricNetworkRequestsReceived,
		project.Config.Id, nw.Label(), method,
		reqFinality.String(), nq.UserId(), nq.AgentName(),
	).Inc()

	jrReq, err := nq.JsonRpcRequest()
	if err != nil {
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, nil, err
	}

	subType := indexer.ExtractSubscriptionType(jrReq.Params)
	var logsFilter *logfilter.LogsFilter
	if subType == indexer.SubTypeLogs {
		if logsFilter, err = parseLogsSubscribe(jrReq.Params); err != nil {
			sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
			return nil, nil, err
		}
	}
	if err := nw.requireSubscriptionSource(ctx, subType); err != nil {
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, nil, err
	}
	clientSubID, err := indexer.GenerateClientSubID()
	if err != nil {
		err = fmt.Errorf("failed to generate subscription ID: %w", err)
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, nil, err
	}

	key := indexerNetworkKey(nw)
	kind, filterHash, err := sm.resolveSubscription(ctx, key, subType, jrReq.Params)
	if err != nil {
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, nil, err
	}

	if !sm.reserveSub(maxTotal) {
		sm.releaseFilter(ctx, key, kind, filterHash)
		telemetry.CounterHandle(telemetry.MetricWebsocketRejectedTotal, project.Config.Id, wsRejectSubGlobal).Inc()
		err := common.NewErrInstanceSubscriptionLimitExceeded(maxTotal)
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, nil, err
	}
	// Counted before it is visible, so a racing cleanup never takes the
	// gauge below zero.
	wsc.subscriptionsGauge(kind).Inc()
	var addErr error
	switch {
	case (kind == indexer.KindNewHead || kind == indexer.KindLog) && nw.chainTracker == nil:
		addErr = common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorInvalidArgument,
			subType+" subscriptions need an evm network", nil, nil)
	case kind == indexer.KindNewHead || kind == indexer.KindLog:
		// The tap ends with its client or its network, whichever ends first;
		// once the network ended, bg refuses it and the tap is moot.
		tapCtx, stop := context.WithCancel(nw.appCtx)
		var opened <-chan struct{}
		if opened, addErr = conn.adapter.AddTapSubscription(clientSubID, key, kind, maxSubs, stop); addErr == nil {
			sub := wsclient.Subscription{ClientSubID: clientSubID, Kind: kind, NetworkID: key}
			if kind == indexer.KindLog {
				nw.bg.Go(func() { sm.runLogs(tapCtx, wsc, sub, opened, nw, logsFilter) })
			} else {
				nw.bg.Go(func() { sm.runNewHeads(tapCtx, wsc, sub, opened, nw) })
			}
		} else {
			stop()
		}
	default:
		addErr = conn.adapter.AddSubscription(clientSubID, key, kind, filterHash, maxSubs)
	}
	if err := addErr; err != nil {
		wsc.subscriptionsGauge(kind).Dec()
		sm.activeSubs.Add(-1)
		sm.releaseFilter(ctx, key, kind, filterHash)
		if errors.Is(err, wsclient.ErrLimitExceeded) {
			err = common.NewErrSubscriptionLimitExceeded(maxSubs)
		}
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, nil, err
	}

	lg.Info().
		Str("clientSubId", clientSubID).
		Str("subType", subType).
		Msg("subscription established")

	sm.recordSuccessMetrics(project, nw, method, reqFinality, start, nq)
	return newJsonRpcResultResponse(nq, jrReq, []byte(`"`+clientSubID+`"`)), func() { conn.adapter.Open(clientSubID) }, nil
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

	// Only the connection that created a subscription can remove it, and
	// only the caller that actually removed it releases its filter.
	sm.connMu.Lock()
	conn := sm.conns[wsc.id]
	sm.connMu.Unlock()
	var kind indexer.EventKind
	var subNetworkID, filterHash string
	existed := false
	if conn != nil {
		kind, subNetworkID, filterHash, existed = conn.adapter.RemoveSubscription(clientSubID)
	}
	if !existed {
		err := common.NewErrSubscriptionNotFound(clientSubID)
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}
	wsc.subscriptionsGauge(kind).Dec()
	sm.releaseFilter(ctx, subNetworkID, kind, filterHash)
	sm.activeSubs.Add(-1)

	lg.Info().Str("clientSubId", clientSubID).Str("subType", kind.String()).Msg("subscription removed")

	sm.recordSuccessMetrics(project, nw, method, reqFinality, start, nq)
	return newJsonRpcResultResponse(nq, jrReq, []byte("true")), nil
}

// CleanupConnection removes every subscription of a closed connection and
// detaches it from the indexer. Later Subscribe calls on it fail.
func (sm *SubscriptionManager) CleanupConnection(wsc *WsConnection) {
	sm.connMu.Lock()
	wsc.subsClosed = true
	conn := sm.conns[wsc.id]
	delete(sm.conns, wsc.id)
	sm.connMu.Unlock()
	if conn == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), unsubscribeTimeout)
	defer cancel()
	for _, sub := range conn.adapter.Drain() {
		wsc.subscriptionsGauge(sub.Kind).Dec()
		sm.releaseFilter(ctx, sub.NetworkID, sub.Kind, sub.FilterHash)
		sm.activeSubs.Add(-1)
	}
	conn.detach()

	sm.logger.Debug().Str("connId", wsc.id).Msg("cleaned up all subscriptions for connection")
}

// reserveSub takes one slot of the instance-wide subscription cap (0 =
// unlimited); the caller gives it back with activeSubs.Add(-1).
func (sm *SubscriptionManager) reserveSub(maxTotal int) bool {
	n := sm.activeSubs.Add(1)
	if maxTotal > 0 && n > int64(maxTotal) {
		sm.activeSubs.Add(-1)
		return false
	}
	return true
}

// releaseFilter drops a subscription's filter reference; tap
// subscriptions hold none.
func (sm *SubscriptionManager) releaseFilter(ctx context.Context, networkID string, kind indexer.EventKind, filterHash string) {
	if filterHash != "" {
		sm.idx.ReleaseFilter(ctx, networkID, kind.String(), filterHash)
	}
}

// subIngressSelector picks the ingresses that carry a filter subscription.
// With failover on, the defaults are the upstreams outside the fallback tier
// that the selection policy routes to, and the fallback-tier upstreams stand
// in for them (see indexer.IngressSelector). Without failover every
// WebSocket upstream is a default.
type subIngressSelector struct {
	nw *Network
}

func (s *subIngressSelector) Select(_, _ string, _ []interface{}) (defaults, fallbacks []string) {
	ctx := context.Background()
	failoverOn := s.nw.cfg != nil && s.nw.cfg.Failover.Enabled()
	eligible := make(map[string]struct{})
	if failoverOn {
		for u := range s.nw.tipCandidateUpstreams(ctx) {
			eligible[u.Id()] = struct{}{}
		}
	}
	var ws []common.Upstream
	for _, up := range s.nw.upstreamsRegistry.GetWsUpstreams(ctx, s.nw.networkId) {
		ws = append(ws, up)
	}
	return tierWsIngresses(ws, eligible, failoverOn)
}

// tierWsIngresses names the ingresses of the WebSocket upstreams ws: with
// failover on, the eligible ones outside the fallback tier are the defaults
// and the fallback tier the fallbacks; otherwise all are defaults.
func tierWsIngresses(ws []common.Upstream, eligible map[string]struct{}, failoverOn bool) (defaults, fallbacks []string) {
	for _, up := range ws {
		name := "ws:" + up.Id()
		switch {
		case !failoverOn:
			defaults = append(defaults, name)
		case isFallbackTier(up):
			fallbacks = append(fallbacks, name)
		default:
			if _, ok := eligible[up.Id()]; ok {
				defaults = append(defaults, name)
			}
		}
	}
	return defaults, fallbacks
}

// resolveSubscription maps subType to its event kind and, for filter
// subscriptions, takes a filter reference.
func (sm *SubscriptionManager) resolveSubscription(ctx context.Context, networkID, subType string, params []interface{}) (indexer.EventKind, string, error) {
	switch subType {
	case indexer.SubTypeNewHeads:
		return indexer.KindNewHead, "", nil
	case indexer.SubTypeLogs:
		return indexer.KindLog, "", nil
	case indexer.SubTypeNewPendingTransactions:
		hash, err := sm.idx.EnsureFilter(ctx, networkID, subType, params)
		if err != nil {
			return 0, "", err
		}
		return indexer.KindPendingTx, hash, nil
	default:
		return 0, "", common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorInvalidArgument,
			fmt.Sprintf("unsupported subscription type: %q", subType), nil, nil)
	}
}

// getOrCreateConn returns the connection's entry, attaching a new egress
// on first use, or nil once the connection was cleaned up.
func (sm *SubscriptionManager) getOrCreateConn(wsc *WsConnection, networkLabel string) *connEntry {
	sm.connMu.Lock()
	defer sm.connMu.Unlock()
	if wsc.subsClosed {
		return nil
	}
	if existing, ok := sm.conns[wsc.id]; ok {
		return existing
	}
	wsc.networkLabel = networkLabel
	adapter := wsclient.New(wsc.id, wsc, sm.logger, wsc.server.serverCfg.WebSocket.SubscriptionBufferSize)
	entry := &connEntry{adapter: adapter, detach: sm.idx.Attach(adapter)}
	sm.conns[wsc.id] = entry
	return entry
}

func (wsc *WsConnection) subscriptionsGauge(kind indexer.EventKind) prometheus.Gauge {
	return telemetry.GaugeHandle(telemetry.MetricWebsocketSubscriptionsActive, wsc.project.Config.Id, wsc.networkLabel, kind.String())
}

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

func newJsonRpcResultResponse(nq *common.NormalizedRequest, jrReq *common.JsonRpcRequest, result []byte) *common.NormalizedResponse {
	jrr := &common.JsonRpcResponse{}
	_ = jrr.SetID(jrReq.ID)
	jrr.SetResult(result)
	return common.NewNormalizedResponse().WithRequest(nq).WithJsonRpcResponse(jrr)
}

func (sm *SubscriptionManager) recordSuccessMetrics(
	project *PreparedProject,
	nw *Network,
	method string,
	finality common.DataFinalityState,
	start time.Time,
	nq *common.NormalizedRequest,
) {
	telemetry.CounterHandle(telemetry.MetricNetworkSuccessfulRequests,
		project.Config.Id, nw.Label(), "proxy", "proxy",
		method, "1", finality.String(), "false", nq.UserId(), nq.AgentName(),
	).Inc()
	telemetry.ObserverHandle(telemetry.MetricNetworkRequestDuration,
		project.Config.Id, nw.Label(), "proxy", "proxy",
		method, finality.String(), nq.UserId(),
	).Observe(time.Since(start).Seconds())
}

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

// networkHandle adapts *Network to indexer.NetworkHandle.
type networkHandle struct {
	nw *Network
}

func (h *networkHandle) Id() string { return indexerNetworkKey(h.nw) }

func (h *networkHandle) DedupDropped(kind indexer.EventKind) {
	telemetry.CounterHandle(telemetry.MetricSubscriptionDedupDroppedTotal, h.nw.projectId, h.nw.Label(), kind.String()).Inc()
}

func (h *networkHandle) DedupEntriesAdded(delta int) {
	telemetry.GaugeHandle(telemetry.MetricSubscriptionDedupEntries, h.nw.projectId, h.nw.Label()).Add(float64(delta))
}

func (h *networkHandle) Undelivered(kind indexer.EventKind) {
	telemetry.CounterHandle(telemetry.MetricSubscriptionUndeliveredTotal, h.nw.projectId, h.nw.Label(), kind.String()).Inc()
}

// indexerNetworkKey names nw in the indexer and its adapters. Projects can
// define the same network, each with its own upstreams, so the key includes
// the project.
func indexerNetworkKey(nw *Network) string { return nw.projectId + "/" + nw.networkId }

var (
	_ wsclient.NotificationWriter = (*WsConnection)(nil)
	_ indexer.NetworkHandle       = (*networkHandle)(nil)
)
