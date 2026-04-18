package evm

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
)

func shimQueryBlocks(ctx context.Context, network common.Network, parentReqID interface{}, pinToUpstreamId string, qs *common.EvmQueryShimConfig, req *QueryRequest) (*QueryResponse, error) {
	if queryRangeIsEmpty(req) {
		return &QueryResponse{
			FromBlock: &QueryCursorBlock{Number: req.FromBlock},
			ToBlock:   &QueryCursorBlock{Number: req.ToBlock},
		}, nil
	}
	concurrency, _, _, _ := queryShimConfig(qs)
	rawBlocks, err := fetchBlockRange(ctx, network, parentReqID, pinToUpstreamId, req.FromBlock, req.ToBlock, req.Order, false, concurrency)
	if err != nil {
		return nil, err
	}

	pageBlocks := make([]map[string]interface{}, 0, len(rawBlocks))
	var lastScanned *QueryCursorBlock
	var hasMore bool

	for _, rawBlock := range rawBlocks {
		block, err := blockMapFromRaw(rawBlock)
		if err != nil {
			return nil, err
		}
		currentCursor := buildCursorBlock(block)

		if uint64(len(pageBlocks)) >= req.Limit {
			hasMore = true
			break
		}

		pageBlocks = append(pageBlocks, projectFields(block, req.Fields.Blocks, []string{"number", "hash", "parentHash"}))
		lastScanned = currentCursor
	}

	return &QueryResponse{
		Blocks:      pageBlocks,
		FromBlock:   &QueryCursorBlock{Number: req.FromBlock},
		ToBlock:     &QueryCursorBlock{Number: req.ToBlock},
		CursorBlock: nextCursor(lastScanned, hasMore),
	}, nil
}

func shimQueryTransactions(ctx context.Context, network common.Network, parentReqID interface{}, pinToUpstreamId string, qs *common.EvmQueryShimConfig, req *QueryRequest) (*QueryResponse, error) {
	if queryRangeIsEmpty(req) {
		return &QueryResponse{
			FromBlock: &QueryCursorBlock{Number: req.FromBlock},
			ToBlock:   &QueryCursorBlock{Number: req.ToBlock},
		}, nil
	}
	concurrency, _, _, _ := queryShimConfig(qs)
	rawBlocks, err := fetchBlockRange(ctx, network, parentReqID, pinToUpstreamId, req.FromBlock, req.ToBlock, req.Order, true, concurrency)
	if err != nil {
		return nil, err
	}

	transactions := make([]map[string]interface{}, 0)
	parentBlocks := make([]map[string]interface{}, 0)
	var lastScanned *QueryCursorBlock
	var hasMore bool

	for _, rawBlock := range rawBlocks {
		block, err := blockMapFromRaw(rawBlock)
		if err != nil {
			return nil, err
		}
		currentCursor := buildCursorBlock(block)

		rawTransactions, _ := block["transactions"].([]interface{})
		blockTransactions := make([]map[string]interface{}, 0, len(rawTransactions))
		for _, rawTransaction := range rawTransactions {
			tx, ok := rawTransaction.(map[string]interface{})
			if !ok {
				continue
			}
			if matchesTransactionFilter(tx, req.Filter) {
				blockTransactions = append(blockTransactions, projectFields(
					tx,
					req.Fields.Transactions,
					[]string{"hash", "blockNumber", "blockHash", "transactionIndex"},
				))
			}
		}

		if len(blockTransactions) == 0 {
			lastScanned = currentCursor
			continue
		}
		if len(transactions) > 0 && uint64(len(transactions)+len(blockTransactions)) > req.Limit {
			hasMore = true
			break
		}

		transactions = append(transactions, blockTransactions...)
		if req.Fields.Blocks != nil {
			parentBlocks = append(parentBlocks, projectFields(block, req.Fields.Blocks, []string{"number", "hash", "parentHash"}))
		}
		lastScanned = currentCursor
	}

	return &QueryResponse{
		Transactions: transactions,
		ParentBlocks: deduplicateByKey(parentBlocks, "hash"),
		FromBlock:    &QueryCursorBlock{Number: req.FromBlock},
		ToBlock:      &QueryCursorBlock{Number: req.ToBlock},
		CursorBlock:  nextCursor(lastScanned, hasMore),
	}, nil
}

func shimQueryLogs(ctx context.Context, network common.Network, parentReqID interface{}, pinToUpstreamId string, qs *common.EvmQueryShimConfig, req *QueryRequest) (*QueryResponse, error) {
	if queryRangeIsEmpty(req) {
		return &QueryResponse{
			FromBlock: &QueryCursorBlock{Number: req.FromBlock},
			ToBlock:   &QueryCursorBlock{Number: req.ToBlock},
		}, nil
	}
	filterFromBlock, filterToBlock := req.FromBlock, req.ToBlock
	if filterFromBlock > filterToBlock {
		filterFromBlock, filterToBlock = filterToBlock, filterFromBlock
	}
	filter := map[string]interface{}{
		"fromBlock": fmt.Sprintf("0x%x", filterFromBlock),
		"toBlock":   fmt.Sprintf("0x%x", filterToBlock),
	}
	if req.Filter != nil {
		if len(req.Filter.LogAddresses) == 1 {
			filter["address"] = bdsevm.BytesToHex(req.Filter.LogAddresses[0])
		} else if len(req.Filter.LogAddresses) > 1 {
			addresses := make([]string, 0, len(req.Filter.LogAddresses))
			for _, address := range req.Filter.LogAddresses {
				addresses = append(addresses, bdsevm.BytesToHex(address))
			}
			filter["address"] = addresses
		}
		if len(req.Filter.Topics) > 0 {
			topics := make([]interface{}, 0, len(req.Filter.Topics))
			for _, group := range req.Filter.Topics {
				if len(group) == 0 {
					topics = append(topics, nil)
					continue
				}
				if len(group) == 1 {
					topics = append(topics, bdsevm.BytesToHex([]byte(group[0])))
					continue
				}
				values := make([]string, 0, len(group))
				for _, item := range group {
					values = append(values, bdsevm.BytesToHex([]byte(item)))
				}
				topics = append(topics, values)
			}
			filter["topics"] = topics
		}
	}

	result, err := forwardSubRequest(ctx, network, parentReqID, pinToUpstreamId, "eth_getLogs", []interface{}{filter})
	if err != nil {
		return nil, err
	}

	var logs []map[string]interface{}
	if err := sonic.Unmarshal(result, &logs); err != nil {
		return nil, err
	}
	sortLogs(logs, req.Order)

	pageLogs := make([]map[string]interface{}, 0)
	parentTransactions := make([]map[string]interface{}, 0)
	parentBlocks := make([]map[string]interface{}, 0)
	var lastScanned *QueryCursorBlock
	var hasMore bool

	for i := 0; i < len(logs); {
		blockNumber, _ := parseUint64Value(logs[i]["blockNumber"])
		blockLogs := make([]map[string]interface{}, 0)
		for i < len(logs) {
			currentBlock, _ := parseUint64Value(logs[i]["blockNumber"])
			if currentBlock != blockNumber {
				break
			}
			blockLogs = append(blockLogs, logs[i])
			i++
		}

		if len(pageLogs) > 0 && uint64(len(pageLogs)+len(blockLogs)) > req.Limit {
			hasMore = true
			break
		}

		for _, log := range blockLogs {
			pageLogs = append(pageLogs, projectFields(
				log,
				req.Fields.Logs,
				[]string{"blockNumber", "blockHash", "transactionHash", "transactionIndex", "logIndex"},
			))
		}

		block, err := fetchBlockByNumber(ctx, network, parentReqID, pinToUpstreamId, blockNumber, req.Fields.Blocks != nil)
		if err != nil {
			return nil, err
		}
		if cursor := buildCursorBlock(block); cursor != nil {
			lastScanned = cursor
		}

		if req.Fields.Blocks != nil && block != nil {
			parentBlocks = append(parentBlocks, projectFields(block, req.Fields.Blocks, []string{"number", "hash", "parentHash"}))
		}
		if req.Fields.Transactions != nil {
			for _, log := range blockLogs {
				txHash, _ := log["transactionHash"].(string)
				if txHash == "" {
					continue
				}
				tx, err := fetchTransactionByHash(ctx, network, parentReqID, pinToUpstreamId, txHash)
				if err != nil {
					return nil, err
				}
				if tx != nil {
					parentTransactions = append(parentTransactions, projectFields(
						tx,
						req.Fields.Transactions,
						[]string{"hash", "blockNumber", "blockHash", "transactionIndex"},
					))
				}
			}
		}
	}

	return &QueryResponse{
		Logs:               pageLogs,
		ParentTransactions: deduplicateByKey(parentTransactions, "hash"),
		ParentBlocks:       deduplicateByKey(parentBlocks, "hash"),
		FromBlock:          &QueryCursorBlock{Number: req.FromBlock},
		ToBlock:            &QueryCursorBlock{Number: req.ToBlock},
		CursorBlock:        nextCursor(lastScanned, hasMore),
	}, nil
}

func shimQueryTraces(ctx context.Context, network common.Network, parentReqID interface{}, pinToUpstreamId string, qs *common.EvmQueryShimConfig, req *QueryRequest) (*QueryResponse, error) {
	if queryRangeIsEmpty(req) {
		return &QueryResponse{
			FromBlock: &QueryCursorBlock{Number: req.FromBlock},
			ToBlock:   &QueryCursorBlock{Number: req.ToBlock},
		}, nil
	}
	concurrency, _, _, _ := queryShimConfig(qs)
	rawBlocks, err := fetchBlockRange(ctx, network, parentReqID, pinToUpstreamId, req.FromBlock, req.ToBlock, req.Order, true, concurrency)
	if err != nil {
		return nil, err
	}

	traces := make([]map[string]interface{}, 0)
	parentTransactions := make([]map[string]interface{}, 0)
	parentBlocks := make([]map[string]interface{}, 0)
	var lastScanned *QueryCursorBlock
	var hasMore bool

	for _, rawBlock := range rawBlocks {
		block, err := blockMapFromRaw(rawBlock)
		if err != nil {
			return nil, err
		}
		currentCursor := buildCursorBlock(block)

		blockTraces, err := fetchTracesForBlock(ctx, network, parentReqID, pinToUpstreamId, block)
		if err != nil {
			return nil, err
		}
		filtered := make([]map[string]interface{}, 0, len(blockTraces))
		for _, trace := range blockTraces {
			if matchesTraceFilter(trace, req.Filter) {
				filtered = append(filtered, projectFields(
					trace,
					req.Fields.Traces,
					[]string{"blockNumber", "blockHash", "transactionHash", "transactionIndex", "traceAddress"},
				))
			}
		}

		if len(filtered) == 0 {
			lastScanned = currentCursor
			continue
		}
		if len(traces) > 0 && uint64(len(traces)+len(filtered)) > req.Limit {
			hasMore = true
			break
		}

		traces = append(traces, filtered...)
		if req.Fields.Blocks != nil {
			parentBlocks = append(parentBlocks, projectFields(block, req.Fields.Blocks, []string{"number", "hash", "parentHash"}))
		}
		if req.Fields.Transactions != nil {
			for _, trace := range filtered {
				txHash, _ := trace["transactionHash"].(string)
				if txHash == "" || txHash == "0x" {
					continue
				}
				tx, err := fetchTransactionByHash(ctx, network, parentReqID, pinToUpstreamId, txHash)
				if err != nil {
					return nil, err
				}
				if tx != nil {
					parentTransactions = append(parentTransactions, projectFields(
						tx,
						req.Fields.Transactions,
						[]string{"hash", "blockNumber", "blockHash", "transactionIndex"},
					))
				}
			}
		}
		lastScanned = currentCursor
	}

	return &QueryResponse{
		Traces:             traces,
		ParentTransactions: deduplicateByKey(parentTransactions, "hash"),
		ParentBlocks:       deduplicateByKey(parentBlocks, "hash"),
		FromBlock:          &QueryCursorBlock{Number: req.FromBlock},
		ToBlock:            &QueryCursorBlock{Number: req.ToBlock},
		CursorBlock:        nextCursor(lastScanned, hasMore),
	}, nil
}

func shimQueryTransfers(ctx context.Context, network common.Network, parentReqID interface{}, pinToUpstreamId string, qs *common.EvmQueryShimConfig, req *QueryRequest) (*QueryResponse, error) {
	if queryRangeIsEmpty(req) {
		return &QueryResponse{
			FromBlock: &QueryCursorBlock{Number: req.FromBlock},
			ToBlock:   &QueryCursorBlock{Number: req.ToBlock},
		}, nil
	}
	var filter *QueryFilter
	if req.Filter != nil {
		filter = &QueryFilter{
			FromAddresses: req.Filter.FromAddresses,
			ToAddresses:   req.Filter.ToAddresses,
			IsTopLevel:    req.Filter.IsTopLevel,
		}
	}
	traceReq := &QueryRequest{
		Method:    "eth_queryTraces",
		FromBlock: req.FromBlock,
		ToBlock:   req.ToBlock,
		Order:     req.Order,
		Limit:     req.Limit,
		Cursor:    req.Cursor,
		Filter:    filter,
		Fields: &QueryFieldSelection{
			Blocks:       req.Fields.Blocks,
			Transactions: req.Fields.Transactions,
			Traces:       true,
		},
	}

	traceResp, err := shimQueryTraces(ctx, network, parentReqID, pinToUpstreamId, qs, traceReq)
	if err != nil {
		return nil, err
	}

	transfers := make([]map[string]interface{}, 0)
	for _, trace := range traceResp.Traces {
		protoTrace, err := protoTraceFromJSON(trace)
		if err != nil {
			return nil, err
		}
		for _, transfer := range bdsevm.NativeTransfersFromTraces([]*bdsevm.Trace{protoTrace}) {
			transferJSON := jsonMapFromProtoTransfer(transfer)
			if matchesTransferFilter(transferJSON, req.Filter) {
				transfers = append(transfers, projectFields(
					transferJSON,
					req.Fields.Transfers,
					[]string{"blockNumber", "blockHash", "transactionHash", "transactionIndex", "traceAddress"},
				))
			}
		}
	}

	return &QueryResponse{
		Transfers:          transfers,
		ParentTransactions: traceResp.ParentTransactions,
		ParentBlocks:       traceResp.ParentBlocks,
		FromBlock:          traceResp.FromBlock,
		ToBlock:            traceResp.ToBlock,
		CursorBlock:        traceResp.CursorBlock,
	}, nil
}

func fetchBlockByNumber(ctx context.Context, network common.Network, parentReqID interface{}, pinToUpstreamId string, blockNumber uint64, fullTx bool) (map[string]interface{}, error) {
	result, err := forwardSubRequest(ctx, network, parentReqID, pinToUpstreamId, "eth_getBlockByNumber", []interface{}{fmt.Sprintf("0x%x", blockNumber), fullTx})
	if err != nil {
		if common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
			return nil, nil
		}
		return nil, err
	}
	if bytes.Equal(result, []byte("null")) {
		return nil, nil
	}
	return blockMapFromRaw(result)
}

func fetchTransactionByHash(ctx context.Context, network common.Network, parentReqID interface{}, pinToUpstreamId string, txHash string) (map[string]interface{}, error) {
	result, err := forwardSubRequest(ctx, network, parentReqID, pinToUpstreamId, "eth_getTransactionByHash", []interface{}{txHash})
	if err != nil {
		if common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
			return nil, nil
		}
		return nil, err
	}
	if bytes.Equal(result, []byte("null")) {
		return nil, nil
	}
	var tx map[string]interface{}
	if err := sonic.Unmarshal(result, &tx); err != nil {
		return nil, err
	}
	return tx, nil
}

func fetchTracesForBlock(ctx context.Context, network common.Network, parentReqID interface{}, pinToUpstreamId string, block map[string]interface{}) ([]map[string]interface{}, error) {
	blockNumber, _ := parseUint64Value(block["number"])
	blockHashHex, _ := block["hash"].(string)
	blockHash, _ := common.HexToBytes(blockHashHex)
	blockNumberHex, _ := block["number"].(string)
	if blockNumberHex == "" {
		blockNumberHex = fmt.Sprintf("0x%x", blockNumber)
	}
	var blockTimestamp *uint64
	if block["timestamp"] != nil {
		if ts, err := parseUint64Value(block["timestamp"]); err == nil {
			blockTimestamp = &ts
		}
	}

	rawTransactions, _ := block["transactions"].([]interface{})
	traceResult, err := forwardSubRequest(ctx, network, parentReqID, pinToUpstreamId, "trace_block", []interface{}{blockNumberHex})
	if err == nil {
		if bytes.Equal(traceResult, []byte("null")) {
			return nil, nil
		}
		var rawItems []map[string]interface{}
		if err := sonic.Unmarshal(traceResult, &rawItems); err != nil {
			return nil, err
		}
		out := make([]map[string]interface{}, 0, len(rawItems))
		for _, rawItem := range rawItems {
			trace, err := bdsevm.TraceFromParity(rawItem, blockNumber, blockHash, blockTimestamp)
			if err != nil {
				return nil, err
			}
			out = append(out, jsonMapFromProtoTrace(trace))
		}
		return out, nil
	}
	if !isUnsupportedTraceMethod(err) {
		return nil, err
	}

	debugResult, err := forwardSubRequest(
		ctx,
		network,
		parentReqID,
		pinToUpstreamId,
		"debug_traceBlockByNumber",
		[]interface{}{blockNumberHex, map[string]interface{}{"tracer": "callTracer"}},
	)
	if err != nil {
		if isUnsupportedTraceMethod(err) {
			return nil, common.NewErrEndpointUnsupported(
				fmt.Errorf("eth_queryTraces requires trace_block or debug_traceBlockByNumber support"),
			)
		}
		return nil, err
	}
	if bytes.Equal(debugResult, []byte("null")) {
		return nil, nil
	}

	var batch []map[string]interface{}
	if err := sonic.Unmarshal(debugResult, &batch); err == nil {
		out := make([]map[string]interface{}, 0, len(batch))
		for idx, item := range batch {
			injectTransactionContext(item, rawTransactions, idx)
			traces, err := bdsevm.TraceFromGethDebug(item, blockNumber, blockHash, blockTimestamp)
			if err != nil {
				return nil, err
			}
			for _, trace := range traces {
				out = append(out, jsonMapFromProtoTrace(trace))
			}
		}
		return out, nil
	}

	var single map[string]interface{}
	if err := sonic.Unmarshal(debugResult, &single); err != nil {
		return nil, err
	}
	injectTransactionContext(single, rawTransactions, 0)
	traces, err := bdsevm.TraceFromGethDebug(single, blockNumber, blockHash, blockTimestamp)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]interface{}, 0, len(traces))
	for _, trace := range traces {
		out = append(out, jsonMapFromProtoTrace(trace))
	}
	return out, nil
}

func protoTraceFromJSON(trace map[string]interface{}) (*bdsevm.Trace, error) {
	raw, err := sonic.Marshal(trace)
	if err != nil {
		return nil, err
	}

	type traceJSON struct {
		TraceType        string   `json:"traceType"`
		CallType         string   `json:"callType"`
		From             string   `json:"from"`
		To               *string  `json:"to"`
		Value            string   `json:"value"`
		Input            string   `json:"input"`
		Output           string   `json:"output"`
		Gas              string   `json:"gas"`
		GasUsed          string   `json:"gasUsed"`
		Error            *string  `json:"error"`
		Subtraces        string   `json:"subtraces"`
		TraceAddress     []string `json:"traceAddress"`
		TransactionHash  string   `json:"transactionHash"`
		TransactionIndex string   `json:"transactionIndex"`
		BlockNumber      string   `json:"blockNumber"`
		BlockHash        string   `json:"blockHash"`
		BlockTimestamp   *string  `json:"blockTimestamp"`
	}

	var decoded traceJSON
	if err := sonic.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}

	from, _ := common.HexToBytes(decoded.From)
	var to []byte
	if decoded.To != nil && *decoded.To != "" {
		to, _ = common.HexToBytes(*decoded.To)
	}
	input, _ := common.HexToBytes(decoded.Input)
	output, _ := common.HexToBytes(decoded.Output)
	txHash, _ := common.HexToBytes(decoded.TransactionHash)
	blockHash, _ := common.HexToBytes(decoded.BlockHash)
	gas, _ := parseUint64Value(decoded.Gas)
	gasUsed, _ := parseUint64Value(decoded.GasUsed)
	subtraces, _ := parseUint64Value(decoded.Subtraces)
	transactionIndex, _ := parseUint64Value(decoded.TransactionIndex)
	blockNumber, _ := parseUint64Value(decoded.BlockNumber)
	subtraces32, err := uint32FromUint64(subtraces, "subtraces")
	if err != nil {
		return nil, err
	}
	transactionIndex32, err := uint32FromUint64(transactionIndex, "transactionIndex")
	if err != nil {
		return nil, err
	}
	var traceAddress []uint32
	for _, idx := range decoded.TraceAddress {
		value, _ := parseUint64Value(idx)
		value32, err := uint32FromUint64(value, "traceAddress")
		if err != nil {
			return nil, err
		}
		traceAddress = append(traceAddress, value32)
	}
	var timestamp *uint64
	if decoded.BlockTimestamp != nil {
		if parsed, err := parseUint64Value(*decoded.BlockTimestamp); err == nil {
			timestamp = &parsed
		}
	}

	traceType := bdsevm.TraceType_TRACE_CALL
	switch strings.ToLower(decoded.TraceType) {
	case "create":
		traceType = bdsevm.TraceType_TRACE_CREATE
	case "selfdestruct":
		traceType = bdsevm.TraceType_TRACE_SELFDESTRUCT
	case "reward":
		traceType = bdsevm.TraceType_TRACE_REWARD
	}

	callType := bdsevm.TraceCallType_TRACE_CALL_CALL
	switch strings.ToLower(decoded.CallType) {
	case "staticcall":
		callType = bdsevm.TraceCallType_TRACE_CALL_STATICCALL
	case "delegatecall":
		callType = bdsevm.TraceCallType_TRACE_CALL_DELEGATECALL
	case "callcode":
		callType = bdsevm.TraceCallType_TRACE_CALL_CALLCODE
	}

	return &bdsevm.Trace{
		TraceType:        traceType,
		CallType:         callType,
		From:             from,
		To:               to,
		Value:            decoded.Value,
		Input:            input,
		Output:           output,
		Gas:              gas,
		GasUsed:          gasUsed,
		Error:            decoded.Error,
		Subtraces:        subtraces32,
		TraceAddress:     traceAddress,
		TransactionHash:  txHash,
		TransactionIndex: transactionIndex32,
		BlockNumber:      blockNumber,
		BlockHash:        blockHash,
		BlockTimestamp:   timestamp,
	}, nil
}

func nextCursor(lastScanned *QueryCursorBlock, hasMore bool) *QueryCursorBlock {
	if !hasMore {
		return nil
	}
	return lastScanned
}

func injectTransactionContext(frame map[string]interface{}, rawTransactions []interface{}, index int) {
	if frame == nil || index < 0 || index >= len(rawTransactions) {
		return
	}
	tx, ok := rawTransactions[index].(map[string]interface{})
	if !ok {
		return
	}
	txHash, _ := tx["hash"].(string)
	txIndex := tx["transactionIndex"]
	if result, ok := frame["result"].(map[string]interface{}); ok {
		propagateTransactionContext(result, txHash, txIndex)
		return
	}
	propagateTransactionContext(frame, txHash, txIndex)
}

func propagateTransactionContext(frame map[string]interface{}, txHash string, txIndex interface{}) {
	if frame == nil {
		return
	}
	if txHash != "" {
		frame["transactionHash"] = txHash
	}
	if txIndex != nil {
		frame["transactionIndex"] = txIndex
	}
	children, _ := frame["calls"].([]interface{})
	for _, childRaw := range children {
		child, ok := childRaw.(map[string]interface{})
		if !ok {
			continue
		}
		propagateTransactionContext(child, txHash, txIndex)
	}
}

func isUnsupportedTraceMethod(err error) bool {
	if err == nil {
		return false
	}
	if common.HasErrorCode(err, common.ErrCodeEndpointUnsupported) {
		return true
	}
	errMsg := strings.ToLower(err.Error())
	return strings.Contains(errMsg, "method not found") || strings.Contains(errMsg, "unsupported")
}
