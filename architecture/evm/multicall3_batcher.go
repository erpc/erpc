package evm

import (
	"context"
	"encoding/hex"
	"fmt"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
)

// stopAndDrainTimer safely stops a timer and drains its channel if needed.
// This pattern is required because timer.Stop() returns false if the timer already fired,
// and in that case the channel must be drained to avoid goroutine leaks.
func stopAndDrainTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

// Forwarder is the interface for forwarding requests through the network layer.
type Forwarder interface {
	Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)
	// SetCache writes a response to the cache for a request.
	// Returns nil if caching is disabled or not available.
	SetCache(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) error
}

// Batcher aggregates eth_call requests into Multicall3 batches.
type Batcher struct {
	cfg          *common.Multicall3AggregationConfig
	forwarder    Forwarder
	logger       *zerolog.Logger
	batches      map[string]*Batch // keyed by BatchingKey.String()
	mu           sync.RWMutex
	queueSize    int64 // counter for backpressure
	shutdown     chan struct{}
	shutdownOnce sync.Once
	wg           sync.WaitGroup

	// runtimeBypass holds contracts detected at runtime that revert when called via Multicall3
	// but succeed when called individually. This in-memory cache resets on process restart.
	// For persistent bypass configuration, use the BypassContracts config field.
	// Note: This map can grow without bound; in practice this is limited by the number of
	// unique contracts that fail via multicall3 but succeed individually (typically few).
	// Protected by runtimeBypassMu.
	runtimeBypass   map[string]bool
	runtimeBypassMu sync.RWMutex
}

// NewBatcher creates a new Multicall3 batcher.
// Returns nil if cfg is nil or disabled - callers should check the return value.
// The logger parameter is optional (can be nil) - if nil, debug logging is disabled.
// Panics if forwarder is nil (programming error - caller must provide a valid forwarder).
func NewBatcher(cfg *common.Multicall3AggregationConfig, forwarder Forwarder, logger *zerolog.Logger) *Batcher {
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	if forwarder == nil {
		panic("NewBatcher: forwarder cannot be nil")
	}
	b := &Batcher{
		cfg:           cfg,
		forwarder:     forwarder,
		logger:        logger,
		batches:       make(map[string]*Batch),
		shutdown:      make(chan struct{}),
		runtimeBypass: make(map[string]bool),
	}
	return b
}

// logBypass logs a debug message when a request bypasses batching.
// Does nothing if logger is nil.
func (b *Batcher) logBypass(key BatchingKey, reason string) {
	if b.logger == nil {
		return
	}
	b.logger.Debug().
		Str("reason", reason).
		Str("projectId", key.ProjectId).
		Str("networkId", key.NetworkId).
		Str("blockRef", key.BlockRef).
		Msg("request bypassing multicall3 batching")
}

// isRuntimeBypassed checks if a contract address is in the runtime bypass cache.
// The address should be lowercase hex without 0x prefix.
func (b *Batcher) isRuntimeBypassed(addrHex string) bool {
	b.runtimeBypassMu.RLock()
	defer b.runtimeBypassMu.RUnlock()
	return b.runtimeBypass[addrHex]
}

// addRuntimeBypass adds a contract address to the runtime bypass cache.
// The address should be lowercase hex without 0x prefix.
func (b *Batcher) addRuntimeBypass(addrHex string, projectId, networkId string) {
	b.runtimeBypassMu.Lock()
	defer b.runtimeBypassMu.Unlock()
	if !b.runtimeBypass[addrHex] {
		b.runtimeBypass[addrHex] = true
		telemetry.MetricMulticall3RuntimeBypassTotal.WithLabelValues(projectId, networkId).Inc()
		if b.logger != nil {
			b.logger.Info().
				Str("contract", "0x"+addrHex).
				Str("projectId", projectId).
				Str("networkId", networkId).
				Msg("auto-detected contract that reverts via multicall3, added to runtime bypass")
		}
	}
}

// IsRuntimeBypassed checks if a contract address should bypass batching due to runtime detection.
// This is a public method for external callers (e.g., tests, diagnostics) to query the runtime
// bypass cache. The internal Enqueue method uses isRuntimeBypassed for actual bypass checks.
// The address can be provided with or without 0x prefix, and is case-insensitive.
func (b *Batcher) IsRuntimeBypassed(targetHex string) bool {
	if targetHex == "" {
		return false
	}
	// Normalize: lowercase and remove 0x/0X prefix
	normalized := strings.ToLower(targetHex)
	normalized = strings.TrimPrefix(normalized, "0x")
	return b.isRuntimeBypassed(normalized)
}

// Enqueue adds a request to a batch. Returns:
// - entry: the batch entry (nil if bypass)
// - bypass: true if request should be forwarded individually
// - error: any error during processing
func (b *Batcher) Enqueue(ctx context.Context, key BatchingKey, req *common.NormalizedRequest) (*BatchEntry, bool, error) {
	// Validate batching key
	if err := key.Validate(); err != nil {
		b.logBypass(key, fmt.Sprintf("invalid_key: %v", err))
		return nil, true, err
	}

	// Extract call info
	target, callData, _, err := ExtractCallInfo(req)
	if err != nil {
		b.logBypass(key, fmt.Sprintf("extract_call_info_error: %v", err))
		return nil, true, err
	}

	// Validate target address length (must be 20 bytes for EVM)
	if len(target) != 20 {
		err := fmt.Errorf("invalid target address length: got %d, expected 20", len(target))
		b.logBypass(key, fmt.Sprintf("invalid_target: %v", err))
		return nil, true, err
	}

	// Check runtime bypass cache (contracts detected as reverting via multicall3)
	targetHex := strings.ToLower(hex.EncodeToString(target))
	if b.isRuntimeBypassed(targetHex) {
		b.logBypass(key, "runtime_bypass_detected")
		return nil, true, nil
	}

	// Derive call key for deduplication
	callKey, err := DeriveCallKey(req)
	if err != nil {
		b.logBypass(key, fmt.Sprintf("derive_call_key_error: %v", err))
		return nil, true, err
	}

	// Get deadline from context (if any)
	// We don't create a synthetic deadline for no-timeout requests to avoid
	// causing unnecessary timeouts on the upstream multicall call.
	now := time.Now()
	deadline, hasDeadline := ctx.Deadline()

	// Check if deadline is too tight (only if there's a deadline)
	minWait := time.Duration(b.cfg.MinWaitMs) * time.Millisecond
	if hasDeadline && deadline.Before(now.Add(minWait)) {
		telemetry.MetricMulticall3QueueOverflowTotal.WithLabelValues(key.ProjectId, key.NetworkId, "deadline_too_tight").Inc()
		b.logBypass(key, "deadline_too_tight")
		return nil, true, nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// Check caps
	if b.queueSize >= int64(b.cfg.MaxQueueSize) {
		telemetry.MetricMulticall3QueueOverflowTotal.WithLabelValues(key.ProjectId, key.NetworkId, "queue_full").Inc()
		b.logBypass(key, "queue_full")
		return nil, true, nil
	}
	if len(b.batches) >= b.cfg.MaxPendingBatches {
		// Check if this is a new batch key
		if _, exists := b.batches[key.String()]; !exists {
			telemetry.MetricMulticall3QueueOverflowTotal.WithLabelValues(key.ProjectId, key.NetworkId, "max_batches").Inc()
			b.logBypass(key, "max_pending_batches")
			return nil, true, nil
		}
	}

	// Get or create batch
	keyStr := key.String()
	batch, exists := b.batches[keyStr]
	if !exists {
		// If OnlyIfPending is true, bypass batching when no batch is pending
		if b.cfg.OnlyIfPending {
			b.logBypass(key, "only_if_pending_no_batch")
			return nil, true, nil
		}
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
		// If OnlyIfPending is true, bypass batching since the current batch is flushing
		// and we'd need to create a new one
		if b.cfg.OnlyIfPending {
			b.logBypass(key, "only_if_pending_batch_flushing")
			return nil, true, nil
		}
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
			b.logBypass(key, "batch_full")
			return nil, true, nil
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
			b.logBypass(key, "calldata_too_large")
			return nil, true, nil
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
	// Only update if the request has a deadline - requests without deadlines
	// should not cause early flushes.
	if hasDeadline {
		safetyMargin := time.Duration(b.cfg.SafetyMarginMs) * time.Millisecond
		proposedFlush := deadline.Add(-safetyMargin)
		if proposedFlush.Before(batch.FlushTime) {
			batch.FlushTime = proposedFlush
			// Clamp to minimum wait
			minFlush := now.Add(minWait)
			if batch.FlushTime.Before(minFlush) {
				batch.FlushTime = minFlush
			}
			// Notify the flush goroutine that FlushTime was shortened
			select {
			case batch.notifyCh <- struct{}{}:
			default:
				// Already has a pending notification
			}
		}
	}

	batch.mu.Unlock()
	b.queueSize++
	telemetry.MetricMulticall3QueueLen.WithLabelValues(key.ProjectId, key.NetworkId).Inc()

	return entry, false, nil
}

// scheduleFlush waits until flush time and then flushes the batch.
func (b *Batcher) scheduleFlush(keyStr string, batch *Batch) {
	defer b.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			// Record metric regardless of logger availability
			telemetry.MetricMulticall3PanicTotal.WithLabelValues(batch.Key.ProjectId, batch.Key.NetworkId, "scheduleFlush").Inc()

			// Log panic with stack trace if logger available
			if b.logger != nil {
				b.logger.Error().
					Str("panic", fmt.Sprintf("%v", r)).
					Str("stack", string(debug.Stack())).
					Str("batchKey", keyStr).
					Msg("panic in scheduleFlush goroutine")
			}
			// Deliver error to all entries in the batch
			batch.mu.Lock()
			entries := batch.Entries
			batch.mu.Unlock()
			panicErr := common.NewErrJsonRpcExceptionInternal(
				0,
				common.JsonRpcErrorServerSideException,
				fmt.Sprintf("internal error: panic in batch scheduler: %v", r),
				nil,
				nil,
			)
			b.deliverError(entries, panicErr, batch.Key.ProjectId, batch.Key.NetworkId)
		}
	}()

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
		case <-batch.notifyCh:
			// FlushTime was shortened, stop current timer and recalculate
			stopAndDrainTimer(timer)
			continue
		case <-b.shutdown:
			stopAndDrainTimer(timer)
			// On shutdown, flush the batch with error to avoid orphaned entries
			b.flushWithShutdownError(keyStr, batch)
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

	// Decrement queue length metric
	telemetry.MetricMulticall3QueueLen.WithLabelValues(batch.Key.ProjectId, batch.Key.NetworkId).Sub(float64(len(entries)))

	if len(entries) == 0 {
		return
	}

	// Capture flush time for wait time calculations
	flushTime := time.Now()

	// Build ordered unique calls list by iterating entries slice (which preserves insertion order)
	// and deduplicating based on CallKey. This ensures deterministic ordering.
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

	// Emit batching metrics
	projectId := batch.Key.ProjectId
	networkId := batch.Key.NetworkId

	// Record batch size (unique calls)
	telemetry.MetricMulticall3BatchSize.WithLabelValues(projectId, networkId).Observe(float64(len(uniqueCalls)))

	// Record wait time for each entry
	for _, entry := range entries {
		waitMs := float64(flushTime.Sub(entry.CreatedAt).Milliseconds())
		telemetry.MetricMulticall3BatchWaitMs.WithLabelValues(projectId, networkId).Observe(waitMs)
	}

	// Record dedupe count if there were duplicates
	totalEntries := len(entries)
	uniqueCount := len(uniqueCalls)
	if totalEntries > uniqueCount {
		dedupeCount := totalEntries - uniqueCount
		telemetry.MetricMulticall3DedupeTotal.WithLabelValues(projectId, networkId).Add(float64(dedupeCount))
	}

	// Build requests for BuildMulticall3Request
	requests := make([]*common.NormalizedRequest, len(uniqueCalls))
	for i, uc := range uniqueCalls {
		requests[i] = uc.entry.Request
	}

	// Build the multicall3 request
	blockParam, err := blockParamForMulticall(batch.Key.BlockRef)
	if err != nil {
		telemetry.MetricMulticall3FallbackTotal.WithLabelValues(projectId, networkId, "invalid_block_param").Inc()
		b.deliverError(entries, err, projectId, networkId)
		return
	}
	mcReq, _, err := BuildMulticall3Request(requests, blockParam)
	if err != nil {
		telemetry.MetricMulticall3FallbackTotal.WithLabelValues(projectId, networkId, "build_failed").Inc()
		b.deliverError(entries, err, projectId, networkId)
		return
	}

	// Mark request as composite type for metrics/tracing
	mcReq.SetCompositeType(common.CompositeTypeMulticall3)

	// Create a context with the earliest deadline from all entries.
	// We don't use a single entry's context to avoid canceling the whole batch
	// if one entry's context is canceled.
	var earliestDeadline time.Time
	for _, entry := range entries {
		if entry.Deadline.IsZero() {
			continue
		}
		if earliestDeadline.IsZero() || entry.Deadline.Before(earliestDeadline) {
			earliestDeadline = entry.Deadline
		}
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if !earliestDeadline.IsZero() {
		ctx, cancel = context.WithDeadline(context.Background(), earliestDeadline)
		defer cancel()
	} else {
		ctx = context.Background()
	}

	// Forward the multicall request
	mcResp, err := b.forwarder.Forward(ctx, mcReq)
	if err != nil {
		// Check if we should fallback to individual requests
		if ShouldFallbackMulticall3(err) {
			telemetry.MetricMulticall3FallbackTotal.WithLabelValues(projectId, networkId, "forward_error").Inc()
			b.fallbackIndividual(entries, projectId, networkId)
			return
		}
		// Wrap context errors with batching context for better debugging (after fallback check)
		if ctx.Err() != nil {
			err = fmt.Errorf("multicall3 batch forward failed (batch size: %d): %w", len(uniqueCalls), err)
		}
		b.deliverError(entries, err, projectId, networkId)
		return
	}

	// Decode the multicall response
	results, err := b.decodeMulticallResponse(mcResp)
	if err != nil {
		// Release the multicall response before fallback/error
		mcResp.Release()
		// Check if we should fallback to individual requests
		if ShouldFallbackMulticall3(err) {
			telemetry.MetricMulticall3FallbackTotal.WithLabelValues(projectId, networkId, "decode_error").Inc()
			b.fallbackIndividual(entries, projectId, networkId)
			return
		}
		b.deliverError(entries, err, projectId, networkId)
		return
	}

	// Verify result count matches unique calls
	if len(results) != len(uniqueCalls) {
		mcResp.Release()
		b.deliverError(entries, fmt.Errorf("multicall3 result count mismatch: got %d, expected %d", len(results), len(uniqueCalls)), projectId, networkId)
		return
	}

	// Check if per-call caching is enabled (defaults to true)
	cachePerCall := b.cfg.CachePerCall == nil || *b.cfg.CachePerCall

	// Map results to entries, fanning out deduplicated results
	for i, uc := range uniqueCalls {
		result := results[i]
		entriesForCall := callKeys[uc.callKey]

		if result.Success {
			// Build success response for each entry
			resultHex := "0x" + hex.EncodeToString(result.ReturnData)

			// For per-call caching, we only need to cache once per unique call
			// Use the first entry's request for the cache write
			var cachedOnce bool

			for _, entry := range entriesForCall {
				jrr, err := common.NewJsonRpcResponse(entry.Request.ID(), resultHex, nil)
				if err != nil {
					if b.logger != nil {
						b.logger.Warn().
							Err(err).
							Str("projectId", projectId).
							Str("networkId", networkId).
							Str("callKey", uc.callKey).
							Msg("multicall3 response construction failed for entry")
					}
					b.sendResult(entry, BatchResult{Error: err}, projectId, networkId)
					continue
				}
				resp := common.NewNormalizedResponse().WithRequest(entry.Request).WithJsonRpcResponse(jrr)
				// Propagate upstream metadata from multicall response
				resp.SetUpstream(mcResp.Upstream())
				resp.SetFromCache(mcResp.FromCache())

				// Write to cache once per unique call (not once per duplicate entry)
				if cachePerCall && !cachedOnce {
					// Use background context for cache write to avoid request deadline issues
					if err := b.forwarder.SetCache(context.Background(), entry.Request, resp); err != nil {
						// Cache write failures are non-critical but we track them for observability
						telemetry.MetricMulticall3CacheWriteErrorsTotal.WithLabelValues(projectId, networkId).Inc()
						if b.logger != nil {
							b.logger.Warn().
								Err(err).
								Str("projectId", projectId).
								Str("networkId", networkId).
								Str("callKey", uc.callKey).
								Msg("multicall3 per-call cache write failed")
						}
					}
					cachedOnce = true
				}

				b.sendResult(entry, BatchResult{Response: resp}, projectId, networkId)
			}
		} else {
			// Call reverted in multicall3 - check if we should try auto-detection
			if b.cfg.AutoDetectBypass && len(entriesForCall) > 0 {
				// Try forwarding the first entry individually to see if it succeeds
				firstEntry := entriesForCall[0]
				targetHex := strings.ToLower(hex.EncodeToString(firstEntry.Target))

				// Skip retry if already in runtime bypass (shouldn't happen, but defensive)
				if !b.isRuntimeBypassed(targetHex) {
					telemetry.MetricMulticall3AutoDetectRetryTotal.WithLabelValues(projectId, networkId, "attempt").Inc()

					// Use the entry's context for the retry, but with a bounded fallback if it's already cancelled
					retryCtx := firstEntry.Ctx
					var retryCancel context.CancelFunc
					select {
					case <-retryCtx.Done():
						// Original context cancelled - use bounded timeout for auto-detection
						retryCtx, retryCancel = context.WithTimeout(context.Background(), 30*time.Second)
						if b.logger != nil {
							b.logger.Debug().
								Str("projectId", projectId).
								Str("networkId", networkId).
								Str("contract", "0x"+targetHex).
								Err(firstEntry.Ctx.Err()).
								Msg("multicall3 auto-detect retry using fallback context (original cancelled)")
						}
					default:
					}

					retryResp, retryErr := b.forwarder.Forward(retryCtx, firstEntry.Request)
					if retryCancel != nil {
						retryCancel()
					}
					if retryErr == nil && retryResp != nil {
						// Individual call succeeded! This contract needs bypass
						b.addRuntimeBypass(targetHex, projectId, networkId)
						telemetry.MetricMulticall3AutoDetectRetryTotal.WithLabelValues(projectId, networkId, "detected").Inc()

						// Extract the result from the retry response to create per-entry responses
						retryJrr, err := retryResp.JsonRpcResponse()
						if err != nil {
							if b.logger != nil {
								b.logger.Warn().
									Err(err).
									Str("projectId", projectId).
									Str("networkId", networkId).
									Str("contract", "0x"+targetHex).
									Msg("multicall3 auto-detect failed to extract retry response")
							}
							// Fallback: propagate error to all entries
							for _, entry := range entriesForCall {
								b.sendResult(entry, BatchResult{Error: err}, projectId, networkId)
							}
							retryResp.Release()
							continue
						}

						// Get the result value to clone for each entry
						resultValue := retryJrr.GetResultString()

						// Deliver fresh response to each entry with correct request ID
						for _, entry := range entriesForCall {
							jrr, err := common.NewJsonRpcResponse(entry.Request.ID(), resultValue, nil)
							if err != nil {
								if b.logger != nil {
									b.logger.Warn().
										Err(err).
										Str("projectId", projectId).
										Str("networkId", networkId).
										Str("contract", "0x"+targetHex).
										Msg("multicall3 auto-detect response construction failed")
								}
								b.sendResult(entry, BatchResult{Error: err}, projectId, networkId)
								continue
							}
							resp := common.NewNormalizedResponse().WithRequest(entry.Request).WithJsonRpcResponse(jrr)
							resp.SetUpstream(retryResp.Upstream())
							resp.SetFromCache(retryResp.FromCache())
							b.sendResult(entry, BatchResult{Response: resp}, projectId, networkId)
						}

						// Release the original retry response after all entries are processed
						retryResp.Release()
						continue // Move to next unique call
					}
					// Individual call also failed - not a bypass candidate
					telemetry.MetricMulticall3AutoDetectRetryTotal.WithLabelValues(projectId, networkId, "same_error").Inc()
					if b.logger != nil {
						b.logger.Debug().
							Err(retryErr).
							Str("projectId", projectId).
							Str("networkId", networkId).
							Str("contract", "0x"+targetHex).
							Msg("multicall3 auto-detect retry also failed, not adding to bypass")
					}
					if retryResp != nil {
						retryResp.Release()
					}
				}
			}

			// Build error for reverted call with proper JSON-RPC format
			dataHex := "0x" + hex.EncodeToString(result.ReturnData)
			revertErr := common.NewErrEndpointExecutionException(
				common.NewErrJsonRpcExceptionInternal(
					int(common.JsonRpcErrorEvmReverted), // original code
					common.JsonRpcErrorEvmReverted,      // normalized code
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
				b.sendResult(entry, BatchResult{Error: revertErr}, projectId, networkId)
			}
		}
	}

	// Release the multicall response after all results have been mapped
	mcResp.Release()
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

// sendResult safely sends a result to an entry, skipping if context is cancelled.
// Returns true if sent, false if skipped due to cancelled context.
// Records a metric when context is cancelled to track abandoned requests.
func (b *Batcher) sendResult(entry *BatchEntry, result BatchResult, projectId, networkId string) bool {
	// Check if the entry's context is cancelled - no point sending if caller has given up
	select {
	case <-entry.Ctx.Done():
		telemetry.MetricMulticall3AbandonedTotal.WithLabelValues(projectId, networkId).Inc()
		if b.logger != nil {
			b.logger.Warn().
				Str("projectId", projectId).
				Str("networkId", networkId).
				Err(entry.Ctx.Err()).
				Msg("multicall3 batch result not delivered: caller context cancelled")
		}
		// Release response if present to avoid memory leak
		if result.Response != nil {
			result.Response.Release()
		}
		return false // Caller abandoned request, skip sending
	default:
	}
	// ResultCh is buffered size 1, so this won't block
	entry.ResultCh <- result
	return true
}

// deliverError sends an error to all entries in a batch.
// Skips entries whose context has been cancelled.
func (b *Batcher) deliverError(entries []*BatchEntry, err error, projectId, networkId string) {
	result := BatchResult{Error: err}
	for _, entry := range entries {
		b.sendResult(entry, result, projectId, networkId)
	}
}

// fallbackIndividual forwards each entry individually when multicall3 fails.
// Uses parallel goroutines for concurrent forwarding with panic recovery.
// Records metrics for each fallback request outcome.
func (b *Batcher) fallbackIndividual(entries []*BatchEntry, projectId, networkId string) {
	var wg sync.WaitGroup
	for _, entry := range entries {
		wg.Add(1)
		go func(e *BatchEntry) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					// Log panic with stack trace
					if b.logger != nil {
						b.logger.Error().
							Str("panic", fmt.Sprintf("%v", r)).
							Str("stack", string(debug.Stack())).
							Str("projectId", projectId).
							Str("networkId", networkId).
							Msg("panic in fallback forward goroutine")
					}
					// Send error to entry
					err := fmt.Errorf("panic in fallback forward: %v", r)
					b.sendResult(e, BatchResult{Error: err}, projectId, networkId)
					telemetry.MetricMulticall3FallbackRequestsTotal.WithLabelValues(projectId, networkId, "panic").Inc()
				}
			}()
			// Skip if context is already cancelled
			select {
			case <-e.Ctx.Done():
				b.sendResult(e, BatchResult{Error: e.Ctx.Err()}, projectId, networkId)
				telemetry.MetricMulticall3FallbackRequestsTotal.WithLabelValues(projectId, networkId, "cancelled").Inc()
				return
			default:
			}
			resp, err := b.forwarder.Forward(e.Ctx, e.Request)
			b.sendResult(e, BatchResult{Response: resp, Error: err}, projectId, networkId)
			if err != nil {
				telemetry.MetricMulticall3FallbackRequestsTotal.WithLabelValues(projectId, networkId, "error").Inc()
			} else {
				telemetry.MetricMulticall3FallbackRequestsTotal.WithLabelValues(projectId, networkId, "success").Inc()
			}
		}(entry)
	}
	wg.Wait()
}

// Shutdown stops the batcher and waits for pending operations.
// Safe to call multiple times.
func (b *Batcher) Shutdown() {
	b.shutdownOnce.Do(func() {
		close(b.shutdown)
	})
	b.wg.Wait()
}

// flushWithShutdownError delivers shutdown errors to all pending entries.
func (b *Batcher) flushWithShutdownError(keyStr string, batch *Batch) {
	batch.mu.Lock()
	if batch.Flushing {
		batch.mu.Unlock()
		return
	}
	batch.Flushing = true
	entries := batch.Entries
	batch.mu.Unlock()

	// Remove from active batches
	b.mu.Lock()
	if b.batches[keyStr] == batch {
		delete(b.batches, keyStr)
	}
	entriesLen := int64(len(entries))
	if b.queueSize >= entriesLen {
		b.queueSize -= entriesLen
	} else {
		b.queueSize = 0
	}
	b.mu.Unlock()

	// Decrement queue length metric
	telemetry.MetricMulticall3QueueLen.WithLabelValues(batch.Key.ProjectId, batch.Key.NetworkId).Sub(float64(len(entries)))

	// Deliver shutdown error to all entries
	shutdownErr := common.NewErrJsonRpcExceptionInternal(
		0,
		common.JsonRpcErrorServerSideException,
		"batcher shutting down",
		nil,
		nil,
	)
	b.deliverError(entries, shutdownErr, batch.Key.ProjectId, batch.Key.NetworkId)
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

// Validate checks that the BatchingKey has required fields set.
// Returns an error if any required field is empty.
func (k BatchingKey) Validate() error {
	if k.ProjectId == "" {
		return fmt.Errorf("BatchingKey.ProjectId is required")
	}
	if k.NetworkId == "" {
		return fmt.Errorf("BatchingKey.NetworkId is required")
	}
	if k.BlockRef == "" {
		return fmt.Errorf("BatchingKey.BlockRef is required")
	}
	return nil
}

func (k BatchingKey) String() string {
	// Use null byte separator to prevent key collisions from field values containing the separator
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s", k.ProjectId, k.NetworkId, k.BlockRef, k.DirectivesKey, k.UserId)
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
	Ctx       context.Context           // Original request context (for individual fallback)
	Request   *common.NormalizedRequest // The original eth_call request
	CallKey   string                    // Deduplication key (target + calldata + blockRef)
	Target    []byte                    // Contract address (20 bytes)
	CallData  []byte                    // Encoded function call data
	ResultCh  chan BatchResult          // Channel to receive the result (buffered, size 1)
	CreatedAt time.Time                 // When the entry was created (for wait time metrics)
	Deadline  time.Time                 // Deadline from context (for deadline-aware flushing)
}

// BatchResult is the outcome delivered to a waiting request.
type BatchResult struct {
	Response *common.NormalizedResponse
	Error    error
}

// Batch holds pending requests for a single batching key.
// All entries in a batch share the same project, network, block reference, directives, and user ID.
type Batch struct {
	Key       BatchingKey              // Composite key identifying this batch
	Entries   []*BatchEntry            // All entries (may include duplicates)
	CallKeys  map[string][]*BatchEntry // Map from call key to entries (for deduplication)
	FlushTime time.Time                // When this batch should be flushed (deadline-aware)
	Flushing  bool                     // True once flush has started (prevents double-flush)
	notifyCh  chan struct{}            // Signals flush time was shortened (buffered, size 1)
	mu        sync.Mutex               // Protects all fields
}

func NewBatch(key BatchingKey, flushTime time.Time) *Batch {
	return &Batch{
		Key:       key,
		Entries:   make([]*BatchEntry, 0, 16),
		CallKeys:  make(map[string][]*BatchEntry),
		FlushTime: flushTime,
		notifyCh:  make(chan struct{}, 1),
	}
}

// ineligibleCallFields are fields that make an eth_call ineligible for batching.
// Multicall3 aggregate3 only supports target + calldata, not gas/value/etc.
var ineligibleCallFields = []string{
	"from", "gas", "gasPrice", "maxFeePerGas", "maxPriorityFeePerGas", "value",
}

var allowedCallFields = map[string]bool{
	"to":    true,
	"data":  true,
	"input": true,
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

	// Check if contract should bypass multicall3 batching
	// (e.g., contracts that check msg.sender code size like Chronicle Oracle)
	if cfg.ShouldBypassContractHex(toStr) {
		return false, "contract in bypass list"
	}

	// Check for ineligible fields
	for _, field := range ineligibleCallFields {
		if _, has := callObj[field]; has {
			return false, fmt.Sprintf("has %s field", field)
		}
	}

	// Reject unsupported call object fields early to avoid batcher failures.
	for field := range callObj {
		if !allowedCallFields[field] {
			return false, fmt.Sprintf("unsupported call field: %s", field)
		}
	}

	// Recursion guard: don't batch calls to multicall3 contract
	if strings.EqualFold(toStr, multicall3Address) {
		return false, "already multicall"
	}

	// Check block tag
	blockTag := "latest"
	if len(params) >= 2 && params[1] != nil {
		// Check for EIP-1898 block params with requireCanonical: false
		// These cannot be safely batched because the flag would be lost when
		// rebuilding the block param as {blockHash: "0x..."}
		if blockObj, ok := params[1].(map[string]interface{}); ok {
			if reqCanonical, hasReqCanonical := blockObj["requireCanonical"]; hasReqCanonical {
				if reqCanonicalBool, ok := reqCanonical.(bool); ok && !reqCanonicalBool {
					return false, "has requireCanonical:false"
				}
			}
		}

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

func blockParamForMulticall(blockRef string) (interface{}, error) {
	if blockRef == "" {
		return "latest", nil
	}
	if strings.HasPrefix(blockRef, "0x") {
		// Check if this is a block hash (66 chars = 0x + 64 hex chars = 32 bytes)
		// Block hashes need to be wrapped in EIP-1898 format for correct interpretation
		if len(blockRef) == 66 {
			return map[string]interface{}{"blockHash": blockRef}, nil
		}
		// Regular hex block number - pass through
		return blockRef, nil
	}
	if isDecimalBlockRef(blockRef) {
		blockNum, err := strconv.ParseInt(blockRef, 10, 64)
		if err != nil {
			return nil, err
		}
		return fmt.Sprintf("0x%x", blockNum), nil
	}
	return blockRef, nil
}

func isDecimalBlockRef(blockRef string) bool {
	if blockRef == "" {
		return false
	}
	for i := 0; i < len(blockRef); i++ {
		if blockRef[i] < '0' || blockRef[i] > '9' {
			return false
		}
	}
	return true
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
