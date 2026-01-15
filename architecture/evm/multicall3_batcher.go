package evm

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
)

// Forwarder is the interface for forwarding requests through the network layer.
type Forwarder interface {
	Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)
}

// Batcher aggregates eth_call requests into Multicall3 batches.
type Batcher struct {
	cfg       *common.Multicall3AggregationConfig
	forwarder Forwarder
	batches   map[string]*Batch // keyed by BatchingKey.String()
	mu        sync.RWMutex
	queueSize int64 // counter for backpressure
	shutdown  chan struct{}
	wg        sync.WaitGroup
}

// NewBatcher creates a new Multicall3 batcher.
func NewBatcher(cfg *common.Multicall3AggregationConfig, forwarder Forwarder) *Batcher {
	b := &Batcher{
		cfg:       cfg,
		forwarder: forwarder,
		batches:   make(map[string]*Batch),
		shutdown:  make(chan struct{}),
	}
	return b
}

// Enqueue adds a request to a batch. Returns:
// - entry: the batch entry (nil if bypass)
// - bypass: true if request should be forwarded individually
// - error: any error during processing
func (b *Batcher) Enqueue(ctx context.Context, key BatchingKey, req *common.NormalizedRequest) (*BatchEntry, bool, error) {
	// Extract call info
	target, callData, _, err := ExtractCallInfo(req)
	if err != nil {
		return nil, true, err
	}

	// Derive call key for deduplication
	callKey, err := DeriveCallKey(req)
	if err != nil {
		return nil, true, err
	}

	// Calculate deadline from context
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(time.Duration(b.cfg.WindowMs) * time.Millisecond)
	}

	// Check if deadline is too tight
	now := time.Now()
	minWait := time.Duration(b.cfg.MinWaitMs) * time.Millisecond
	if deadline.Before(now.Add(minWait)) {
		// Deadline too tight, bypass batching
		return nil, true, nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// Check caps
	if b.queueSize >= int64(b.cfg.MaxQueueSize) {
		return nil, true, nil // bypass: queue full
	}
	if len(b.batches) >= b.cfg.MaxPendingBatches {
		// Check if this is a new batch key
		if _, exists := b.batches[key.String()]; !exists {
			return nil, true, nil // bypass: too many pending batches
		}
	}

	// Get or create batch
	keyStr := key.String()
	batch, exists := b.batches[keyStr]
	if !exists {
		flushTime := now.Add(time.Duration(b.cfg.WindowMs) * time.Millisecond)
		batch = NewBatch(key, flushTime)
		b.batches[keyStr] = batch

		// Start flush timer
		b.wg.Add(1)
		go b.scheduleFlush(keyStr, batch)
	}

	// Check if batch is flushing - create new batch if so
	batch.mu.Lock()
	if batch.Flushing {
		batch.mu.Unlock()
		// Create new batch for this key
		flushTime := now.Add(time.Duration(b.cfg.WindowMs) * time.Millisecond)
		batch = NewBatch(key, flushTime)
		b.batches[keyStr] = batch

		b.wg.Add(1)
		go b.scheduleFlush(keyStr, batch)

		batch.mu.Lock()
	}

	// Check if batch is at capacity (unique calls, not entries)
	uniqueCalls := len(batch.CallKeys)
	if _, isDupe := batch.CallKeys[callKey]; !isDupe {
		if uniqueCalls >= b.cfg.MaxCalls {
			batch.mu.Unlock()
			return nil, true, nil // bypass: batch full
		}
	}

	// Check calldata size cap
	currentSize := 0
	for _, entries := range batch.CallKeys {
		if len(entries) > 0 {
			currentSize += len(entries[0].CallData)
		}
	}
	if _, isDupe := batch.CallKeys[callKey]; !isDupe {
		if currentSize+len(callData) > b.cfg.MaxCalldataBytes {
			batch.mu.Unlock()
			return nil, true, nil // bypass: calldata too large
		}
	}

	// Create entry
	entry := &BatchEntry{
		Ctx:       ctx,
		Request:   req,
		CallKey:   callKey,
		Target:    target,
		CallData:  callData,
		ResultCh:  make(chan BatchResult, 1),
		CreatedAt: now,
		Deadline:  deadline,
	}

	// Add to batch
	batch.Entries = append(batch.Entries, entry)
	batch.CallKeys[callKey] = append(batch.CallKeys[callKey], entry)

	// Update flush time based on deadline (deadline-aware)
	safetyMargin := time.Duration(b.cfg.SafetyMarginMs) * time.Millisecond
	proposedFlush := deadline.Add(-safetyMargin)
	if proposedFlush.Before(batch.FlushTime) {
		batch.FlushTime = proposedFlush
		// Clamp to minimum wait
		minFlush := now.Add(minWait)
		if batch.FlushTime.Before(minFlush) {
			batch.FlushTime = minFlush
		}
	}

	batch.mu.Unlock()
	b.queueSize++

	return entry, false, nil
}

// scheduleFlush waits until flush time and then flushes the batch.
func (b *Batcher) scheduleFlush(keyStr string, batch *Batch) {
	defer b.wg.Done()

	for {
		batch.mu.Lock()
		flushTime := batch.FlushTime
		batch.mu.Unlock()

		waitDuration := time.Until(flushTime)
		if waitDuration <= 0 {
			b.flush(keyStr, batch)
			return
		}

		timer := time.NewTimer(waitDuration)
		select {
		case <-timer.C:
			b.flush(keyStr, batch)
			return
		case <-b.shutdown:
			timer.Stop()
			return
		}
	}
}

// flush processes a batch and delivers results.
func (b *Batcher) flush(keyStr string, batch *Batch) {
	batch.mu.Lock()
	if batch.Flushing {
		batch.mu.Unlock()
		return
	}
	batch.Flushing = true
	entries := batch.Entries
	callKeys := batch.CallKeys
	batch.mu.Unlock()

	// Remove from active batches
	b.mu.Lock()
	if b.batches[keyStr] == batch {
		delete(b.batches, keyStr)
	}
	// Defensive: ensure queueSize doesn't go negative
	entriesLen := int64(len(entries))
	if b.queueSize >= entriesLen {
		b.queueSize -= entriesLen
	} else {
		b.queueSize = 0
	}
	b.mu.Unlock()

	if len(entries) == 0 {
		return
	}

	// Build ordered unique calls list (maintains insertion order via CallKeys map iteration order)
	// We need to build unique calls in the order they were first seen
	type uniqueCall struct {
		callKey string
		entry   *BatchEntry // first entry for this callKey
	}
	seenCallKeys := make(map[string]bool)
	uniqueCalls := make([]uniqueCall, 0, len(callKeys))

	for _, entry := range entries {
		if !seenCallKeys[entry.CallKey] {
			seenCallKeys[entry.CallKey] = true
			uniqueCalls = append(uniqueCalls, uniqueCall{
				callKey: entry.CallKey,
				entry:   entry,
			})
		}
	}

	// Build requests for BuildMulticall3Request
	requests := make([]*common.NormalizedRequest, len(uniqueCalls))
	for i, uc := range uniqueCalls {
		requests[i] = uc.entry.Request
	}

	// Build the multicall3 request
	mcReq, _, err := BuildMulticall3Request(requests, batch.Key.BlockRef)
	if err != nil {
		b.deliverError(entries, err)
		return
	}

	// Mark request as composite type for metrics/tracing
	mcReq.SetCompositeType(common.CompositeTypeMulticall3)

	// Use any entry context (they all share same project/network)
	ctx := entries[0].Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	// Forward the multicall request
	mcResp, err := b.forwarder.Forward(ctx, mcReq)
	if err != nil {
		// Check if we should fallback to individual requests
		if ShouldFallbackMulticall3(err) {
			b.fallbackIndividual(entries)
			return
		}
		b.deliverError(entries, err)
		return
	}

	// Decode the multicall response
	results, err := b.decodeMulticallResponse(mcResp)
	if err != nil {
		// Check if we should fallback to individual requests
		if ShouldFallbackMulticall3(err) {
			b.fallbackIndividual(entries)
			return
		}
		b.deliverError(entries, err)
		return
	}

	// Verify result count matches unique calls
	if len(results) != len(uniqueCalls) {
		b.deliverError(entries, fmt.Errorf("multicall3 result count mismatch: got %d, expected %d", len(results), len(uniqueCalls)))
		return
	}

	// Map results to entries, fanning out deduplicated results
	for i, uc := range uniqueCalls {
		result := results[i]
		entriesForCall := callKeys[uc.callKey]

		if result.Success {
			// Build success response for each entry
			resultHex := "0x" + hex.EncodeToString(result.ReturnData)
			for _, entry := range entriesForCall {
				jrr, err := common.NewJsonRpcResponse(entry.Request.ID(), resultHex, nil)
				if err != nil {
					entry.ResultCh <- BatchResult{Error: err}
					continue
				}
				resp := common.NewNormalizedResponse().WithRequest(entry.Request).WithJsonRpcResponse(jrr)
				// Propagate upstream metadata from multicall response
				resp.SetUpstream(mcResp.Upstream())
				resp.SetFromCache(mcResp.FromCache())
				entry.ResultCh <- BatchResult{Response: resp}
			}
		} else {
			// Build error for reverted call with proper JSON-RPC format
			dataHex := "0x" + hex.EncodeToString(result.ReturnData)
			revertErr := common.NewErrEndpointExecutionException(
				common.NewErrJsonRpcExceptionInternal(
					int(common.JsonRpcErrorEvmReverted), // original code
					common.JsonRpcErrorEvmReverted,     // normalized code
					"execution reverted",
					nil,
					map[string]interface{}{
						"data":       dataHex,
						"multicall3": true,
						"stage":      "per-call",
					},
				),
			)
			for _, entry := range entriesForCall {
				entry.ResultCh <- BatchResult{Error: revertErr}
			}
		}
	}
}

// decodeMulticallResponse extracts and decodes the multicall3 result from a response.
func (b *Batcher) decodeMulticallResponse(resp *common.NormalizedResponse) ([]Multicall3Result, error) {
	if resp == nil {
		return nil, fmt.Errorf("nil response")
	}

	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return nil, err
	}
	if jrr == nil {
		return nil, fmt.Errorf("nil json-rpc response")
	}

	// Check for JSON-RPC error
	if jrr.Error != nil {
		return nil, common.NewErrEndpointExecutionException(jrr.Error)
	}

	// Get result as hex string (JSON encoded, so has quotes)
	resultStr := jrr.GetResultString()
	if resultStr == "" || resultStr == "null" {
		return nil, fmt.Errorf("empty result")
	}

	// Parse the JSON string to get the hex value
	var hexStr string
	if err := common.SonicCfg.UnmarshalFromString(resultStr, &hexStr); err != nil {
		return nil, fmt.Errorf("failed to parse result: %w", err)
	}

	// Decode the hex bytes
	resultBytes, err := common.HexToBytes(hexStr)
	if err != nil {
		return nil, fmt.Errorf("failed to decode hex result: %w", err)
	}

	// Decode the multicall3 result
	return DecodeMulticall3Aggregate3Result(resultBytes)
}

// deliverError sends an error to all entries in a batch.
func (b *Batcher) deliverError(entries []*BatchEntry, err error) {
	result := BatchResult{Error: err}
	for _, entry := range entries {
		select {
		case entry.ResultCh <- result:
		default:
		}
	}
}

// fallbackIndividual forwards each entry individually when multicall3 fails.
// Uses parallel goroutines for concurrent forwarding.
func (b *Batcher) fallbackIndividual(entries []*BatchEntry) {
	var wg sync.WaitGroup
	for _, entry := range entries {
		wg.Add(1)
		go func(e *BatchEntry) {
			defer wg.Done()
			resp, err := b.forwarder.Forward(e.Ctx, e.Request)
			select {
			case e.ResultCh <- BatchResult{Response: resp, Error: err}:
			default:
			}
		}(entry)
	}
	wg.Wait()
}

// Shutdown stops the batcher and waits for pending operations.
func (b *Batcher) Shutdown() {
	close(b.shutdown)
	b.wg.Wait()
}

// DirectivesKeyVersion should be bumped when the set of directives
// included in the key changes. This prevents cross-node key mismatches.
const DirectivesKeyVersion = 1

// BatchingKey uniquely identifies a batch for grouping eth_call requests.
type BatchingKey struct {
	ProjectId     string
	NetworkId     string
	BlockRef      string
	DirectivesKey string
	UserId        string // empty if cross-user batching is allowed
}

func (k BatchingKey) String() string {
	return fmt.Sprintf("%s|%s|%s|%s|%s", k.ProjectId, k.NetworkId, k.BlockRef, k.DirectivesKey, k.UserId)
}

// DeriveDirectivesKey creates a stable, versioned key from relevant directives.
// Only includes directives that affect batching behavior.
func DeriveDirectivesKey(dirs *common.RequestDirectives) string {
	if dirs == nil {
		return fmt.Sprintf("v%d:", DirectivesKeyVersion)
	}

	parts := make([]string, 0, 5)
	if dirs.UseUpstream != "" {
		parts = append(parts, fmt.Sprintf("use-upstream=%s", dirs.UseUpstream))
	}
	if dirs.SkipInterpolation {
		parts = append(parts, "skip-interpolation=true")
	}
	if dirs.RetryEmpty {
		parts = append(parts, "retry-empty=true")
	}
	if dirs.RetryPending {
		parts = append(parts, "retry-pending=true")
	}
	if dirs.SkipCacheRead {
		parts = append(parts, "skip-cache-read=true")
	}

	sort.Strings(parts)
	return fmt.Sprintf("v%d:%s", DirectivesKeyVersion, strings.Join(parts, ","))
}

// DeriveCallKey creates a unique key for deduplication within a batch.
// For eth_call, uses target + calldata + blockRef to create a deterministic key
// that doesn't depend on JSON map key ordering.
func DeriveCallKey(req *common.NormalizedRequest) (string, error) {
	if req == nil {
		return "", fmt.Errorf("request is nil")
	}

	// Extract the call components deterministically
	target, callData, blockRef, err := ExtractCallInfo(req)
	if err != nil {
		return "", err
	}

	// Create key from the extracted components (deterministic order)
	return fmt.Sprintf("eth_call:%x:%x:%s", target, callData, blockRef), nil
}

// BatchEntry represents a request waiting in a batch.
type BatchEntry struct {
	Ctx       context.Context
	Request   *common.NormalizedRequest
	CallKey   string
	Target    []byte
	CallData  []byte
	ResultCh  chan BatchResult
	CreatedAt time.Time
	Deadline  time.Time
}

// BatchResult is the outcome delivered to a waiting request.
type BatchResult struct {
	Response *common.NormalizedResponse
	Error    error
}

// Batch holds pending requests for a single batching key.
type Batch struct {
	Key       BatchingKey
	Entries   []*BatchEntry
	CallKeys  map[string][]*BatchEntry // for deduplication
	FlushTime time.Time
	Flushing  bool
	mu        sync.Mutex
}

func NewBatch(key BatchingKey, flushTime time.Time) *Batch {
	return &Batch{
		Key:       key,
		Entries:   make([]*BatchEntry, 0, 16),
		CallKeys:  make(map[string][]*BatchEntry),
		FlushTime: flushTime,
	}
}

// ineligibleCallFields are fields that make an eth_call ineligible for batching.
// Multicall3 aggregate3 only supports target + calldata, not gas/value/etc.
var ineligibleCallFields = []string{
	"from", "gas", "gasPrice", "maxFeePerGas", "maxPriorityFeePerGas", "value",
}

// allowedBlockTags are block tags that can be batched by default.
var allowedBlockTags = map[string]bool{
	"latest":    true,
	"finalized": true,
	"safe":      true,
	"earliest":  true,
}

// IsEligibleForBatching checks if a request can be batched via Multicall3.
// Returns (eligible, reason) where reason explains why not eligible.
func IsEligibleForBatching(req *common.NormalizedRequest, cfg *common.Multicall3AggregationConfig) (bool, string) {
	if req == nil {
		return false, "request is nil"
	}
	if cfg == nil || !cfg.Enabled {
		return false, "batching disabled"
	}

	jrq, err := req.JsonRpcRequest()
	if err != nil {
		return false, fmt.Sprintf("json-rpc error: %v", err)
	}

	jrq.RLock()
	method := strings.ToLower(jrq.Method)
	params := jrq.Params
	jrq.RUnlock()

	// Must be eth_call
	if method != "eth_call" {
		return false, "not eth_call"
	}

	// Must have 1-3 params (call object, optional block, optional state override)
	if len(params) < 1 {
		return false, fmt.Sprintf("invalid param count: %d", len(params))
	}

	// Check for state override (3rd param) - not supported with multicall3
	if len(params) > 2 {
		return false, "has state override"
	}

	// Parse call object
	callObj, ok := params[0].(map[string]interface{})
	if !ok {
		return false, "invalid call object type"
	}

	// Must have 'to' address
	toVal, hasTo := callObj["to"]
	if !hasTo {
		return false, "missing to address"
	}
	toStr, ok := toVal.(string)
	if !ok || toStr == "" {
		return false, "invalid to address"
	}

	// Check for ineligible fields
	for _, field := range ineligibleCallFields {
		if _, has := callObj[field]; has {
			return false, fmt.Sprintf("has %s field", field)
		}
	}

	// Recursion guard: don't batch calls to multicall3 contract
	if strings.EqualFold(toStr, multicall3Address) {
		return false, "already multicall"
	}

	// Check block tag
	blockTag := "latest"
	if len(params) >= 2 && params[1] != nil {
		normalized, err := NormalizeBlockParam(params[1])
		if err != nil {
			return false, fmt.Sprintf("invalid block param: %v", err)
		}
		blockTag = strings.ToLower(normalized)
	}

	// Check if pending tag is allowed
	if blockTag == "pending" && !cfg.AllowPendingTagBatching {
		return false, "pending tag not allowed"
	}

	// Check if block tag is eligible for batching:
	// - Known named tags (latest, finalized, safe, earliest) are always allowed
	// - pending is allowed if AllowPendingTagBatching is true (checked above)
	// - Numeric block numbers (decimal strings after normalization) are allowed
	// - Block hashes (0x + 64 hex chars) are allowed
	if !isBlockRefEligibleForBatching(blockTag) {
		return false, fmt.Sprintf("block tag not allowed: %s", blockTag)
	}

	return true, ""
}

// isBlockRefEligibleForBatching checks if a normalized block reference is eligible for batching.
// It allows: known block tags, numeric block numbers, and block hashes.
func isBlockRefEligibleForBatching(blockRef string) bool {
	// Check known block tags (including pending, which is handled separately)
	if allowedBlockTags[blockRef] || blockRef == "pending" {
		return true
	}

	// Check if it's a numeric block number (decimal string after normalization)
	if len(blockRef) > 0 && blockRef[0] >= '0' && blockRef[0] <= '9' {
		return true
	}

	// Check if it's a block hash (0x + 64 hex chars = 66 chars total for 32 bytes)
	if strings.HasPrefix(blockRef, "0x") && len(blockRef) == 66 {
		return true
	}

	return false
}

// ExtractCallInfo extracts target and calldata from an eligible eth_call request.
// PRECONDITION: req must have passed IsEligibleForBatching - this function assumes
// the request structure has been validated.
func ExtractCallInfo(req *common.NormalizedRequest) (target []byte, callData []byte, blockRef string, err error) {
	jrq, err := req.JsonRpcRequest()
	if err != nil {
		return nil, nil, "", err
	}

	jrq.RLock()
	params := jrq.Params
	jrq.RUnlock()

	callObj, ok := params[0].(map[string]interface{})
	if !ok {
		return nil, nil, "", fmt.Errorf("invalid call object type")
	}

	toVal, ok := callObj["to"]
	if !ok {
		return nil, nil, "", fmt.Errorf("missing to address")
	}
	toStr, ok := toVal.(string)
	if !ok {
		return nil, nil, "", fmt.Errorf("invalid to address type")
	}

	target, err = common.HexToBytes(toStr)
	if err != nil {
		return nil, nil, "", err
	}

	dataHex := "0x"
	if dataVal, ok := callObj["data"]; ok {
		if dataStr, ok := dataVal.(string); ok {
			dataHex = dataStr
		}
	} else if inputVal, ok := callObj["input"]; ok {
		if inputStr, ok := inputVal.(string); ok {
			dataHex = inputStr
		}
	}

	callData, err = common.HexToBytes(dataHex)
	if err != nil {
		return nil, nil, "", err
	}

	blockRef = "latest"
	if len(params) >= 2 && params[1] != nil {
		blockRef, err = NormalizeBlockParam(params[1])
		if err != nil {
			return nil, nil, "", err
		}
	}

	return target, callData, blockRef, nil
}
