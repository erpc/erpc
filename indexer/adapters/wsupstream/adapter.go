// Package wsupstream adapts a single WebSocket JSON-RPC upstream into an
// indexer.EventIngress. One adapter per (upstream, network). The adapter
// owns the eth_subscribe / eth_unsubscribe RPCs, the per-subscription
// handler registration, and the re-subscribe-on-reconnect hook. Dedup,
// lifecycle tagging, and client fan-out live in the indexer core — this
// package is strictly a WS ↔ StreamEvent bridge.
package wsupstream

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
)

const (
	methodEthSubscribe   = "eth_subscribe"
	methodEthUnsubscribe = "eth_unsubscribe"
)

// internalReqIDOffset keeps our JSON-RPC IDs out of the range clients
// typically use (small incrementing ints). 900M gives us ~9.2×10^18
// distinct internal IDs before the int64 overflows.
const internalReqIDOffset = 900_000_000

var internalReqIDCounter atomic.Int64

// Adapter bridges one WS upstream into the indexer pipeline. It is
// created per (upstream, network) at startup and registered via
// indexer.AddIngress.
type Adapter struct {
	upstreamID string
	networkID  string
	upstream   *upstream.Upstream
	wsClient   *clients.WsJsonRpcClient
	logger     *zerolog.Logger

	// stripSubscribeFromBlockZero controls whether fromBlock: "0x0" is
	// removed from eth_subscribe logs filters before forwarding upstream.
	// See common.EvmNetworkConfig.StripSubscribeFromBlockZero for details.
	stripSubscribeFromBlockZero bool

	nw   indexer.NetworkHandle
	sink indexer.Sink

	// subsMu guards all mutable state below. Kept coarse-grained because
	// (a) the hot path (handleNotification) doesn't touch it and (b) the
	// reconnect path needs consistency across all maps.
	subsMu sync.Mutex
	// newHeadsSubID is the upstream-assigned ID for our newHeads sub, or ""
	// when not (yet) subscribed.
	newHeadsSubID string
	// filters keyed by `subType + ":" + paramsHash`. Survives disconnects
	// so we can re-subscribe on reconnect.
	filters map[string]*filterSub
}

// Options carries optional settings for New. Fields zero-valued by default
// preserve the adapter's standard behaviour — only set what you want to
// override.
type Options struct {
	// StripSubscribeFromBlockZero, when true, removes fromBlock: "0x0" from
	// eth_subscribe logs filters before sending to the upstream. See
	// common.EvmNetworkConfig.StripSubscribeFromBlockZero.
	StripSubscribeFromBlockZero bool
}

type filterSub struct {
	subType     string
	paramsHash  string
	params      []interface{}
	upstreamSub string // assigned on each (re)subscribe; empty before first attempt
}

// New constructs an adapter for one upstream. Returns nil if the upstream
// is not backed by a WsJsonRpcClient (i.e. it's HTTP) — callers filter
// upstream lists up-front but this is a cheap safety net.
//
// Pass opts for network-level behaviour overrides; a nil opts preserves
// default behaviour.
func New(up *upstream.Upstream, networkID string, logger *zerolog.Logger, opts *Options) *Adapter {
	wsClient, ok := up.Client.(*clients.WsJsonRpcClient)
	if !ok {
		return nil
	}
	lg := logger.With().Str("upstreamId", up.Id()).Str("networkId", networkID).Logger()
	a := &Adapter{
		upstreamID: up.Id(),
		networkID:  networkID,
		upstream:   up,
		wsClient:   wsClient,
		logger:     &lg,
		filters:    make(map[string]*filterSub),
	}
	if opts != nil {
		a.stripSubscribeFromBlockZero = opts.StripSubscribeFromBlockZero
	}
	return a
}

// Name identifies the adapter in indexer registries and logs. Unique per
// upstream — two networks never share a WS client today.
func (a *Adapter) Name() string { return "ws:" + a.upstreamID }

// Start wires the adapter to the indexer's sink, registers the
// reconnect/disconnect hooks, and kicks off the initial newHeads
// subscribe in a background goroutine. Per the EventIngress contract
// Start returns once background workers are set up, not once the first
// event arrives — the initial subscribe uses its own detached context
// so a cancelled caller ctx doesn't abort the upstream RPC.
func (a *Adapter) Start(_ context.Context, nw indexer.NetworkHandle, sink indexer.Sink) error {
	a.nw = nw
	a.sink = sink

	cbID := a.Name()
	a.wsClient.SetOnReconnect(cbID, func() {
		a.logger.Info().Msg("WS reconnected — re-subscribing to all active subs")
		a.resubscribeAll(context.Background())
	})
	a.wsClient.SetOnDisconnect(cbID, func() {
		a.logger.Info().Msg("WS disconnected — active subs will re-subscribe on reconnect")
	})

	go a.initialSubscribe()
	return nil
}

// initialSubscribe attempts the first newHeads subscribe. Retries on its
// own are unnecessary — if the subscribe fails, the reconnect callback
// picks it up the next time the WS client reconnects. If the WS is down
// at Start time, we wait briefly; no-op after that since the reconnect
// hook will fire Start's equivalent.
func (a *Adapter) initialSubscribe() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !a.wsClient.IsConnected() {
		a.logger.Debug().Msg("WS not yet connected at Start; newHeads will subscribe on first connect")
		return
	}
	a.subscribeNewHeads(ctx)
}

// EnsureFilter (re)subscribes a filter on this upstream. Safe to call more
// than once for the same paramsHash — the second call replaces the first.
func (a *Adapter) EnsureFilter(ctx context.Context, subType, paramsHash string, params []interface{}) error {
	key := filterKey(subType, paramsHash)

	a.subsMu.Lock()
	sub, exists := a.filters[key]
	if !exists {
		sub = &filterSub{subType: subType, paramsHash: paramsHash, params: params}
		a.filters[key] = sub
	}
	a.subsMu.Unlock()

	if !a.wsClient.IsConnected() {
		a.logger.Debug().Str("subType", subType).Str("paramsHash", paramsHash).
			Msg("WS not connected; filter will subscribe on reconnect")
		return nil
	}
	return a.subscribeFilter(ctx, sub)
}

// RemoveFilter unsubscribes a filter from the upstream and drops it from
// the adapter's state so it won't be resubscribed on reconnect. Any
// notifications that arrive in the race window are dropped by the
// indexer (no refcount → no fan-out).
func (a *Adapter) RemoveFilter(ctx context.Context, subType, paramsHash string) error {
	key := filterKey(subType, paramsHash)

	a.subsMu.Lock()
	sub, ok := a.filters[key]
	delete(a.filters, key)
	a.subsMu.Unlock()
	if !ok {
		return nil
	}
	if sub.upstreamSub != "" {
		a.wsClient.UnregisterSubscriptionHandler(sub.upstreamSub)
		a.sendUnsubscribe(ctx, sub.upstreamSub)
	}
	return nil
}

// Stop tears down all upstream subscriptions and removes the reconnect
// hook. Best-effort — upstream RPCs may fail if the connection is
// already gone; we only care that the adapter's state is released.
func (a *Adapter) Stop(ctx context.Context) error {
	cbID := a.Name()
	a.wsClient.RemoveOnReconnect(cbID)
	a.wsClient.RemoveOnDisconnect(cbID)

	a.subsMu.Lock()
	subs := a.filters
	a.filters = make(map[string]*filterSub)
	newHeads := a.newHeadsSubID
	a.newHeadsSubID = ""
	a.subsMu.Unlock()

	if newHeads != "" {
		a.wsClient.UnregisterSubscriptionHandler(newHeads)
		a.sendUnsubscribe(ctx, newHeads)
	}
	for _, sub := range subs {
		if sub.upstreamSub == "" {
			continue
		}
		a.wsClient.UnregisterSubscriptionHandler(sub.upstreamSub)
		a.sendUnsubscribe(ctx, sub.upstreamSub)
	}
	return nil
}

// --- internals ---------------------------------------------------------

func filterKey(subType, paramsHash string) string {
	return subType + ":" + paramsHash
}

// resubscribeAll re-subscribes newHeads + every tracked filter after a
// WS reconnect. Runs in a dedicated goroutine (reconnect callback) so it
// must be self-sufficient w.r.t. context.
func (a *Adapter) resubscribeAll(ctx context.Context) {
	a.subscribeNewHeads(ctx)
	a.subsMu.Lock()
	subs := make([]*filterSub, 0, len(a.filters))
	for _, s := range a.filters {
		subs = append(subs, s)
	}
	a.subsMu.Unlock()

	for _, sub := range subs {
		if err := a.subscribeFilter(ctx, sub); err != nil {
			a.logger.Warn().Err(err).Str("subType", sub.subType).Str("paramsHash", sub.paramsHash).
				Msg("failed to re-subscribe filter after reconnect")
		}
	}
}

func (a *Adapter) subscribeNewHeads(ctx context.Context) {
	subID, err := a.sendSubscribe(ctx, []interface{}{indexer.SubTypeNewHeads})
	if err != nil {
		a.logger.Warn().Err(err).Msg("failed to subscribe newHeads")
		return
	}
	a.subsMu.Lock()
	if a.newHeadsSubID != "" {
		a.wsClient.UnregisterSubscriptionHandler(a.newHeadsSubID)
	}
	a.newHeadsSubID = subID
	a.subsMu.Unlock()

	a.wsClient.RegisterSubscriptionHandler(subID, func(params []byte) {
		a.handleNewHeads(params)
	})
	a.logger.Info().Str("upstreamSubId", subID).Msg("subscribed to newHeads")
}

func (a *Adapter) subscribeFilter(ctx context.Context, sub *filterSub) error {
	outParams := append([]interface{}{sub.subType}, sub.params[1:]...)
	if a.stripSubscribeFromBlockZero {
		if cleaned, changed := stripFromBlockZero(outParams); changed {
			a.logger.Info().
				Str("subType", sub.subType).
				Str("paramsHash", sub.paramsHash).
				Msg("stripping fromBlock:0x0 from eth_subscribe filter (stripSubscribeFromBlockZero)")
			outParams = cleaned
		}
	}
	subID, err := a.sendSubscribe(ctx, outParams)
	if err != nil {
		return fmt.Errorf("filter subscribe: %w", err)
	}
	a.subsMu.Lock()
	// Replace any previous upstreamSub for this (subType, paramsHash).
	if sub.upstreamSub != "" {
		a.wsClient.UnregisterSubscriptionHandler(sub.upstreamSub)
	}
	sub.upstreamSub = subID
	a.subsMu.Unlock()

	a.wsClient.RegisterSubscriptionHandler(subID, func(params []byte) {
		a.handleFilter(sub.subType, sub.paramsHash, params)
	})
	a.logger.Info().Str("upstreamSubId", subID).Str("subType", sub.subType).
		Str("paramsHash", sub.paramsHash).Msg("subscribed filter")
	return nil
}

// handleNewHeads converts a newHeads notification into a StreamEvent and
// pushes it at the indexer's Sink.
func (a *Adapter) handleNewHeads(raw []byte) {
	var outer struct {
		Subscription string          `json:"subscription"`
		Result       json.RawMessage `json:"result"`
	}
	if err := common.SonicCfg.Unmarshal(raw, &outer); err != nil {
		a.logger.Warn().Err(err).Msg("failed to parse newHeads notification envelope")
		return
	}
	var header struct {
		Number     string `json:"number"`
		Hash       string `json:"hash"`
		ParentHash string `json:"parentHash"`
	}
	if err := common.SonicCfg.Unmarshal(outer.Result, &header); err != nil {
		a.logger.Warn().Err(err).Msg("failed to parse newHeads result")
		return
	}
	num, err := common.HexToInt64(header.Number)
	if err != nil {
		a.logger.Warn().Err(err).Str("number", header.Number).Msg("failed to parse block number")
		return
	}
	a.sink.Ingest(indexer.StreamEvent{
		Kind:      indexer.KindNewHead,
		NetworkId: a.networkID,
		SourceId:  a.Name(),
		Block:     indexer.BlockRef{Number: num, Hash: header.Hash, ParentHash: header.ParentHash},
		Payload:   outer.Result,
		ObservedAt: time.Now(),
	})
}

// handleFilter converts a filter notification into a StreamEvent.
func (a *Adapter) handleFilter(subType, paramsHash string, raw []byte) {
	var outer struct {
		Subscription string          `json:"subscription"`
		Result       json.RawMessage `json:"result"`
	}
	if err := common.SonicCfg.Unmarshal(raw, &outer); err != nil {
		a.logger.Warn().Err(err).Str("subType", subType).Msg("failed to parse filter notification envelope")
		return
	}
	kind := indexer.KindLog
	if subType == indexer.SubTypeNewPendingTransactions {
		kind = indexer.KindPendingTx
	}
	ev := indexer.StreamEvent{
		Kind:       kind,
		NetworkId:  a.networkID,
		SourceId:   a.Name(),
		FilterHash: paramsHash,
		Payload:    outer.Result,
		ObservedAt: time.Now(),
	}
	// For logs, opportunistically extract the BlockRef so the indexer
	// can lifecycle-tag it and feed the canonical-chain tracker.
	if kind == indexer.KindLog {
		var logProbe struct {
			BlockNumber string `json:"blockNumber"`
			BlockHash   string `json:"blockHash"`
		}
		if err := common.SonicCfg.Unmarshal(outer.Result, &logProbe); err == nil {
			if n, err := common.HexToInt64(logProbe.BlockNumber); err == nil {
				ev.Block = indexer.BlockRef{Number: n, Hash: logProbe.BlockHash}
			}
		}
	}
	a.sink.Ingest(ev)
}

// sendSubscribe performs the eth_subscribe RPC via the upstream's
// failsafe executor so retry/timeout policies still apply.
func (a *Adapter) sendSubscribe(ctx context.Context, params []interface{}) (string, error) {
	body, err := buildJSONRPCBody(methodEthSubscribe, params)
	if err != nil {
		return "", err
	}
	nq := common.NewNormalizedRequest(body)
	resp, err := a.upstream.Forward(ctx, nq, false)
	if err != nil {
		return "", err
	}
	jrResp, err := resp.JsonRpcResponse()
	if err != nil {
		return "", err
	}
	if jrResp.Error != nil {
		return "", jrResp.Error
	}
	subID := strings.Trim(string(jrResp.GetResultBytes()), "\"")
	if subID == "" {
		return "", fmt.Errorf("upstream returned empty subscription ID")
	}
	return subID, nil
}

// sendUnsubscribe is best-effort; errors are logged by the WS client and
// not surfaced (cleanup paths shouldn't fail on upstream errors).
func (a *Adapter) sendUnsubscribe(ctx context.Context, subID string) {
	body, err := buildJSONRPCBody(methodEthUnsubscribe, []interface{}{subID})
	if err != nil {
		return
	}
	nq := common.NewNormalizedRequest(body)
	_, _ = a.upstream.Forward(ctx, nq, false)
}

// buildJSONRPCBody marshals a JSON-RPC request with a unique internal
// ID. Kept self-contained here rather than imported from erpc/ to
// avoid a circular dependency (erpc depends on the adapter for wiring).
func buildJSONRPCBody(method string, params interface{}) ([]byte, error) {
	id := internalReqIDCounter.Add(1) + internalReqIDOffset
	return common.SonicCfg.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
}

// stripFromBlockZero returns a copy of params with fromBlock removed from
// any filter object whose fromBlock equals "0x0" or "0". toBlock is left
// alone. Other fromBlock values (including "latest", "finalized", or a
// specific hex block number) are not touched. The input slice is not
// mutated — filter maps are shallow-copied so the caller's params remain
// stable for paramsHash computation and resubscribe.
func stripFromBlockZero(params []interface{}) ([]interface{}, bool) {
	out := make([]interface{}, len(params))
	changed := false
	for i, p := range params {
		f, ok := p.(map[string]interface{})
		if !ok {
			out[i] = p
			continue
		}
		fb, hasFrom := f["fromBlock"]
		if !hasFrom {
			out[i] = f
			continue
		}
		s, isStr := fb.(string)
		if !isStr || !isZeroBlockRef(s) {
			out[i] = f
			continue
		}
		clean := make(map[string]interface{}, len(f))
		for k, v := range f {
			if k == "fromBlock" {
				continue
			}
			clean[k] = v
		}
		out[i] = clean
		changed = true
	}
	return out, changed
}

// isZeroBlockRef reports whether s parses to zero in any form eth clients
// typically emit ("0", "0x0", "0x00", …). strconv.ParseInt with base 0
// auto-detects the 0x prefix for hex.
func isZeroBlockRef(s string) bool {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 0, 64)
	return err == nil && n == 0
}
