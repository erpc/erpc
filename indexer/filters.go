package indexer

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"hash/fnv"

	"github.com/erpc/erpc/common"
)

// BuildParamsKey returns a stable short hash of eth_subscribe params. The
// same (subType, params) tuple always hashes to the same key across
// processes, so the key doubles as a fan-out lookup across sources and
// across pods.
func BuildParamsKey(params []interface{}) string {
	data, err := common.SonicCfg.Marshal(params)
	if err != nil {
		// Fall back to Go's default formatting. Worse than JSON for
		// cross-process stability, but only hit on marshal errors that
		// would already have broken the upstream subscribe call.
		return fmt.Sprintf("%v", params)
	}
	h := fnv.New64a()
	_, _ = h.Write(data)
	return fmt.Sprintf("%x", h.Sum64())
}

// ExtractSubscriptionType pulls the subscription type (e.g. "newHeads",
// "logs") from the first element of an eth_subscribe params array.
// Returns "" when params is empty or the first element isn't a string.
func ExtractSubscriptionType(params []interface{}) string {
	if len(params) == 0 {
		return ""
	}
	st, _ := params[0].(string)
	return st
}

// ExtractClientSubID pulls the client-facing subscription ID from an
// eth_unsubscribe params array. Returns a typed "subscription not found"
// error when the ID is missing or not a string.
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

// GenerateClientSubID creates a cryptographically random subscription ID
// formatted as "0x" + 32 hex characters (16 random bytes). The format
// matches what Ethereum clients emit so consumers can treat our IDs as
// opaque strings.
func GenerateClientSubID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(b), nil
}
