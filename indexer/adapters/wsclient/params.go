package wsclient

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
)

// EventKind is the eth_subscribe subscription type of a client
// subscription.
type EventKind uint8

const (
	KindUnknown EventKind = iota
	KindNewHead
	KindLog
	KindPendingTx
)

// String returns the matching eth_subscribe subscription type.
func (k EventKind) String() string {
	switch k {
	case KindNewHead:
		return indexer.SubTypeNewHeads
	case KindLog:
		return indexer.SubTypeLogs
	case KindPendingTx:
		return indexer.SubTypeNewPendingTransactions
	default:
		return "unknown"
	}
}

// ExtractSubscriptionType returns the subscription type of eth_subscribe
// params, or "" if the first param is missing or not a string.
func ExtractSubscriptionType(params []interface{}) string {
	if len(params) == 0 {
		return ""
	}
	st, _ := params[0].(string)
	return st
}

// ExtractClientSubID returns the subscription ID of eth_unsubscribe params,
// or a subscription-not-found error if it is missing or not a string.
func ExtractClientSubID(params []interface{}) (string, error) {
	if len(params) == 0 {
		return "", common.NewErrSubscriptionNotFound("")
	}
	id, ok := params[0].(string)
	if !ok {
		return "", common.NewErrSubscriptionNotFound(fmt.Sprintf("%v", params[0]))
	}
	return id, nil
}

// GenerateClientSubID returns a random subscription ID in the "0x" + 32 hex
// format Ethereum clients use.
func GenerateClientSubID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(b), nil
}
