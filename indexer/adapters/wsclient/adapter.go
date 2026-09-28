// Package wsclient adapts a client WebSocket connection into an
// indexer.EventEgress with a buffer and writer goroutine per subscription.
package wsclient

import (
	"encoding/json"
	"errors"
	"sync"

	"github.com/erpc/erpc/indexer"
	"github.com/rs/zerolog"
)

// ErrClosed is returned by AddSubscription once the adapter was drained.
var ErrClosed = errors.New("connection closed")

// ErrLimitExceeded is returned by AddSubscription when the connection
// already holds the maximum number of subscriptions.
var ErrLimitExceeded = errors.New("subscription limit exceeded")

// NotificationWriter writes notifications to the client connection.
type NotificationWriter interface {
	WriteSubscriptionNotification(clientSubId string, result json.RawMessage) error
	// NotificationDropped is called each time a full buffer costs a
	// notification. lossy is false when the client cannot re-derive what
	// was lost (see isLossy), in which case the connection should end.
	// Must not block.
	NotificationDropped(sub Subscription, lossy bool)
}

// isLossy reports whether a kind may shed its oldest queued notification
// on overflow: a newer head supersedes an older one, and pending
// transactions are best-effort by nature. Every other kind (logs, and any
// kind added later) is treated as data that must not silently vanish.
func isLossy(kind indexer.EventKind) bool {
	return kind == indexer.KindNewHead || kind == indexer.KindPendingTx
}

// Adapter is the EventEgress of one client connection.
type Adapter struct {
	connID     string
	writer     NotificationWriter
	logger     *zerolog.Logger
	bufferSize int

	mu      sync.RWMutex
	drained bool
	subs    map[string]*clientSub // clientSubId -> sub
	// routes indexes subs by event key for InterestedIn and Deliver.
	routes map[routeKey]map[string]struct{}
}

type routeKey struct {
	kind       indexer.EventKind
	networkID  string
	filterHash string
}

type clientSub struct {
	id         string
	kind       indexer.EventKind
	networkID  string
	filterHash string // "" for newHeads

	notify chan json.RawMessage
	done   chan struct{} // closed by whoever removes the sub from Adapter.subs
}

// New creates the adapter of a client connection. writer is called from
// every subscription's goroutine, so it must be safe for concurrent use.
// bufferSize is the per-subscription notification queue depth.
func New(connID string, writer NotificationWriter, logger *zerolog.Logger, bufferSize int) *Adapter {
	lg := logger.With().Str("connId", connID).Logger()
	return &Adapter{
		connID:     connID,
		writer:     writer,
		logger:     &lg,
		bufferSize: bufferSize,
		subs:       make(map[string]*clientSub),
		routes:     make(map[routeKey]map[string]struct{}),
	}
}

func (a *Adapter) Name() string { return "ws:client:" + a.connID }

func (a *Adapter) InterestedIn(kind indexer.EventKind, networkID, filterHash string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.routes[routeKey{kind: kind, networkID: networkID, filterHash: filterHash}]
	return ok
}

// Deliver enqueues the event on every matching subscription without
// blocking; see enqueue for what happens when a buffer is full.
func (a *Adapter) Deliver(ev indexer.IndexedEvent) {
	a.mu.RLock()
	routes := a.routes[routeKey{kind: ev.Kind, networkID: ev.NetworkId, filterHash: ev.FilterHash}]
	subs := make([]*clientSub, 0, len(routes))
	for id := range routes {
		subs = append(subs, a.subs[id])
	}
	a.mu.RUnlock()

	for _, sub := range subs {
		if !enqueue(sub, ev.Payload) {
			a.writer.NotificationDropped(sub.snapshot(), isLossy(sub.kind))
		}
	}
}

// AddSubscription registers a subscription and starts its writer. It fails
// with ErrLimitExceeded when the connection already holds maxSubs, and with
// ErrClosed once the adapter was drained.
func (a *Adapter) AddSubscription(clientSubID, networkID string, kind indexer.EventKind, filterHash string, maxSubs int) error {
	sub := &clientSub{
		id:         clientSubID,
		kind:       kind,
		networkID:  networkID,
		filterHash: filterHash,
		notify:     make(chan json.RawMessage, a.bufferSize),
		done:       make(chan struct{}),
	}

	a.mu.Lock()
	if a.drained {
		a.mu.Unlock()
		return ErrClosed
	}
	if len(a.subs) >= maxSubs {
		a.mu.Unlock()
		return ErrLimitExceeded
	}
	a.subs[clientSubID] = sub
	key := routeKey{kind: kind, networkID: networkID, filterHash: filterHash}
	set, ok := a.routes[key]
	if !ok {
		set = make(map[string]struct{})
		a.routes[key] = set
	}
	set[clientSubID] = struct{}{}
	a.mu.Unlock()

	go a.runWriter(sub)
	return nil
}

// RemoveSubscription removes a subscription, stops its writer, and returns
// what the caller needs to release its filter.
func (a *Adapter) RemoveSubscription(clientSubID string) (kind indexer.EventKind, networkID, filterHash string, existed bool) {
	a.mu.Lock()
	sub, ok := a.subs[clientSubID]
	if !ok {
		a.mu.Unlock()
		return 0, "", "", false
	}
	delete(a.subs, clientSubID)
	key := routeKey{kind: sub.kind, networkID: sub.networkID, filterHash: sub.filterHash}
	if set, ok := a.routes[key]; ok {
		delete(set, clientSubID)
		if len(set) == 0 {
			delete(a.routes, key)
		}
	}
	a.mu.Unlock()

	close(sub.done)
	return sub.kind, sub.networkID, sub.filterHash, true
}

// Drain removes every subscription and makes later AddSubscription calls
// fail. It returns the subscriptions it removed, so the caller releases
// exactly those; a concurrent RemoveSubscription owns any it removed first.
func (a *Adapter) Drain() []Subscription {
	a.mu.Lock()
	subs := a.subs
	a.drained = true
	a.subs = make(map[string]*clientSub)
	a.routes = make(map[routeKey]map[string]struct{})
	a.mu.Unlock()

	out := make([]Subscription, 0, len(subs))
	for _, sub := range subs {
		close(sub.done)
		out = append(out, sub.snapshot())
	}
	return out
}

func (sub *clientSub) snapshot() Subscription {
	return Subscription{
		ClientSubID: sub.id,
		Kind:        sub.kind,
		NetworkID:   sub.networkID,
		FilterHash:  sub.filterHash,
	}
}

// Subscription describes one subscription.
type Subscription struct {
	ClientSubID string
	Kind        indexer.EventKind
	NetworkID   string
	FilterHash  string
}

// Count returns the number of active subscriptions.
func (a *Adapter) Count() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.subs)
}

func (a *Adapter) runWriter(sub *clientSub) {
	for {
		select {
		case <-sub.done:
			return
		case payload := <-sub.notify:
			if err := a.writer.WriteSubscriptionNotification(sub.id, payload); err != nil {
				a.logger.Debug().Err(err).Str("clientSubId", sub.id).
					Msg("failed to write subscription notification")
			}
		}
	}
}

// enqueue pushes a payload onto the sub's buffer without blocking. When the
// buffer is full a lossy sub evicts its oldest notification to make room;
// any other sub drops the new one. Returns false if a notification was
// dropped either way.
func enqueue(sub *clientSub, payload json.RawMessage) bool {
	dropped := false
	for {
		select {
		case sub.notify <- payload:
			return !dropped
		default:
		}
		if !isLossy(sub.kind) {
			return false
		}
		select {
		case <-sub.notify:
			dropped = true
		default:
		}
	}
}
