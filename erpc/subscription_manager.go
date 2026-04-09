package erpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
)

const (
	defaultMaxSubscriptionsPerConn = 100
	unsubscribeTimeout             = 5 * time.Second
)

// SubscriptionManager manages the lifecycle of WebSocket subscriptions.
// It maps client subscription IDs to upstream subscription IDs and routes
// notifications from upstreams to the correct client connections.
type SubscriptionManager struct {
	logger *zerolog.Logger

	// clientSubID -> *SubscriptionEntry
	byClientSubID sync.Map

	// "upstreamId:upstreamSubID" -> *SubscriptionEntry
	byUpstreamSubID sync.Map

	// Deduplication: "networkId:subType:paramsHash" -> *sharedSubscription
	shared sync.Map
}

// SubscriptionEntry tracks a single active subscription per client connection.
type SubscriptionEntry struct {
	ClientSubID   string
	UpstreamSubID string
	UpstreamID    string
	Upstream      common.Upstream
	ClientConn    *WsConnection
	NetworkID     string
	SubType       string        // subscription type: "newHeads", "logs", "newPendingTransactions", etc.
	Params        []interface{} // original eth_subscribe params for resubscribe on reconnect
	SharedKey     string        // key into shared map for dedup
}

// sharedSubscription represents a single upstream subscription shared by multiple client connections.
type sharedSubscription struct {
	mu            sync.Mutex
	upstreamSubID string
	upstreamID    string
	upstream      common.Upstream
	clients       map[string]*SubscriptionEntry // clientSubID -> entry
	subType       string
	params        []interface{}
	networkID     string
}

func NewSubscriptionManager(logger *zerolog.Logger) *SubscriptionManager {
	return &SubscriptionManager{
		logger: logger,
	}
}

// Subscribe handles an eth_subscribe request from a client WS connection.
// It applies rate limiting, selects a WS-capable upstream (respecting scoring and directives),
// records metrics, and deduplicates shared subscriptions.
func (sm *SubscriptionManager) Subscribe(
	ctx context.Context,
	wsc *WsConnection,
	nq *common.NormalizedRequest,
	project *PreparedProject,
	networkId string,
) (*common.NormalizedResponse, error) {
	start := time.Now()
	method := "eth_subscribe"
	lg := sm.logger.With().Str("connId", wsc.id).Str("networkId", networkId).Logger()

	// Check subscription limit per connection
	count := 0
	wsc.subscriptions.Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	if count >= defaultMaxSubscriptionsPerConn {
		return nil, common.NewErrSubscriptionLimitExceeded(defaultMaxSubscriptionsPerConn)
	}

	// Get the network
	nw, err := project.GetNetwork(ctx, networkId)
	if err != nil {
		return nil, err
	}
	nq.SetNetwork(nw)

	// --- Rate limiting (project + network level) ---
	if err := project.acquireRateLimitPermit(ctx, nq); err != nil {
		return nil, err
	}
	if err := nw.acquireRateLimitPermit(ctx, nq); err != nil {
		return nil, err
	}

	// --- Metrics: request received ---
	reqFinality := nq.Finality(ctx)
	telemetry.CounterHandle(telemetry.MetricNetworkRequestsReceived,
		project.Config.Id, nw.Label(), method, reqFinality.String(), nq.UserId(), nq.AgentName(),
	).Inc()

	// Parse the JSON-RPC request to extract subscription type and params
	jrReq, err := nq.JsonRpcRequest()
	if err != nil {
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}

	// Extract subscription type (first param) for dedup key
	subType := ""
	if len(jrReq.Params) > 0 {
		if st, ok := jrReq.Params[0].(string); ok {
			subType = st
		}
	}

	// --- Upstream selection: sorted by score, respecting useUpstream directive ---
	upsList, err := nw.upstreamsRegistry.GetSortedUpstreams(ctx, networkId, method)
	if err != nil {
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}

	// Filter for WS-capable upstreams, respecting useUpstream directive
	var selectedUpstream common.Upstream
	useUpstreamPattern := ""
	if dr := nq.Directives(); dr != nil {
		useUpstreamPattern = dr.UseUpstream
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
		selectedUpstream = u
		break
	}

	if selectedUpstream == nil {
		err := common.NewErrNoWsUpstreamAvailable(networkId)
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}

	lg.Debug().Str("upstreamId", selectedUpstream.Id()).Str("subType", subType).Msg("forwarding eth_subscribe to WS upstream")

	// --- Deduplication: check if identical subscription already exists on this upstream ---
	sharedKey := fmt.Sprintf("%s:%s:%s", networkId, selectedUpstream.Id(), buildParamsHash(jrReq.Params))

	var upstreamSubID string
	var shared *sharedSubscription
	needsUpstreamSubscribe := false

	if existing, ok := sm.shared.Load(sharedKey); ok {
		shared = existing.(*sharedSubscription)
		shared.mu.Lock()
		upstreamSubID = shared.upstreamSubID
		shared.mu.Unlock()
		lg.Debug().Str("sharedKey", sharedKey).Msg("reusing existing upstream subscription")
	} else {
		needsUpstreamSubscribe = true
	}

	if needsUpstreamSubscribe {
		// Forward the eth_subscribe request to the selected upstream
		resp, err := selectedUpstream.Forward(ctx, nq, false)
		if err != nil {
			sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
			return nil, err
		}

		// Extract the upstream subscription ID from the response
		jrResp, err := resp.JsonRpcResponse()
		if err != nil {
			sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
			return nil, err
		}
		if jrResp.Error != nil {
			sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, jrResp.Error)
			return resp, nil
		}

		upstreamSubID = strings.Trim(string(jrResp.GetResultBytes()), "\"")
		if upstreamSubID == "" {
			err := fmt.Errorf("upstream returned empty subscription ID")
			sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
			return nil, err
		}

		shared = &sharedSubscription{
			upstreamSubID: upstreamSubID,
			upstreamID:    selectedUpstream.Id(),
			upstream:      selectedUpstream,
			clients:       make(map[string]*SubscriptionEntry),
			subType:       subType,
			params:        jrReq.Params,
			networkID:     networkId,
		}
		sm.shared.Store(sharedKey, shared)
	}

	// Generate a client-facing subscription ID
	clientSubID, err := generateSubscriptionID()
	if err != nil {
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, fmt.Errorf("failed to generate subscription ID: %w", err))
		return nil, fmt.Errorf("failed to generate subscription ID: %w", err)
	}

	// Create the subscription entry
	entry := &SubscriptionEntry{
		ClientSubID:   clientSubID,
		UpstreamSubID: upstreamSubID,
		UpstreamID:    selectedUpstream.Id(),
		Upstream:      selectedUpstream,
		ClientConn:    wsc,
		NetworkID:     networkId,
		SubType:       subType,
		Params:        jrReq.Params,
		SharedKey:     sharedKey,
	}

	upstreamKey := fmt.Sprintf("%s:%s", selectedUpstream.Id(), upstreamSubID)
	sm.byClientSubID.Store(clientSubID, entry)
	sm.byUpstreamSubID.Store(upstreamKey, entry)
	wsc.subscriptions.Store(clientSubID, entry)

	// Add to shared subscription's client set
	shared.mu.Lock()
	shared.clients[clientSubID] = entry
	shared.mu.Unlock()

	// Register notification handler on the upstream WS client (idempotent)
	sm.registerNotificationHandler(selectedUpstream, upstreamSubID)

	lg.Info().
		Str("clientSubId", clientSubID).
		Str("upstreamSubId", upstreamSubID).
		Str("upstreamId", selectedUpstream.Id()).
		Str("subType", subType).
		Bool("shared", !needsUpstreamSubscribe).
		Msg("subscription established")

	// --- Metrics: success ---
	telemetry.CounterHandle(telemetry.MetricNetworkSuccessfulRequests,
		project.Config.Id, nw.Label(), selectedUpstream.VendorName(), selectedUpstream.Id(),
		method, "1", reqFinality.String(), "false", nq.UserId(), nq.AgentName(),
	).Inc()
	telemetry.ObserverHandle(telemetry.MetricNetworkRequestDuration,
		project.Config.Id, nw.Label(), selectedUpstream.VendorName(), selectedUpstream.Id(),
		method, reqFinality.String(), nq.UserId(),
	).Observe(time.Since(start).Seconds())

	// Build response with client subscription ID instead of upstream's
	clientResp := common.NewNormalizedResponse().WithRequest(nq)
	jrr := &common.JsonRpcResponse{}
	_ = jrr.SetID(jrReq.ID)
	jrr.SetResult([]byte(fmt.Sprintf(`"%s"`, clientSubID)))
	clientResp.WithJsonRpcResponse(jrr)

	return clientResp, nil
}

// Unsubscribe handles an eth_unsubscribe request from a client WS connection.
func (sm *SubscriptionManager) Unsubscribe(
	ctx context.Context,
	wsc *WsConnection,
	nq *common.NormalizedRequest,
	project *PreparedProject,
	networkId string,
) (*common.NormalizedResponse, error) {
	start := time.Now()
	method := "eth_unsubscribe"
	lg := sm.logger.With().Str("connId", wsc.id).Str("networkId", networkId).Logger()

	nw, err := project.GetNetwork(ctx, networkId)
	if err != nil {
		return nil, err
	}
	nq.SetNetwork(nw)

	// --- Rate limiting ---
	if err := project.acquireRateLimitPermit(ctx, nq); err != nil {
		return nil, err
	}
	if err := nw.acquireRateLimitPermit(ctx, nq); err != nil {
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

	// Extract the client subscription ID from params
	if len(jrReq.Params) == 0 {
		err := common.NewErrSubscriptionNotFound("")
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}
	clientSubID, ok := jrReq.Params[0].(string)
	if !ok {
		err := common.NewErrSubscriptionNotFound(fmt.Sprintf("%v", jrReq.Params[0]))
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}

	// Look up the subscription entry
	entryRaw, ok := sm.byClientSubID.Load(clientSubID)
	if !ok {
		err := common.NewErrSubscriptionNotFound(clientSubID)
		sm.recordFailureMetrics(project, nw, method, reqFinality, start, nq, err)
		return nil, err
	}
	entry := entryRaw.(*SubscriptionEntry)

	// Remove this client from the shared subscription
	shouldUnsubUpstream := sm.removeClientFromShared(entry)

	if shouldUnsubUpstream {
		// Last client using this upstream subscription — unsubscribe from upstream
		unsubReq := common.NewNormalizedRequest(mustMarshal(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      jrReq.ID,
			"method":  "eth_unsubscribe",
			"params":  []interface{}{entry.UpstreamSubID},
		}))
		unsubReq.SetNetwork(nq.Network())

		if entry.Upstream != nil {
			_, fwdErr := entry.Upstream.Forward(ctx, unsubReq, false)
			if fwdErr != nil {
				lg.Warn().Err(fwdErr).Str("upstreamId", entry.UpstreamID).Msg("failed to forward eth_unsubscribe to upstream")
			}
		}
		sm.unregisterNotificationHandler(entry)
	}

	// Clean up client-side maps
	sm.byClientSubID.Delete(clientSubID)
	wsc.subscriptions.Delete(clientSubID)

	lg.Info().Str("clientSubId", clientSubID).Bool("upstreamUnsubscribed", shouldUnsubUpstream).Msg("subscription removed")

	// --- Metrics: success ---
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

	// Build success response
	resp := common.NewNormalizedResponse().WithRequest(nq)
	jrr := &common.JsonRpcResponse{}
	_ = jrr.SetID(jrReq.ID)
	jrr.SetResult([]byte("true"))
	resp.WithJsonRpcResponse(jrr)

	return resp, nil
}

// CleanupConnection removes all subscriptions for a disconnected client connection.
func (sm *SubscriptionManager) CleanupConnection(wsc *WsConnection, project *PreparedProject) {
	lg := sm.logger.With().Str("connId", wsc.id).Logger()

	wsc.subscriptions.Range(func(key, value interface{}) bool {
		entry := value.(*SubscriptionEntry)
		clientSubID := key.(string)

		shouldUnsubUpstream := sm.removeClientFromShared(entry)

		if shouldUnsubUpstream {
			// Best-effort unsubscribe from upstream
			ctx, cancel := context.WithTimeout(context.Background(), unsubscribeTimeout)

			if entry.Upstream != nil {
				nw, nwErr := project.GetNetwork(ctx, entry.NetworkID)
				if nwErr == nil {
					unsubReq := common.NewNormalizedRequest(mustMarshal(map[string]interface{}{
						"jsonrpc": "2.0",
						"id":      1,
						"method":  "eth_unsubscribe",
						"params":  []interface{}{entry.UpstreamSubID},
					}))
					unsubReq.SetNetwork(nw)
					_, fwdErr := entry.Upstream.Forward(ctx, unsubReq, false)
					if fwdErr != nil {
						lg.Debug().Err(fwdErr).Str("upstreamId", entry.UpstreamID).Msg("failed to unsubscribe from upstream during cleanup")
					}
				}
			}

			cancel()
			sm.unregisterNotificationHandler(entry)
		}

		// Remove from client-side maps
		sm.byClientSubID.Delete(clientSubID)
		wsc.subscriptions.Delete(clientSubID)

		return true
	})

	lg.Debug().Msg("cleaned up all subscriptions for connection")
}

// HandleUpstreamNotification is called when an upstream WS client receives a subscription notification.
// It fans out the notification to all client connections sharing this upstream subscription.
func (sm *SubscriptionManager) HandleUpstreamNotification(upstreamID, upstreamSubID string, params []byte) {
	upstreamKey := fmt.Sprintf("%s:%s", upstreamID, upstreamSubID)

	// Parse the notification params to extract the result
	var notifParams struct {
		Subscription string          `json:"subscription"`
		Result       json.RawMessage `json:"result"`
	}
	if err := common.SonicCfg.Unmarshal(params, &notifParams); err != nil {
		sm.logger.Warn().Err(err).Msg("failed to parse notification params")
		return
	}

	// Find the shared subscription and fan out to all clients
	// First try the upstream key directly for entries
	entryRaw, ok := sm.byUpstreamSubID.Load(upstreamKey)
	if !ok {
		sm.logger.Debug().Str("upstreamKey", upstreamKey).Msg("notification for unknown subscription")
		return
	}
	entry := entryRaw.(*SubscriptionEntry)

	// Find the shared subscription to get all clients
	if sharedRaw, ok := sm.shared.Load(entry.SharedKey); ok {
		shared := sharedRaw.(*sharedSubscription)
		shared.mu.Lock()
		clients := make([]*SubscriptionEntry, 0, len(shared.clients))
		for _, e := range shared.clients {
			clients = append(clients, e)
		}
		shared.mu.Unlock()

		for _, clientEntry := range clients {
			if err := clientEntry.ClientConn.WriteSubscriptionNotification(clientEntry.ClientSubID, notifParams.Result); err != nil {
				sm.logger.Debug().Err(err).Str("clientSubId", clientEntry.ClientSubID).Msg("failed to write notification to client")
			}
		}
	} else {
		// Fallback: no shared entry, write to the single known client
		if err := entry.ClientConn.WriteSubscriptionNotification(entry.ClientSubID, notifParams.Result); err != nil {
			sm.logger.Debug().Err(err).Str("clientSubId", entry.ClientSubID).Msg("failed to write notification to client")
		}
	}
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
	delete(shared.clients, entry.ClientSubID)
	remaining := len(shared.clients)
	shared.mu.Unlock()

	if remaining == 0 {
		sm.shared.Delete(entry.SharedKey)
		return true
	}
	return false
}

func (sm *SubscriptionManager) registerNotificationHandler(up common.Upstream, upstreamSubID string) {
	upstreamID := up.Id()

	// Access the WsJsonRpcClient to register per-subscription notification handler
	if concreteUp, ok := up.(*upstream.Upstream); ok {
		if wsClient, ok := concreteUp.Client.(*clients.WsJsonRpcClient); ok {
			wsClient.RegisterSubscriptionHandler(upstreamSubID, func(params []byte) {
				sm.HandleUpstreamNotification(upstreamID, upstreamSubID, params)
			})
		}
	}
}

func (sm *SubscriptionManager) unregisterNotificationHandler(entry *SubscriptionEntry) {
	upstreamKey := fmt.Sprintf("%s:%s", entry.UpstreamID, entry.UpstreamSubID)
	sm.byUpstreamSubID.Delete(upstreamKey)

	if entry.Upstream != nil {
		if concreteUp, ok := entry.Upstream.(*upstream.Upstream); ok {
			if wsClient, ok := concreteUp.Client.(*clients.WsJsonRpcClient); ok {
				wsClient.UnregisterSubscriptionHandler(entry.UpstreamSubID)
			}
		}
	}
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

// IsSubscriptionMethod returns true if the method is eth_subscribe or eth_unsubscribe.
func IsSubscriptionMethod(method string) bool {
	return method == "eth_subscribe" || method == "eth_unsubscribe"
}

func generateSubscriptionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(b), nil
}

// buildParamsHash creates a stable hash key from subscription params for deduplication.
func buildParamsHash(params []interface{}) string {
	data, err := common.SonicCfg.Marshal(params)
	if err != nil {
		return fmt.Sprintf("%v", params)
	}
	return string(data)
}

func mustMarshal(v interface{}) []byte {
	data, err := common.SonicCfg.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("mustMarshal failed: %v", err))
	}
	return data
}
