package erpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/adapters/wsclient"
	"github.com/erpc/erpc/indexer/adapters/wsupstream"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
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
// WebSocket connections through the indexer. A network's upstream ingresses
// are registered the first time a client subscribes on it.
type SubscriptionManager struct {
	logger *zerolog.Logger
	idx    *indexer.Indexer

	// connMu guards conns and every WsConnection.subsClosed, so no adapter
	// is created for a connection after CleanupConnection.
	connMu sync.Mutex
	conns  map[string]*connEntry // connId -> entry

	networks    sync.Map // networkId -> struct{}, once bootstrapped
	bootstrapMu sync.Mutex
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
// exists and registers a new client subscription on the connection.
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

	if err := sm.bootstrapNetwork(ctx, nw); err != nil {
		return nil, err
	}

	conn := sm.getOrCreateConn(wsc)
	if conn == nil {
		return nil, wsclient.ErrClosed
	}
	// Fail fast before touching upstream filters; AddSubscription below
	// enforces the limit atomically.
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
		err = fmt.Errorf("failed to generate subscription ID: %w", err)
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}

	kind, filterHash, err := sm.resolveSubscription(ctx, networkId, subType, jrReq.Params)
	if err != nil {
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}

	if err := conn.adapter.AddSubscription(clientSubID, networkId, kind, filterHash, maxSubs); err != nil {
		sm.releaseFilter(ctx, networkId, kind, filterHash)
		if errors.Is(err, wsclient.ErrLimitExceeded) {
			err = common.NewErrSubscriptionLimitExceeded(maxSubs)
		}
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}

	lg.Info().
		Str("clientSubId", clientSubID).
		Str("subType", subType).
		Msg("subscription established")

	sm.recordSuccessMetrics(project, nw, method, reqFinality, start, nq)
	return newJsonRpcResultResponse(nq, jrReq, []byte(`"`+clientSubID+`"`)), nil
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
	sm.releaseFilter(ctx, subNetworkID, kind, filterHash)

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
		sm.releaseFilter(ctx, sub.NetworkID, sub.Kind, sub.FilterHash)
	}
	conn.detach()

	sm.logger.Debug().Str("connId", wsc.id).Msg("cleaned up all subscriptions for connection")
}

// releaseFilter drops a subscription's filter reference; newHeads
// subscriptions hold none.
func (sm *SubscriptionManager) releaseFilter(ctx context.Context, networkID string, kind indexer.EventKind, filterHash string) {
	if kind != indexer.KindNewHead && filterHash != "" {
		sm.idx.ReleaseFilter(ctx, networkID, kind.String(), filterHash)
	}
}

// bootstrapNetwork registers the network with the indexer, with an ingress
// per WebSocket upstream, once.
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

	var adapterOpts wsupstream.Options
	if cfg := nw.cfg; cfg != nil && cfg.Evm != nil && cfg.Evm.StripSubscribeFromBlockZero != nil {
		adapterOpts.StripSubscribeFromBlockZero = *cfg.Evm.StripSubscribeFromBlockZero
	}
	adapters := make(map[string]*wsupstream.Adapter, len(wsUpstreams))
	heads := make(map[string]headSource, len(wsUpstreams))
	for _, up := range wsUpstreams {
		if adapter := wsupstream.New(up, networkID, sm.logger, adapterOpts); adapter != nil {
			adapters[up.Id()] = adapter
			heads[up.Id()] = adapter
		}
	}
	sm.idx.RegisterNetwork(&networkHandle{nw: nw, heads: heads})
	sm.idx.RegisterNetworkSelector(networkID, &subIngressSelector{nw: nw})
	for _, up := range wsUpstreams {
		adapter := adapters[up.Id()]
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

// subIngressSelector picks the ingresses of a filter subscribe the way the
// HTTP path picks upstreams: by score for eth_subscribe, skipping upstreams
// whose circuit breaker is open. Fallback-tier upstreams form the fallback
// tier only when the network's failover is enabled, as over HTTP.
type subIngressSelector struct {
	nw *Network
}

func (s *subIngressSelector) Select(_, _ string, _ []interface{}) (defaults, fallbacks []string) {
	ups, err := s.nw.upstreamsRegistry.GetSortedUpstreams(context.Background(), s.nw.networkId, MethodEthSubscribe)
	if err != nil {
		return nil, nil
	}

	failoverOn := s.nw.cfg != nil && s.nw.cfg.Failover.Enabled()
	for _, u := range ups {
		up, ok := u.(*upstream.Upstream)
		if !ok {
			continue
		}
		cfg := up.Config()
		if cfg == nil || !upstream.IsWsEndpoint(cfg.Endpoint) || up.IsDown(MethodEthSubscribe) {
			continue
		}
		name := "ws:" + up.Id()
		if failoverOn && cfg.HasTag(common.TagTierFallback) {
			fallbacks = append(fallbacks, name)
			continue
		}
		defaults = append(defaults, name)
	}
	return defaults, fallbacks
}

// resolveSubscription maps subType to its event kind and, for filter
// subscriptions, takes a filter reference.
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
		return 0, "", common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorInvalidArgument,
			fmt.Sprintf("unsupported subscription type: %q", subType), nil, nil)
	}
}

// getOrCreateConn returns the connection's entry, attaching a new egress
// on first use, or nil once the connection was cleaned up.
func (sm *SubscriptionManager) getOrCreateConn(wsc *WsConnection) *connEntry {
	sm.connMu.Lock()
	defer sm.connMu.Unlock()
	if wsc.subsClosed {
		return nil
	}
	if existing, ok := sm.conns[wsc.id]; ok {
		return existing
	}
	adapter := wsclient.New(wsc.id, wsc, sm.logger, wsc.server.serverCfg.WebSocket.SubscriptionBufferSize)
	entry := &connEntry{adapter: adapter, detach: sm.idx.Attach(adapter)}
	sm.conns[wsc.id] = entry
	return entry
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
	// heads are the network's WebSocket ingresses by upstream id; fixed once
	// the network is registered.
	heads map[string]headSource
}

// headSource is an ingress that streams newHeads.
type headSource interface {
	// HeadsLive reports whether its newHeads subscription is live.
	HeadsLive() bool
}

func (h *networkHandle) Id() string { return h.nw.networkId }

// SuggestLatestBlock passes a head from the ingress "ws:<upstreamId>" to
// that upstream's state poller and reports whether clients may receive it
// (see deliversHeadsFrom). Once the poller has accepted a deliverable head (a
// major jump is verified asynchronously first), it also advances the
// network's delivered-head floor before clients see it.
func (h *networkHandle) SuggestLatestBlock(sourceId string, blockNumber int64) bool {
	upstreamID, ok := strings.CutPrefix(sourceId, "ws:")
	if !ok {
		return true
	}
	ctx := context.Background()
	for _, u := range h.nw.upstreamsRegistry.GetNetworkUpstreams(ctx, h.nw.networkId) {
		if u.Id() != upstreamID {
			continue
		}
		deliver := h.deliversHeadsFrom(ctx, u)
		poller := u.EvmStatePoller()
		if poller == nil || poller.IsObjectNull() {
			return deliver
		}
		poller.SuggestLatestBlock(blockNumber)
		if deliver && poller.LatestBlock() >= blockNumber {
			h.nw.NoteObservedLatestBlock(h.nw.appCtx, blockNumber)
		}
		return deliver
	}
	return true
}

// deliversHeadsFrom reports whether clients may receive u's heads. With
// failover on, a fallback-tier upstream's heads are held back while some
// upstream outside that tier, which the selection policy routes to, has a
// live newHeads subscription of its own: clients are not told of a block
// only a fallback has while the primaries are up. Whether they are up is the
// policy's verdict, as for reads. Without such a primary (none eligible, or
// none streaming heads) the fallbacks' heads are delivered.
func (h *networkHandle) deliversHeadsFrom(ctx context.Context, u common.Upstream) bool {
	if h.nw.cfg == nil || !h.nw.cfg.Failover.Enabled() || !isFallbackTier(u) {
		return true
	}
	for _, c := range h.nw.tipCandidateUpstreams(ctx, "*") {
		if s := h.heads[c.Id()]; s != nil && !isFallbackTier(c) && s.HeadsLive() {
			return false
		}
	}
	return true
}

var (
	_ wsclient.NotificationWriter = (*WsConnection)(nil)
	_ indexer.NetworkHandle       = (*networkHandle)(nil)
)
