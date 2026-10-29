// Package wsclient holds the subscriptions of one client WebSocket
// connection. Each subscription is a tap that writes its own notifications.
package wsclient

import (
	"errors"
	"sync"
	"sync/atomic"
)

// ErrClosed is returned by AddSubscription once the adapter was drained.
var ErrClosed = errors.New("connection closed")

// ErrLimitExceeded is returned by AddSubscription when the connection
// already holds the maximum number of subscriptions.
var ErrLimitExceeded = errors.New("subscription limit exceeded")

// Adapter is the subscription set of one client connection.
type Adapter struct {
	mu      sync.RWMutex
	drained bool
	subs    map[string]*clientSub // clientSubId -> sub
	// ended are the ids of subscriptions their stream ended, for the
	// connection's life; the oldest leave first past maxEnded (order).
	ended map[string]struct{}
	order []string
}

// maxEnded bounds the ended ids a connection keeps.
const maxEnded = 1024

// End removes a subscription its stream ended, as RemoveSubscription
// does, and keeps its id for Ended. It reports whether it removed one.
func (a *Adapter) End(clientSubID string) bool {
	a.mu.Lock()
	sub, ok := a.subs[clientSubID]
	if ok {
		delete(a.subs, clientSubID)
		if a.ended == nil {
			a.ended = make(map[string]struct{})
		}
		if len(a.order) >= maxEnded {
			delete(a.ended, a.order[0])
			a.order = append(a.order[:0], a.order[1:]...)
		}
		a.ended[clientSubID] = struct{}{}
		a.order = append(a.order, clientSubID)
	}
	a.mu.Unlock()
	if ok {
		sub.removed.Store(true)
		sub.stop()
	}
	return ok
}

// Ended reports whether End removed clientSubID.
func (a *Adapter) Ended(clientSubID string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.ended[clientSubID]
	return ok
}

type clientSub struct {
	id        string
	kind      EventKind
	networkID string

	// opened is closed once the client has the subscription id: nothing is
	// written before it, as geth sends the id first.
	opened   chan struct{}
	openOnce sync.Once
	// removed is set before the unsubscribe answer is written; a
	// notification written after that answer sees it and is dropped.
	removed atomic.Bool
	// stop ends the subscription's tap.
	stop func()
}

func (s *clientSub) describe() Subscription {
	return Subscription{ClientSubID: s.id, Kind: s.kind, NetworkID: s.networkID, removed: &s.removed}
}

// New creates the adapter of a client connection.
func New() *Adapter {
	return &Adapter{subs: make(map[string]*clientSub)}
}

// Open lets clientSubID's notifications flow, once the response carrying
// its id is written.
func (a *Adapter) Open(clientSubID string) {
	a.mu.RLock()
	sub := a.subs[clientSubID]
	a.mu.RUnlock()
	if sub != nil {
		sub.openOnce.Do(func() { close(sub.opened) })
	}
}

// AddSubscription registers a subscription whose tap writes after opened
// is closed by Open; stop ends that tap when the subscription is removed.
// It fails with ErrLimitExceeded when the connection already holds
// maxSubs, and with ErrClosed once the adapter was drained.
func (a *Adapter) AddSubscription(clientSubID, networkID string, kind EventKind, maxSubs int, stop func()) (Subscription, <-chan struct{}, error) {
	sub := &clientSub{id: clientSubID, kind: kind, networkID: networkID, opened: make(chan struct{}), stop: stop}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.drained {
		return Subscription{}, nil, ErrClosed
	}
	if len(a.subs) >= maxSubs {
		return Subscription{}, nil, ErrLimitExceeded
	}
	a.subs[clientSubID] = sub
	return sub.describe(), sub.opened, nil
}

// RemoveSubscription marks a subscription removed, removes it and stops
// its tap.
func (a *Adapter) RemoveSubscription(clientSubID string) (kind EventKind, existed bool) {
	a.mu.Lock()
	sub, ok := a.subs[clientSubID]
	delete(a.subs, clientSubID)
	a.mu.Unlock()
	if !ok {
		return 0, false
	}
	sub.removed.Store(true)
	sub.stop()
	return sub.kind, true
}

// Drain removes every subscription and makes later AddSubscription calls
// fail. It returns the subscriptions it removed; a concurrent
// RemoveSubscription owns any it removed first.
func (a *Adapter) Drain() []Subscription {
	a.mu.Lock()
	subs := a.subs
	a.drained = true
	a.subs = make(map[string]*clientSub)
	a.mu.Unlock()

	out := make([]Subscription, 0, len(subs))
	for _, sub := range subs {
		sub.removed.Store(true)
		sub.stop()
		out = append(out, sub.describe())
	}
	return out
}

// Subscription describes one subscription.
type Subscription struct {
	ClientSubID string
	Kind        EventKind
	NetworkID   string

	removed *atomic.Bool
}

// Removed reports whether the subscription was removed: its tap must not
// write any more.
func (s Subscription) Removed() bool {
	return s.removed.Load()
}

// MarkRemoved stops the subscription's writes, as removal does, while the
// caller holds the connection's write lock after its last message.
func (s Subscription) MarkRemoved() { s.removed.Store(true) }

// Count returns the number of active subscriptions.
func (a *Adapter) Count() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.subs)
}
