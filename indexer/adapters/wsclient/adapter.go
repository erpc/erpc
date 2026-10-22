// Package wsclient holds the subscriptions of one client WebSocket
// connection. Each subscription is a tap that writes its own notifications.
package wsclient

import (
	"errors"
	"sync"
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
}

type clientSub struct {
	id        string
	kind      EventKind
	networkID string

	// opened is closed once the client has the subscription id: nothing is
	// written before it, as geth sends the id first.
	opened   chan struct{}
	openOnce sync.Once
	// stop ends the subscription's tap.
	stop func()
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
func (a *Adapter) AddSubscription(clientSubID, networkID string, kind EventKind, maxSubs int, stop func()) (opened <-chan struct{}, err error) {
	sub := &clientSub{id: clientSubID, kind: kind, networkID: networkID, opened: make(chan struct{}), stop: stop}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.drained {
		return nil, ErrClosed
	}
	if len(a.subs) >= maxSubs {
		return nil, ErrLimitExceeded
	}
	a.subs[clientSubID] = sub
	return sub.opened, nil
}

// RemoveSubscription removes a subscription and stops its tap.
func (a *Adapter) RemoveSubscription(clientSubID string) (kind EventKind, existed bool) {
	a.mu.Lock()
	sub, ok := a.subs[clientSubID]
	delete(a.subs, clientSubID)
	a.mu.Unlock()
	if !ok {
		return 0, false
	}
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
		sub.stop()
		out = append(out, Subscription{ClientSubID: sub.id, Kind: sub.kind, NetworkID: sub.networkID})
	}
	return out
}

// Subscription describes one subscription.
type Subscription struct {
	ClientSubID string
	Kind        EventKind
	NetworkID   string
}

// Count returns the number of active subscriptions.
func (a *Adapter) Count() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.subs)
}
