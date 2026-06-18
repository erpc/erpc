package consensus

import (
	"context"

	"github.com/erpc/erpc/common"
)

// maxPlausibleEvmLogIndex bounds logIndex / transactionIndex to a value that is
// far above any real block (block gas limits cap the number of logs and
// transactions per block in the tens of thousands at most) yet far below the
// ~4.29e9 values produced by a signed-32-bit integer underflow.
//
// A value at or above this is provably impossible on any real chain and signals
// a corrupt upstream response. For example a buggy node returned logIndex
// "0xfffffff7" (== 4294967287, i.e. -9 read as int32) for a receipt whose
// canonical logIndex values were 0..8.
const maxPlausibleEvmLogIndex = uint64(1) << 24 // 16,777,216

// methodsWithIndexIntegrity enumerates the methods whose results carry
// logIndex / transactionIndex fields that we can sanity-check using only the
// data at hand (no external lookups).
var methodsWithIndexIntegrity = map[string]bool{
	"eth_getTransactionReceipt": true,
	"eth_getBlockReceipts":      true,
	"eth_getLogs":               true,
}

// hasInvalidIntegrity reports whether resp contains a provably-impossible value
// for the given method — currently a logIndex or transactionIndex beyond any
// physically possible block.
//
// This is a cheap, self-contained sanity check, NOT a correctness/consensus
// check: it only fires on values that cannot occur on any real chain. It exists
// so that the preferLargerResponses behavior does not treat a corrupt-but-larger
// response (e.g. one whose logIndex hex strings are longer because they hold
// underflowed garbage) as a legitimately larger response that should win or
// dispute against an honest majority.
func hasInvalidIntegrity(ctx context.Context, resp *common.NormalizedResponse, method string) bool {
	if resp == nil || !methodsWithIndexIntegrity[method] {
		return false
	}
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return false
	}
	raw := jrr.GetResultBytes()
	if len(raw) == 0 {
		return false
	}
	var result interface{}
	if err := common.SonicCfg.Unmarshal(raw, &result); err != nil {
		// Unparseable result — leave it to the normal grouping/classification
		// path; do not claim invalid integrity on a parse failure.
		return false
	}
	return walkForImplausibleIndex(result)
}

// walkForImplausibleIndex recursively scans a decoded JSON value for any
// "logIndex" or "transactionIndex" field whose value is implausibly large.
func walkForImplausibleIndex(v interface{}) bool {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			if k == "logIndex" || k == "transactionIndex" {
				if isImplausibleIndex(val) {
					return true
				}
				continue
			}
			if walkForImplausibleIndex(val) {
				return true
			}
		}
	case []interface{}:
		for _, item := range t {
			if walkForImplausibleIndex(item) {
				return true
			}
		}
	}
	return false
}

// isImplausibleIndex returns true when a hex-quantity index value is at or above
// the maximum value any real block could produce.
func isImplausibleIndex(val interface{}) bool {
	s, ok := val.(string)
	if !ok {
		return false
	}
	n, err := common.HexToUint64(s)
	if err != nil {
		// Could not parse as a hex quantity — don't claim invalid.
		return false
	}
	return n >= maxPlausibleEvmLogIndex
}
