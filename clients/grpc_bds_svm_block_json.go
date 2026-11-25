package clients

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/blockchain-data-standards/manifesto/svm"
	"github.com/blockchain-data-standards/manifesto/svm/parse"
)

// svmBlockJson renders the Solana getBlock result for a Prism block straight
// from the proto into JSON bytes: encoding "json", or "jsonParsed" with
// Agave's instruction parsers run in-process. It emits what
// svm.BlockToJsonRpc / svm.BlockToJsonRpcParsed (after parse.AttachToBlock)
// emit, without building a map tree and marshaling it: no intermediate
// json.Marshal / json.Decode of parsed instructions, and every account key
// base58-encoded once per transaction instead of up to four times. Keys come
// out in Agave's field order.
//
// nullBlockTime / nullBlockHeight render those keys as null for slots where
// Agave's history records no value.
func svmBlockJson(block *svm.ConfirmedBlock, signatures [][]byte, parsed, nullBlockTime, nullBlockHeight bool) []byte {
	if block == nil {
		return []byte("null")
	}
	txs := block.Transactions
	dst := make([]byte, 0, 512+len(signatures)*92+len(txs)*svmTxSizeHint(parsed))

	dst = append(dst, `{"previousBlockhash":`...)
	dst = appendBase58String(dst, block.PreviousBlockhash)
	dst = append(dst, `,"blockhash":`...)
	dst = appendBase58String(dst, block.Blockhash)
	dst = append(dst, `,"parentSlot":`...)
	dst = strconv.AppendUint(dst, block.ParentSlot, 10)

	switch {
	case signatures != nil:
		dst = append(dst, `,"signatures":[`...)
		for i, s := range signatures {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendBase58String(dst, s)
		}
		dst = append(dst, ']')
	case txs != nil:
		dst = append(dst, `,"transactions":[`...)
		dst = appendSvmTransactions(dst, txs, parsed)
		dst = append(dst, ']')
	}

	if block.Rewards != nil {
		dst = append(dst, `,"rewards":`...)
		dst = appendSvmRewards(dst, block.Rewards)
	}
	if block.NumRewardPartitions != nil {
		dst = append(dst, `,"numRewardPartitions":`...)
		dst = strconv.AppendUint(dst, *block.NumRewardPartitions, 10)
	}
	dst = append(dst, `,"blockTime":`...)
	if block.BlockTime != nil && !nullBlockTime {
		dst = strconv.AppendInt(dst, *block.BlockTime, 10)
	} else {
		dst = append(dst, "null"...)
	}
	dst = append(dst, `,"blockHeight":`...)
	if block.BlockHeight != nil && !nullBlockHeight {
		dst = strconv.AppendUint(dst, *block.BlockHeight, 10)
	} else {
		dst = append(dst, "null"...)
	}
	return append(dst, '}')
}

// svmTxSizeHint is the mean rendered transaction size on a mainnet block
// (321 transactions: 3.35 MB jsonParsed, 2.15 MB json), so one block
// renders with no buffer regrowth in the common case.
func svmTxSizeHint(parsed bool) int {
	if parsed {
		return 11 << 10
	}
	return 7 << 10
}

// svmParallelMinTxs is the block size from which transactions render on
// several goroutines. Below it the fan-out costs more than it saves.
const svmParallelMinTxs = 64

// appendSvmTransactions appends the comma-separated transactions[] entries.
// Each transaction renders independently, so a large block is split into
// contiguous chunks rendered concurrently and concatenated in order.
func appendSvmTransactions(dst []byte, txs []*svm.ConfirmedTransaction, parsed bool) []byte {
	workers := min(runtime.GOMAXPROCS(0), len(txs)/(svmParallelMinTxs/2))
	if len(txs) < svmParallelMinTxs || workers < 2 {
		var keys []string
		for i, ct := range txs {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst, keys = appendSvmTransaction(dst, ct, parsed, keys)
		}
		return dst
	}

	chunk := (len(txs) + workers - 1) / workers
	parts := make([][]byte, workers)
	var wg sync.WaitGroup
	for w := range workers {
		lo := w * chunk
		hi := min(lo+chunk, len(txs))
		if lo >= hi {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 0, (hi-lo)*svmTxSizeHint(parsed))
			var keys []string
			for i := lo; i < hi; i++ {
				if i > lo {
					buf = append(buf, ',')
				}
				buf, keys = appendSvmTransaction(buf, txs[i], parsed, keys)
			}
			parts[w] = buf
		}()
	}
	wg.Wait()
	for i, p := range parts {
		if len(p) == 0 {
			continue
		}
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, p...)
	}
	return dst
}

// appendSvmTransaction appends one transactions[] entry. keys is scratch
// space for the merged key list, reused across transactions.
func appendSvmTransaction(dst []byte, ct *svm.ConfirmedTransaction, parsed bool, keys []string) ([]byte, []string) {
	if ct == nil {
		return append(dst, "null"...), keys
	}
	version := ct.Version
	isV0 := version != nil && *version == "0"

	// Merged key list (static ++ loadedWritable ++ loadedReadonly),
	// base58-encoded once and shared by accountKeys, instruction accounts and
	// the parsers.
	keys = keys[:0]
	if ct.Transaction != nil && ct.Transaction.Message != nil {
		for _, k := range ct.Transaction.Message.AccountKeys {
			keys = append(keys, base58String(k))
		}
	}
	numStatic := len(keys)
	var loadedW, loadedR [][]byte
	if ct.Meta != nil {
		loadedW, loadedR = ct.Meta.LoadedWritableAddresses, ct.Meta.LoadedReadonlyAddresses
		if parsed {
			for _, k := range loadedW {
				keys = append(keys, base58String(k))
			}
			for _, k := range loadedR {
				keys = append(keys, base58String(k))
			}
		}
	}

	dst = append(dst, '{')
	if t := ct.Transaction; t != nil {
		dst = append(dst, `"transaction":{"signatures":[`...)
		for i, s := range t.Signatures {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendBase58String(dst, s)
		}
		dst = append(dst, ']')
		if m := t.Message; m != nil {
			dst = append(dst, `,"message":`...)
			if parsed {
				dst = appendSvmParsedMessage(dst, m, keys, numStatic, len(loadedW), isV0)
			} else {
				dst = appendSvmMessage(dst, m, keys, isV0)
			}
		}
		dst = append(dst, "},"...)
	}
	dst = append(dst, `"meta":`...)
	if ct.Meta != nil {
		dst = appendSvmMeta(dst, ct.Meta, parsed, keys)
	} else {
		dst = append(dst, "null"...)
	}
	if version != nil {
		dst = append(dst, `,"version":`...)
		if *version == "legacy" {
			dst = append(dst, `"legacy"`...)
		} else if n, err := strconv.ParseUint(*version, 10, 32); err == nil {
			dst = strconv.AppendUint(dst, n, 10)
		} else {
			dst = appendJsonString(dst, *version)
		}
	}
	return append(dst, '}'), keys
}

// appendSvmMessage renders transaction.message under encoding "json". keys
// holds the static keys already encoded.
func appendSvmMessage(dst []byte, m *svm.Message, keys []string, isV0 bool) []byte {
	dst = append(dst, '{')
	if h := m.Header; h != nil {
		dst = append(dst, `"header":{"numRequiredSignatures":`...)
		dst = strconv.AppendUint(dst, uint64(h.NumRequiredSignatures), 10)
		dst = append(dst, `,"numReadonlySignedAccounts":`...)
		dst = strconv.AppendUint(dst, uint64(h.NumReadonlySignedAccounts), 10)
		dst = append(dst, `,"numReadonlyUnsignedAccounts":`...)
		dst = strconv.AppendUint(dst, uint64(h.NumReadonlyUnsignedAccounts), 10)
		dst = append(dst, "},"...)
	}
	dst = append(dst, `"accountKeys":[`...)
	for i, k := range keys {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = appendQuoted(dst, k)
	}
	dst = append(dst, `],"recentBlockhash":`...)
	dst = appendBase58String(dst, m.RecentBlockhash)
	dst = append(dst, `,"instructions":[`...)
	for i, ci := range m.Instructions {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = appendSvmCompiledInstruction(dst, ci)
	}
	dst = append(dst, ']')
	// A v0 message carries the key even with zero lookups; others only when
	// they have lookups.
	if len(m.AddressTableLookups) > 0 || isV0 {
		dst = appendSvmLookups(dst, m.AddressTableLookups)
	}
	if m.TransactionConfig != nil {
		dst = appendSvmTransactionConfig(dst, m.TransactionConfig)
	}
	return append(dst, '}')
}

// appendSvmParsedMessage renders transaction.message under "jsonParsed":
// accountKeys as {pubkey, writable, signer, source} over the merged list, no
// header, and every instruction parsed or partiallyDecoded.
func appendSvmParsedMessage(dst []byte, m *svm.Message, keys []string, numStatic, numLoadedW int, isV0 bool) []byte {
	var reqSigs, roSigned, roUnsigned int
	if m.Header != nil {
		reqSigs = int(m.Header.NumRequiredSignatures)
		roSigned = int(m.Header.NumReadonlySignedAccounts)
		roUnsigned = int(m.Header.NumReadonlyUnsignedAccounts)
	}
	// Writability is header math, then Agave's demotion: reserved keys, and
	// keys called as a top-level program unless the upgradeable loader is
	// among the message's keys.
	upgradeableLoaderPresent := false
	for _, k := range keys {
		if k == svm.BpfUpgradeableLoaderID {
			upgradeableLoaderPresent = true
			break
		}
	}
	demoted := func(i int, key string) bool {
		if svmReservedAccountKeys[key] {
			return true
		}
		if upgradeableLoaderPresent {
			return false
		}
		for _, ix := range m.Instructions {
			if ix != nil && int(ix.ProgramIdIndex) == i {
				return true
			}
		}
		return false
	}

	dst = append(dst, `{"accountKeys":[`...)
	for i, k := range keys {
		if i > 0 {
			dst = append(dst, ',')
		}
		var signer, writable bool
		source := `"transaction"}`
		switch {
		case i < numStatic:
			signer = i < reqSigs
			if signer {
				writable = i < reqSigs-roSigned
			} else {
				writable = i < numStatic-roUnsigned
			}
			writable = writable && !demoted(i, k)
		case i < numStatic+numLoadedW:
			writable = !demoted(i, k)
			source = `"lookupTable"}`
		default:
			source = `"lookupTable"}`
		}
		dst = append(dst, `{"pubkey":`...)
		dst = appendQuoted(dst, k)
		dst = append(dst, `,"writable":`...)
		dst = strconv.AppendBool(dst, writable)
		dst = append(dst, `,"signer":`...)
		dst = strconv.AppendBool(dst, signer)
		dst = append(dst, `,"source":`...)
		dst = append(dst, source...)
	}
	dst = append(dst, `],"recentBlockhash":`...)
	dst = appendBase58String(dst, m.RecentBlockhash)
	dst = append(dst, `,"instructions":[`...)
	for i, ci := range m.Instructions {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = appendSvmParsedInstruction(dst, ci, keys)
	}
	dst = append(dst, ']')
	// Parsed form keys lookups on the version alone.
	if isV0 {
		dst = appendSvmLookups(dst, m.AddressTableLookups)
	}
	if m.TransactionConfig != nil {
		dst = appendSvmTransactionConfig(dst, m.TransactionConfig)
	}
	return append(dst, '}')
}

func appendSvmLookups(dst []byte, lookups []*svm.AddressTableLookup) []byte {
	dst = append(dst, `,"addressTableLookups":[`...)
	for i, l := range lookups {
		if i > 0 {
			dst = append(dst, ',')
		}
		if l == nil {
			dst = append(dst, "null"...)
			continue
		}
		dst = append(dst, `{"accountKey":`...)
		dst = appendBase58String(dst, l.AccountKey)
		dst = append(dst, `,"writableIndexes":`...)
		dst = appendByteIndexes(dst, l.WritableIndexes)
		dst = append(dst, `,"readonlyIndexes":`...)
		dst = appendByteIndexes(dst, l.ReadonlyIndexes)
		dst = append(dst, '}')
	}
	return append(dst, ']')
}

func appendSvmTransactionConfig(dst []byte, c *svm.TransactionConfig) []byte {
	dst = append(dst, `,"transactionConfig":{"priorityFee":`...)
	dst = appendOptUint(dst, c.PriorityFee)
	dst = append(dst, `,"computeUnitLimit":`...)
	dst = appendOptUint32(dst, c.ComputeUnitLimit)
	dst = append(dst, `,"loadedAccountsDataSizeLimit":`...)
	dst = appendOptUint32(dst, c.LoadedAccountsDataSizeLimit)
	dst = append(dst, `,"heapSize":`...)
	dst = appendOptUint32(dst, c.HeapSize)
	return append(dst, '}')
}

// appendSvmCompiledInstruction renders one instruction under "json".
func appendSvmCompiledInstruction(dst []byte, ci *svm.CompiledInstruction) []byte {
	if ci == nil {
		return append(dst, "null"...)
	}
	dst = append(dst, `{"programIdIndex":`...)
	dst = strconv.AppendUint(dst, uint64(ci.ProgramIdIndex), 10)
	dst = append(dst, `,"accounts":`...)
	dst = appendByteIndexes(dst, ci.Accounts)
	dst = append(dst, `,"data":`...)
	dst = appendBase58String(dst, ci.Data)
	dst = append(dst, `,"stackHeight":`...)
	dst = appendOptUint32(dst, ci.StackHeight)
	return append(dst, '}')
}

// appendSvmParsedInstruction renders one instruction under "jsonParsed":
// Agave's ParsedInstruction when a parser covers it, else partiallyDecoded
// over the merged key list. It runs the parser directly — what
// parse.AttachToBlock does, without the attach round trip through
// CompiledInstruction.Parsed — so the result never depends on a Parsed value
// the reader sent.
func appendSvmParsedInstruction(dst []byte, ci *svm.CompiledInstruction, keys []string) []byte {
	if ci == nil {
		return append(dst, "null"...)
	}
	if ix := svmParseInstruction(ci, keys); ix != nil {
		dst = append(dst, `{"program":`...)
		dst = appendJsonString(dst, ix.Program)
		dst = append(dst, `,"programId":`...)
		dst = appendQuoted(dst, ix.ProgramID)
		dst = append(dst, `,"parsed":`...)
		dst = append(dst, ix.Parsed...)
		dst = append(dst, `,"stackHeight":`...)
		dst = appendOptUint32(dst, ix.StackHeight)
		return append(dst, '}')
	}
	dst = append(dst, `{"programId":`...)
	dst = appendQuoted(dst, keyAtOrEmpty(keys, int(ci.ProgramIdIndex)))
	dst = append(dst, `,"accounts":[`...)
	for i, idx := range ci.Accounts {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = appendQuoted(dst, keyAtOrEmpty(keys, int(idx)))
	}
	dst = append(dst, `],"data":`...)
	dst = appendBase58String(dst, ci.Data)
	dst = append(dst, `,"stackHeight":`...)
	dst = appendOptUint32(dst, ci.StackHeight)
	return append(dst, '}')
}

// svmParseInstruction is parse.AttachToBlock's per-instruction step: nil when
// an index is out of range or no parser covers the instruction.
func svmParseInstruction(ci *svm.CompiledInstruction, keys []string) *parse.Instruction {
	if int(ci.ProgramIdIndex) >= len(keys) {
		return nil
	}
	accounts := make([]string, len(ci.Accounts))
	for i, idx := range ci.Accounts {
		if int(idx) >= len(keys) {
			return nil
		}
		accounts[i] = keys[idx]
	}
	ix, err := parse.Parse(keys[ci.ProgramIdIndex], ci.Data, accounts, ci.StackHeight)
	if err != nil || !json.Valid(ix.Parsed) {
		return nil
	}
	return ix
}

func keyAtOrEmpty(keys []string, i int) string {
	if i >= 0 && i < len(keys) {
		return keys[i]
	}
	return ""
}

// appendSvmMeta renders transaction.meta. keys is the merged key list (only
// read under jsonParsed, for inner instructions).
func appendSvmMeta(dst []byte, m *svm.TransactionStatusMeta, parsed bool, keys []string) []byte {
	// err is stored as its JSON text; splice it when it is valid JSON, else
	// render the text as a string.
	dst = append(dst, `{"err":`...)
	var errJson []byte
	if m.Err != nil {
		if json.Valid([]byte(*m.Err)) {
			errJson = []byte(*m.Err)
		} else {
			errJson = appendJsonString(nil, *m.Err)
		}
		dst = append(dst, errJson...)
		dst = append(dst, `,"status":{"Err":`...)
		dst = append(dst, errJson...)
		dst = append(dst, '}')
	} else {
		dst = append(dst, `null,"status":{"Ok":null}`...)
	}
	dst = append(dst, `,"fee":`...)
	dst = strconv.AppendUint(dst, m.Fee, 10)
	dst = append(dst, `,"preBalances":`...)
	dst = appendUint64s(dst, m.PreBalances)
	dst = append(dst, `,"postBalances":`...)
	dst = appendUint64s(dst, m.PostBalances)

	dst = append(dst, `,"innerInstructions":`...)
	if m.InnerInstructionsNone {
		dst = append(dst, "null"...)
	} else {
		dst = append(dst, '[')
		for i, ii := range m.InnerInstructions {
			if i > 0 {
				dst = append(dst, ',')
			}
			if ii == nil {
				dst = append(dst, "null"...)
				continue
			}
			dst = append(dst, `{"index":`...)
			dst = strconv.AppendUint(dst, uint64(ii.Index), 10)
			dst = append(dst, `,"instructions":[`...)
			for j, ci := range ii.Instructions {
				if j > 0 {
					dst = append(dst, ',')
				}
				if parsed {
					dst = appendSvmParsedInstruction(dst, ci, keys)
				} else {
					dst = appendSvmCompiledInstruction(dst, ci)
				}
			}
			dst = append(dst, "]}"...)
		}
		dst = append(dst, ']')
	}

	dst = append(dst, `,"logMessages":`...)
	if m.LogMessagesNone {
		dst = append(dst, "null"...)
	} else {
		dst = append(dst, '[')
		for i, l := range m.LogMessages {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJsonString(dst, l)
		}
		dst = append(dst, ']')
	}

	dst = append(dst, `,"preTokenBalances":`...)
	if m.PreTokenBalancesNone {
		dst = append(dst, "null"...)
	} else {
		dst = appendSvmTokenBalances(dst, m.PreTokenBalances)
	}
	dst = append(dst, `,"postTokenBalances":`...)
	if m.PostTokenBalancesNone {
		dst = append(dst, "null"...)
	} else {
		dst = appendSvmTokenBalances(dst, m.PostTokenBalances)
	}

	dst = append(dst, `,"rewards":`...)
	if m.RewardsNone {
		dst = append(dst, "null"...)
	} else {
		dst = appendSvmRewards(dst, m.Rewards)
	}

	// jsonParsed merges loaded addresses into accountKeys and omits them here.
	if !parsed {
		dst = append(dst, `,"loadedAddresses":{"writable":`...)
		dst = appendBase58Strings(dst, m.LoadedWritableAddresses)
		dst = append(dst, `,"readonly":`...)
		dst = appendBase58Strings(dst, m.LoadedReadonlyAddresses)
		dst = append(dst, '}')
	}

	if rd := m.ReturnData; rd != nil {
		dst = append(dst, `,"returnData":{"programId":`...)
		dst = appendBase58String(dst, rd.ProgramId)
		dst = append(dst, `,"data":["`...)
		dst = base64.StdEncoding.AppendEncode(dst, rd.Data)
		dst = append(dst, `","base64"]}`...)
	}
	if m.ComputeUnitsConsumed != nil {
		dst = append(dst, `,"computeUnitsConsumed":`...)
		dst = strconv.AppendUint(dst, *m.ComputeUnitsConsumed, 10)
	}
	if m.CostUnits != nil {
		dst = append(dst, `,"costUnits":`...)
		dst = strconv.AppendUint(dst, *m.CostUnits, 10)
	}
	return append(dst, '}')
}

func appendSvmTokenBalances(dst []byte, tbs []*svm.TokenBalance) []byte {
	dst = append(dst, '[')
	for i, tb := range tbs {
		if i > 0 {
			dst = append(dst, ',')
		}
		if tb == nil {
			dst = append(dst, "null"...)
			continue
		}
		dst = append(dst, `{"accountIndex":`...)
		dst = strconv.AppendUint(dst, uint64(tb.AccountIndex), 10)
		dst = append(dst, `,"mint":`...)
		dst = appendBase58String(dst, tb.Mint)
		if a := tb.UiTokenAmount; a != nil {
			dst = append(dst, `,"uiTokenAmount":{"uiAmount":`...)
			dst = appendSvmUiAmount(dst, a)
			dst = append(dst, `,"decimals":`...)
			dst = strconv.AppendUint(dst, uint64(a.Decimals), 10)
			dst = append(dst, `,"amount":`...)
			dst = appendJsonString(dst, a.Amount)
			dst = append(dst, `,"uiAmountString":`...)
			if a.UiAmountString != nil {
				dst = appendJsonString(dst, *a.UiAmountString)
			} else {
				dst = appendJsonString(dst, svmShiftDecimalString(a.Amount, a.Decimals))
			}
			dst = append(dst, '}')
		}
		if tb.Owner != nil {
			dst = append(dst, `,"owner":`...)
			dst = appendBase58String(dst, tb.Owner)
		}
		if tb.ProgramId != nil {
			dst = append(dst, `,"programId":`...)
			dst = appendBase58String(dst, tb.ProgramId)
		}
		dst = append(dst, '}')
	}
	return append(dst, ']')
}

// appendSvmUiAmount is the manifesto's uiAmount: amount-as-f64 divided by
// 10^decimals (Agave's token_amount_to_ui_amount_v3, double rounding
// included), null for zero or unparseable amounts.
func appendSvmUiAmount(dst []byte, a *svm.UiTokenAmount) []byte {
	raw, err := strconv.ParseFloat(a.Amount, 64)
	if err != nil || raw == 0 {
		return append(dst, "null"...)
	}
	return appendJsonFloat(dst, raw/math.Pow10(int(a.Decimals)))
}

// svmShiftDecimalString scales a non-negative integer digit string down by
// 10^decimals exactly, trimming trailing fraction zeros the way Agave's
// uiAmountString does ("1.500000" -> "1.5", "1000000" -> "1").
func svmShiftDecimalString(amount string, decimals uint32) string {
	if decimals == 0 || amount == "" {
		return amount
	}
	for _, c := range amount {
		if c < '0' || c > '9' {
			return amount
		}
	}
	d := int(decimals)
	if len(amount) <= d {
		amount = strings.Repeat("0", d-len(amount)+1) + amount
	}
	split := len(amount) - d
	intPart, frac := amount[:split], strings.TrimRight(amount[split:], "0")
	if frac == "" {
		return intPart
	}
	return intPart + "." + frac
}

func appendSvmRewards(dst []byte, rs []*svm.Reward) []byte {
	dst = append(dst, '[')
	for i, r := range rs {
		if i > 0 {
			dst = append(dst, ',')
		}
		if r == nil {
			dst = append(dst, "null"...)
			continue
		}
		dst = append(dst, `{"pubkey":`...)
		dst = appendBase58String(dst, r.Pubkey)
		dst = append(dst, `,"lamports":`...)
		dst = strconv.AppendInt(dst, r.Lamports, 10)
		if r.PostBalance != nil {
			dst = append(dst, `,"postBalance":`...)
			dst = strconv.AppendUint(dst, *r.PostBalance, 10)
		}
		dst = append(dst, `,"rewardType":`...)
		if r.RewardType != nil {
			dst = appendJsonString(dst, *r.RewardType)
		} else {
			dst = append(dst, "null"...)
		}
		dst = append(dst, `,"commission":`...)
		dst = appendOptUint32(dst, r.Commission)
		dst = append(dst, '}')
	}
	return append(dst, ']')
}

func appendByteIndexes(dst []byte, b []byte) []byte {
	dst = append(dst, '[')
	for i, v := range b {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = strconv.AppendUint(dst, uint64(v), 10)
	}
	return append(dst, ']')
}

func appendUint64s(dst []byte, vs []uint64) []byte {
	dst = append(dst, '[')
	for i, v := range vs {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = strconv.AppendUint(dst, v, 10)
	}
	return append(dst, ']')
}

func appendBase58Strings(dst []byte, bs [][]byte) []byte {
	dst = append(dst, '[')
	for i, b := range bs {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = appendBase58String(dst, b)
	}
	return append(dst, ']')
}

func appendOptUint(dst []byte, v *uint64) []byte {
	if v == nil {
		return append(dst, "null"...)
	}
	return strconv.AppendUint(dst, *v, 10)
}

func appendOptUint32(dst []byte, v *uint32) []byte {
	if v == nil {
		return append(dst, "null"...)
	}
	return strconv.AppendUint(dst, uint64(*v), 10)
}

// appendQuoted quotes a string known to need no escaping (base58).
func appendQuoted(dst []byte, s string) []byte {
	dst = append(dst, '"')
	dst = append(dst, s...)
	return append(dst, '"')
}

func appendBase58String(dst, b []byte) []byte {
	dst = append(dst, '"')
	dst = appendBase58(dst, b)
	return append(dst, '"')
}

func base58String(b []byte) string {
	var buf [96]byte
	return string(appendBase58(buf[:0], b))
}

const (
	svmBase58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	// svmBase58Limb is 58^5, the largest power of 58 below 2^30: one limb
	// holds five base58 digits, so the conversion does a fifth of the
	// divisions of a digit-at-a-time encoder.
	svmBase58Limb = 656356768
)

// appendBase58 appends svm.Base58Encode(in) (Bitcoin alphabet, one '1' per
// leading zero byte). It converts 32 input bits at a time into base-58^5
// limbs on the stack, so a 32-byte key costs ~40 constant divisions instead
// of ~700 and allocates nothing.
func appendBase58(dst, in []byte) []byte {
	zeros := 0
	for zeros < len(in) && in[zeros] == 0 {
		zeros++
	}
	for range zeros {
		dst = append(dst, '1')
	}
	rest := in[zeros:]
	if len(rest) == 0 {
		return dst
	}

	// log2(58^5) ~= 29.29 bits per limb.
	var stack [24]uint32
	limbs := stack[:0]
	if need := len(rest)*8/29 + 2; need > len(stack) {
		limbs = make([]uint32, 0, need)
	}
	// Feed a short head first so the remainder is whole 32-bit words.
	for i := 0; i < len(rest); {
		var word uint64
		var shift uint
		take := 4
		if i == 0 && len(rest)%4 != 0 {
			take = len(rest) % 4
		}
		for _, b := range rest[i : i+take] {
			word = word<<8 | uint64(b)
		}
		shift = uint(take) * 8
		i += take

		carry := word
		for j := range limbs {
			carry += uint64(limbs[j]) << shift
			limbs[j] = uint32(carry % svmBase58Limb)
			carry /= svmBase58Limb
		}
		for carry > 0 {
			limbs = append(limbs, uint32(carry%svmBase58Limb))
			carry /= svmBase58Limb
		}
	}

	var digits [5]byte
	top := limbs[len(limbs)-1]
	k := len(digits)
	for top > 0 {
		k--
		digits[k] = svmBase58Alphabet[top%58]
		top /= 58
	}
	dst = append(dst, digits[k:]...)
	for j := len(limbs) - 2; j >= 0; j-- {
		v := limbs[j]
		for d := 4; d >= 0; d-- {
			digits[d] = svmBase58Alphabet[v%58]
			v /= 58
		}
		dst = append(dst, digits[:]...)
	}
	return dst
}

const svmHex = "0123456789abcdef"

// appendJsonString appends s as a JSON string: quote, backslash and control
// characters escaped, invalid UTF-8 replaced with U+FFFD (encoding/json's
// rules, without HTML escaping).
func appendJsonString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			switch c {
			case '"', '\\':
				dst = append(dst, '\\', c)
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', svmHex[c>>4], svmHex[c&0xF])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			dst = append(dst, s[start:i]...)
			dst = append(dst, `\ufffd`...)
			i += size
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// appendJsonFloat formats f the way encoding/json does; non-finite values,
// which JSON cannot carry, render as null.
func appendJsonFloat(dst []byte, f float64) []byte {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return append(dst, "null"...)
	}
	abs := math.Abs(f)
	format := byte('f')
	if abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	dst = strconv.AppendFloat(dst, f, format, -1, 64)
	if format == 'e' {
		// clean up e-09 to e-9
		n := len(dst)
		if n >= 4 && dst[n-4] == 'e' && dst[n-3] == '-' && dst[n-2] == '0' {
			dst[n-2] = dst[n-1]
			dst = dst[:n-1]
		}
	}
	return dst
}

// svmReservedAccountKeys is the manifesto's reservedAccountKeys
// (ReservedAccountKeys::new_all_activated from agave-reserved-account-keys
// 2.3.13), which the parsed renderer demotes to read-only.
var svmReservedAccountKeys = map[string]bool{
	"AddressLookupTab1e1111111111111111111111111": true,
	"BPFLoader2111111111111111111111111111111111": true,
	"BPFLoader1111111111111111111111111111111111": true,
	svm.BpfUpgradeableLoaderID:                    true,
	"ComputeBudget111111111111111111111111111111": true,
	"Config1111111111111111111111111111111111111": true,
	"Ed25519SigVerify111111111111111111111111111": true,
	"Feature111111111111111111111111111111111111": true,
	"LoaderV411111111111111111111111111111111111": true,
	"KeccakSecp256k11111111111111111111111111111": true,
	"Secp256r1SigVerify1111111111111111111111111": true,
	"StakeConfig11111111111111111111111111111111": true,
	"Stake11111111111111111111111111111111111111": true,
	"11111111111111111111111111111111":            true,
	"Vote111111111111111111111111111111111111111": true,
	"ZkE1Gama1Proof11111111111111111111111111111": true,
	"ZkTokenProof1111111111111111111111111111111": true,
}
