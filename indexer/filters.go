package indexer

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"

	"github.com/erpc/erpc/common"
)

// BuildParamsKey returns a short hash of eth_subscribe params that is stable
// across sources and processes. encoding/json is used because it sorts map
// keys.
func BuildParamsKey(params []interface{}) string {
	data, err := json.Marshal(params)
	if err != nil {
		return fmt.Sprintf("%v", params)
	}
	h := fnv.New64a()
	_, _ = h.Write(data)
	return fmt.Sprintf("%x", h.Sum64())
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
