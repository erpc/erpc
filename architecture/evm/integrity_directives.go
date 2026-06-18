package evm

import (
	"strconv"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
)

// integrityCheckSet maps request directives to the set of integrity checks to
// run, bridging the existing directive flags to the unified check ids. This is
// the single place that assembles a CheckSet for the engine; as the
// level/header config model lands, it composes with this rather than replacing
// it.
//
// The engine itself selects which enabled checks apply to the request method,
// so enabling a check here that doesn't apply to the method is a harmless no-op.
func integrityCheckSet(dirs *common.RequestDirectives) integrity.CheckSet {
	cs := integrity.CheckSet{}

	// Always-on sanity check (independent of directives), preserving the
	// original behavior of the standalone logIndex validator.
	cs.Enable("indexMagnitude", nil)

	if dirs == nil {
		return cs
	}

	// Decode-or-reject guard for the aggregate methods, matching the inline
	// validators that rejected a malformed result before any other check.
	cs.Enable("schemaConformance", nil)

	// Receipt structural checks that the previous validator ran unconditionally
	// once any directive was present.
	cs.Enable("sameBlockHash", nil)
	if dirs.ValidateTxHashUniqueness {
		cs.Enable("txHashUniqueness", map[string]string{"strict": "true"})
	} else {
		cs.Enable("txHashUniqueness", nil)
	}

	if dirs.ValidateTransactionIndex {
		cs.Enable("transactionIndexConsistency", nil)
	}
	if dirs.ValidateLogFields {
		cs.Enable("logFieldShapes", nil)
		cs.Enable("logMetadata", nil)
	}
	if dirs.ValidateLogsBloomEmptiness {
		cs.Enable("bloomEmptiness", nil)
	}
	if dirs.ValidateLogsBloomMatch {
		cs.Enable("bloomMatch", nil)
	}
	if dirs.EnforceLogIndexStrictIncrements {
		cs.Enable("logIndexContiguity", nil)
	}

	// Block checks (eth_getBlockByNumber / eth_getBlockByHash).
	if dirs.ValidateTransactionsRoot {
		cs.Enable("transactionsRootConsistency", nil)
	}
	if dirs.ValidateHeaderFieldLengths {
		cs.Enable("headerFieldShapes", nil)
	}
	if dirs.ValidateTransactionFields {
		cs.Enable("txFieldUniqueness", nil)
	}
	if dirs.ValidateTransactionBlockInfo {
		cs.Enable("txBlockInfo", nil)
	}

	// Caller-provided (ground-truth / expected) checks.
	if dirs.ValidationExpectedBlockHash != "" || dirs.ValidationExpectedBlockNumber != nil {
		params := map[string]string{}
		if dirs.ValidationExpectedBlockHash != "" {
			params["hash"] = dirs.ValidationExpectedBlockHash
		}
		if dirs.ValidationExpectedBlockNumber != nil {
			params["number"] = strconv.FormatInt(*dirs.ValidationExpectedBlockNumber, 10)
		}
		cs.Enable("expectedBlock", params)
	}
	if dirs.ReceiptsCountExact != nil || dirs.ReceiptsCountAtLeast != nil {
		params := map[string]string{}
		if dirs.ReceiptsCountExact != nil {
			params["exact"] = strconv.FormatInt(*dirs.ReceiptsCountExact, 10)
		}
		if dirs.ReceiptsCountAtLeast != nil {
			params["atLeast"] = strconv.FormatInt(*dirs.ReceiptsCountAtLeast, 10)
		}
		cs.Enable("receiptsCount", params)
	}
	// Authoritative corroboration (force-fetches the canonical block).
	if dirs.EnforceReceiptCorroboration {
		cs.Enable("receiptVsBlock", nil)
	}
	if dirs.ValidateReceiptTransactionMatch && len(dirs.GroundTruthTransactions) > 0 {
		gt := make([]integrity.GroundTruthTx, len(dirs.GroundTruthTransactions))
		for i, t := range dirs.GroundTruthTransactions {
			if t != nil {
				gt[i] = integrity.GroundTruthTx{Hash: t.Hash, To: t.To, TransactionIndex: t.TransactionIndex}
			}
		}
		params := map[string]string{}
		if dirs.ValidateContractCreation {
			params["contractCreation"] = "true"
		}
		cs["receiptTransactionMatch"] = integrity.CheckConfig{Enabled: true, Params: params, Data: gt}
	}

	return cs
}
