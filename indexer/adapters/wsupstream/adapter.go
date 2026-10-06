// Package wsupstream adapts a WebSocket JSON-RPC upstream into an
// indexer.EventIngress: it owns the upstream eth_subscribe/eth_unsubscribe
// calls and resubscribes after every reconnect.
package wsupstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

const (
	methodEthSubscribe   = "eth_subscribe"
	methodEthUnsubscribe = "eth_unsubscribe"

	// resubAttemptTimeout bounds each subscribe attempt and unsubscribe.
	resubAttemptTimeout = 15 * time.Second
)

// Resubscribe backoff bounds; vars so tests can shorten them.
var (
	resubRetryMin = 1 * time.Second
	resubRetryMax = 30 * time.Second
)

var errNotConnected = errors.New("upstream websocket is not connected")

// Adapter is the EventIngress of one WS upstream on one network.
type Adapter struct {
	upstreamID string
	networkID  string
	wsClient   *clients.WsJsonRpcClient
	logger     *zerolog.Logger

	// forward sends subscription RPCs through the upstream's Forward (rate
	// limits, timeouts, metrics); replaced in tests.
	forward func(ctx context.Context, nq *common.NormalizedRequest, bypassMethodExclusion bool) (*common.NormalizedResponse, error)

	retryMin time.Duration
	retryMax time.Duration
	// attemptTimeout bounds each subscribe attempt and unsubscribe.
	attemptTimeout time.Duration

	stripSubscribeFromBlockZero bool

	nw   indexer.NetworkHandle
	sink indexer.Sink

	// subsMu guards all mutable state below. Never held across an RPC.
	subsMu sync.Mutex
	// resubCancel cancels the resubscribe loop of connection resubEpoch.
	resubCancel context.CancelFunc
	resubEpoch  uint64
	// resubDone is set once that loop has established everything;
	// resubAgain asks a running loop for another pass before it finishes.
	resubDone  bool
	resubAgain bool
	heads      upstreamSub
	// filters (by filterKey) survive disconnects to be resubscribed.
	filters map[string]*filterSub
}

// upstreamSub is one upstream subscription: newHeads or a filter.
type upstreamSub struct {
	// mu serialises subscribe attempts so EnsureFilter and the resubscribe
	// loop can't create duplicates.
	mu sync.Mutex
	// id and epoch identify the live upstream subscription (id "" when
	// none); removed is set once it is dropped. Guarded by Adapter.subsMu.
	id      string
	epoch   uint64
	removed bool
}

// Options are the network-level settings of an Adapter.
type Options struct {
	// StripSubscribeFromBlockZero removes a zero fromBlock from logs
	// filters; see common.EvmNetworkConfig.StripSubscribeFromBlockZero.
	StripSubscribeFromBlockZero bool
}

type filterSub struct {
	upstreamSub
	subType    string
	paramsHash string
	params     []interface{}
}

// New returns the adapter of up, or nil if up is not a WebSocket upstream.
func New(up *upstream.Upstream, networkID string, logger *zerolog.Logger, opts Options) *Adapter {
	wsClient, ok := up.Client.(*clients.WsJsonRpcClient)
	if !ok {
		return nil
	}
	lg := logger.With().Str("upstreamId", up.Id()).Str("networkId", networkID).Logger()
	return &Adapter{
		upstreamID: up.Id(),
		networkID:  networkID,
		wsClient:   wsClient,
		logger:     &lg,
		filters:    make(map[string]*filterSub),
		forward: func(ctx context.Context, nq *common.NormalizedRequest, bypassMethodExclusion bool) (*common.NormalizedResponse, error) {
			return up.Forward(ctx, nq, bypassMethodExclusion, false)
		},
		retryMin:                    resubRetryMin,
		retryMax:                    resubRetryMax,
		attemptTimeout:              resubAttemptTimeout,
		stripSubscribeFromBlockZero: opts.StripSubscribeFromBlockZero,
	}
}

func (a *Adapter) Name() string { return "ws:" + a.upstreamID }

// HeadsLive reports whether the newHeads subscription is live on the
// current connection.
func (a *Adapter) HeadsLive() bool {
	a.subsMu.Lock()
	defer a.subsMu.Unlock()
	return a.heads.id != ""
}

// Start registers the connection hooks and subscribes newHeads in the
// background, detached from ctx.
func (a *Adapter) Start(_ context.Context, nw indexer.NetworkHandle, sink indexer.Sink) error {
	a.nw = nw
	a.sink = sink

	// One adapter per client: each project has its own clients, an
	// upstream belongs to one network, and a network is bootstrapped once.
	cbID := a.Name()
	a.wsClient.SetOnReconnect(cbID, func() {
		a.logger.Info().Msg("upstream websocket reconnected, resubscribing")
		a.startResubscribe()
	})
	a.wsClient.SetOnDisconnect(cbID, func() {
		a.logger.Info().Msg("upstream websocket disconnected, will resubscribe on reconnect")
		// The subscriptions died with the connection; forget them so the
		// next connection resubscribes everything.
		a.subsMu.Lock()
		if a.resubCancel != nil {
			a.resubCancel()
		}
		a.resubDone = false
		a.heads.id = ""
		for _, sub := range a.filters {
			sub.id = ""
		}
		a.subsMu.Unlock()
	})

	a.startResubscribe()
	return nil
}

// startResubscribe launches the retry loop for the current connection
// unless one already runs for it.
func (a *Adapter) startResubscribe() {
	epoch := a.wsClient.Epoch()
	a.subsMu.Lock()
	defer a.subsMu.Unlock()
	if epoch == 0 {
		return
	}
	if epoch == a.resubEpoch && !a.resubDone {
		a.resubAgain = true
		return
	}
	if a.resubCancel != nil {
		a.resubCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.resubCancel, a.resubEpoch = cancel, epoch
	a.resubDone, a.resubAgain = false, false
	go a.resubscribeWithRetry(ctx)
}

// EnsureFilter subscribes a filter; a no-op if it is already subscribed. On
// failure the filter is kept and retried in the background, like every
// other subscription of this upstream, until RemoveFilter.
func (a *Adapter) EnsureFilter(ctx context.Context, subType, paramsHash string, params []interface{}) error {
	key := filterKey(subType, paramsHash)

	a.subsMu.Lock()
	sub, exists := a.filters[key]
	if !exists {
		sub = &filterSub{subType: subType, paramsHash: paramsHash, params: params}
		a.filters[key] = sub
	}
	a.subsMu.Unlock()

	// Bound the attempt: callers (the indexer's reconcile, client
	// subscribes) hold locks across it, and a reply the upstream drops
	// while its connection stays busy would otherwise never time out. A
	// timed-out filter keeps retrying in the background.
	ctx, cancel := context.WithTimeout(ctx, a.attemptTimeout)
	defer cancel()
	sub.mu.Lock()
	err := a.subscribeFilterLocked(ctx, sub)
	sub.mu.Unlock()
	if err != nil {
		a.startResubscribe()
	}
	return err
}

// FilterLive reports whether the filter's subscription is live on the
// current connection.
func (a *Adapter) FilterLive(subType, paramsHash string) bool {
	a.subsMu.Lock()
	defer a.subsMu.Unlock()
	sub, ok := a.filters[filterKey(subType, paramsHash)]
	return ok && sub.id != ""
}

// RemoveFilter unsubscribes a filter and stops resubscribing it.
func (a *Adapter) RemoveFilter(ctx context.Context, subType, paramsHash string) error {
	key := filterKey(subType, paramsHash)

	a.subsMu.Lock()
	sub, ok := a.filters[key]
	delete(a.filters, key)
	a.subsMu.Unlock()
	if ok {
		a.drop(ctx, &sub.upstreamSub)
	}
	return nil
}

func filterKey(subType, paramsHash string) string {
	return subType + ":" + paramsHash
}

// resubscribeWithRetry (re)subscribes newHeads and every filter, retrying
// with backoff until all succeed or ctx is cancelled, so one failed
// subscribe doesn't leave the adapter without heads until the next
// reconnect.
func (a *Adapter) resubscribeWithRetry(ctx context.Context) {
	backoff := a.retryMin
	for {
		if ctx.Err() != nil {
			return
		}
		done := true
		attemptCtx, cancel := context.WithTimeout(ctx, a.attemptTimeout)
		a.heads.mu.Lock()
		err := a.subscribeLocked(attemptCtx, &a.heads, []interface{}{indexer.SubTypeNewHeads}, a.handleNewHeads)
		a.heads.mu.Unlock()
		cancel()
		if err != nil {
			done = false
			a.logger.Warn().Err(err).Msg("failed to subscribe newHeads, will retry")
		}

		a.subsMu.Lock()
		filters := make([]*filterSub, 0, len(a.filters))
		for _, sub := range a.filters {
			filters = append(filters, sub)
		}
		a.subsMu.Unlock()
		for _, sub := range filters {
			attemptCtx, cancel := context.WithTimeout(ctx, a.attemptTimeout)
			sub.mu.Lock()
			err := a.subscribeFilterLocked(attemptCtx, sub)
			sub.mu.Unlock()
			cancel()
			if err != nil {
				done = false
				a.logger.Warn().Err(err).Str("subType", sub.subType).Str("paramsHash", sub.paramsHash).
					Msg("failed to re-subscribe filter, will retry")
			}
		}
		if done {
			a.subsMu.Lock()
			again := a.resubAgain && ctx.Err() == nil
			a.resubAgain = false
			if !again && ctx.Err() == nil {
				a.resubDone = true
			}
			a.subsMu.Unlock()
			if again {
				continue
			}
			a.logger.Info().Msg("all upstream subscriptions (re)established")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, a.retryMax)
	}
}

func (a *Adapter) subscribeFilterLocked(ctx context.Context, sub *filterSub) error {
	outParams := append([]interface{}{sub.subType}, sub.params[1:]...)
	if a.stripSubscribeFromBlockZero {
		if cleaned, changed := stripFromBlockZero(outParams); changed {
			a.logger.Debug().
				Str("subType", sub.subType).
				Str("paramsHash", sub.paramsHash).
				Msg("stripped zero fromBlock from eth_subscribe filter")
			outParams = cleaned
		}
	}
	err := a.subscribeLocked(ctx, &sub.upstreamSub, outParams, func(params []byte) {
		a.handleFilter(sub.subType, sub.paramsHash, params)
	})
	if err != nil {
		return fmt.Errorf("filter subscribe: %w", err)
	}
	return nil
}

// subscribeLocked subscribes sub unless it is live or dropped; the caller
// holds sub.mu. The result is kept only if sub is still wanted and its
// connection is still up, and released upstream otherwise.
func (a *Adapter) subscribeLocked(ctx context.Context, sub *upstreamSub, params []interface{}, handler func(params []byte)) error {
	a.subsMu.Lock()
	skip := sub.id != "" || sub.removed
	a.subsMu.Unlock()
	if skip {
		return nil
	}
	if !a.wsClient.IsConnected() {
		return errNotConnected
	}

	ws := &clients.WsSubscription{Handler: handler}
	if err := a.send(clients.WithWsSubscription(ctx, ws), methodEthSubscribe, params); err != nil {
		return err
	}
	if ws.ID == "" {
		return errors.New("upstream returned no subscription id")
	}

	a.subsMu.Lock()
	connUp := ws.Epoch == a.wsClient.Epoch()
	commit := connUp && !sub.removed
	if commit {
		sub.id, sub.epoch = ws.ID, ws.Epoch
	}
	a.subsMu.Unlock()

	if !connUp {
		return errNotConnected
	}
	if !commit {
		a.release(ctx, ws.ID, ws.Epoch)
		return nil
	}
	a.logger.Info().Str("upstreamSubId", ws.ID).Interface("subType", params[0]).Msg("subscribed upstream")
	return nil
}

// drop marks sub removed, so an in-flight subscribe releases its result
// instead of committing it, and releases its live subscription if any.
func (a *Adapter) drop(ctx context.Context, sub *upstreamSub) {
	a.subsMu.Lock()
	id, epoch := sub.id, sub.epoch
	sub.id, sub.removed = "", true
	a.subsMu.Unlock()
	if id != "" {
		a.release(ctx, id, epoch)
	}
}

// release drops a subscription's handler and cancels it upstream. Both are
// no-ops once its connection is gone, since the subscription died with it.
// The cancel outlives a caller context that is already done (a timed-out
// subscribe, a client that disconnected after unsubscribing), but a caller
// deadline still in the future and shorter than attemptTimeout bounds
// it, so teardown keeps its budget.
func (a *Adapter) release(ctx context.Context, id string, epoch uint64) {
	a.wsClient.UnregisterSubscriptionHandler(id, epoch)
	timeout := a.attemptTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	_ = a.send(clients.WithWsSubscription(ctx, &clients.WsSubscription{Epoch: epoch}), methodEthUnsubscribe, []interface{}{id})
}

// send forwards a subscription RPC through the upstream. It is marked
// internal so upstream-scope retry and hedge policies never send it twice:
// a duplicate eth_subscribe would leave an orphan subscription.
func (a *Adapter) send(ctx context.Context, method string, params []interface{}) error {
	body, err := common.SonicCfg.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      util.RandomID(),
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return err
	}
	nq := common.NewNormalizedRequest(body)
	nq.SetDirectives(&common.RequestDirectives{IsInternal: true})
	_, err = a.forward(ctx, nq, false)
	return err
}

type notificationParams struct {
	Result json.RawMessage `json:"result"`
}

func (a *Adapter) handleNewHeads(raw []byte) {
	var outer notificationParams
	if err := common.SonicCfg.Unmarshal(raw, &outer); err != nil {
		a.logger.Warn().Err(err).Msg("failed to parse newHeads notification envelope")
		return
	}
	var header struct {
		Number string `json:"number"`
		Hash   string `json:"hash"`
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
		Block:     indexer.BlockRef{Number: num, Hash: header.Hash},
		Payload:   outer.Result,
	})
}

func (a *Adapter) handleFilter(subType, paramsHash string, raw []byte) {
	var outer notificationParams
	if err := common.SonicCfg.Unmarshal(raw, &outer); err != nil {
		a.logger.Warn().Err(err).Str("subType", subType).Msg("failed to parse filter notification envelope")
		return
	}
	kind := indexer.KindLog
	if subType == indexer.SubTypeNewPendingTransactions {
		kind = indexer.KindPendingTx
	}
	a.sink.Ingest(indexer.StreamEvent{
		Kind:       kind,
		NetworkId:  a.networkID,
		SourceId:   a.Name(),
		FilterHash: paramsHash,
		Payload:    outer.Result,
	})
}

// stripFromBlockZero returns a copy of params without fromBlock in filter
// objects whose fromBlock is zero. params is not mutated, so it stays valid
// for the paramsHash and later resubscribes.
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

// isZeroBlockRef reports whether s is a zero integer literal ("0", "0x0", ...).
func isZeroBlockRef(s string) bool {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 0, 64)
	return err == nil && n == 0
}
