package evm

import (
	"context"
	"fmt"
	"strings"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
)

type topicValue []byte

type QueryRequest struct {
	Method    string
	FromBlock uint64
	ToBlock   uint64
	Order     string
	Limit     int
	Cursor    *QueryCursorBlock
	Filter    *QueryFilter
	Fields    *QueryFieldSelection
}

type QueryCursorBlock struct {
	Number     uint64
	Hash       []byte
	ParentHash []byte
}

type QueryFilter struct {
	FromAddresses [][]byte
	ToAddresses   [][]byte
	Selectors     [][]byte
	LogAddresses  [][]byte
	Topics        [][]topicValue
	IsTopLevel    *bool
}

type QueryFieldSelection struct {
	Blocks       interface{}
	Transactions interface{}
	Logs         interface{}
	Traces       interface{}
	Transfers    interface{}
}

type QueryResponse struct {
	Blocks             []map[string]interface{}
	Transactions       []map[string]interface{}
	Logs               []map[string]interface{}
	Traces             []map[string]interface{}
	Transfers          []map[string]interface{}
	ParentBlocks       []map[string]interface{}
	ParentTransactions []map[string]interface{}
	FromBlock          *QueryCursorBlock
	ToBlock            *QueryCursorBlock
	CursorBlock        *QueryCursorBlock
}

func networkPreForward_eth_query(
	ctx context.Context,
	network common.Network,
	upstreams []common.Upstream,
	nq *common.NormalizedRequest,
) (handled bool, resp *common.NormalizedResponse, err error) {
	if nq == nil || network == nil {
		return false, nil, nil
	}
	if nq.ParentRequestId() != nil {
		return false, nil, nil
	}

	method, err := nq.Method()
	if err != nil {
		return true, nil, err
	}

	for _, ups := range upstreams {
		supported, err := upstreamSupportsQueryMethod(ups, method)
		if err != nil {
			return true, nil, err
		}
		if supported {
			return false, nil, nil
		}
	}

	return executeQueryShim(ctx, network, nq)
}

func executeQueryShim(
	ctx context.Context,
	network common.Network,
	nq *common.NormalizedRequest,
) (handled bool, resp *common.NormalizedResponse, err error) {
	queryReq, err := parseQueryRequest(ctx, network, nq)
	if err != nil {
		return true, nil, err
	}

	switch strings.ToLower(queryReq.Method) {
	case "eth_queryblocks":
		nq.SetCompositeType(common.CompositeTypeQueryBlocksShim)
	case "eth_querytransactions":
		nq.SetCompositeType(common.CompositeTypeQueryTransactionsShim)
	case "eth_querylogs":
		nq.SetCompositeType(common.CompositeTypeQueryLogsShim)
	case "eth_querytraces":
		nq.SetCompositeType(common.CompositeTypeQueryTracesShim)
	case "eth_querytransfers":
		nq.SetCompositeType(common.CompositeTypeQueryTransfersShim)
	}

	var qr *QueryResponse
	switch strings.ToLower(queryReq.Method) {
	case "eth_queryblocks":
		qr, err = shimQueryBlocks(ctx, network, nq.ID(), queryReq)
	case "eth_querytransactions":
		qr, err = shimQueryTransactions(ctx, network, nq.ID(), queryReq)
	case "eth_querylogs":
		qr, err = shimQueryLogs(ctx, network, nq.ID(), queryReq)
	case "eth_querytraces":
		qr, err = shimQueryTraces(ctx, network, nq.ID(), queryReq)
	case "eth_querytransfers":
		qr, err = shimQueryTransfers(ctx, network, nq.ID(), queryReq)
	default:
		err = common.NewErrInvalidRequest(fmt.Errorf("unsupported query method: %s", queryReq.Method))
	}
	if err != nil {
		return true, nil, err
	}

	jrr, err := common.NewJsonRpcResponse(nq.ID(), buildQueryJsonRpcResponse(queryReq.Method, qr), nil)
	if err != nil {
		return true, nil, err
	}

	return true, common.NewNormalizedResponse().WithRequest(nq).WithJsonRpcResponse(jrr), nil
}

func parseQueryRequest(ctx context.Context, network common.Network, nq *common.NormalizedRequest) (*QueryRequest, error) {
	jrq, err := nq.JsonRpcRequest(ctx)
	if err != nil {
		return nil, err
	}
	if jrq == nil || len(jrq.Params) == 0 {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("query params are required"))
	}

	obj, ok := jrq.Params[0].(map[string]interface{})
	if !ok {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("query params must be an object"))
	}

	method, err := nq.Method()
	if err != nil {
		return nil, err
	}

	concurrency, maxBlockRange, maxLimit, defaultLimit := queryShimConfig(network)
	_ = concurrency

	order := "asc"
	if rawOrder, ok := obj["order"].(string); ok && rawOrder != "" {
		switch strings.ToLower(rawOrder) {
		case "asc", "desc":
			order = strings.ToLower(rawOrder)
		default:
			return nil, common.NewErrInvalidRequest(fmt.Errorf("invalid order: %s", rawOrder))
		}
	}

	limit := defaultLimit
	if rawLimit, ok := obj["limit"]; ok {
		parsedLimit, err := parseUint64Value(rawLimit)
		if err != nil {
			return nil, common.NewErrInvalidRequest(fmt.Errorf("invalid limit: %w", err))
		}
		if parsedLimit > uint64(maxLimit) {
			return nil, queryCapacityExceeded(
				"query request exceeded max limit",
				map[string]interface{}{"maxLimit": maxLimit},
			)
		}
		limit = int(parsedLimit)
	}
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		return nil, queryCapacityExceeded(
			"query request exceeded max limit",
			map[string]interface{}{"maxLimit": maxLimit},
		)
	}

	var cursor *QueryCursorBlock
	if rawCursor, ok := obj["cursor"]; ok {
		cursor, err = parseQueryCursorBlock(rawCursor)
		if err != nil {
			return nil, err
		}
	} else if rawCursor, ok := obj["cursorBlock"]; ok {
		cursor, err = parseQueryCursorBlock(rawCursor)
		if err != nil {
			return nil, err
		}
	}

	fromTag, _ := obj["fromBlock"].(string)
	toTag, _ := obj["toBlock"].(string)
	fromBlock, err := resolveBlockTag(ctx, network, fromTag)
	if err != nil {
		return nil, err
	}
	toBlock, err := resolveBlockTag(ctx, network, toTag)
	if err != nil {
		return nil, err
	}

	if strings.EqualFold(order, "desc") {
		if fromBlock < toBlock {
			fromBlock, toBlock = toBlock, fromBlock
		}
		if cursor != nil {
			if cursor.Number == 0 {
				return nil, common.NewErrInvalidRequest(fmt.Errorf("cursor block number must be greater than zero for desc order"))
			}
			fromBlock = cursor.Number - 1
		}
	} else {
		if fromBlock > toBlock {
			fromBlock, toBlock = toBlock, fromBlock
		}
		if cursor != nil {
			fromBlock = cursor.Number + 1
		}
	}

	if fromBlock != toBlock {
		rangeSize := int64(blockSpan(fromBlock, toBlock))
		if rangeSize > maxBlockRange {
			return nil, queryCapacityExceeded(
				"query request exceeded max block range",
				map[string]interface{}{"maxBlockRange": maxBlockRange},
			)
		}
	}

	fields, err := parseQueryFieldSelection(obj["fields"])
	if err != nil {
		return nil, err
	}

	filter, err := parseQueryFilter(strings.ToLower(method), obj["filter"])
	if err != nil {
		return nil, err
	}

	return &QueryRequest{
		Method:    method,
		FromBlock: fromBlock,
		ToBlock:   toBlock,
		Order:     order,
		Limit:     limit,
		Cursor:    cursor,
		Filter:    filter,
		Fields:    fields,
	}, nil
}

func buildQueryJsonRpcResponse(method string, qr *QueryResponse) map[string]interface{} {
	if qr == nil {
		qr = &QueryResponse{}
	}

	data := map[string]interface{}{}
	switch strings.ToLower(method) {
	case "eth_queryblocks":
		data["blocks"] = mapsToInterfaces(qr.Blocks)
	case "eth_querytransactions":
		data["transactions"] = mapsToInterfaces(qr.Transactions)
		data["blocks"] = mapsToInterfaces(qr.ParentBlocks)
	case "eth_querylogs":
		data["logs"] = mapsToInterfaces(qr.Logs)
		data["transactions"] = mapsToInterfaces(qr.ParentTransactions)
		data["blocks"] = mapsToInterfaces(qr.ParentBlocks)
	case "eth_querytraces":
		data["traces"] = mapsToInterfaces(qr.Traces)
		data["transactions"] = mapsToInterfaces(qr.ParentTransactions)
		data["blocks"] = mapsToInterfaces(qr.ParentBlocks)
	case "eth_querytransfers":
		data["transfers"] = mapsToInterfaces(qr.Transfers)
		data["transactions"] = mapsToInterfaces(qr.ParentTransactions)
		data["blocks"] = mapsToInterfaces(qr.ParentBlocks)
	}

	return map[string]interface{}{
		"data":        data,
		"fromBlock":   queryCursorBlockToJSON(qr.FromBlock),
		"toBlock":     queryCursorBlockToJSON(qr.ToBlock),
		"cursorBlock": queryCursorBlockToJSON(qr.CursorBlock),
	}
}

func parseQueryCursorBlock(raw interface{}) (*QueryCursorBlock, error) {
	if raw == nil {
		return nil, nil
	}

	obj, ok := raw.(map[string]interface{})
	if !ok {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("invalid cursor block"))
	}

	number, err := parseUint64Value(obj["number"])
	if err != nil {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("invalid cursor number: %w", err))
	}

	cursor := &QueryCursorBlock{Number: number}
	if hash, ok := obj["hash"].(string); ok && hash != "" {
		cursor.Hash, err = common.HexToBytes(hash)
		if err != nil {
			return nil, common.NewErrInvalidRequest(fmt.Errorf("invalid cursor hash: %w", err))
		}
	}
	if parentHash, ok := obj["parentHash"].(string); ok && parentHash != "" {
		cursor.ParentHash, err = common.HexToBytes(parentHash)
		if err != nil {
			return nil, common.NewErrInvalidRequest(fmt.Errorf("invalid cursor parentHash: %w", err))
		}
	}

	return cursor, nil
}

func parseQueryFieldSelection(raw interface{}) (*QueryFieldSelection, error) {
	fields := &QueryFieldSelection{}
	obj, _ := raw.(map[string]interface{})
	if obj == nil {
		return fields, nil
	}

	fields.Blocks = normalizeFieldSelectionRaw(obj["blocks"])
	fields.Transactions = normalizeFieldSelectionRaw(obj["transactions"])
	fields.Logs = normalizeFieldSelectionRaw(obj["logs"])
	fields.Traces = normalizeFieldSelectionRaw(obj["traces"])
	fields.Transfers = normalizeFieldSelectionRaw(obj["transfers"])

	return fields, nil
}

func normalizeFieldSelectionRaw(raw interface{}) interface{} {
	switch v := raw.(type) {
	case nil:
		return nil
	case bool:
		if v {
			return true
		}
		return []string{}
	case []interface{}:
		fields := make([]string, 0, len(v))
		for _, item := range v {
			field, ok := item.(string)
			if ok && field != "" {
				fields = append(fields, field)
			}
		}
		return fields
	default:
		return nil
	}
}

func parseQueryFilter(method string, raw interface{}) (*QueryFilter, error) {
	obj, _ := raw.(map[string]interface{})
	if obj == nil {
		return nil, nil
	}

	filter := &QueryFilter{
		FromAddresses: parseByteSliceList(obj["from"]),
		ToAddresses:   parseByteSliceList(obj["to"]),
		Selectors:     parseByteSliceList(obj["selector"]),
		LogAddresses:  parseByteSliceList(obj["address"]),
	}

	if rawTopLevel, ok := obj["isTopLevel"].(bool); ok {
		filter.IsTopLevel = &rawTopLevel
	}

	if strings.EqualFold(method, "eth_querylogs") {
		if rawTopics, ok := obj["topics"].([]interface{}); ok {
			filter.Topics = make([][]topicValue, 0, len(rawTopics))
			for _, rawTopic := range rawTopics {
				topicGroup := make([]topicValue, 0)
				switch value := rawTopic.(type) {
				case nil:
				case string:
					if bytesValue, err := common.HexToBytes(value); err == nil {
						topicGroup = append(topicGroup, topicValue(bytesValue))
					}
				case []interface{}:
					for _, rawValue := range value {
						if topicHex, ok := rawValue.(string); ok {
							if bytesValue, err := common.HexToBytes(topicHex); err == nil {
								topicGroup = append(topicGroup, topicValue(bytesValue))
							}
						}
					}
				}
				filter.Topics = append(filter.Topics, topicGroup)
			}
		}
	}

	return filter, nil
}

func parseByteSliceList(raw interface{}) [][]byte {
	switch v := raw.(type) {
	case string:
		if bytesValue, err := common.HexToBytes(v); err == nil {
			return [][]byte{bytesValue}
		}
	case []interface{}:
		out := make([][]byte, 0, len(v))
		for _, rawValue := range v {
			if value, ok := rawValue.(string); ok {
				if bytesValue, err := common.HexToBytes(value); err == nil {
					out = append(out, bytesValue)
				}
			}
		}
		return out
	}

	return nil
}

func queryCursorBlockToJSON(cur *QueryCursorBlock) interface{} {
	if cur == nil {
		return nil
	}

	return map[string]interface{}{
		"number":     fmt.Sprintf("0x%x", cur.Number),
		"hash":       bdsevm.BytesToHex(cur.Hash),
		"parentHash": bdsevm.BytesToHex(cur.ParentHash),
	}
}

func mapsToInterfaces(items []map[string]interface{}) []interface{} {
	out := make([]interface{}, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	return out
}

func upstreamSupportsQueryMethod(ups common.Upstream, method string) (bool, error) {
	if ups == nil {
		return false, nil
	}

	type methodSupporter interface {
		ShouldHandleMethod(method string) (bool, error)
	}

	if supporter, ok := ups.(methodSupporter); ok {
		return supporter.ShouldHandleMethod(method)
	}

	cfg := ups.Config()
	if cfg == nil {
		return true, nil
	}

	allowed := true
	for _, pattern := range cfg.IgnoreMethods {
		match, err := common.WildcardMatch(pattern, method)
		if err != nil {
			return false, err
		}
		if match {
			allowed = false
			break
		}
	}
	for _, pattern := range cfg.AllowMethods {
		match, err := common.WildcardMatch(pattern, method)
		if err != nil {
			return false, err
		}
		if match {
			allowed = true
			break
		}
	}

	return allowed, nil
}
