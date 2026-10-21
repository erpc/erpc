// Package wsupstream adapts a WebSocket JSON-RPC upstream into a source
// (indexer.Source: newHeads into intake) and an indexer.EventIngress (logs
// and pending transactions): it owns the upstream eth_subscribe and
// eth_unsubscribe calls and resubscribes after every reconnect.
package wsupstream

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus"
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

// headsWatchInterval is how long newHeads may stay silent about a block the
// upstream already reports over RPC; a var so tests can shorten it.
var headsWatchInterval = 15 * time.Second

// HeadsReaskInterval is how long a newHeads refused as unsupported waits
// before it is asked again on the same connection; a var so tests can
// shorten it.
var HeadsReaskInterval = util.NewDuration(10 * time.Minute)

var errNotConnected = errors.New("upstream websocket is not connected")

// Adapter is the source and the EventIngress of one WS upstream on one
// network.
type Adapter struct {
	upstreamID string
	networkID  string
	// projectID and networkLabel label the adapter's metrics.
	projectID    string
	networkLabel string
	wsClient     *clients.WsJsonRpcClient
	logger       *zerolog.Logger

	// forward sends subscription RPCs through the upstream's Forward (rate
	// limits, timeouts, metrics); replaced in tests.
	forward func(ctx context.Context, nq *common.NormalizedRequest, bypassMethodExclusion bool) (*common.NormalizedResponse, error)

	retryMin time.Duration
	retryMax time.Duration
	// attemptTimeout bounds each subscribe attempt and unsubscribe.
	attemptTimeout time.Duration

	stripSubscribeFromBlockZero bool

	// polledHead is the upstream's latest block as polled over RPC, 0 if
	// unknown; nil disables watchHeads.
	polledHead func() int64
	headsSeen  atomic.Uint64
	lastHead   atomic.Int64

	// intake takes every newHeads push and subscribe answer; sink takes
	// filter events and a payload-free head event that rechecks where
	// filters belong. sink is set by Start, before Set subscribes anything.
	intake indexer.Intake
	sink   indexer.Sink

	hooksOnce sync.Once

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
	// headsWanted is what Set asked for. headsRefused is set from the
	// upstream's unsupported answer to newHeads until a subscribe succeeds
	// or Set drops newHeads; headsReask, while set, holds off asking again.
	headsWanted  bool
	headsRefused bool
	headsReask   *time.Timer
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
	// NetworkLabel labels the adapter's metrics; the network knows it
	// before its upstreams do.
	NetworkLabel string
}

type filterSub struct {
	upstreamSub
	subType    string
	paramsHash string
	params     []interface{}
}

// New returns the adapter of up, or nil if up is not a WebSocket upstream.
// Its newHeads pushes go to intake.
func New(up *upstream.Upstream, networkID string, intake indexer.Intake, logger *zerolog.Logger, opts Options) *Adapter {
	wsClient, ok := up.Client.(*clients.WsJsonRpcClient)
	if !ok {
		return nil
	}
	lg := logger.With().Str("upstreamId", up.Id()).Str("networkId", networkID).Logger()
	return &Adapter{
		upstreamID:   up.Id(),
		networkID:    networkID,
		projectID:    up.ProjectId,
		networkLabel: opts.NetworkLabel,
		wsClient:     wsClient,
		logger:       &lg,
		filters:      make(map[string]*filterSub),
		intake:       intake,
		forward: func(ctx context.Context, nq *common.NormalizedRequest, bypassMethodExclusion bool) (*common.NormalizedResponse, error) {
			return up.Forward(ctx, nq, bypassMethodExclusion, false)
		},
		retryMin:                    resubRetryMin,
		retryMax:                    resubRetryMax,
		attemptTimeout:              resubAttemptTimeout,
		stripSubscribeFromBlockZero: opts.StripSubscribeFromBlockZero,
		polledHead: func() int64 {
			if p := up.EvmStatePoller(); p != nil && !p.IsObjectNull() {
				return p.LatestBlock()
			}
			return 0
		},
	}
}

func (a *Adapter) Name() string { return "ws:" + a.upstreamID }

// Start takes the indexer's sink and registers the connection hooks;
// filters added later subscribe in the background, detached from ctx.
func (a *Adapter) Start(_ context.Context, _ indexer.NetworkHandle, sink indexer.Sink) error {
	a.sink = sink
	a.attach()
	a.startResubscribe()
	return nil
}

// Set subscribes newHeads while topics include it and drops it otherwise;
// other topics still come through EnsureFilter.
func (a *Adapter) Set(topics []indexer.Topic) {
	want := slices.Contains(topics, indexer.TopicNewHeads)
	a.subsMu.Lock()
	if want == a.headsWanted {
		a.subsMu.Unlock()
		return
	}
	a.headsWanted = want
	if want {
		a.heads.removed = false
	} else {
		a.stopReaskLocked()
		a.headsRefused = false
	}
	a.subsMu.Unlock()
	if want {
		a.attach()
		a.startResubscribe()
		return
	}
	a.drop(context.Background(), &a.heads, indexer.SubTypeNewHeads)
}

var _ indexer.Source = (*Adapter)(nil)

// attach registers the connection hooks and the silent-newHeads watch once.
// One adapter per client: each project has its own clients, an upstream
// belongs to one network, and a network creates its sources once.
func (a *Adapter) attach() {
	a.hooksOnce.Do(func() {
		cbID := a.Name()
		a.wsClient.SetOnReconnect(cbID, func() {
			a.logger.Info().Msg("upstream websocket reconnected, resubscribing")
			a.startResubscribe()
		})
		a.wsClient.SetOnDisconnect(cbID, func() {
			a.logger.Info().Msg("upstream websocket disconnected, will resubscribe on reconnect")
			// The subscriptions died with the connection; forget them so the
			// next connection resubscribes everything, newHeads included.
			a.subsMu.Lock()
			if a.resubCancel != nil {
				a.resubCancel()
			}
			a.resubDone = false
			if a.heads.id != "" {
				a.liveGauge(indexer.SubTypeNewHeads).Dec()
			}
			a.heads.id = ""
			// The next connection asks again; headsRefused stays until
			// it answers.
			a.stopReaskLocked()
			for _, sub := range a.filters {
				if sub.id != "" {
					a.liveGauge(sub.subType).Dec()
				}
				sub.id = ""
			}
			a.subsMu.Unlock()
		})
		if a.polledHead != nil {
			go a.watchHeads()
		}
	})
}

func (a *Adapter) stopReaskLocked() {
	if a.headsReask != nil {
		a.headsReask.Stop()
		a.headsReask = nil
	}
}

// refuseHeadsLocked records an unsupported newHeads answer and asks again
// after HeadsReaskInterval.
func (a *Adapter) refuseHeadsLocked() {
	a.headsRefused = true
	a.stopReaskLocked()
	var t *time.Timer
	// The caller holds subsMu until t is stored, so the callback reads it
	// only after.
	t = time.AfterFunc(HeadsReaskInterval.Load(), func() {
		a.subsMu.Lock()
		if a.headsReask != t {
			a.subsMu.Unlock()
			return
		}
		a.headsReask = nil
		a.subsMu.Unlock()
		a.startResubscribe()
	})
	a.headsReask = t
}

// HeadsRefused reports whether the upstream refused newHeads as unsupported
// and no subscribe has succeeded since.
func (a *Adapter) HeadsRefused() bool {
	a.subsMu.Lock()
	defer a.subsMu.Unlock()
	return a.headsRefused
}

// watchHeads reconnects when the newHeads subscription stays silent for a
// whole interval although the upstream reported a newer block over RPC when
// it began: pings keep such a connection up, so nothing else notices the
// dead subscription. A halted chain or a lagging node trips neither half.
func (a *Adapter) watchHeads() {
	ticker := time.NewTicker(headsWatchInterval)
	defer ticker.Stop()
	var prevID string
	var prevEpoch, prevSeen uint64
	var prevPolled int64
	for {
		select {
		case <-a.wsClient.Done():
			return
		case <-ticker.C:
		}
		a.subsMu.Lock()
		id, epoch := a.heads.id, a.heads.epoch
		a.subsMu.Unlock()
		seen, lastHead := a.headsSeen.Load(), a.lastHead.Load()
		if id != "" && id == prevID && epoch == prevEpoch && seen == prevSeen && prevPolled > lastHead {
			a.logger.Warn().Str("upstreamSubId", id).Int64("lastHead", lastHead).Int64("polledHead", prevPolled).
				Msg("newHeads subscription silent while upstream head advances, reconnecting")
			telemetry.CounterHandle(telemetry.MetricUpstreamWebsocketHeadsStallReconnectsTotal, a.projectID, a.networkLabel, a.upstreamID).Inc()
			a.wsClient.Reconnect(epoch)
		}
		prevID, prevEpoch, prevSeen, prevPolled = id, epoch, seen, a.polledHead()
	}
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
		a.drop(ctx, &sub.upstreamSub, sub.subType)
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
		a.subsMu.Lock()
		askHeads := a.headsWanted && a.headsReask == nil
		a.subsMu.Unlock()
		if askHeads {
			attemptCtx, cancel := context.WithTimeout(ctx, a.attemptTimeout)
			a.heads.mu.Lock()
			err := a.subscribeLocked(attemptCtx, &a.heads, []interface{}{indexer.SubTypeNewHeads}, a.handleNewHeads)
			a.heads.mu.Unlock()
			cancel()
			switch {
			case err == nil:
				a.subsMu.Lock()
				a.headsRefused = false
				a.subsMu.Unlock()
			case common.HasErrorCode(err, common.ErrCodeEndpointUnsupported):
				// Retrying can't change the answer soon: the source polls
				// meanwhile, and the next connection or headsReask asks again.
				a.subsMu.Lock()
				// A dropped connection cancelled ctx under subsMu; its
				// answer must not mute the next connection.
				if ctx.Err() == nil && a.headsWanted {
					a.refuseHeadsLocked()
				}
				a.subsMu.Unlock()
				a.logger.Warn().Err(err).Dur("reaskIn", HeadsReaskInterval.Load()).Msg("upstream does not support newHeads, polling it meanwhile")
			default:
				done = false
				a.logger.Warn().Err(err).Msg("failed to subscribe newHeads, will retry")
			}
			if ctx.Err() == nil {
				a.intake.Subscribed(a.upstreamID, indexer.TopicNewHeads, err)
			}
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
	return a.subscribeLocked(ctx, &sub.upstreamSub, outParams, func(params []byte) {
		a.handleFilter(sub.subType, sub.paramsHash, params)
	})
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
		// params[0] is the eth_subscribe subscription type.
		a.liveGauge(params[0].(string)).Inc()
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
func (a *Adapter) drop(ctx context.Context, sub *upstreamSub, kind string) {
	a.subsMu.Lock()
	id, epoch := sub.id, sub.epoch
	sub.id, sub.removed = "", true
	if id != "" {
		a.liveGauge(kind).Dec()
	}
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
	if err := a.send(clients.WithWsSubscription(ctx, &clients.WsSubscription{Epoch: epoch}), methodEthUnsubscribe, []interface{}{id}); err != nil {
		// A quiet subscription never triggers the client's orphan cancel.
		a.wsClient.UnsubscribeOrphan(id, epoch)
	}
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

// liveGauge is the upstream_subscriptions_active series of kind; changed
// only under subsMu, wherever a subscription's id is set or cleared.
func (a *Adapter) liveGauge(kind string) prometheus.Gauge {
	return telemetry.GaugeHandle(telemetry.MetricUpstreamSubscriptionsActive, a.projectID, a.networkLabel, a.upstreamID, kind)
}

// countEvent counts a notification received from the upstream, before
// parsing and dedup.
func (a *Adapter) countEvent(kind string, raw []byte) {
	telemetry.CounterHandle(telemetry.MetricUpstreamSubscriptionEventsTotal, a.projectID, a.networkLabel, a.upstreamID, kind).Inc()
	telemetry.CounterHandle(telemetry.MetricUpstreamSubscriptionEventBytesTotal, a.projectID, a.networkLabel, a.upstreamID, kind).Add(float64(len(raw)))
}

func (a *Adapter) handleNewHeads(raw []byte) {
	a.countEvent(indexer.SubTypeNewHeads, raw)
	var outer notificationParams
	if err := common.SonicCfg.Unmarshal(raw, &outer); err != nil {
		a.logger.Warn().Err(err).Msg("failed to parse newHeads notification envelope")
		return
	}
	// Counted before intake: a source whose heads intake gates is not silent.
	a.headsSeen.Add(1)
	var header struct {
		Number string `json:"number"`
	}
	if err := common.SonicCfg.Unmarshal(outer.Result, &header); err == nil {
		if num, err := common.HexToInt64(header.Number); err == nil {
			a.lastHead.Store(num)
		}
	}
	a.intake.Header(a.upstreamID, outer.Result)
	if a.sink != nil {
		a.sink.Ingest(indexer.StreamEvent{Kind: indexer.KindNewHead, NetworkId: a.networkID, SourceId: a.Name()})
	}
}

func (a *Adapter) handleFilter(subType, paramsHash string, raw []byte) {
	a.countEvent(subType, raw)
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
