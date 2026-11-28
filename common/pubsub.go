package common

import (
	"encoding/json"
	"time"

	"github.com/erpc/erpc/util"
)

// PubSubDialect is how an architecture's clients and upstreams speak
// WebSocket subscriptions: the methods, the subscription ids, the
// notification envelope and the kinds a subscribe opens. Ids are kept as
// strings, the dialect's key for them, whatever their JSON form.
type PubSubDialect interface {
	// Methods are the subscribe and unsubscribe methods.
	Methods() []string
	IsSubscribe(method string) bool
	IsUnsubscribe(method string) bool
	// SubscribeKind is the kind a subscribe request opens and the params
	// that name its variant; method is one IsSubscribe accepts. A kind the
	// dialect does not know is returned as is, for Kind to refuse.
	SubscribeKind(method string, params []interface{}) (kind string, variant []interface{})
	// Kind is the spec of kind; false if the dialect has no such kind.
	Kind(kind string) (KindSpec, bool)

	// NewSubID is a fresh client subscription id.
	NewSubID() (string, error)
	// EncodeSubID is id as the JSON a client gets it in.
	EncodeSubID(id string) json.RawMessage
	// UnsubscribeID is the subscription id an unsubscribe request names; an
	// ErrSubscriptionNotFound when the params name none.
	UnsubscribeID(params []interface{}) (string, error)

	// SubscribeRequest is the upstream request subscribing kind with variant.
	SubscribeRequest(kind string, variant []interface{}) (method string, params []interface{})
	// UnsubscribeRequest is the upstream request cancelling subscription id
	// of kind; kind is "" when only a notification names the subscription,
	// as NotificationKind returns it.
	UnsubscribeRequest(kind, id string) (method string, params []interface{})
	// NotificationKind reports whether method is an upstream subscription
	// notification, and the kind it names ("" if it names none).
	NotificationKind(method string) (kind string, ok bool)
	// DecodeSubID is the key of an upstream subscription id in JSON.
	DecodeSubID(raw []byte) (string, bool)
}

// KindSpec is one subscription kind of a dialect.
type KindSpec struct {
	// Notification is the method of the kind's notifications to clients.
	Notification string
	// Relay is set for a kind relayed from push sources through the
	// network's relay feed; nil: the chain tracker serves it.
	Relay *RelaySpec
}

// RelaySpec bounds one relayed kind on one network.
type RelaySpec struct {
	// RingSize is how many of its newest messages each variant keeps.
	RingSize int
	// DedupFor is how long a message's key suppresses its repeats, from
	// its first add to any variant of the kind.
	DedupFor time.Duration
	// MaxKeys bounds the keys held for dedup across the kind's variants.
	MaxKeys int
	// MaxVariants bounds the kind's variants open at once.
	MaxVariants int
	// Key is a message's identity for dedup; "" keeps it without dedup.
	Key func(raw json.RawMessage) string
	// SilentAfter: a topic of the kind with no message this long, while
	// heads carry transactions, is subscribed again on that source; each
	// further silence waits twice as long, up to SilentMax. Nil: no
	// silence rule. Shared so tests can shorten it.
	SilentAfter *util.Duration
	SilentMax   time.Duration
	Placement   Placement
}

// Placement is which push sources a relay topic is set on.
type Placement uint8

const (
	// PlacementAll sets the topic on every admitted push source.
	PlacementAll Placement = iota
)

// IsPubSubMethod reports whether some architecture subscribes or
// unsubscribes with method.
func IsPubSubMethod(method string) bool {
	for _, h := range ArchitectureRegistry {
		if d := h.PubSub(); d != nil && (d.IsSubscribe(method) || d.IsUnsubscribe(method)) {
			return true
		}
	}
	return false
}
