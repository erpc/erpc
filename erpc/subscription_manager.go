package erpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/clients"
	"github.com/gorilla/websocket"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
	"golang.org/x/sync/singleflight"
)

const (
	// unsubscribeTimeout is the deadline for best-effort upstream unsubscribe
	// calls during connection cleanup. Kept short to avoid blocking shutdown.
	unsubscribeTimeout = 5 * time.Second
)

// SubscriptionManager manages the lifecycle of WebSocket subscriptions.
// It maps client subscription IDs to upstream subscription IDs and routes
// notifications from upstreams to the correct client connections.
//
// Subscription deduplication ensures that identical subscriptions from
// multiple clients share a single upstream subscription, reducing load
// on upstream nodes.
type SubscriptionManager struct {
	logger *zerolog.Logger
	wsCfg  *common.WebSocketServerConfig

	// clientSubId -> *SubscriptionEntry
	byClientSubId sync.Map

	// "upstreamId:upstreamSubId" -> *SubscriptionEntry
	byUpstreamSubId sync.Map

	// Deduplication: "networkId:upstreamId:paramsHash" -> *sharedSubscription
	shared sync.Map

	// Prevents duplicate upstream subscriptions for the same shared key.
	// Without singleflight, two concurrent eth_subscribe calls could both
	// see no existing shared subscription and both create upstream subscriptions.
	subscribeSF singleflight.Group

	// networkId -> *Network (registered when first subscription is created)
	networks sync.Map
}

// SubscriptionEntry tracks a single active subscription per client connection.
type SubscriptionEntry struct {
	ClientSubId   string
	UpstreamSubId string
	UpstreamId    string
	Upstream      common.Upstream
	ClientConn    *WsConnection
	NetworkId     string
	SubType       string        // e.g. "newHeads", "logs", "newPendingTransactions"
	Params        []interface{} // original eth_subscribe params for resubscribe on reconnect
	SharedKey     string        // key into shared map for dedup
}

// sharedSubscription represents a single upstream subscription shared by
// multiple client connections subscribing to the same event.
type sharedSubscription struct {
	mu            sync.Mutex
	upstreamSubId string
	upstreamId    string
	upstream      common.Upstream
	clients       map[string]*SubscriptionEntry // clientSubId -> entry
	subType       string
	params        []interface{}
	networkId     string
}

// NewSubscriptionManager creates a new SubscriptionManager.
func NewSubscriptionManager(ctx context.Context, logger *zerolog.Logger, wsCfg *common.WebSocketServerConfig) *SubscriptionManager {
	sm := &SubscriptionManager{
		logger: logger,
		wsCfg:  wsCfg,
	}
	go sm.healthCheckLoop(ctx)
	return sm
}

// healthCheckLoop periodically checks upstream block head lag for all active
// subscriptions. If an upstream falls too far behind the network tip, its
// subscribed clients are disconnected with CloseGoingAway so they reconnect
// to a healthier upstream.
func (sm *SubscriptionManager) healthCheckLoop(ctx context.Context) {
	ticker := time.NewTicker(sm.wsCfg.HealthCheckInterval.Duration())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sm.checkUpstreamHealth(ctx)
		}
	}
}

func (sm *SubscriptionManager) checkUpstreamHealth(ctx context.Context) {
	// Collect unique upstream+network pairs with active subscriptions
	type subInfo struct {
		upstreamId string
		networkId  string
	}
	seen := make(map[subInfo]bool)
	var toCheck []subInfo

	sm.shared.Range(func(_, value interface{}) bool {
		shared := value.(*sharedSubscription)
		shared.mu.Lock()
		info := subInfo{upstreamId: shared.upstreamId, networkId: shared.networkId}
		shared.mu.Unlock()

		if !seen[info] {
			seen[info] = true
			toCheck = append(toCheck, info)
		}
		return true
	})

	for _, info := range toCheck {
		nwRaw, ok := sm.networks.Load(info.networkId)
		if !ok {
			continue
		}
		nw := nwRaw.(*Network)

		best, err := sm.selectWsUpstream(ctx, nw, nil, info.networkId, MethodEthSubscribe)
		if err != nil {
			// No healthy WS upstream at all -- disconnect everyone on this upstream
			sm.logger.Warn().
				Str("upstreamId", info.upstreamId).
				Str("networkId", info.networkId).
				Msg("no healthy WS upstream available, disconnecting subscribed clients")
			sm.disconnectSubscribedClients(info.upstreamId)
			continue
		}

		if best.Id() != info.upstreamId {
			sm.logger.Info().
				Str("currentUpstream", info.upstreamId).
				Str("betterUpstream", best.Id()).
				Str("networkId", info.networkId).
				Msg("better WS upstream available, disconnecting clients to trigger re-subscribe")
			sm.disconnectSubscribedClients(info.upstreamId)
		}
	}
}

// Subscribe handles an eth_subscribe request from a client WebSocket connection.
// It validates limits, selects a WS-capable upstream, deduplicates shared
// subscriptions, and returns a client-facing subscription ID.
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

	maxSubs := wsc.server.serverCfg.WebSocket.MaxSubscriptionsPerConnection
	if err := sm.checkSubscriptionLimit(wsc, maxSubs); err != nil {
		return nil, err
	}

	nw, err := project.GetNetwork(ctx, networkId)
	if err != nil {
		return nil, err
	}
	nq.SetNetwork(nw)
	sm.networks.LoadOrStore(networkId, nw)

	if err := sm.acquireRateLimits(ctx, project, nw, nq); err != nil {
		return nil, err
	}

	reqFinality := nq.Finality(ctx)
	telemetry.CounterHandle(telemetry.MetricNetworkRequestsReceived,
		project.Config.Id, nw.Label(), method, reqFinality.String(), nq.UserId(), nq.AgentName(),
	).Inc()

	jrReq, err := nq.JsonRpcRequest()
	if err != nil {
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}

	subType := extractSubscriptionType(jrReq.Params)

	selectedUpstream, err := sm.selectWsUpstream(ctx, nw, nq, networkId, method)
	if err != nil {
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}

	lg.Debug().Str("upstreamId", selectedUpstream.Id()).Str("subType", subType).Msg("forwarding eth_subscribe to WS upstream")

	sharedKey := fmt.Sprintf("%s:%s:%s", networkId, selectedUpstream.Id(), buildParamsKey(jrReq.Params))

	sfr, err := sm.getOrCreateUpstreamSubscription(ctx, sharedKey, selectedUpstream, nq)
	if err != nil {
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		if sfr != nil && sfr.upstreamResp != nil {
			return sfr.upstreamResp, nil
		}
		return nil, err
	}

	clientSubId, err := generateSubscriptionId()
	if err != nil {
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, fmt.Errorf("failed to generate subscription ID: %w", err))
		return nil, fmt.Errorf("failed to generate subscription ID: %w", err)
	}

	entry := &SubscriptionEntry{
		ClientSubId:   clientSubId,
		UpstreamSubId: sfr.upstreamSubId,
		UpstreamId:    selectedUpstream.Id(),
		Upstream:      selectedUpstream,
		ClientConn:    wsc,
		NetworkId:     networkId,
		SubType:       subType,
		Params:        jrReq.Params,
		SharedKey:     sharedKey,
	}

	sm.storeSubscriptionEntry(entry, selectedUpstream, wsc)

	sfr.shared.mu.Lock()
	sfr.shared.clients[clientSubId] = entry
	sfr.shared.mu.Unlock()

	sm.registerNotificationHandler(selectedUpstream, sfr.upstreamSubId)

	wsc.subscriptionCount.Add(1)

	lg.Info().
		Str("clientSubId", clientSubId).
		Str("upstreamSubId", sfr.upstreamSubId).
		Str("upstreamId", selectedUpstream.Id()).
		Str("subType", subType).
		Msg("subscription established")

	sm.recordSuccessMetrics(project, nw, selectedUpstream, method, reqFinality, start, nq)

	return sm.buildSubscribeResponse(nq, jrReq, clientSubId), nil
}

// Unsubscribe handles an eth_unsubscribe request from a client WebSocket connection.
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
		project.Config.Id, nw.Label(), method, reqFinality.String(), nq.UserId(), nq.AgentName(),
	).Inc()

	jrReq, err := nq.JsonRpcRequest()
	if err != nil {
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}

	clientSubId, err := extractClientSubId(jrReq.Params)
	if err != nil {
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}

	entryRaw, ok := sm.byClientSubId.Load(clientSubId)
	if !ok {
		err := common.NewErrSubscriptionNotFound(clientSubId)
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}
	entry := entryRaw.(*SubscriptionEntry)

	shouldUnsubUpstream := sm.removeClientFromShared(entry)

	if shouldUnsubUpstream {
		sm.forwardUnsubscribe(ctx, lg, entry, nq)
		sm.unregisterNotificationHandler(entry)
	}

	sm.byClientSubId.Delete(clientSubId)
	wsc.subscriptions.Delete(clientSubId)
	wsc.subscriptionCount.Add(-1)

	lg.Info().Str("clientSubId", clientSubId).Bool("upstreamUnsubscribed", shouldUnsubUpstream).Msg("subscription removed")

	upstreamId := "n/a"
	vendor := "n/a"
	if entry.Upstream != nil {
		upstreamId = entry.Upstream.Id()
		vendor = entry.Upstream.VendorName()
	}
	telemetry.CounterHandle(telemetry.MetricNetworkSuccessfulRequests,
		project.Config.Id, nw.Label(), vendor, upstreamId,
		method, "1", reqFinality.String(), "false", nq.UserId(), nq.AgentName(),
	).Inc()
	telemetry.ObserverHandle(telemetry.MetricNetworkRequestDuration,
		project.Config.Id, nw.Label(), vendor, upstreamId,
		method, reqFinality.String(), nq.UserId(),
	).Observe(time.Since(start).Seconds())

	resp := common.NewNormalizedResponse().WithRequest(nq)
	jrr := &common.JsonRpcResponse{}
	_ = jrr.SetID(jrReq.ID)
	jrr.SetResult([]byte("true"))
	resp.WithJsonRpcResponse(jrr)

	return resp, nil
}

// CleanupConnection removes all subscriptions for a disconnected client
// connection and sends best-effort unsubscribe requests to upstreams.
func (sm *SubscriptionManager) CleanupConnection(wsc *WsConnection, project *PreparedProject) {
	lg := sm.logger.With().Str("connId", wsc.id).Logger()

	wsc.subscriptions.Range(func(key, value interface{}) bool {
		entry := value.(*SubscriptionEntry)
		clientSubId := key.(string)

		shouldUnsubUpstream := sm.removeClientFromShared(entry)

		if shouldUnsubUpstream {
			ctx, cancel := context.WithTimeout(context.Background(), unsubscribeTimeout)
			sm.cleanupUpstreamSubscription(ctx, lg, entry, project)
			cancel()
			sm.unregisterNotificationHandler(entry)
		}

		sm.byClientSubId.Delete(clientSubId)
		wsc.subscriptions.Delete(clientSubId)
		wsc.subscriptionCount.Add(-1)

		return true
	})

	lg.Debug().Msg("cleaned up all subscriptions for connection")
}

// HandleUpstreamNotification is called when an upstream WebSocket client
// receives a subscription notification. It fans out the notification to
// all client connections sharing this upstream subscription.
func (sm *SubscriptionManager) HandleUpstreamNotification(upstreamId, upstreamSubId string, params []byte) {
	upstreamKey := upstreamSubKey(upstreamId, upstreamSubId)

	var notifParams struct {
		Subscription string          `json:"subscription"`
		Result       json.RawMessage `json:"result"`
	}
	if err := common.SonicCfg.Unmarshal(params, &notifParams); err != nil {
		sm.logger.Warn().Err(err).Msg("failed to parse notification params")
		return
	}

	entryRaw, ok := sm.byUpstreamSubId.Load(upstreamKey)
	if !ok {
		sm.logger.Debug().Str("upstreamKey", upstreamKey).Msg("notification for unknown subscription")
		return
	}
	entry := entryRaw.(*SubscriptionEntry)

	if sharedRaw, ok := sm.shared.Load(entry.SharedKey); ok {
		shared := sharedRaw.(*sharedSubscription)
		shared.mu.Lock()
		clients := make([]*SubscriptionEntry, 0, len(shared.clients))
		for _, e := range shared.clients {
			clients = append(clients, e)
		}
		shared.mu.Unlock()

		for _, clientEntry := range clients {
			if err := clientEntry.ClientConn.WriteSubscriptionNotification(clientEntry.ClientSubId, notifParams.Result); err != nil {
				sm.logger.Debug().Err(err).Str("clientSubId", clientEntry.ClientSubId).Msg("failed to write notification to client")
			}
		}
	} else {
		// Fallback: no shared entry, write to the single known client
		if err := entry.ClientConn.WriteSubscriptionNotification(entry.ClientSubId, notifParams.Result); err != nil {
			sm.logger.Debug().Err(err).Str("clientSubId", entry.ClientSubId).Msg("failed to write notification to client")
		}
	}
}

const (
	MethodEthSubscribe   = "eth_subscribe"
	MethodEthUnsubscribe = "eth_unsubscribe"
)

// IsSubscriptionMethod returns true if the method is eth_subscribe or eth_unsubscribe.
func IsSubscriptionMethod(method string) bool {
	return method == MethodEthSubscribe || method == MethodEthUnsubscribe
}

// IsSubscribeMethod returns true if the method is eth_subscribe.
func IsSubscribeMethod(method string) bool {
	return method == MethodEthSubscribe
}

// IsUnsubscribeMethod returns true if the method is eth_unsubscribe.
func IsUnsubscribeMethod(method string) bool {
	return method == MethodEthUnsubscribe
}

//
// --- Helper methods: Subscribe pipeline ---
//

// checkSubscriptionLimit verifies that the connection has not exceeded
// the maximum number of subscriptions.
func (sm *SubscriptionManager) checkSubscriptionLimit(wsc *WsConnection, max int) error {
	if int(wsc.subscriptionCount.Load()) >= max {
		return common.NewErrSubscriptionLimitExceeded(max)
	}
	return nil
}

// acquireRateLimits acquires rate limit permits at both project and network level.
func (sm *SubscriptionManager) acquireRateLimits(
	ctx context.Context,
	project *PreparedProject,
	nw *Network,
	nq *common.NormalizedRequest,
) error {
	if err := project.acquireRateLimitPermit(ctx, nq); err != nil {
		return err
	}
	return nw.acquireRateLimitPermit(ctx, nq)
}

// extractSubscriptionType returns the subscription type string from the
// first element of the params array (e.g. "newHeads", "logs").
func extractSubscriptionType(params []interface{}) string {
	if len(params) > 0 {
		if st, ok := params[0].(string); ok {
			return st
		}
	}
	return ""
}

// extractClientSubId extracts the client subscription ID from unsubscribe params.
func extractClientSubId(params []interface{}) (string, error) {
	if len(params) == 0 {
		return "", common.NewErrSubscriptionNotFound("")
	}
	clientSubId, ok := params[0].(string)
	if !ok {
		return "", common.NewErrSubscriptionNotFound(fmt.Sprintf("%v", params[0]))
	}
	return clientSubId, nil
}

// selectWsUpstream finds the highest-scoring WebSocket-capable upstream
// for the given network, respecting the useUpstream directive if set.
func (sm *SubscriptionManager) selectWsUpstream(
	ctx context.Context,
	nw *Network,
	nq *common.NormalizedRequest,
	networkId string,
	method string,
) (common.Upstream, error) {
	upsList, err := nw.upstreamsRegistry.GetSortedUpstreams(ctx, networkId, method)
	if err != nil {
		return nil, err
	}

	useUpstreamPattern := ""
	if nq != nil {
		if dr := nq.Directives(); dr != nil {
			useUpstreamPattern = dr.UseUpstream
		}
	}

	for _, u := range upsList {
		cfg := u.Config()
		if cfg == nil {
			continue
		}
		parsed, parseErr := url.Parse(cfg.Endpoint)
		if parseErr != nil {
			continue
		}
		if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
			continue
		}
		if useUpstreamPattern != "" {
			match, _ := common.WildcardMatch(useUpstreamPattern, u.Id())
			if !match {
				continue
			}
		}
		// Only select upstreams whose WS connection is actually established.
		// Without this check, we may return a subscription ID for an upstream
		// that can't deliver notifications (zombie subscription).
		if concreteUp, ok := u.(*upstream.Upstream); ok {
			if wsClient, ok := concreteUp.Client.(*clients.WsJsonRpcClient); ok {
				if !wsClient.IsConnected() {
					continue
				}
			}
		}
		return u, nil
	}

	return nil, common.NewErrNoWsUpstreamAvailable(networkId)
}

// sfResult is the return type for the singleflight subscribe group.
type sfResult struct {
	shared        *sharedSubscription
	upstreamResp  *common.NormalizedResponse // non-nil only if upstream returned an error response
	upstreamSubId string
}

// getOrCreateUpstreamSubscription uses singleflight to ensure only one
// goroutine creates the upstream subscription per dedup key. Returns the
// shared subscription and upstream subscription ID.
func (sm *SubscriptionManager) getOrCreateUpstreamSubscription(
	ctx context.Context,
	sharedKey string,
	selectedUpstream common.Upstream,
	nq *common.NormalizedRequest,
) (*sfResult, error) {
	jrReq, _ := nq.JsonRpcRequest()

	result, err, _ := sm.subscribeSF.Do(sharedKey, func() (interface{}, error) {
		// Check inside singleflight: another call may have stored it
		if existing, ok := sm.shared.Load(sharedKey); ok {
			s := existing.(*sharedSubscription)
			s.mu.Lock()
			subId := s.upstreamSubId
			s.mu.Unlock()
			return &sfResult{shared: s, upstreamSubId: subId}, nil
		}

		resp, fwdErr := selectedUpstream.Forward(ctx, nq, false)
		if fwdErr != nil {
			return nil, fwdErr
		}

		jrResp, jrErr := resp.JsonRpcResponse()
		if jrErr != nil {
			return nil, jrErr
		}
		if jrResp.Error != nil {
			return &sfResult{upstreamResp: resp}, jrResp.Error
		}

		subId := strings.Trim(string(jrResp.GetResultBytes()), "\"")
		if subId == "" {
			return nil, fmt.Errorf("upstream returned empty subscription ID")
		}

		s := &sharedSubscription{
			upstreamSubId: subId,
			upstreamId:    selectedUpstream.Id(),
			upstream:      selectedUpstream,
			clients:       make(map[string]*SubscriptionEntry),
			subType:       extractSubscriptionType(jrReq.Params),
			params:        jrReq.Params,
			networkId:     selectedUpstream.NetworkId(),
		}
		sm.shared.Store(sharedKey, s)
		return &sfResult{shared: s, upstreamSubId: subId}, nil
	})

	if err != nil {
		if sfr, ok := result.(*sfResult); ok {
			return sfr, err
		}
		return nil, err
	}

	return result.(*sfResult), nil
}

// storeSubscriptionEntry registers the entry in all lookup maps.
func (sm *SubscriptionManager) storeSubscriptionEntry(entry *SubscriptionEntry, up common.Upstream, wsc *WsConnection) {
	sm.byClientSubId.Store(entry.ClientSubId, entry)
	sm.byUpstreamSubId.Store(upstreamSubKey(up.Id(), entry.UpstreamSubId), entry)
	wsc.subscriptions.Store(entry.ClientSubId, entry)
}

// buildSubscribeResponse constructs the JSON-RPC response with the
// client-facing subscription ID.
func (sm *SubscriptionManager) buildSubscribeResponse(
	nq *common.NormalizedRequest,
	jrReq *common.JsonRpcRequest,
	clientSubId string,
) *common.NormalizedResponse {
	resp := common.NewNormalizedResponse().WithRequest(nq)
	jrr := &common.JsonRpcResponse{}
	_ = jrr.SetID(jrReq.ID)
	jrr.SetResult([]byte(fmt.Sprintf(`"%s"`, clientSubId)))
	resp.WithJsonRpcResponse(jrr)
	return resp
}

//
// --- Helper methods: Unsubscribe / cleanup ---
//

// forwardUnsubscribe sends an eth_unsubscribe request to the upstream
// for the given subscription entry. Errors are logged but not returned
// since unsubscribe is best-effort.
func (sm *SubscriptionManager) forwardUnsubscribe(
	ctx context.Context,
	lg zerolog.Logger,
	entry *SubscriptionEntry,
	nq *common.NormalizedRequest,
) {
	if entry.Upstream == nil {
		return
	}

	unsubReq, err := sm.buildUnsubscribeRequest(entry.UpstreamSubId)
	if err != nil {
		lg.Warn().Err(err).Msg("failed to build eth_unsubscribe request")
		return
	}
	unsubReq.SetNetwork(nq.Network())

	if _, fwdErr := entry.Upstream.Forward(ctx, unsubReq, false); fwdErr != nil {
		lg.Warn().Err(fwdErr).Str("upstreamId", entry.UpstreamId).Msg("failed to forward eth_unsubscribe to upstream")
	}
}

// cleanupUpstreamSubscription sends a best-effort unsubscribe to the
// upstream during connection cleanup (no existing JSON-RPC request context).
func (sm *SubscriptionManager) cleanupUpstreamSubscription(
	ctx context.Context,
	lg zerolog.Logger,
	entry *SubscriptionEntry,
	project *PreparedProject,
) {
	if entry.Upstream == nil {
		return
	}

	nw, nwErr := project.GetNetwork(ctx, entry.NetworkId)
	if nwErr != nil {
		return
	}

	unsubReq, err := sm.buildUnsubscribeRequest(entry.UpstreamSubId)
	if err != nil {
		lg.Warn().Err(err).Msg("failed to build eth_unsubscribe during cleanup")
		return
	}
	unsubReq.SetNetwork(nw)

	if _, fwdErr := entry.Upstream.Forward(ctx, unsubReq, false); fwdErr != nil {
		lg.Debug().Err(fwdErr).Str("upstreamId", entry.UpstreamId).Msg("failed to unsubscribe from upstream during cleanup")
	}
}

// buildUnsubscribeRequest creates a NormalizedRequest for eth_unsubscribe.
func (sm *SubscriptionManager) buildUnsubscribeRequest(subId string) (*common.NormalizedRequest, error) {
	body, err := buildJsonRpcBody(MethodEthUnsubscribe, []interface{}{subId})
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedRequest(body), nil
}

// removeClientFromShared removes a client entry from the shared subscription.
// Returns true if the upstream subscription should be torn down (last client removed).
func (sm *SubscriptionManager) removeClientFromShared(entry *SubscriptionEntry) bool {
	if entry.SharedKey == "" {
		return true
	}

	sharedRaw, ok := sm.shared.Load(entry.SharedKey)
	if !ok {
		return true
	}
	shared := sharedRaw.(*sharedSubscription)

	shared.mu.Lock()
	delete(shared.clients, entry.ClientSubId)
	remaining := len(shared.clients)
	shared.mu.Unlock()

	if remaining == 0 {
		sm.shared.Delete(entry.SharedKey)
		return true
	}
	return false
}

//
// --- Helper methods: Notification routing ---
//

func (sm *SubscriptionManager) registerNotificationHandler(up common.Upstream, upstreamSubId string) {
	upstreamId := up.Id()

	if concreteUp, ok := up.(*upstream.Upstream); ok {
		if wsClient, ok := concreteUp.Client.(*clients.WsJsonRpcClient); ok {
			wsClient.RegisterSubscriptionHandler(upstreamSubId, func(params []byte) {
				sm.HandleUpstreamNotification(upstreamId, upstreamSubId, params)
			})

			// When the upstream WS drops, close all client connections that depend
			// on it. Clients receive CloseGoingAway (1001) and can reconnect
			// cleanly instead of holding zombie subscriptions.
			wsClient.SetOnDisconnect(func() {
				sm.disconnectSubscribedClients(upstreamId)
			})
		}
	}
}

// disconnectSubscribedClients closes all client WS connections that have active
// subscriptions on the given upstream. This forces clients to reconnect and
// re-subscribe cleanly, preventing zombie subscriptions that never deliver data.
func (sm *SubscriptionManager) disconnectSubscribedClients(upstreamId string) {
	sm.logger.Warn().Str("upstreamId", upstreamId).Msg("upstream WS disconnected, closing affected client connections")

	// Collect unique connections first, then close outside any locks.
	conns := sm.collectClientConns(upstreamId)
	for _, conn := range conns {
		conn.closeWithCode(websocket.CloseGoingAway, "upstream connection lost")
	}

	sm.logger.Info().Str("upstreamId", upstreamId).Int("clientsClosed", len(conns)).Msg("closed client connections due to upstream disconnect")
}

// collectClientConns returns the deduplicated set of client connections that
// have subscriptions on the given upstream.
func (sm *SubscriptionManager) collectClientConns(upstreamId string) []*WsConnection {
	seen := make(map[string]struct{})
	var conns []*WsConnection

	sm.shared.Range(func(_, value interface{}) bool {
		shared := value.(*sharedSubscription)
		shared.mu.Lock()
		defer shared.mu.Unlock()

		if shared.upstreamId != upstreamId {
			return true
		}
		for _, entry := range shared.clients {
			if entry.ClientConn == nil {
				continue
			}
			if _, dup := seen[entry.ClientConn.id]; dup {
				continue
			}
			seen[entry.ClientConn.id] = struct{}{}
			conns = append(conns, entry.ClientConn)
		}
		return true
	})

	return conns
}

// buildJsonRpcBody marshals a JSON-RPC request with the given method and params.
func buildJsonRpcBody(method string, params interface{}) ([]byte, error) {
	return common.SonicCfg.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
}

// upstreamSubKey builds the lookup key for byUpstreamSubId.
func upstreamSubKey(upstreamId, subId string) string {
	return fmt.Sprintf("%s:%s", upstreamId, subId)
}

func (sm *SubscriptionManager) unregisterNotificationHandler(entry *SubscriptionEntry) {
	sm.byUpstreamSubId.Delete(upstreamSubKey(entry.UpstreamId, entry.UpstreamSubId))

	if entry.Upstream != nil {
		if concreteUp, ok := entry.Upstream.(*upstream.Upstream); ok {
			if wsClient, ok := concreteUp.Client.(*clients.WsJsonRpcClient); ok {
				wsClient.UnregisterSubscriptionHandler(entry.UpstreamSubId)
			}
		}
	}
}

//
// --- Helper methods: Metrics ---
//

func (sm *SubscriptionManager) recordSuccessMetrics(
	project *PreparedProject,
	nw *Network,
	up common.Upstream,
	method string,
	finality common.DataFinalityState,
	start time.Time,
	nq *common.NormalizedRequest,
) {
	telemetry.CounterHandle(telemetry.MetricNetworkSuccessfulRequests,
		project.Config.Id, nw.Label(), up.VendorName(), up.Id(),
		method, "1", finality.String(), "false", nq.UserId(), nq.AgentName(),
	).Inc()
	telemetry.ObserverHandle(telemetry.MetricNetworkRequestDuration,
		project.Config.Id, nw.Label(), up.VendorName(), up.Id(),
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
	var resp *common.NormalizedResponse
	telemetry.CounterHandle(telemetry.MetricNetworkFailedRequests,
		project.Config.Id, nw.Label(), method,
		strconv.FormatInt(int64(resp.Attempts()), 10),
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

//
// --- Utility functions ---
//

// generateSubscriptionId creates a cryptographically random subscription ID
// in the form "0x" + 32 hex characters (16 random bytes).
func generateSubscriptionId() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(b), nil
}

// buildParamsKey creates a stable hash key from subscription params for deduplication.
func buildParamsKey(params []interface{}) string {
	data, err := common.SonicCfg.Marshal(params)
	if err != nil {
		return fmt.Sprintf("%v", params)
	}
	h := fnv.New64a()
	_, _ = h.Write(data)
	return fmt.Sprintf("%x", h.Sum64())
}
