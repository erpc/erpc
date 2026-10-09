package erpc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
)

// The query shim builds MIP-16 pages from standard JSON-RPC sub-requests
// (eth_getBlockByNumber, eth_getBlockReceipts, eth_getLogs, trace_block or
// debug_traceBlockByNumber), each sent through Network.Forward so cache,
// failsafe and upstream selection apply as to any request.
//
// A page scans blocks from its fromBlock in traversal order, in chunks fetched
// concurrently. A block is complete when its header, its primary objects and
// the transactions they join (with receipts when selected) are fetched; all
// of it runs under the page's time budget. The page ends at the first of:
// toBlock; the block that brings the primary count to target; the budget
// (blocks per page, or time), which discards the unfinished block; a break
// in chain consistency.
//
// Chain consistency (MIP-16): every object of a block must carry the hash of
// the block's header (log, receipt and trace blockHash; the transaction hash
// at each index for callTracer results, which carry no block hash), and the
// headers of the page must link by parentHash in traversal order. The
// sub-requests of a page prefer the upstream that answered the first one. At
// a break the blocks from the one before it are scanned once more with cache
// reads skipped, and the page is cut at the last consistent block.

// errQueryPageBudget is the cause of a page's own scan deadline, which tells
// it apart from the caller's deadline and from a sub-request's timeout.
var errQueryPageBudget = errors.New("query page time budget exhausted")

const (
	queryShimConcurrency = 16
	queryShimFirstChunk  = 4
	queryShimMaxChunk    = 64
)

// queryTraceSource* are the values of Network.queryTraceSource.
const (
	queryTraceSourceUnknown int32 = iota
	queryTraceSourceParity
	queryTraceSourceDebug
)

// shimBlock is one scanned block: its header, its matching primary objects
// sorted by their MIP-16 ordering key ascending (only the slice of the
// method's primary type is set), and the transactions they join.
type shimBlock struct {
	number uint64
	header *evm.BlockHeader
	// consistent: every object and joined transaction of the block carries
	// the hash of header.
	consistent   bool
	blocks       []*evm.BlockHeader
	transactions []*evm.Transaction
	logs         []*evm.Log
	traces       []*evm.Trace
	transfers    []*evm.NativeTransfer
	related      []*evm.Transaction
}

func (b *shimBlock) count() int {
	return len(b.blocks) + len(b.transactions) + len(b.logs) + len(b.traces) + len(b.transfers)
}

// shimPageState is the state the sub-requests of one page share.
type shimPageState struct {
	plan      *queryPlan
	req       proto.Message
	skipCache bool

	mu     sync.Mutex
	pinned string
}

func (pg *shimPageState) pin() string {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	return pg.pinned
}

// setPin pins the page to the upstream that served a sub-request, the first
// time one is known, or in place of replaced when that pinned upstream just
// failed or lagged. An id that is not pinnable is not pinned.
func (pg *shimPageState) setPin(upstreamID, replaced string) {
	if !pinnable(upstreamID) {
		return
	}
	pg.mu.Lock()
	if pg.pinned == "" || (replaced != "" && pg.pinned == replaced) {
		pg.pinned = upstreamID
	}
	pg.mu.Unlock()
}

func (qe *EvmQueryExecutor) runShim(ctx context.Context, p *queryPlan, req proto.Message, onPage func(proto.Message) error) error {
	pageFrom := p.from
	for {
		page, cursor, err := qe.shimPage(ctx, p, req, pageFrom)
		if err != nil {
			return err
		}
		if qe.projectShimPages {
			page = projectQueryPage(req, page)
		}
		if err := onPage(page); err != nil {
			return err
		}
		if cursor == p.to {
			return nil
		}
		pageFrom = p.next(cursor)
	}
}

// shimPage builds the page that starts at pageFrom and returns it with its
// cursor block number.
func (qe *EvmQueryExecutor) shimPage(ctx context.Context, p *queryPlan, req proto.Message, pageFrom uint64) (proto.Message, uint64, error) {
	ctx, span := common.StartDetailSpan(ctx, "Query.ShimPage", trace.WithAttributes(
		attribute.String("query.method", p.method),
		attribute.String("query.pageFrom", fmt.Sprintf("%#x", pageFrom)),
	))
	defer span.End()
	pg := &shimPageState{plan: p, req: req}

	remaining := p.to - pageFrom + 1
	if p.desc {
		remaining = pageFrom - p.to + 1
	}
	maxBlocks := min(remaining, uint64(p.budget.maxBlocks))

	scanCtx, cancelScan := context.WithTimeoutCause(ctx, p.budget.maxDuration, errQueryPageBudget)
	defer cancelScan()

	var scanned []*shimBlock
	count := 0
	chunk := uint64(queryShimFirstChunk)
	next := pageFrom
scan:
	for uint64(len(scanned)) < maxBlocks {
		numbers := make([]uint64, min(chunk, maxBlocks-uint64(len(scanned))))
		for i := range numbers {
			numbers[i] = next
			next = p.next(next)
		}
		blocks, err := qe.scanChunk(scanCtx, pg, numbers)
		for _, b := range blocks {
			scanned = append(scanned, b)
			count += b.count()
			if p.target > 0 && count >= int(p.target) {
				break scan
			}
		}
		if err != nil {
			if !isBudgetStop(ctx, scanCtx, err) {
				common.SetTraceSpanError(span, err)
				return nil, 0, err
			}
			// The time budget ran out inside this chunk: the page ends at the
			// last complete block.
			if len(scanned) == 0 {
				return nil, 0, queryBudgetExceeded("block %#x did not complete within the page budget of %s", pageFrom, p.budget.maxDuration)
			}
			span.SetAttributes(attribute.Bool("query.budgetReached", true))
			break
		}
		// Blocks past a chain break cannot join the page unless the re-scan
		// below mends it, so scanning further is wasted.
		if consistentPrefix(scanned, p.desc) < len(scanned) {
			break
		}
		chunk = min(chunk*2, queryShimMaxChunk)
	}

	// The consistency re-scan and the toBlock header get their own bound, so
	// a page whose scan used the whole budget can still be returned.
	finishCtx, cancelFinish := context.WithTimeout(ctx, p.budget.maxDuration)
	defer cancelFinish()
	page, err := qe.consistentBlocks(ctx, finishCtx, pg, scanned)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, 0, err
	}
	if len(page) == 0 {
		return nil, 0, queryBudgetExceeded("block %#x is not consistent across sub-requests (reorg in progress)", pageFrom)
	}
	cursor := page[len(page)-1].number
	span.SetAttributes(attribute.Int("query.blocksScanned", len(scanned)), attribute.Int("query.pageBlocks", len(page)))

	resp, err := qe.assemblePage(finishCtx, pg, page)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, 0, err
	}
	return resp, cursor, nil
}

// isBudgetStop reports whether err ended the scan because the page's own time
// budget ran out, rather than an upstream failure, a sub-request's timeout or
// the caller leaving. Network.Forward returns the deadline's cause itself, or
// a deadline or timeout error caused by it.
func isBudgetStop(ctx, scanCtx context.Context, err error) bool {
	if ctx.Err() != nil || !errors.Is(context.Cause(scanCtx), errQueryPageBudget) {
		return false
	}
	return errors.Is(err, errQueryPageBudget) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		common.HasErrorCode(err,
			common.ErrCodeNetworkRequestTimeout,
			common.ErrCodeFailsafeTimeoutExceeded,
			common.ErrCodeEndpointRequestTimeout,
		)
}

// consistentBlocks returns the longest leading run of scanned that is
// consistent: each block's objects carry its header hash, and the headers
// link in traversal order. At the first break it scans the blocks from the
// one before the break again, once, skipping cache reads and pinned afresh,
// and keeps whichever run is longer. The run is then cut at target again,
// since a re-scanned block may hold more objects. The re-scan runs under
// finishCtx; ctx is the caller's.
func (qe *EvmQueryExecutor) consistentBlocks(ctx, finishCtx context.Context, pg *shimPageState, scanned []*shimBlock) ([]*shimBlock, error) {
	p := pg.plan
	k := consistentPrefix(scanned, p.desc)
	if k == len(scanned) {
		return scanned, nil
	}
	start := max(k-1, 0)
	qe.logger.Debug().Str("method", p.method).Uint64("block", scanned[k].number).
		Msg("query page is not consistent across sub-requests, scanning its tail again without cache")
	numbers := make([]uint64, len(scanned)-start)
	for i := range numbers {
		numbers[i] = scanned[start+i].number
	}
	again := &shimPageState{plan: p, req: pg.req, skipCache: true}
	merged := append(make([]*shimBlock, 0, len(scanned)), scanned[:start]...)
	count := 0
	for _, b := range merged {
		count += b.count()
	}
	// Best effort, in chunks of the scan's size: a re-scan that fails, runs
	// out of time or breaks again keeps the blocks it completed, and the
	// first scan's consistent run stays the floor.
	for i := 0; i < len(numbers); i += queryShimMaxChunk {
		blocks, err := qe.scanChunk(finishCtx, again, numbers[i:min(i+queryShimMaxChunk, len(numbers))])
		merged = append(merged, blocks...)
		for _, b := range blocks {
			count += b.count()
		}
		if err != nil {
			qe.logger.Debug().Err(err).Str("method", p.method).Msg("query page re-scan ended early")
			break
		}
		if consistentPrefix(merged, p.desc) < len(merged) || (p.target > 0 && count >= int(p.target)) {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	best := scanned[:k]
	if k2 := consistentPrefix(merged, p.desc); k2 > k {
		best = merged[:k2]
	}
	if p.target == 0 {
		return best, nil
	}
	count = 0
	for i, b := range best {
		if count += b.count(); count >= int(p.target) {
			return best[:i+1], nil
		}
	}
	return best, nil
}

// consistentPrefix returns how many leading blocks are consistent and link
// by hash in traversal order.
func consistentPrefix(blocks []*shimBlock, desc bool) int {
	for i, b := range blocks {
		if !b.consistent {
			return i
		}
		if i == 0 {
			continue
		}
		prev := blocks[i-1].header
		linked := bytes.Equal(b.header.ParentHash, prev.Hash)
		if desc {
			linked = bytes.Equal(prev.ParentHash, b.header.Hash)
		}
		if !linked {
			return i
		}
	}
	return len(blocks)
}

// scanChunk scans the blocks numbers (consecutive, in traversal order) and
// returns the complete ones up to the first that failed or did not finish.
func (qe *EvmQueryExecutor) scanChunk(ctx context.Context, pg *shimPageState, numbers []uint64) ([]*shimBlock, error) {
	var logs map[uint64][]*evm.Log
	if r, ok := pg.req.(*evm.QueryLogsRequest); ok {
		var err error
		if logs, err = qe.chunkLogs(ctx, pg, r, numbers); err != nil {
			return nil, err
		}
	}
	results := make([]*shimBlock, len(numbers))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(queryShimConcurrency)
	for i, n := range numbers {
		g.Go(func() error {
			b, err := qe.scanBlock(gctx, pg, n, logs[n])
			if err != nil {
				return err
			}
			results[i] = b
			return nil
		})
	}
	err := g.Wait()
	done := 0
	for done < len(results) && results[done] != nil {
		done++
	}
	return results[:done], err
}

// chunkLogs reads the logs of a chunk with one eth_getLogs, which applies the
// address and topics filter with the same semantics MIP-16 requires, and
// groups them by block, sorted by logIndex.
func (qe *EvmQueryExecutor) chunkLogs(ctx context.Context, pg *shimPageState, r *evm.QueryLogsRequest, numbers []uint64) (map[uint64][]*evm.Log, error) {
	lo, hi := numbers[0], numbers[len(numbers)-1]
	if pg.plan.desc {
		lo, hi = hi, lo
	}
	logs, err := qe.fetchLogs(ctx, pg, lo, hi, r.GetFilter())
	if err != nil {
		return nil, err
	}
	byBlock := make(map[uint64][]*evm.Log)
	// Each topics position is a condition, a null one included: a log needs
	// at least as many topics as the filter has positions (eth_getLogs
	// semantics). Some nodes drop trailing null positions, so check here.
	minTopics := len(r.GetFilter().GetTopics())
	for _, l := range logs {
		if l.BlockNumber < lo || l.BlockNumber > hi {
			return nil, fmt.Errorf("eth_getLogs for %#x-%#x returned a log of block %#x", lo, hi, l.BlockNumber)
		}
		if len(l.Topics) < minTopics {
			continue
		}
		byBlock[l.BlockNumber] = append(byBlock[l.BlockNumber], l)
	}
	for _, blockLogs := range byBlock {
		slices.SortFunc(blockLogs, func(a, b *evm.Log) int { return compareUint(a.LogIndex, b.LogIndex) })
	}
	return byBlock, nil
}

// scanBlock completes block n: its header, its primary objects (logs are
// read per chunk and passed in) and the transactions they join. Every check
// of the block's objects against its header sets b.consistent.
func (qe *EvmQueryExecutor) scanBlock(ctx context.Context, pg *shimPageState, n uint64, logs []*evm.Log) (*shimBlock, error) {
	b := &shimBlock{number: n, consistent: true}
	switch r := pg.req.(type) {
	case *evm.QueryBlocksRequest:
		block, err := qe.fetchBlock(ctx, pg, n, false)
		if err != nil {
			return nil, err
		}
		b.header = block.Header
		if matchesAny(b.header.Miner, r.GetFilter().GetMiner()) {
			b.blocks = []*evm.BlockHeader{b.header}
		}
		return b, nil

	case *evm.QueryTransactionsRequest:
		block, err := qe.fetchBlock(ctx, pg, n, true)
		if err != nil {
			return nil, err
		}
		b.header = block.Header
		f := r.GetFilter()
		for _, tx := range block.FullTransactions {
			if !bytes.Equal(tx.BlockHash, b.header.Hash) {
				b.consistent = false
			}
			if matchesAny(tx.From, f.GetFrom()) && matchesAny(tx.To, f.GetTo()) && matchesSelector(tx.Input, f.GetSelector()) {
				b.transactions = append(b.transactions, tx)
			}
		}
		if len(b.transactions) > 0 && needsReceipts(r.GetTransactionFields()) {
			ok, err := qe.mergeReceipts(ctx, pg, b.header, b.transactions)
			if err != nil {
				return nil, err
			}
			b.consistent = b.consistent && ok
		}
		slices.SortFunc(b.transactions, compareTransactions)
		return b, nil

	case *evm.QueryLogsRequest:
		// The header is fetched for every block: the page's hash chain needs
		// it. Full transactions only when the block has logs to join.
		block, err := qe.fetchBlock(ctx, pg, n, r.GetTransactionFields() != nil && len(logs) > 0)
		if err != nil {
			return nil, err
		}
		b.header = block.Header
		b.logs = logs
		hashes := make([][]byte, len(logs))
		for i, l := range logs {
			if !bytes.Equal(l.BlockHash, b.header.Hash) {
				b.consistent = false
			}
			// Nodes that omit blockTimestamp get the header's.
			if l.BlockTimestamp == nil {
				ts := b.header.Timestamp
				l.BlockTimestamp = &ts
			}
			hashes[i] = l.TransactionHash
		}
		return b, qe.joinTransactions(ctx, pg, b, r.GetTransactionFields(), hashes, block.FullTransactions)

	case *evm.QueryTracesRequest:
		block, err := qe.fetchBlock(ctx, pg, n, r.GetTransactionFields() != nil)
		if err != nil {
			return nil, err
		}
		b.header = block.Header
		traces, ok, err := qe.blockTraces(ctx, pg, block)
		if err != nil {
			return nil, err
		}
		b.consistent = ok
		f := r.GetFilter()
		var hashes [][]byte
		for _, t := range traces {
			if (t.Reverted && !f.GetIncludeReverted()) || !matchesAny(t.From, f.GetFrom()) || !matchesAny(t.To, f.GetTo()) || !matchesSelector(t.Input, f.GetSelector()) {
				continue
			}
			b.traces = append(b.traces, t)
			hashes = append(hashes, t.TransactionHash)
		}
		slices.SortFunc(b.traces, func(x, y *evm.Trace) int {
			return compareFrames(x.TransactionIndex, x.TraceAddress, y.TransactionIndex, y.TraceAddress)
		})
		return b, qe.joinTransactions(ctx, pg, b, r.GetTransactionFields(), hashes, block.FullTransactions)

	case *evm.QueryTransfersRequest:
		block, err := qe.fetchBlock(ctx, pg, n, r.GetTransactionFields() != nil)
		if err != nil {
			return nil, err
		}
		b.header = block.Header
		traces, ok, err := qe.blockTraces(ctx, pg, block)
		if err != nil {
			return nil, err
		}
		b.consistent = ok
		f := r.GetFilter()
		var hashes [][]byte
		for _, t := range evm.NativeTransfersFromTraces(traces) {
			if (t.Reverted && !f.GetIncludeReverted()) || !matchesAny(t.From, f.GetFrom()) || !matchesAny(t.To, f.GetTo()) {
				continue
			}
			b.transfers = append(b.transfers, t)
			hashes = append(hashes, t.TransactionHash)
		}
		slices.SortFunc(b.transfers, func(x, y *evm.NativeTransfer) int {
			return compareFrames(x.TransactionIndex, x.TraceAddress, y.TransactionIndex, y.TraceAddress)
		})
		return b, qe.joinTransactions(ctx, pg, b, r.GetTransactionFields(), hashes, block.FullTransactions)
	}
	return nil, queryInvalidParams("unknown query request type %T", pg.req)
}

// joinTransactions picks, from txs (the block's transactions, fetched with its
// header), the ones the block's primary objects reference, once each, when
// the request joins them. A missing transaction, or a transaction or receipt
// of another block hash than b.header's, marks the block inconsistent.
func (qe *EvmQueryExecutor) joinTransactions(ctx context.Context, pg *shimPageState, b *shimBlock, sel *evm.TransactionFieldSelection, hashes [][]byte, txs []*evm.Transaction) error {
	if sel == nil || len(hashes) == 0 || !b.consistent {
		return nil
	}
	wanted := make(map[string]struct{}, len(hashes))
	for _, hash := range hashes {
		wanted[string(hash)] = struct{}{}
	}
	for _, tx := range txs {
		if _, ok := wanted[string(tx.Hash)]; ok {
			if !bytes.Equal(tx.BlockHash, b.header.Hash) {
				b.consistent = false
				return nil
			}
			b.related = append(b.related, tx)
		}
	}
	if len(b.related) < len(wanted) {
		// An object references a transaction this block does not hold.
		b.consistent = false
		return nil
	}
	if needsReceipts(sel) {
		ok, err := qe.mergeReceipts(ctx, pg, b.header, b.related)
		if err != nil {
			return err
		}
		b.consistent = b.consistent && ok
	}
	slices.SortFunc(b.related, compareTransactions)
	return nil
}

// assemblePage builds the response of the page's blocks: primary objects in
// traversal order, the selected relations, and the block references, all
// from the verified headers of the page (toBlock's when it is beyond it).
func (qe *EvmQueryExecutor) assemblePage(ctx context.Context, pg *shimPageState, page []*shimBlock) (proto.Message, error) {
	p := pg.plan
	first, last := page[0], page[len(page)-1]
	fromRef, cursorRef := blockRefOf(first.header), blockRefOf(last.header)
	toRef := cursorRef
	if last.number != p.to {
		block, err := qe.fetchBlock(ctx, pg, p.to, false)
		if err != nil {
			return nil, err
		}
		toRef = blockRefOf(block.Header)
	}

	relatedTxs := func(sel *evm.TransactionFieldSelection) []*evm.Transaction {
		if sel == nil {
			return nil
		}
		txs := []*evm.Transaction{}
		for _, b := range page {
			txs = appendInOrder(txs, b.related, p.desc)
		}
		return txs
	}
	relatedBlocks := func(sel *evm.BlockFieldSelection) []*evm.BlockHeader {
		if sel == nil {
			return nil
		}
		blocks := []*evm.BlockHeader{}
		for _, b := range page {
			if b.count() > 0 {
				blocks = append(blocks, b.header)
			}
		}
		return blocks
	}

	switch r := pg.req.(type) {
	case *evm.QueryBlocksRequest:
		resp := &evm.QueryBlocksResponse{Blocks: []*evm.BlockHeader{}, FromBlock: fromRef, ToBlock: toRef, CursorBlock: cursorRef}
		for _, b := range page {
			resp.Blocks = append(resp.Blocks, b.blocks...)
		}
		return resp, nil

	case *evm.QueryTransactionsRequest:
		resp := &evm.QueryTransactionsResponse{Transactions: []*evm.Transaction{}, FromBlock: fromRef, ToBlock: toRef, CursorBlock: cursorRef}
		for _, b := range page {
			resp.Transactions = appendInOrder(resp.Transactions, b.transactions, p.desc)
		}
		resp.Blocks = relatedBlocks(r.GetBlockFields())
		return resp, nil

	case *evm.QueryLogsRequest:
		resp := &evm.QueryLogsResponse{Logs: []*evm.Log{}, FromBlock: fromRef, ToBlock: toRef, CursorBlock: cursorRef}
		for _, b := range page {
			resp.Logs = appendInOrder(resp.Logs, b.logs, p.desc)
		}
		resp.Transactions = relatedTxs(r.GetTransactionFields())
		resp.Blocks = relatedBlocks(r.GetBlockFields())
		return resp, nil

	case *evm.QueryTracesRequest:
		resp := &evm.QueryTracesResponse{Traces: []*evm.Trace{}, FromBlock: fromRef, ToBlock: toRef, CursorBlock: cursorRef}
		for _, b := range page {
			resp.Traces = appendInOrder(resp.Traces, b.traces, p.desc)
		}
		resp.Transactions = relatedTxs(r.GetTransactionFields())
		resp.Blocks = relatedBlocks(r.GetBlockFields())
		return resp, nil

	case *evm.QueryTransfersRequest:
		resp := &evm.QueryTransfersResponse{Transfers: []*evm.NativeTransfer{}, FromBlock: fromRef, ToBlock: toRef, CursorBlock: cursorRef}
		for _, b := range page {
			resp.Transfers = appendInOrder(resp.Transfers, b.transfers, p.desc)
		}
		resp.Transactions = relatedTxs(r.GetTransactionFields())
		resp.Blocks = relatedBlocks(r.GetBlockFields())
		return resp, nil
	}
	return nil, queryInvalidParams("unknown query request type %T", pg.req)
}

func blockRefOf(h *evm.BlockHeader) *evm.CursorBlock {
	return &evm.CursorBlock{Number: h.Number, Hash: h.Hash, ParentHash: h.ParentHash}
}

// appendInOrder appends one block's objects, sorted ascending, in traversal
// order: reversed in desc, so the whole array is descending.
func appendInOrder[T any](dst, blockObjects []T, desc bool) []T {
	if !desc {
		return append(dst, blockObjects...)
	}
	for i := len(blockObjects) - 1; i >= 0; i-- {
		dst = append(dst, blockObjects[i])
	}
	return dst
}

// Filters. An empty list places no constraint (MIP-16 Filters).

func matchesAny(value []byte, candidates [][]byte) bool {
	if len(candidates) == 0 {
		return true
	}
	for _, c := range candidates {
		if bytes.Equal(value, c) {
			return true
		}
	}
	return false
}

// matchesSelector matches the first 4 bytes of input; an input shorter than 4
// bytes never matches a selector filter.
func matchesSelector(input []byte, selectors [][]byte) bool {
	if len(selectors) == 0 {
		return true
	}
	return len(input) >= 4 && matchesAny(input[:4], selectors)
}

// needsReceipts reports whether a transaction selection reads receipt fields;
// nil selects every field.
func needsReceipts(sel *evm.TransactionFieldSelection) bool {
	return sel == nil || sel.Status || sel.GasUsed || sel.CumulativeGasUsed || sel.EffectiveGasPrice || sel.ContractAddress || sel.LogsBloom
}

// Ordering keys.

func compareUint[T ~uint32 | ~uint64](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func compareTransactions(a, b *evm.Transaction) int {
	return compareUint(a.GetTransactionIndex(), b.GetTransactionIndex())
}

// compareFrames orders call frames by (transactionIndex, traceAddress), with
// traceAddress compared element-wise and a prefix before its extensions.
func compareFrames(txA uint32, addrA []uint32, txB uint32, addrB []uint32) int {
	if c := compareUint(txA, txB); c != 0 {
		return c
	}
	return slices.Compare(addrA, addrB)
}

// Sub-requests.

// fetchBlock returns block n: its header and transaction hashes, and its
// full transactions when fullTx.
func (qe *EvmQueryExecutor) fetchBlock(ctx context.Context, pg *shimPageState, n uint64, fullTx bool) (*evm.Block, error) {
	result, err := qe.forwardSubrequest(ctx, pg, "eth_getBlockByNumber", []interface{}{hexQuantity(n), fullTx})
	if err != nil {
		if common.HasErrorCode(err, common.ErrCodeEndpointMissingData, common.ErrCodeUpstreamBlockUnavailable) {
			return nil, queryRangeUnavailable("block %#x is not available: %v", n, err)
		}
		return nil, err
	}
	if string(result) == "null" {
		return nil, queryRangeUnavailable("block %#x is not available", n)
	}
	var block evm.JsonRpcBlock
	if err := sonic.Unmarshal(result, &block); err != nil {
		return nil, fmt.Errorf("decode block %#x: %w", n, err)
	}
	protoBlock, err := block.ToProto()
	if err != nil {
		return nil, fmt.Errorf("decode block %#x: %w", n, err)
	}
	if protoBlock.Header.Number != n {
		return nil, fmt.Errorf("eth_getBlockByNumber %#x returned block %#x", n, protoBlock.Header.Number)
	}
	return protoBlock, nil
}

// mergeReceipts merges the receipts of the block of header into txs. It
// reports false when a receipt is missing or belongs to another block hash.
func (qe *EvmQueryExecutor) mergeReceipts(ctx context.Context, pg *shimPageState, header *evm.BlockHeader, txs []*evm.Transaction) (bool, error) {
	n := header.Number
	result, err := qe.forwardSubrequest(ctx, pg, "eth_getBlockReceipts", []interface{}{hexQuantity(n)})
	if err != nil {
		return false, err
	}
	var raw []*evm.JsonRpcReceipt
	if err := sonic.Unmarshal(result, &raw); err != nil {
		return false, fmt.Errorf("decode receipts of block %#x: %w", n, err)
	}
	byHash := make(map[string]*evm.Receipt, len(raw))
	for _, r := range raw {
		receipt, err := r.ToProto()
		if err != nil {
			return false, fmt.Errorf("decode receipts of block %#x: %w", n, err)
		}
		if !bytes.Equal(receipt.BlockHash, header.Hash) {
			return false, nil
		}
		byHash[string(receipt.TransactionHash)] = receipt
	}
	for _, tx := range txs {
		receipt, ok := byHash[string(tx.Hash)]
		if !ok {
			return false, nil
		}
		evm.MergeReceipt(tx, receipt)
	}
	return true, nil
}

func (qe *EvmQueryExecutor) fetchLogs(ctx context.Context, pg *shimPageState, fromBlock, toBlock uint64, filter *evm.LogFilter) ([]*evm.Log, error) {
	payload := map[string]interface{}{
		"fromBlock": hexQuantity(fromBlock),
		"toBlock":   hexQuantity(toBlock),
	}
	if addrs := filter.GetAddress(); len(addrs) > 0 {
		hexes := make([]string, len(addrs))
		for i, a := range addrs {
			hexes[i] = evm.BytesToHex(a)
		}
		payload["address"] = hexes
	}
	if topicFilters := filter.GetTopics(); len(topicFilters) > 0 {
		topics := make([]interface{}, len(topicFilters))
		for i, tf := range topicFilters {
			if len(tf.GetValues()) == 0 {
				continue // null: any value, but the position must exist
			}
			values := make([]string, len(tf.GetValues()))
			for j, v := range tf.GetValues() {
				values[j] = evm.BytesToHex(v)
			}
			topics[i] = values
		}
		payload["topics"] = topics
	}
	result, err := qe.forwardSubrequest(ctx, pg, "eth_getLogs", []interface{}{payload})
	if err != nil {
		return nil, err
	}
	var raw []*evm.JsonRpcLog
	if err := sonic.Unmarshal(result, &raw); err != nil {
		return nil, fmt.Errorf("decode logs: %w", err)
	}
	out := make([]*evm.Log, len(raw))
	for i, r := range raw {
		if out[i], err = r.ToProto(); err != nil {
			return nil, fmt.Errorf("decode logs: %w", err)
		}
	}
	return out, nil
}

// blockTraces returns every call frame of block, with reverted set per
// MIP-16, and whether the frames belong to block: trace_block frames carry a
// block hash; callTracer results carry none, so each result's txHash must be
// the block's transaction at its index. It uses trace_block, else
// debug_traceBlockByNumber with the callTracer, and remembers on the network
// which one answered.
func (qe *EvmQueryExecutor) blockTraces(ctx context.Context, pg *shimPageState, block *evm.Block) ([]*evm.Trace, bool, error) {
	h := block.Header
	n := h.Number
	source := &qe.network.queryTraceSource
	if source.Load() != queryTraceSourceDebug {
		result, err := qe.forwardSubrequest(ctx, pg, "trace_block", []interface{}{hexQuantity(n)})
		if err == nil {
			var raw []map[string]interface{}
			if err := sonic.Unmarshal(result, &raw); err != nil {
				return nil, false, fmt.Errorf("decode trace_block %#x: %w", n, err)
			}
			consistent := true
			wantHash := evm.BytesToHex(h.Hash)
			traces := make([]*evm.Trace, 0, len(raw))
			for _, item := range raw {
				if hash, ok := item["blockHash"].(string); ok && !strings.EqualFold(hash, wantHash) {
					consistent = false
				}
				t, err := evm.TraceFromParity(item, n, h.Hash, &h.Timestamp)
				if err != nil {
					return nil, false, fmt.Errorf("decode trace_block %#x: %w", n, err)
				}
				if t.TraceType == evm.TraceType_TRACE_REWARD {
					continue // a block reward is no call frame of a transaction
				}
				traces = append(traces, t)
			}
			evm.PropagateParityReverted(traces)
			source.Store(queryTraceSourceParity)
			return traces, consistent, nil
		}
		if !isUnsupportedSubrequest(err) {
			return nil, false, err
		}
		source.Store(queryTraceSourceDebug)
	}

	result, err := qe.forwardSubrequest(ctx, pg, "debug_traceBlockByNumber", []interface{}{
		hexQuantity(n),
		map[string]interface{}{"tracer": "callTracer"},
	})
	if err != nil {
		if isUnsupportedSubrequest(err) {
			source.Store(queryTraceSourceUnknown)
			return nil, false, queryMethodNotServed("%s needs trace_block or debug_traceBlockByNumber, and no upstream serves either", pg.plan.method)
		}
		return nil, false, err
	}
	var items []map[string]interface{}
	if err := sonic.Unmarshal(result, &items); err != nil {
		return nil, false, fmt.Errorf("decode debug_traceBlockByNumber %#x: %w", n, err)
	}
	consistent := len(items) == len(block.TransactionHashes)
	var traces []*evm.Trace
	for i, item := range items {
		frames, err := evm.TraceFromGethDebug(item, uint32(i), n, h.Hash, &h.Timestamp)
		if err != nil {
			return nil, false, fmt.Errorf("decode debug_traceBlockByNumber %#x: %w", n, err)
		}
		if consistent && len(frames) > 0 && !bytes.Equal(frames[0].TransactionHash, block.TransactionHashes[i]) {
			consistent = false
		}
		traces = append(traces, frames...)
	}
	return traces, consistent, nil
}

func isUnsupportedSubrequest(err error) bool {
	return common.HasErrorCode(err, common.ErrCodeEndpointUnsupported, common.ErrCodeUpstreamMethodIgnored)
}

func hexQuantity(n uint64) string {
	return fmt.Sprintf("0x%x", n)
}

// forwardSubrequest sends a shim sub-request through the network, pinned to
// the page's upstream once one answered, when that upstream takes the method.
// A pinned request that fails is sent again unpinned, so a page still fails
// over. A null answer (an upstream that lags and does not have the block yet)
// is asked once more of every other upstream, pinned or not, so a lagging
// upstream never hides an available block. The upstream that answers becomes
// the page's pin, in place of a pin that failed or lagged. The consistency
// check guards what comes back.
func (qe *EvmQueryExecutor) forwardSubrequest(ctx context.Context, pg *shimPageState, method string, params []interface{}) ([]byte, error) {
	pin := pg.pin()
	if pin != "" && !qe.upstreamHandles(ctx, pin, method) {
		// The pinned upstream does not take this method (allow/ignore config,
		// or a method it reported unsupported): a pinned attempt is wasted.
		pin = ""
	}
	result, served, err := qe.forwardOnce(ctx, pg, method, params, pin)
	if err != nil && pin != "" && ctx.Err() == nil {
		result, served, err = qe.forwardOnce(ctx, pg, method, params, "")
	}
	if err != nil {
		return nil, err
	}
	if isJSONNull(result) && pinnable(served) && ctx.Err() == nil {
		// "!served" selects every other upstream. Without another upstream
		// that has the data, the null stands and pins nothing.
		other, otherServed, otherErr := qe.forwardOnce(ctx, pg, method, params, "!"+served)
		if otherErr != nil || isJSONNull(other) {
			return result, nil
		}
		result, served = other, otherServed
	}
	pg.setPin(served, pin)
	return result, nil
}

// upstreamHandles reports whether the network's upstream id accepts method:
// ShouldHandleMethod returns (true, nil).
func (qe *EvmQueryExecutor) upstreamHandles(ctx context.Context, id, method string) bool {
	for _, u := range qe.network.upstreamsRegistry.GetNetworkUpstreams(ctx, qe.network.Id()) {
		if u.Id() == id {
			ok, err := u.ShouldHandleMethod(method)
			return err == nil && ok
		}
	}
	return false
}

// pinnable reports whether an upstream id can go in a UseUpstream selector
// as itself: not empty (a cache answer) and free of selector operators.
func pinnable(upstreamID string) bool {
	return upstreamID != "" && !strings.ContainsAny(upstreamID, "|&!()*? ")
}

func isJSONNull(result []byte) bool {
	return bytes.Equal(bytes.TrimSpace(result), []byte("null"))
}

// forwardOnce returns the result and the id of the upstream that served it
// ("" when it came from the cache).
func (qe *EvmQueryExecutor) forwardOnce(ctx context.Context, pg *shimPageState, method string, params []interface{}, pin string) ([]byte, string, error) {
	ctx, span := common.StartDetailSpan(ctx, "Query.ForwardSubrequest")
	defer span.End()
	span.SetAttributes(attribute.String("subrequest.method", method), attribute.String("subrequest.pin", pin))

	jrq := common.NewJsonRpcRequest(method, params)
	if err := jrq.SetID(util.RandomID()); err != nil {
		return nil, "", err
	}
	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	req.SetNetwork(qe.network)
	if qe.parentRequestId != nil {
		req.SetParentRequestId(qe.parentRequestId)
	}
	req.ApplyDirectiveDefaults(qe.network.Config().DirectiveDefaults)
	if pin != "" || pg.skipCache {
		d := req.Directives().Clone()
		if pin != "" && d.UseUpstream == "" {
			d.UseUpstream = pin
		}
		if pg.skipCache {
			d.SkipCacheRead = "true"
		}
		req.SetDirectives(d)
	}
	resp, err := qe.network.Forward(ctx, req)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, "", err
	}
	served := ""
	if !resp.FromCache() {
		if u := resp.Upstream(); u != nil {
			served = u.Id()
		}
	}
	result, err := parseJSONRPCResult(ctx, resp)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, "", err
	}
	return result, served, nil
}
