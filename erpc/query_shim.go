package erpc

import (
	"context"
	"fmt"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func (qe *EvmQueryExecutor) shimQueryBlocks(ctx context.Context, req *evm.QueryBlocksRequest, fromBlock, toBlock uint64, onPage func(proto.Message) error) error {
	order := req.GetOrder()
	limit := queryLimit(req.GetLimit())

	blocks := make([]*evm.BlockHeader, 0, limit)
	var last *evm.BlockHeader
	var cursor *evm.CursorBlock
	iter := newBlockIterator(fromBlock, toBlock, order)
	for iter.Next() {
		header, _, err := qe.fetchBlockViaForward(ctx, iter.Value(), false)
		if err != nil {
			return err
		}
		if header == nil {
			continue
		}
		last = header
		blocks = append(blocks, projectBlockForResponse(header, req.BlockFields))
		if len(blocks) >= int(limit) {
			if iter.HasMore() {
				cursor = cursorFromBlock(last)
			}
			break
		}
	}
	return onPage(&evm.QueryBlocksResponse{
		Blocks:      blocks,
		FromBlock:   cursorFromNumber(fromBlock),
		ToBlock:     cursorFromNumber(toBlock),
		CursorBlock: cursor,
	})
}

func (qe *EvmQueryExecutor) shimQueryTransactions(ctx context.Context, req *evm.QueryTransactionsRequest, fromBlock, toBlock uint64, onPage func(proto.Message) error) error {
	order := req.GetOrder()
	limit := queryLimit(req.GetLimit())

	txs := make([]*evm.Transaction, 0, limit)
	blocks := make([]*evm.BlockHeader, 0)
	var lastIncluded *evm.BlockHeader
	var hasMore bool
	iter := newBlockIterator(fromBlock, toBlock, order)
	for iter.Next() {
		header, blockTxs, err := qe.fetchBlockViaForward(ctx, iter.Value(), true)
		if err != nil {
			return err
		}
		if header == nil {
			continue
		}
		matched := make([]*evm.Transaction, 0)
		for _, tx := range blockTxs {
			if matchTransactionFilter(tx, req.Filter) {
				matched = append(matched, projectTransactionForResponse(tx, req.TransactionFields))
			}
		}
		if len(matched) == 0 {
			continue
		}
		if len(txs) > 0 && len(txs)+len(matched) > int(limit) {
			hasMore = true
			break
		}
		txs = append(txs, matched...)
		if req.BlockFields != nil {
			blocks = append(blocks, projectBlockForResponse(header, req.BlockFields))
		}
		lastIncluded = header
		if len(txs) >= int(limit) {
			hasMore = iter.HasMore()
			break
		}
	}
	var cursor *evm.CursorBlock
	if hasMore && lastIncluded != nil {
		cursor = cursorFromBlock(lastIncluded)
	}
	return onPage(&evm.QueryTransactionsResponse{
		Transactions: txs,
		Blocks:       blocks,
		FromBlock:    cursorFromNumber(fromBlock),
		ToBlock:      cursorFromNumber(toBlock),
		CursorBlock:  cursor,
	})
}

func (qe *EvmQueryExecutor) shimQueryLogs(ctx context.Context, req *evm.QueryLogsRequest, fromBlock, toBlock uint64, onPage func(proto.Message) error) error {
	if fromBlock > toBlock {
		return onPage(&evm.QueryLogsResponse{
			Logs:         []*evm.Log{},
			Transactions: []*evm.Transaction{},
			Blocks:       []*evm.BlockHeader{},
			FromBlock:    cursorFromNumber(fromBlock),
			ToBlock:      cursorFromNumber(toBlock),
		})
	}

	rawLogs, err := qe.fetchLogsViaForward(ctx, fromBlock, toBlock, req.Filter)
	if err != nil {
		return err
	}

	order := req.GetOrder()
	if order == evm.SortOrder_DESC {
		reverseLogs(rawLogs)
	}
	limit := queryLimit(req.GetLimit())
	rawLogs, cursor := paginateLogsByBlock(rawLogs, limit)

	logs := make([]*evm.Log, 0, len(rawLogs))
	blockMap := map[uint64]*evm.BlockHeader{}
	txMap := map[string]*evm.Transaction{}
	parentData := map[uint64]*queryLogParentData{}
	for _, log := range rawLogs {
		if req.BlockFields != nil || req.TransactionFields != nil {
			data, err := loadQueryLogParentData(
				ctx,
				parentData,
				log.BlockNumber,
				func(ctx context.Context, blockNum uint64) (*evm.BlockHeader, []*evm.Transaction, error) {
					return qe.fetchBlockViaForward(ctx, blockNum, true)
				},
			)
			if err != nil {
				return err
			}
			if req.BlockFields != nil && data.header != nil {
				if _, ok := blockMap[data.header.Number]; !ok {
					blockMap[data.header.Number] = projectBlockForResponse(data.header, req.BlockFields)
				}
			}
			if req.TransactionFields != nil && len(data.txsByHash) > 0 {
				key := string(log.TransactionHash)
				if tx, ok := data.txsByHash[key]; ok {
					if _, exists := txMap[key]; !exists {
						txCopy := proto.Clone(tx).(*evm.Transaction)
						ProjectTransactionFields(txCopy, req.TransactionFields)
						txMap[key] = txCopy
					}
				}
			}
		}
		logs = append(logs, projectLogForResponse(log, req.LogFields))
	}
	blocks := mapsValuesUint64(blockMap)
	txs := mapsValuesString(txMap)
	return onPage(&evm.QueryLogsResponse{
		Logs:         logs,
		Transactions: txs,
		Blocks:       blocks,
		FromBlock:    cursorFromNumber(fromBlock),
		ToBlock:      cursorFromNumber(toBlock),
		CursorBlock:  cursor,
	})
}

func (qe *EvmQueryExecutor) shimQueryTraces(ctx context.Context, req *evm.QueryTracesRequest, fromBlock, toBlock uint64, onPage func(proto.Message) error) error {
	order := req.GetOrder()
	limit := queryLimit(req.GetLimit())

	out := make([]*evm.Trace, 0, limit)
	blocks := map[uint64]*evm.BlockHeader{}
	txs := map[string]*evm.Transaction{}
	iter := newBlockIterator(fromBlock, toBlock, order)
	var lastIncluded uint64
	var hasMore bool
	for iter.Next() {
		blockNum := iter.Value()
		header, blockTxs, err := qe.fetchBlockViaForward(ctx, blockNum, req.TransactionFields != nil)
		if err != nil {
			return err
		}
		traces, err := qe.fetchTracesViaForward(ctx, blockNum, header)
		if err != nil {
			return err
		}
		filtered := make([]*evm.Trace, 0, len(traces))
		for _, trace := range traces {
			if matchTraceFilter(trace, req.Filter) {
				filtered = append(filtered, projectTraceForResponse(trace, req.TraceFields))
				if req.TransactionFields != nil && len(trace.TransactionHash) > 0 {
					txHash := string(trace.TransactionHash)
					if _, ok := txs[txHash]; !ok {
						for _, tx := range blockTxs {
							if string(tx.Hash) == txHash {
								txs[txHash] = projectTransactionForResponse(tx, req.TransactionFields)
								break
							}
						}
					}
				}
			}
		}
		if len(filtered) == 0 {
			continue
		}
		if len(out) > 0 && len(out)+len(filtered) > int(limit) {
			hasMore = true
			break
		}
		out = append(out, filtered...)
		if req.BlockFields != nil && header != nil {
			blockCopy := projectBlockForResponse(header, req.BlockFields)
			blocks[blockCopy.Number] = blockCopy
		}
		lastIncluded = blockNum
		if len(out) >= int(limit) {
			hasMore = iter.HasMore()
			break
		}
	}
	var cursor *evm.CursorBlock
	if hasMore && lastIncluded > 0 {
		cursor = cursorFromNumber(lastIncluded)
	}
	return onPage(&evm.QueryTracesResponse{
		Traces:       out,
		Transactions: mapsValuesString(txs),
		Blocks:       mapsValuesUint64(blocks),
		FromBlock:    cursorFromNumber(fromBlock),
		ToBlock:      cursorFromNumber(toBlock),
		CursorBlock:  cursor,
	})
}

func (qe *EvmQueryExecutor) shimQueryTransfers(ctx context.Context, req *evm.QueryTransfersRequest, fromBlock, toBlock uint64, onPage func(proto.Message) error) error {
	traceReq := &evm.QueryTracesRequest{
		FromBlock:         req.FromBlock,
		ToBlock:           req.ToBlock,
		Order:             req.Order,
		Limit:             req.Limit,
		Cursor:            req.Cursor,
		TraceFields:       &evm.TraceFieldSelection{TraceType: true, CallType: true, From: true, To: true, Value: true, TransactionHash: true, TransactionIndex: true, BlockNumber: true, BlockHash: true, TraceAddress: true, BlockTimestamp: true},
		BlockFields:       req.BlockFields,
		TransactionFields: req.TransactionFields,
	}
	var tracesPage *evm.QueryTracesResponse
	if err := qe.shimQueryTraces(ctx, traceReq, fromBlock, toBlock, func(page proto.Message) error {
		tracesPage = page.(*evm.QueryTracesResponse)
		return nil
	}); err != nil {
		return err
	}
	transfers := evm.NativeTransfersFromTraces(tracesPage.Traces)
	filtered := make([]*evm.NativeTransfer, 0, len(transfers))
	for _, transfer := range transfers {
		if matchTransferFilter(transfer, req.Filter) {
			ProjectTransferFields(transfer, req.TransferFields)
			filtered = append(filtered, transfer)
		}
	}
	return onPage(&evm.QueryTransfersResponse{
		Transfers:    filtered,
		Transactions: tracesPage.Transactions,
		Blocks:       tracesPage.Blocks,
		FromBlock:    tracesPage.FromBlock,
		ToBlock:      tracesPage.ToBlock,
		CursorBlock:  tracesPage.CursorBlock,
	})
}

func (qe *EvmQueryExecutor) fetchBlockViaForward(ctx context.Context, blockNum uint64, fullTx bool) (*evm.BlockHeader, []*evm.Transaction, error) {
	params := []interface{}{fmt.Sprintf("0x%x", blockNum), fullTx}
	result, err := qe.forwardSubrequest(ctx, "eth_getBlockByNumber", params)
	if err != nil {
		if common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if string(result) == "null" {
		return nil, nil, nil
	}
	var block evm.JsonRpcBlock
	if err := sonic.Unmarshal(result, &block); err != nil {
		return nil, nil, err
	}
	protoBlock, err := block.ToProto()
	if err != nil {
		return nil, nil, err
	}
	return protoBlock.Header, protoBlock.FullTransactions, nil
}

func (qe *EvmQueryExecutor) fetchLogsViaForward(ctx context.Context, fromBlock, toBlock uint64, filter *evm.LogFilter) ([]*evm.Log, error) {
	payload := map[string]interface{}{
		"fromBlock": fmt.Sprintf("0x%x", fromBlock),
		"toBlock":   fmt.Sprintf("0x%x", toBlock),
	}
	if filter != nil {
		if len(filter.Address) == 1 {
			payload["address"] = evm.BytesToHex(filter.Address[0])
		} else if len(filter.Address) > 1 {
			addresses := make([]string, 0, len(filter.Address))
			for _, address := range filter.Address {
				addresses = append(addresses, evm.BytesToHex(address))
			}
			payload["address"] = addresses
		}
		if len(filter.Topics) > 0 {
			topics := make([]interface{}, 0, len(filter.Topics))
			for _, topicFilter := range filter.Topics {
				if topicFilter == nil || len(topicFilter.Values) == 0 {
					topics = append(topics, nil)
					continue
				}
				if len(topicFilter.Values) == 1 {
					topics = append(topics, evm.BytesToHex(topicFilter.Values[0]))
					continue
				}
				values := make([]string, 0, len(topicFilter.Values))
				for _, value := range topicFilter.Values {
					values = append(values, evm.BytesToHex(value))
				}
				topics = append(topics, values)
			}
			payload["topics"] = topics
		}
	}
	result, err := qe.forwardSubrequest(ctx, "eth_getLogs", []interface{}{payload})
	if err != nil {
		return nil, err
	}
	var rawLogs []*evm.JsonRpcLog
	if err := sonic.Unmarshal(result, &rawLogs); err != nil {
		return nil, err
	}
	out := make([]*evm.Log, 0, len(rawLogs))
	for _, rawLog := range rawLogs {
		log, err := rawLog.ToProto()
		if err != nil {
			return nil, err
		}
		out = append(out, log)
	}
	return out, nil
}

func (qe *EvmQueryExecutor) fetchTracesViaForward(ctx context.Context, blockNum uint64, header *evm.BlockHeader) ([]*evm.Trace, error) {
	result, err := qe.forwardSubrequest(ctx, "trace_block", []interface{}{fmt.Sprintf("0x%x", blockNum)})
	if err == nil {
		var rawItems []map[string]interface{}
		if err := sonic.Unmarshal(result, &rawItems); err != nil {
			return nil, err
		}
		out := make([]*evm.Trace, 0, len(rawItems))
		for _, rawItem := range rawItems {
			trace, err := evm.TraceFromParity(rawItem, blockNum, headerHash(header), headerTimestamp(header))
			if err != nil {
				return nil, err
			}
			out = append(out, trace)
		}
		return out, nil
	}
	if !common.HasErrorCode(err, common.ErrCodeEndpointUnsupported) {
		return nil, err
	}

	debugResult, err := qe.forwardSubrequest(ctx, "debug_traceBlockByNumber", []interface{}{
		fmt.Sprintf("0x%x", blockNum),
		map[string]interface{}{"tracer": "callTracer"},
	})
	if err != nil {
		if common.HasErrorCode(err, common.ErrCodeEndpointUnsupported) {
			return nil, status.Error(codes.Unimplemented, "eth_queryTraces requires trace_block or debug_traceBlockByNumber support")
		}
		return nil, err
	}
	var nested []map[string]interface{}
	if err := sonic.Unmarshal(debugResult, &nested); err == nil {
		out := make([]*evm.Trace, 0, len(nested))
		for _, item := range nested {
			traces, err := evm.TraceFromGethDebug(item, blockNum, headerHash(header), headerTimestamp(header))
			if err != nil {
				return nil, err
			}
			out = append(out, traces...)
		}
		return out, nil
	}
	var single map[string]interface{}
	if err := sonic.Unmarshal(debugResult, &single); err != nil {
		return nil, err
	}
	return evm.TraceFromGethDebug(single, blockNum, headerHash(header), headerTimestamp(header))
}

func (qe *EvmQueryExecutor) forwardSubrequest(ctx context.Context, method string, params interface{}) ([]byte, error) {
	if qe.forwardSubrequestFn != nil {
		return qe.forwardSubrequestFn(ctx, method, params)
	}

	body := buildJSONRPCRequest(method, params)
	req := common.NewNormalizedRequest(body)
	req.SetNetwork(qe.network)
	req.ApplyDirectiveDefaults(qe.network.Config().DirectiveDefaults)
	resp, err := qe.network.Forward(ctx, req)
	if err != nil {
		return nil, err
	}
	return parseJSONRPCResult(ctx, resp)
}

func queryLimit(limit uint32) uint32 {
	if limit == 0 {
		return 100
	}
	return limit
}

type queryLogParentData struct {
	header    *evm.BlockHeader
	txsByHash map[string]*evm.Transaction
}

func loadQueryLogParentData(
	ctx context.Context,
	cache map[uint64]*queryLogParentData,
	blockNum uint64,
	fetch func(context.Context, uint64) (*evm.BlockHeader, []*evm.Transaction, error),
) (*queryLogParentData, error) {
	if data, ok := cache[blockNum]; ok {
		return data, nil
	}

	header, txs, err := fetch(ctx, blockNum)
	if err != nil {
		return nil, err
	}

	data := &queryLogParentData{
		header:    header,
		txsByHash: make(map[string]*evm.Transaction, len(txs)),
	}
	for _, tx := range txs {
		data.txsByHash[string(tx.Hash)] = tx
	}
	cache[blockNum] = data
	return data, nil
}

type blockIterator struct {
	current uint64
	end     uint64
	desc    bool
	started bool
}

func newBlockIterator(from, to uint64, order evm.SortOrder) *blockIterator {
	if order == evm.SortOrder_DESC {
		return &blockIterator{current: to, end: from, desc: true}
	}
	return &blockIterator{current: from, end: to}
}

func (it *blockIterator) Next() bool {
	if !it.started {
		it.started = true
		if it.desc {
			return it.current >= it.end
		}
		return it.current <= it.end
	}
	if it.desc {
		if it.current == 0 {
			return false
		}
		it.current--
		return it.current >= it.end
	}
	it.current++
	return it.current <= it.end
}

func (it *blockIterator) Value() uint64 { return it.current }

func (it *blockIterator) HasMore() bool {
	if it.desc {
		return it.current > it.end
	}
	return it.current < it.end
}

func matchTransactionFilter(tx *evm.Transaction, filter *evm.TransactionFilter) bool {
	if filter == nil || tx == nil {
		return true
	}
	if len(filter.From) > 0 && !bytesMatchAny(tx.From, filter.From) {
		return false
	}
	if len(filter.To) > 0 && !bytesMatchAny(tx.To, filter.To) {
		return false
	}
	if len(filter.Selector) > 0 {
		if len(tx.Input) < 4 || !bytesPrefixMatchAny(tx.Input[:4], filter.Selector) {
			return false
		}
	}
	return true
}

func matchTraceFilter(trace *evm.Trace, filter *evm.TraceFilter) bool {
	if filter == nil || trace == nil {
		return true
	}
	if len(filter.From) > 0 && !bytesMatchAny(trace.From, filter.From) {
		return false
	}
	if len(filter.To) > 0 && !bytesMatchAny(trace.To, filter.To) {
		return false
	}
	if len(filter.Selector) > 0 {
		if len(trace.Input) < 4 || !bytesPrefixMatchAny(trace.Input[:4], filter.Selector) {
			return false
		}
	}
	if filter.IsTopLevel != nil && *filter.IsTopLevel && len(trace.TraceAddress) > 0 {
		return false
	}
	return true
}

func matchTransferFilter(transfer *evm.NativeTransfer, filter *evm.TransferFilter) bool {
	if filter == nil || transfer == nil {
		return true
	}
	if len(filter.From) > 0 && !bytesMatchAny(transfer.From, filter.From) {
		return false
	}
	if len(filter.To) > 0 && !bytesMatchAny(transfer.To, filter.To) {
		return false
	}
	if filter.IsTopLevel != nil && *filter.IsTopLevel && len(transfer.TraceAddress) > 0 {
		return false
	}
	return true
}

func bytesMatchAny(value []byte, candidates [][]byte) bool {
	for _, candidate := range candidates {
		if string(value) == string(candidate) {
			return true
		}
	}
	return false
}

func bytesPrefixMatchAny(value []byte, candidates [][]byte) bool {
	for _, candidate := range candidates {
		if string(value) == string(candidate) {
			return true
		}
	}
	return false
}

func reverseLogs(logs []*evm.Log) {
	for i, j := 0, len(logs)-1; i < j; i, j = i+1, j-1 {
		logs[i], logs[j] = logs[j], logs[i]
	}
}

func paginateLogsByBlock(rawLogs []*evm.Log, limit uint32) ([]*evm.Log, *evm.CursorBlock) {
	if len(rawLogs) == 0 || limit == 0 {
		return rawLogs, nil
	}

	page := make([]*evm.Log, 0, min(int(limit), len(rawLogs)))
	var lastBlock uint64
	var hasMore bool

	for i := 0; i < len(rawLogs); {
		blockNumber := rawLogs[i].BlockNumber
		blockEnd := i + 1
		for blockEnd < len(rawLogs) && rawLogs[blockEnd].BlockNumber == blockNumber {
			blockEnd++
		}

		blockLogs := rawLogs[i:blockEnd]
		if len(page) > 0 && len(page)+len(blockLogs) > int(limit) {
			hasMore = true
			break
		}

		page = append(page, blockLogs...)
		lastBlock = blockNumber
		i = blockEnd
	}

	if !hasMore {
		return page, nil
	}

	return page, cursorFromNumber(lastBlock)
}

func cursorFromBlock(block *evm.BlockHeader) *evm.CursorBlock {
	if block == nil {
		return nil
	}
	return &evm.CursorBlock{
		Number:     block.Number,
		Hash:       block.Hash,
		ParentHash: block.ParentHash,
	}
}

func projectBlockForResponse(block *evm.BlockHeader, sel *evm.BlockFieldSelection) *evm.BlockHeader {
	if block == nil {
		return nil
	}
	blockCopy := proto.Clone(block).(*evm.BlockHeader)
	ProjectBlockFields(blockCopy, sel)
	return blockCopy
}

func projectLogForResponse(log *evm.Log, sel *evm.LogFieldSelection) *evm.Log {
	if log == nil {
		return nil
	}
	logCopy := proto.Clone(log).(*evm.Log)
	ProjectLogFields(logCopy, sel)
	return logCopy
}

func projectTransactionForResponse(tx *evm.Transaction, sel *evm.TransactionFieldSelection) *evm.Transaction {
	if tx == nil {
		return nil
	}
	txCopy := proto.Clone(tx).(*evm.Transaction)
	ProjectTransactionFields(txCopy, sel)
	return txCopy
}

func projectTraceForResponse(trace *evm.Trace, sel *evm.TraceFieldSelection) *evm.Trace {
	if trace == nil {
		return nil
	}
	traceCopy := proto.Clone(trace).(*evm.Trace)
	ProjectTraceFields(traceCopy, sel)
	return traceCopy
}

func cursorFromNumber(num uint64) *evm.CursorBlock {
	return &evm.CursorBlock{Number: num}
}

func headerHash(header *evm.BlockHeader) []byte {
	if header == nil {
		return nil
	}
	return header.Hash
}

func headerTimestamp(header *evm.BlockHeader) *uint64 {
	if header == nil {
		return nil
	}
	return &header.Timestamp
}

func mapsValuesUint64[T any](m map[uint64]T) []T {
	out := make([]T, 0, len(m))
	for _, value := range m {
		out = append(out, value)
	}
	return out
}

func mapsValuesString[T any](m map[string]T) []T {
	out := make([]T, 0, len(m))
	for _, value := range m {
		out = append(out, value)
	}
	return out
}
