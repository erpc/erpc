// Package wsupstream adapts a WebSocket JSON-RPC upstream into a source
// (indexer.Source: newHeads and pending transactions into intake): it owns
// the upstream eth_subscribe and eth_unsubscribe calls and resubscribes
// after every reconnect.
package wsupstream

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
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

// HeadsWatchInterval is how long newHeads may stay silent while the
// source's hash age runs past it; a var so tests can shorten it.
var HeadsWatchInterval = util.NewDuration(15 * time.Second)

// HeadsReaskInterval is how long a newHeads refused as unsupported waits
// before it is asked again on the same connection; a var so tests can
// shorten it.
var HeadsReaskInterval = util.NewDuration(10 * time.Minute)

var errNotConnected = errors.New("upstream websocket is not connected")

// Adapter is the source of one WS upstream on one network.
type Adapter struct {
	upstreamID string
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
	// hashAge is how long the network's lag reference has stood above this
	// source's newest hash vote (spec §6.1); nil disables watchHeads.
	hashAge func() time.Duration
	// pushedAt is when the last newHeads push arrived, in unix nanoseconds.
	pushedAt atomic.Int64
	// disconnected tells the network the socket closed, so the source's
	// hash vote expires at once.
	disconnected func()

	// intake takes every newHeads and pending-transaction push and every
	// subscribe answer.
	intake indexer.Intake

	hooksOnce sync.Once

	// subsMu guards all mutable state below. Never held across an RPC.
	subsMu sync.Mutex
	// resubCancel cancels the resubscribe loop of connection resubEpoch.
	resubCancel context.CancelFunc
	resubEpoch  uint64
	// resubDone is set once that loop has established everything;
	// resubAgain asks a running loop for another pass, and resubWake cuts
	// its backoff short, so a topic Set adds is asked at once.
	resubDone  bool
	resubAgain bool
	resubWake  chan struct{}
	heads      upstreamSub
	// headsWanted is what Set asked for. headsRefused is set from the
	// upstream's unsupported answer to newHeads until a subscribe succeeds
	// or Set drops newHeads; headsReask, while set, holds off asking again.
	// headsRefused is written under subsMu and read without it: routing
	// reads it per request.
	headsWanted  bool
	headsRefused atomic.Bool
	headsReask   *time.Timer
	// topics are the pending-transaction topics Set asked for; they
	// survive disconnects to be resubscribed.
	topics map[indexer.Topic]*pendingSub
}

// upstreamSub is one upstream subscription: newHeads or a pending-transaction
// topic.
type upstreamSub struct {
	// mu serialises subscribe attempts so Set and the resubscribe loop
	// can't create duplicates.
	mu sync.Mutex
	// id and epoch identify the live upstream subscription (id "" when
	// none), made at since; removed is set once it is dropped. Guarded by
	// Adapter.subsMu.
	id      string
	epoch   uint64
	since   time.Time
	removed bool
}

// Options are the network-level settings of an Adapter.
type Options struct {
	// NetworkLabel labels the adapter's metrics; the network knows it
	// before its upstreams do.
	NetworkLabel string
	// HashAge is the source's hash age (spec §6.1); nil disables the
	// silent-newHeads watch.
	HashAge func() time.Duration
	// Disconnected runs when the socket closes.
	Disconnected func()
}

// pendingSub is one pending-transaction topic of Set.
type pendingSub struct {
	upstreamSub
	topic   indexer.Topic
	variant string
	params  []interface{}
	// heard is when its last message arrived, in unix nanoseconds.
	heard atomic.Int64
	// resub marks a subscribe Resubscribe asked for and none sent yet.
	// Guarded by Adapter.subsMu.
	resub bool
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
		projectID:    up.ProjectId,
		networkLabel: opts.NetworkLabel,
		wsClient:     wsClient,
		logger:       &lg,
		topics:       make(map[indexer.Topic]*pendingSub),
		resubWake:    make(chan struct{}, 1),
		intake:       intake,
		forward: func(ctx context.Context, nq *common.NormalizedRequest, bypassMethodExclusion bool) (*common.NormalizedResponse, error) {
			return up.Forward(ctx, nq, bypassMethodExclusion, false)
		},
		retryMin:       resubRetryMin,
		retryMax:       resubRetryMax,
		attemptTimeout: resubAttemptTimeout,
		hashAge:        opts.HashAge,
		disconnected:   opts.Disconnected,
	}
}

// Set feeds what topics name: newHeads, and each pending-transaction topic.
// A topic Set adds is subscribed in the background and its answer reported
// through Intake.Subscribed; a dropped one is released upstream.
func (a *Adapter) Set(topics []indexer.Topic) {
	wantHeads := slices.Contains(topics, indexer.TopicNewHeads)
	type released struct {
		id    string
		epoch uint64
	}
	var added []*pendingSub
	var releases []released
	a.subsMu.Lock()
	headsChanged := wantHeads != a.headsWanted
	a.headsWanted = wantHeads
	if headsChanged {
		if wantHeads {
			a.heads.removed = false
		} else {
			a.stopReaskLocked()
			a.headsRefused.Store(false)
		}
	}
	for _, t := range topics {
		variant, ok := t.PendingTxVariant()
		if !ok || a.topics[t] != nil {
			continue
		}
		params, err := indexer.PendingTxParams(variant)
		if err != nil {
			a.logger.Warn().Err(err).Str("topic", string(t)).Msg("cannot subscribe a pending-transaction topic with unparsable params")
			continue
		}
		sub := &pendingSub{topic: t, variant: variant, params: params}
		a.topics[t] = sub
		added = append(added, sub)
	}
	for t, sub := range a.topics {
		if slices.Contains(topics, t) {
			continue
		}
		delete(a.topics, t)
		// Marked here, so a subscribe in flight releases its result.
		if sub.id != "" {
			a.liveGauge(indexer.SubTypeNewPendingTransactions).Dec()
			releases = append(releases, released{sub.id, sub.epoch})
		}
		sub.id, sub.removed = "", true
	}
	a.subsMu.Unlock()

	for _, r := range releases {
		go a.release(context.Background(), r.id, r.epoch)
	}
	if headsChanged && !wantHeads {
		a.drop(context.Background(), &a.heads, indexer.SubTypeNewHeads)
	}
	if (headsChanged && wantHeads) || len(added) > 0 {
		a.attach()
		if !a.wsClient.IsConnected() {
			for _, sub := range added {
				a.intake.Subscribed(a.upstreamID, sub.topic, errNotConnected)
			}
		}
		a.startResubscribe()
	}
}

var _ indexer.Source = (*Adapter)(nil)

// Connected reports whether the upstream's socket is up.
func (a *Adapter) Connected() bool { return a.wsClient.IsConnected() }

// Activity is pending-transaction topic t's last message and the start of
// its live upstream subscription, each zero if none.
func (a *Adapter) Activity(t indexer.Topic) (heard, subscribed time.Time) {
	a.subsMu.Lock()
	defer a.subsMu.Unlock()
	sub := a.topics[t]
	if sub == nil {
		return time.Time{}, time.Time{}
	}
	if n := sub.heard.Load(); n != 0 {
		heard = time.Unix(0, n)
	}
	if sub.id != "" {
		subscribed = sub.since
	}
	return heard, subscribed
}

// Resubscribe drops pending-transaction topic t's live upstream
// subscription and subscribes it again. The new subscribe counts in
// erpc_source_resubscribe_total once it is sent.
func (a *Adapter) Resubscribe(t indexer.Topic) {
	a.subsMu.Lock()
	sub := a.topics[t]
	if sub == nil {
		a.subsMu.Unlock()
		return
	}
	id, epoch := sub.id, sub.epoch
	if id != "" {
		a.liveGauge(indexer.SubTypeNewPendingTransactions).Dec()
	}
	sub.id, sub.resub = "", true
	a.subsMu.Unlock()
	if id != "" {
		go a.release(context.Background(), id, epoch)
	}
	a.startResubscribe()
}

// attach registers the connection hooks and the silent-newHeads watch once.
// One adapter per client: each project has its own clients, an upstream
// belongs to one network, and a network creates its sources once.
func (a *Adapter) attach() {
	a.hooksOnce.Do(func() {
		cbID := "ws:" + a.upstreamID
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
			for _, sub := range a.topics {
				if sub.id != "" {
					a.liveGauge(indexer.SubTypeNewPendingTransactions).Dec()
				}
				sub.id = ""
			}
			a.subsMu.Unlock()
			if a.disconnected != nil {
				a.disconnected()
			}
		})
		if a.hashAge != nil {
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
	a.headsRefused.Store(true)
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
	return a.headsRefused.Load()
}

// watchHeads reconnects when the newHeads subscription stays silent for
// HeadsWatchInterval while the source's hash age runs past it (spec §15.1):
// the network's lag reference moved on without this source, so its socket
// is half-open even if pings or polls riding it still pass. A halted chain
// moves no reference and a lagging node keeps pushing: neither trips it. A
// network with one upstream has no other reference, so only the socket's
// ping/pong ends a dead one there.
func (a *Adapter) watchHeads() {
	ticker := time.NewTicker(max(HeadsWatchInterval.Load()/8, time.Millisecond))
	defer ticker.Stop()
	for {
		select {
		case <-a.wsClient.Done():
			return
		case <-ticker.C:
		}
		silent := HeadsWatchInterval.Load()
		a.subsMu.Lock()
		id, epoch, since := a.heads.id, a.heads.epoch, a.heads.since
		a.subsMu.Unlock()
		quiet := since
		if at := time.Unix(0, a.pushedAt.Load()); at.After(quiet) {
			quiet = at
		}
		if id == "" || time.Since(quiet) < silent {
			continue
		}
		if age := a.hashAge(); age > silent {
			a.logger.Warn().Str("upstreamSubId", id).Dur("hashAge", age).Dur("silentFor", time.Since(quiet)).
				Msg("newHeads subscription silent while the network moved on without it, reconnecting")
			telemetry.CounterHandle(telemetry.MetricUpstreamWebsocketHeadsStallReconnectsTotal, a.projectID, a.networkLabel, a.upstreamID).Inc()
			a.wsClient.Reconnect(epoch)
		}
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
		select {
		case a.resubWake <- struct{}{}:
		default:
		}
		return
	}
	if a.resubCancel != nil {
		a.resubCancel()
	}
	select {
	case <-a.resubWake:
	default:
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.resubCancel, a.resubEpoch = cancel, epoch
	a.resubDone, a.resubAgain = false, false
	go a.resubscribeWithRetry(ctx)
}

// resubscribeWithRetry (re)subscribes newHeads and every pending-transaction
// topic, retrying with backoff until all succeed or ctx is cancelled, so
// one failed subscribe doesn't leave the adapter without heads until the
// next reconnect.
func (a *Adapter) resubscribeWithRetry(ctx context.Context) {
	backoff := a.retryMin
	for {
		if ctx.Err() != nil {
			return
		}
		done := true
		a.subsMu.Lock()
		// This pass covers every ask made so far.
		a.resubAgain = false
		askHeads := a.headsWanted && a.headsReask == nil
		a.subsMu.Unlock()
		if askHeads {
			attemptCtx, cancel := context.WithTimeout(ctx, a.attemptTimeout)
			a.heads.mu.Lock()
			err := a.subscribeLocked(attemptCtx, &a.heads, []interface{}{indexer.SubTypeNewHeads}, a.handleNewHeads, nil)
			a.heads.mu.Unlock()
			cancel()
			switch {
			case err == nil:
				a.subsMu.Lock()
				a.headsRefused.Store(false)
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
		subs := make([]*pendingSub, 0, len(a.topics))
		for _, sub := range a.topics {
			subs = append(subs, sub)
		}
		a.subsMu.Unlock()
		for _, sub := range subs {
			attemptCtx, cancel := context.WithTimeout(ctx, a.attemptTimeout)
			sub.mu.Lock()
			err := a.subscribePendingLocked(attemptCtx, sub)
			sub.mu.Unlock()
			cancel()
			if err != nil {
				done = false
				a.logger.Warn().Err(err).Str("topic", string(sub.topic)).
					Msg("failed to re-subscribe pending-transaction topic, will retry")
			}
			if ctx.Err() != nil {
				continue
			}
			// A topic Set dropped (or replaced) meanwhile has no answer.
			a.subsMu.Lock()
			current := a.topics[sub.topic] == sub
			a.subsMu.Unlock()
			if current {
				a.intake.Subscribed(a.upstreamID, sub.topic, err)
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
		case <-a.resubWake:
			continue
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, a.retryMax)
	}
}

func (a *Adapter) subscribePendingLocked(ctx context.Context, sub *pendingSub) error {
	return a.subscribeLocked(ctx, &sub.upstreamSub, sub.params, func(params []byte) {
		a.handlePendingTx(sub, params)
	}, func() {
		a.subsMu.Lock()
		resub := sub.resub
		sub.resub = false
		a.subsMu.Unlock()
		if resub {
			telemetry.CounterHandle(telemetry.MetricSourceResubscribeTotal, a.projectID, a.networkLabel, a.upstreamID, string(sub.topic)).Inc()
		}
	})
}

// subscribeLocked subscribes sub unless it is live or dropped; the caller
// holds sub.mu. sending, if set, runs just before the subscribe is sent.
// The result is kept only if sub is still wanted and its connection is
// still up, and released upstream otherwise.
func (a *Adapter) subscribeLocked(ctx context.Context, sub *upstreamSub, params []interface{}, handler func(params []byte), sending func()) error {
	a.subsMu.Lock()
	skip := sub.id != "" || sub.removed
	a.subsMu.Unlock()
	if skip {
		return nil
	}
	if !a.wsClient.IsConnected() {
		return errNotConnected
	}

	if sending != nil {
		sending()
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
		sub.id, sub.epoch, sub.since = ws.ID, ws.Epoch, time.Now()
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

// send forwards a subscription RPC through the upstream as eRPC's own,
// timeout-only, so upstream-scope retry and hedge policies never send it
// twice: a duplicate eth_subscribe would leave an orphan subscription.
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
	nq.SetDirectives(&common.RequestDirectives{UpstreamPolicies: common.UpstreamPoliciesTimeoutOnly})
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
	a.pushedAt.Store(time.Now().UnixNano())
	a.intake.Header(a.upstreamID, outer.Result)
}

func (a *Adapter) handlePendingTx(sub *pendingSub, raw []byte) {
	// Counted before intake: a topic whose messages intake gates is not
	// silent.
	sub.heard.Store(time.Now().UnixNano())
	a.countEvent(indexer.SubTypeNewPendingTransactions, raw)
	var outer notificationParams
	if err := common.SonicCfg.Unmarshal(raw, &outer); err != nil {
		a.logger.Warn().Err(err).Msg("failed to parse newPendingTransactions notification envelope")
		return
	}
	a.intake.PendingTx(a.upstreamID, sub.variant, outer.Result)
}
