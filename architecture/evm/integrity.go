package evm

import (
	"context"
	"fmt"

	"github.com/erpc/erpc/common"
)

// maxPlausibleEvmIndex bounds logIndex / transactionIndex to a value that is far
// above any real block (block gas limits cap the number of logs and
// transactions per block to the tens of thousands at most) yet far below the
// ~4.29e9 values produced by a signed-32-bit integer underflow. A value at or
// above this is provably impossible on any real chain.
//
// Real incident (Polygon Amoy eth_getTransactionReceipt): an upstream returned
// logIndex "0xfffffff7" (== 4294967287, i.e. -9 read as int32) where the
// canonical values were 0..8.
const maxPlausibleEvmIndex = uint64(1) << 24 // 16,777,216

// indexIntegrityMethods are the methods whose results carry logIndex /
// transactionIndex fields that can be sanity-checked using only the data at
// hand (no external lookups). Keys are lowercased method names.
var indexIntegrityMethods = map[string]bool{
	"eth_gettransactionreceipt": true,
	"eth_getblockreceipts":      true,
	"eth_getlogs":               true,
}

// validateIndexIntegrity converts a response that carries a provably-impossible
// logIndex / transactionIndex into an ErrEndpointContentValidation. It is the
// always-on integrity counterpart to the directive-gated content validators: it
// fires only on values that cannot occur on any real chain, so a corrupt
// upstream is routed around by the normal retry/consensus machinery instead of
// being served — or, when its garbage happens to be byte-larger, used to
// override an honest majority via preferLargerResponses.
func validateIndexIntegrity(ctx context.Context, u common.Upstream, rs *common.NormalizedResponse) error {
	if rs == nil || rs.IsObjectNull() || rs.IsResultEmptyish() {
		return nil
	}
	jrr, err := rs.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return nil
	}
	raw := jrr.GetResultBytes()
	if len(raw) == 0 {
		return nil
	}
	var result interface{}
	if err := common.SonicCfg.Unmarshal(raw, &result); err != nil {
		// Malformed JSON is surfaced by other layers; don't shadow it here.
		return nil
	}
	if field, value, ok := findImplausibleIndex(result); ok {
		return common.NewErrEndpointContentValidation(
			fmt.Errorf("implausible %s %q exceeds maximum plausible block index %d (likely a 32-bit underflow from upstream)", field, value, maxPlausibleEvmIndex),
			u,
		)
	}
	return nil
}

// findImplausibleIndex recursively scans a decoded JSON value for a "logIndex"
// or "transactionIndex" field whose hex-quantity value is at or above the
// maximum any real block could produce. Handles all three result shapes: a
// receipt object, an array of receipts, and an array of logs.
func findImplausibleIndex(v interface{}) (field string, value string, ok bool) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			if k == "logIndex" || k == "transactionIndex" {
				if s, isStr := val.(string); isStr && isImplausibleIndex(s) {
					return k, s, true
				}
				continue
			}
			if f, vv, found := findImplausibleIndex(val); found {
				return f, vv, true
			}
		}
	case []interface{}:
		for _, item := range t {
			if f, vv, found := findImplausibleIndex(item); found {
				return f, vv, true
			}
		}
	}
	return "", "", false
}

func isImplausibleIndex(hexValue string) bool {
	n, err := common.HexToUint64(hexValue)
	if err != nil {
		return false
	}
	return n >= maxPlausibleEvmIndex
}
