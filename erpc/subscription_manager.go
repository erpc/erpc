package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/architecture/evm/logfilter"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/adapters/wsclient"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
)

const (
	MethodEthSubscribe   = "eth_subscribe"
	MethodEthUnsubscribe = "eth_unsubscribe"
)

// SubscriptionManager serves eth_subscribe and eth_unsubscribe on client
// WebSocket connections. newHeads and logs subscriptions are taps on the
// network-wide chain tracker; newPendingTransactions are taps on the
// network's mempool feed.
type SubscriptionManager struct {
	logger *zerolog.Logger
	heads  *blockEncodings[json.RawMessage]

	// connMu guards conns and every WsConnection.subsClosed, so no adapter
	// is created for a connection after CleanupConnection.
	connMu sync.Mutex
	conns  map[string]*wsclient.Adapter // connId -> its subscriptions

	// activeSubs counts client subscriptions on this instance for
	// server.webSocket.maxSubscriptions.
	activeSubs atomic.Int64
}

func NewSubscriptionManager(logger *zerolog.Logger) *SubscriptionManager {
	return &SubscriptionManager{
		logger: logger,
		heads:  newBlockEncodings(evm.NewHeadsPayload),
		conns:  make(map[string]*wsclient.Adapter),
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

// Subscribe handles eth_subscribe: it opens a tap for a new client
// subscription on the connection. The subscription writes nothing until
// the caller calls open, once the response carrying its id is written.
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
	// Fail fast before touching sources; AddSubscription below enforces
	// the limit atomically.
	maxSubs := wsc.server.serverCfg.WebSocket.MaxSubscriptionsPerConnection
	if conn.Count() >= maxSubs {
		return nil, nil, common.NewErrSubscriptionLimitExceeded(maxSubs)
	}
	maxTotal := wsc.server.serverCfg.WebSocket.MaxSubscriptions
	if maxTotal > 0 && sm.activeSubs.Load() >= int64(maxTotal) {
		telemetry.CounterHandle(telemetry.MetricWebsocketRejectedTotal, project.Config.Id, wsRejectSubGlobal).Inc()
		return nil, nil, common.NewErrInstanceSubscriptionLimitExceeded(maxTotal)
	}

	if err := acquireSubscribeRateLimits(ctx, project, nw, nq); err != nil {
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

	subType := wsclient.ExtractSubscriptionType(jrReq.Params)
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
	clientSubID, err := wsclient.GenerateClientSubID()
	if err != nil {
		err = fmt.Errorf("failed to generate subscription ID: %w", err)
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, nil, err
	}

	key := indexerNetworkKey(nw)
	kind, err := resolveSubscription(subType)
	if err != nil {
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, nil, err
	}
	var variant string
	releaseVariant := func() {}
	if kind == wsclient.KindPendingTx {
		if variant, releaseVariant, err = sm.openPendingVariant(ctx, nw, jrReq.Params); err != nil {
			sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
			return nil, nil, err
		}
	}

	if !sm.reserveSub(maxTotal) {
		releaseVariant()
		telemetry.CounterHandle(telemetry.MetricWebsocketRejectedTotal, project.Config.Id, wsRejectSubGlobal).Inc()
		err := common.NewErrInstanceSubscriptionLimitExceeded(maxTotal)
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, nil, err
	}
	var addErr error
	if (kind == wsclient.KindNewHead || kind == wsclient.KindLog) && nw.chainTracker == nil {
		addErr = common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorInvalidArgument,
			subType+" subscriptions need an evm network", nil, nil)
	} else {
		// The tap ends with its client or its network, whichever ends first;
		// once the network ended, bg refuses it and the tap is moot.
		tapCtx, stop := context.WithCancel(nw.appCtx)
		var opened <-chan struct{}
		if opened, addErr = conn.AddSubscription(clientSubID, key, kind, maxSubs, stop); addErr == nil {
			sub := wsclient.Subscription{ClientSubID: clientSubID, Kind: kind, NetworkID: key}
			switch kind {
			case wsclient.KindLog:
				nw.bg.Go(func() { sm.runLogs(tapCtx, wsc, sub, opened, nw, logsFilter) })
			case wsclient.KindPendingTx:
				release := releaseVariant
				releaseVariant = func() {}
				from := nw.mempool.Tail(variant)
				nw.bg.Go(func() {
					defer release()
					sm.runPendingTxs(tapCtx, wsc, sub, opened, nw, variant, from)
				})
			default:
				nw.bg.Go(func() { sm.runNewHeads(tapCtx, wsc, sub, opened, nw) })
			}
		} else {
			stop()
		}
	}
	if err := addErr; err != nil {
		releaseVariant()
		sm.activeSubs.Add(-1)
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
	return newJsonRpcResultResponse(nq, jrReq, []byte(`"`+clientSubID+`"`)), func() { conn.Open(clientSubID) }, nil
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

	if err := acquireSubscribeRateLimits(ctx, project, nw, nq); err != nil {
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

	clientSubID, err := wsclient.ExtractClientSubID(jrReq.Params)
	if err != nil {
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}

	// Only the connection that created a subscription can remove it.
	sm.connMu.Lock()
	conn := sm.conns[wsc.id]
	sm.connMu.Unlock()
	var kind wsclient.EventKind
	existed := false
	if conn != nil {
		kind, existed = conn.RemoveSubscription(clientSubID)
	}
	if !existed {
		err := common.NewErrSubscriptionNotFound(clientSubID)
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}
	sm.activeSubs.Add(-1)

	lg.Info().Str("clientSubId", clientSubID).Str("subType", kind.String()).Msg("subscription removed")

	sm.recordSuccessMetrics(project, nw, method, reqFinality, start, nq)
	return newJsonRpcResultResponse(nq, jrReq, []byte("true")), nil
}

// CleanupConnection removes every subscription of a closed connection.
// Later Subscribe calls on it fail.
func (sm *SubscriptionManager) CleanupConnection(wsc *WsConnection) {
	sm.connMu.Lock()
	wsc.subsClosed = true
	conn := sm.conns[wsc.id]
	delete(sm.conns, wsc.id)
	sm.connMu.Unlock()
	if conn == nil {
		return
	}
	sm.activeSubs.Add(-int64(len(conn.Drain())))
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

// resolveSubscription maps subType to its kind.
func resolveSubscription(subType string) (wsclient.EventKind, error) {
	switch subType {
	case indexer.SubTypeNewHeads:
		return wsclient.KindNewHead, nil
	case indexer.SubTypeLogs:
		return wsclient.KindLog, nil
	case indexer.SubTypeNewPendingTransactions:
		return wsclient.KindPendingTx, nil
	default:
		return 0, common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorInvalidArgument,
			fmt.Sprintf("unsupported subscription type: %q", subType), nil, nil)
	}
}

// openPendingVariant opens the variant of newPendingTransactions params on
// nw's mempool feed until release; the reconciler admits it and decides
// whether its push sources refused it (spec §11).
func (sm *SubscriptionManager) openPendingVariant(ctx context.Context, nw *Network, params []interface{}) (variant string, release func(), err error) {
	if nw.mempool == nil {
		return "", nil, common.NewErrNoSubscriptionSource(nw.networkId, indexer.SubTypeNewPendingTransactions)
	}
	if variant, err = indexer.PendingTxVariantOf(params); err != nil {
		return "", nil, common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorInvalidArgument,
			"invalid newPendingTransactions params: "+err.Error(), nil, nil)
	}
	if release, err = nw.sources.openPending(ctx, variant); err != nil {
		return "", nil, err
	}
	return variant, release, nil
}

// getOrCreateConn returns the connection's subscriptions, creating them on
// first use, or nil once the connection was cleaned up.
func (sm *SubscriptionManager) getOrCreateConn(wsc *WsConnection, networkLabel string) *wsclient.Adapter {
	sm.connMu.Lock()
	defer sm.connMu.Unlock()
	if wsc.subsClosed {
		return nil
	}
	if existing, ok := sm.conns[wsc.id]; ok {
		return existing
	}
	wsc.networkLabel = networkLabel
	conn := wsclient.New()
	sm.conns[wsc.id] = conn
	return conn
}

// acquireSubscribeRateLimits takes a subscription's project and network
// rate limit permits, once, when it opens.
func acquireSubscribeRateLimits(
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

// indexerNetworkKey names nw in its sources and client subscriptions.
// Projects can define the same network, each with its own upstreams, so the
// key includes the project.
func indexerNetworkKey(nw *Network) string { return nw.projectId + "/" + nw.networkId }
