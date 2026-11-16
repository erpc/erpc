// Package evmpubsub is the EVM WebSocket subscription dialect: eth_subscribe
// and eth_unsubscribe, "0x"-hex subscription ids and eth_subscription
// notifications. A leaf package, so clients can use it without importing
// architecture/evm.
package evmpubsub

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

const (
	MethodSubscribe    = "eth_subscribe"
	MethodUnsubscribe  = "eth_unsubscribe"
	MethodNotification = "eth_subscription"
)

// Subscription kinds, the first param of eth_subscribe.
const (
	KindNewHeads               = "newHeads"
	KindLogs                   = "logs"
	KindNewPendingTransactions = "newPendingTransactions"
)

// Bounds of a network's newPendingTransactions relay (spec §13.2).
const (
	PendingRingSize    = 10_000
	PendingMaxKeys     = 100_000
	PendingMaxVariants = 16
	PendingDedupFor    = 60 * time.Second
	PendingSilentMax   = 30 * time.Minute
)

// PendingSilentAfter is the newPendingTransactions RelaySpec.SilentAfter; a
// var so tests can shorten it.
var PendingSilentAfter = util.NewDuration(60 * time.Second)

var pendingRelay = &common.RelaySpec{
	RingSize:    PendingRingSize,
	DedupFor:    PendingDedupFor,
	MaxKeys:     PendingMaxKeys,
	MaxVariants: PendingMaxVariants,
	Key:         PendingTxKey,
	SilentAfter: PendingSilentAfter,
	SilentMax:   PendingSilentMax,
	Placement:   common.PlacementAll,
}

var methods = []string{MethodSubscribe, MethodUnsubscribe}

// Dialect is the EVM common.PubSubDialect.
type Dialect struct{}

var _ common.PubSubDialect = Dialect{}

func (Dialect) Methods() []string { return methods }

func (Dialect) IsSubscribe(method string) bool { return method == MethodSubscribe }

func (Dialect) IsUnsubscribe(method string) bool { return method == MethodUnsubscribe }

// SubscribeKind is the first param; the rest name the variant.
func (Dialect) SubscribeKind(_ string, params []interface{}) (string, []interface{}) {
	if len(params) == 0 {
		return "", nil
	}
	kind, _ := params[0].(string)
	return kind, params[1:]
}

func (Dialect) Kind(kind string) (common.KindSpec, bool) {
	switch kind {
	case KindNewHeads, KindLogs:
		return common.KindSpec{Notification: MethodNotification}, true
	case KindNewPendingTransactions:
		return common.KindSpec{Notification: MethodNotification, Relay: pendingRelay}, true
	}
	return common.KindSpec{}, false
}

// NewSubID is "0x" and 32 random hex digits, as Ethereum clients make them.
func (Dialect) NewSubID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(b), nil
}

// EncodeSubID quotes id; ids are hex, so no escaping is needed.
func (Dialect) EncodeSubID(id string) json.RawMessage {
	return json.RawMessage(`"` + id + `"`)
}

func (Dialect) UnsubscribeID(params []interface{}) (string, error) {
	if len(params) == 0 {
		return "", common.NewErrSubscriptionNotFound("")
	}
	id, ok := params[0].(string)
	if !ok {
		return "", common.NewErrSubscriptionNotFound(fmt.Sprintf("%v", params[0]))
	}
	return id, nil
}

func (Dialect) SubscribeRequest(kind string, variant []interface{}) (string, []interface{}) {
	params := make([]interface{}, 0, 1+len(variant))
	return MethodSubscribe, append(append(params, kind), variant...)
}

func (Dialect) UnsubscribeRequest(_, id string) (string, []interface{}) {
	return MethodUnsubscribe, []interface{}{id}
}

// NotificationKind: an eth_subscription notification does not name its kind.
func (Dialect) NotificationKind(method string) (string, bool) {
	return "", method == MethodNotification
}

// DecodeSubID accepts the JSON string an EVM node names a subscription by.
func (Dialect) DecodeSubID(raw []byte) (string, bool) {
	var id string
	if err := common.SonicCfg.Unmarshal(raw, &id); err != nil {
		return "", false
	}
	return id, true
}

// PendingTxKey is a pending transaction's identity: the hash a string
// payload is, or an object payload's hash, lowercased; "" when it has none.
func PendingTxKey(raw json.RawMessage) string {
	var s string
	if err := common.SonicCfg.Unmarshal(raw, &s); err == nil && s != "" {
		return strings.ToLower(s)
	}
	var obj struct {
		Hash string `json:"hash"`
	}
	if err := common.SonicCfg.Unmarshal(raw, &obj); err == nil {
		return strings.ToLower(obj.Hash)
	}
	return ""
}
