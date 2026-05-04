package erpc

import "github.com/blockchain-data-standards/manifesto/evm"

func ProjectBlockFields(block *evm.BlockHeader, sel *evm.BlockFieldSelection) {
	if block == nil || sel == nil {
		return
	}
	if !sel.Timestamp {
		block.Timestamp = 0
	}
	if !sel.Hash {
		// hash is always preserved for cursor semantics
	}
	if !sel.ParentHash {
		block.ParentHash = nil
	}
	if !sel.StateRoot {
		block.StateRoot = nil
	}
	if !sel.TransactionsRoot {
		block.TransactionsRoot = nil
	}
	if !sel.ReceiptsRoot {
		block.ReceiptsRoot = nil
	}
	if !sel.LogsBloom {
		block.LogsBloom = nil
	}
	if !sel.GasLimit {
		block.GasLimit = 0
	}
	if !sel.GasUsed {
		block.GasUsed = 0
	}
	if !sel.Miner {
		block.Miner = nil
	}
	if !sel.ExtraData {
		block.ExtraData = nil
	}
	if !sel.Size {
		block.Size = 0
	}
	if !sel.Sha3Uncles {
		block.Sha3Uncles = nil
	}
	if !sel.Nonce {
		block.Nonce = nil
	}
	if !sel.BaseFeePerGas {
		block.BaseFeePerGas = nil
	}
	if !sel.Difficulty {
		block.Difficulty = nil
	}
	if !sel.TotalDifficulty {
		block.TotalDifficulty = nil
	}
	if !sel.MixHash {
		block.MixHash = nil
	}
	if !sel.BlobGasUsed {
		block.BlobGasUsed = nil
	}
	if !sel.ExcessBlobGas {
		block.ExcessBlobGas = nil
	}
	if !sel.WithdrawalsRoot {
		block.WithdrawalsRoot = nil
	}
	if !sel.ParentBeaconBlockRoot {
		block.ParentBeaconBlockRoot = nil
	}
	if !sel.TransactionCount {
		block.TransactionCount = nil
	}
}

func ProjectTransactionFields(tx *evm.Transaction, sel *evm.TransactionFieldSelection) {
	if tx == nil || sel == nil {
		return
	}
	if !sel.Nonce {
		tx.Nonce = 0
	}
	if !sel.From {
		tx.From = nil
	}
	if !sel.To {
		tx.To = nil
	}
	if !sel.Value {
		tx.Value = ""
	}
	if !sel.Input {
		tx.Input = nil
	}
	if !sel.Type {
		tx.Type = 0
	}
	if !sel.GasLimit {
		tx.GasLimit = 0
	}
	if !sel.GasPrice {
		tx.GasPrice = nil
	}
	if !sel.MaxFeePerGas {
		tx.MaxFeePerGas = nil
	}
	if !sel.MaxPriorityFeePerGas {
		tx.MaxPriorityFeePerGas = nil
	}
	if !sel.GasUsed {
		tx.GasUsed = nil
	}
	if !sel.EffectiveGasPrice {
		tx.EffectiveGasPrice = nil
	}
	if !sel.BlockNumber {
		tx.BlockNumber = nil
	}
	if !sel.BlockHash {
		tx.BlockHash = nil
	}
	if !sel.TransactionIndex {
		tx.TransactionIndex = nil
	}
	if !sel.BlockTimestamp {
		tx.BlockTimestamp = nil
	}
	if !sel.ChainId {
		tx.ChainId = nil
	}
	if !sel.AccessList {
		tx.AccessList = nil
	}
	if !sel.MaxFeePerBlobGas {
		tx.MaxFeePerBlobGas = nil
	}
	if !sel.BlobVersionedHashes {
		tx.BlobVersionedHashes = nil
	}
	if !sel.R {
		tx.R = nil
	}
	if !sel.S {
		tx.S = nil
	}
	if !sel.V {
		tx.V = nil
	}
	if !sel.YParity {
		tx.YParity = nil
	}
}

func ProjectLogFields(log *evm.Log, sel *evm.LogFieldSelection) {
	if log == nil || sel == nil {
		return
	}
	if !sel.Address {
		log.Address = nil
	}
	if !sel.Topics {
		log.Topics = nil
	}
	if !sel.Data {
		log.Data = nil
	}
	if !sel.BlockNumber {
		log.BlockNumber = 0
	}
	if !sel.BlockHash {
		log.BlockHash = nil
	}
	if !sel.TransactionHash {
		log.TransactionHash = nil
	}
	if !sel.TransactionIndex {
		log.TransactionIndex = 0
	}
	if !sel.LogIndex {
		log.LogIndex = 0
	}
	if !sel.BlockTimestamp {
		log.BlockTimestamp = nil
	}
}

func ProjectTraceFields(trace *evm.Trace, sel *evm.TraceFieldSelection) {
	if trace == nil || sel == nil {
		return
	}
	if !sel.TraceType {
		trace.TraceType = evm.TraceType_TRACE_CALL
	}
	if !sel.CallType {
		trace.CallType = evm.TraceCallType_TRACE_CALL_CALL
	}
	if !sel.From {
		trace.From = nil
	}
	if !sel.To {
		trace.To = nil
	}
	if !sel.Value {
		trace.Value = ""
	}
	if !sel.Input {
		trace.Input = nil
	}
	if !sel.Output {
		trace.Output = nil
	}
	if !sel.Gas {
		trace.Gas = 0
	}
	if !sel.GasUsed {
		trace.GasUsed = 0
	}
	if !sel.Error {
		trace.Error = nil
	}
	if !sel.Subtraces {
		trace.Subtraces = 0
	}
	if !sel.TraceAddress {
		trace.TraceAddress = nil
	}
	if !sel.TransactionHash {
		trace.TransactionHash = nil
	}
	if !sel.TransactionIndex {
		trace.TransactionIndex = 0
	}
	if !sel.BlockNumber {
		trace.BlockNumber = 0
	}
	if !sel.BlockHash {
		trace.BlockHash = nil
	}
	if !sel.BlockTimestamp {
		trace.BlockTimestamp = nil
	}
}

func ProjectTransferFields(transfer *evm.NativeTransfer, sel *evm.TransferFieldSelection) {
	if transfer == nil || sel == nil {
		return
	}
	if !sel.From {
		transfer.From = nil
	}
	if !sel.To {
		transfer.To = nil
	}
	if !sel.Value {
		transfer.Value = ""
	}
	if !sel.TransactionHash {
		transfer.TransactionHash = nil
	}
	if !sel.TransactionIndex {
		transfer.TransactionIndex = 0
	}
	if !sel.BlockNumber {
		transfer.BlockNumber = 0
	}
	if !sel.BlockHash {
		transfer.BlockHash = nil
	}
	if !sel.TraceAddress {
		transfer.TraceAddress = nil
	}
	if !sel.BlockTimestamp {
		transfer.BlockTimestamp = nil
	}
}
