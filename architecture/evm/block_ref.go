package evm

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"go.opentelemetry.io/otel/attribute"
)

// ExtractBlockReferenceFromRequest extracts any possible block reference and number from the request and from response if it exists.
// This method is more 'complete' than ExtractBlockReferenceFromResponse() because we might not even have a response
// in certain situations (for example, when we are using failsafe retries).
func ExtractBlockReferenceFromRequest(ctx context.Context, r *common.NormalizedRequest) (string, int64, error) {
	ctx, span := common.StartDetailSpan(ctx, "Evm.ExtractBlockReferenceFromRequest")
	defer span.End()

	if r == nil {
		return "", 0, nil
	}

	var blockNumber int64
	var blockRef string

	// Try to load from local cache
	if bn := r.EvmBlockNumber(); bn != nil {
		blockNumber = bn.(int64)
	}
	if br := r.EvmBlockRef(); br != nil {
		blockRef = br.(string)
	}

	if blockRef != "" && blockNumber != 0 {
		if common.IsTracingDetailed {
			span.SetAttributes(
				attribute.String("block.ref", blockRef),
				attribute.Int64("block.number", blockNumber),
			)
		}
		return blockRef, blockNumber, nil
	}

	// Try to load from JSON-RPC request
	rpcReq, err := r.JsonRpcRequest(ctx)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return blockRef, blockNumber, err
	}

	br, bn, err := extractRefFromJsonRpcRequest(ctx, r, rpcReq)
	if br != "" {
		blockRef = br
	}
	if bn > 0 {
		blockNumber = bn
	}
	if err != nil {
		common.SetTraceSpanError(span, err)
		return blockRef, blockNumber, err
	}

	// Try to load from last valid response
	if blockRef == "" || blockNumber == 0 {
		lvr := r.LastValidResponse()
		if lvr != nil {
			br, bn, err = ExtractBlockReferenceFromResponse(ctx, lvr)
			if br != "" && (blockRef == "" || blockRef == "*") {
				// The condition (blockRef == ""|"*") makes sure if request already has a ref we won't override it from response.
				// For example eth_getBlockByNumber(latest) will have a "latest" ref, so it'll be cached under "latest" ref,
				// and we don't want it to be stored as the actual blockHash returned in the response, so that we can have a cache hit.
				// In case of "*" since it means any block, we can still augment it from response ref, because during cache.Get()
				// we'll be using reverse index (i.e. ignoring ref), but after reorg invalidation is added a specific block ref is useful.
				//
				// For moving-tag requests ("latest", "finalized", "safe"), the
				// cache layer calls ResolveCacheBlockRef instead, which resolves
				// the tag to a concrete block number so each tip advance gets
				// its own cache key.
				blockRef = br
			}
			if bn > 0 {
				blockNumber = bn
			}
			if err != nil {
				common.SetTraceSpanError(span, err)
				return blockRef, blockNumber, err
			}
		}
	}

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("block.ref", blockRef),
			attribute.Int64("block.number", blockNumber),
		)
	}

	// Store to local cache
	if blockNumber > 0 {
		r.SetEvmBlockNumber(blockNumber)
	}
	if blockRef != "" {
		// Only set request blockRef when it's not already set.
		// Preserve original non-wildcard tag refs ("latest", "finalized", "safe", "pending", "earliest")
		// and "*" (composite cases) that may already be present on the request.
		if cur := r.EvmBlockRef(); cur == nil || cur == "" {
			r.SetEvmBlockRef(blockRef)
		}
	}

	return blockRef, blockNumber, nil
}

// ResolveCacheBlockRef returns the block reference the cache layer should use
// when keying an eth_getBlockByNumber("latest") response (and other moving
// tags). Regular ExtractBlockReferenceFromRequest preserves the literal tag
// string ("latest") as blockRef so the cache hits on repeat tag queries —
// but that makes every request within the TTL window return the same pinned
// response regardless of chain progression (see the bug fixed alongside this
// helper: stale "latest" responses served from cache until TTL expiry, with
// enforceHighestBlock explicitly skipping cached responses).
//
// This helper substitutes the tag with a concrete block number so each tip
// advance is a distinct cache key: on WRITE we use the response's own block
// number (definitive answer for what the cached payload represents); on READ
// we consult the network's tip tracker (EvmHighestLatestBlockNumber, which
// aggregates max over upstream pollers and the cross-pod shared counter) to
// decide which block we'd be asking for *right now*. Within a single tip
// the key is stable and concurrent "latest" queries coalesce onto one cached
// entry; across tip advances the key changes and the next request forwards
// upstream.
//
// The function does NOT mutate the request's EvmBlockRef — the original
// "latest" tag is preserved on the request so downstream finality computation
// and other tag-aware logic keeps working.
//
// Fallback: if the tag can't be resolved to a concrete block number (no
// response, no network attached to the request, or the tracker hasn't seen
// a block yet), the original tag is returned and the cache key stays
// tag-literal — same as prior behaviour. That path should be rare in
// production since every normal HTTP request has a Network and an upstream
// response by the SET stage.
func ResolveCacheBlockRef(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) (string, int64, error) {
	blockRef, blockNumber, err := ExtractBlockReferenceFromRequest(ctx, req)
	if err != nil {
		return blockRef, blockNumber, err
	}

	// Only rewrite moving tip-bound tags. Numeric refs, block-hash refs, "*",
	// and slower-moving tags like "earliest" are already correct.
	if blockRef != "latest" && blockRef != "finalized" && blockRef != "safe" {
		return blockRef, blockNumber, nil
	}

	// WRITE path: prefer the response's own block number, which is the
	// definitive answer for what payload we're about to cache.
	if resp != nil {
		if _, respBN, rerr := ExtractBlockReferenceFromResponse(ctx, resp); rerr == nil && respBN > 0 {
			hex, herr := common.NormalizeHex(respBN)
			if herr == nil {
				return hex, respBN, nil
			}
		}
	}

	// READ path (and WRITE fallback): consult the network's aggregated view
	// of the tag's current value. Guarded against panics because this helper
	// is purely an optimization — if the network state isn't reachable for
	// any reason (partially-constructed Network in a test, nil upstream
	// registry, transient initialization race), we fall back to the tag-
	// literal blockRef and retain the previous behaviour rather than
	// aborting a live cache operation.
	net := req.Network()
	if net == nil {
		return blockRef, blockNumber, nil
	}
	var num int64
	func() {
		defer func() {
			if r := recover(); r != nil {
				num = 0
			}
		}()
		switch blockRef {
		case "latest":
			num = net.EvmHighestLatestBlockNumber(ctx)
		case "finalized", "safe":
			num = net.EvmHighestFinalizedBlockNumber(ctx)
		}
	}()
	if num > 0 {
		hex, herr := common.NormalizeHex(num)
		if herr == nil {
			return hex, num, nil
		}
	}

	return blockRef, blockNumber, nil
}

func ExtractBlockReferenceFromResponse(ctx context.Context, r *common.NormalizedResponse) (string, int64, error) {
	ctx, span := common.StartDetailSpan(ctx, "Evm.ExtractBlockReferenceFromResponse")
	defer span.End()

	if r == nil {
		return "", 0, nil
	}

	var blockNumber int64
	var blockRef string

	// Try to load from local cache
	if n := r.EvmBlockNumber(); n != nil {
		blockNumber = n.(int64)
	}
	if br := r.EvmBlockRef(); br != nil {
		blockRef = br.(string)
	}
	if blockRef != "" && blockNumber != 0 {
		span.SetAttributes(
			attribute.String("block.ref", blockRef),
			attribute.Int64("block.number", blockNumber),
		)
		return blockRef, blockNumber, nil
	}

	// Try to load from response (enriched with request context)
	nreq := r.Request()
	if nreq == nil {
		common.SetTraceSpanError(span, errors.New("unexpected nil request"))
		return blockRef, blockNumber, nil
	}
	jrr, err := r.JsonRpcResponse()
	if jrr == nil || err != nil {
		common.SetTraceSpanError(span, err)
		return blockRef, blockNumber, err
	}
	rq, err := nreq.JsonRpcRequest(ctx)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return blockRef, blockNumber, err
	}
	br, bn, err := extractRefFromJsonRpcResponse(ctx, nreq, rq, jrr)
	if br != "" {
		blockRef = br
	}
	if bn != 0 {
		blockNumber = bn
	}
	if err != nil {
		common.SetTraceSpanError(span, err)
		return blockRef, blockNumber, err
	}

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("block.ref", blockRef),
			attribute.Int64("block.number", blockNumber),
		)
	}

	// Store to local cache
	if blockNumber != 0 {
		r.SetEvmBlockNumber(blockNumber)
	}
	if blockRef != "" {
		r.SetEvmBlockRef(blockRef)
	}

	return blockRef, blockNumber, nil
}

// ExtractBlockTimestampFromResponse extracts the block timestamp from a response containing block data
func ExtractBlockTimestampFromResponse(ctx context.Context, r *common.NormalizedResponse) (int64, error) {
	ctx, span := common.StartDetailSpan(ctx, "Evm.ExtractBlockTimestampFromResponse")
	defer span.End()

	if r == nil {
		return 0, nil
	}

	jrr, err := r.JsonRpcResponse()
	if jrr == nil || err != nil {
		common.SetTraceSpanError(span, err)
		return 0, err
	}

	if jrr.IsResultEmptyish(ctx) {
		return 0, nil
	}

	// Extract timestamp - can be hex string (0x...), decimal string, or integer
	timestampStr, err := jrr.PeekStringByPath(ctx, "timestamp")
	if err != nil || timestampStr == "" {
		return 0, err
	}

	// Force-copy the small string to avoid any potential reference to backing buffers
	timestampStr = string(append([]byte(nil), timestampStr...))

	var blockTimestamp int64
	// Handle both hex (0x...) and decimal formats
	if strings.HasPrefix(timestampStr, "0x") {
		blockTimestamp, err = common.HexToInt64(timestampStr)
	} else {
		blockTimestamp, err = strconv.ParseInt(timestampStr, 10, 64)
	}

	if err != nil {
		common.SetTraceSpanError(span, err)
		return 0, err
	}

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.Int64("block.timestamp", blockTimestamp),
		)
	}

	return blockTimestamp, nil
}

func extractRefFromJsonRpcRequest(ctx context.Context, req *common.NormalizedRequest, r *common.JsonRpcRequest) (string, int64, error) {
	ctx, span := common.StartDetailSpan(ctx, "Evm.extractRefFromJsonRpcRequest")
	defer span.End()

	if r == nil {
		err := errors.New("cannot extract block reference when json-rpc request is nil")
		common.SetTraceSpanError(span, err)
		return "", 0, err
	}
	var network common.Network
	if req != nil {
		network = req.Network()
	}
	methodConfig := getMethodConfig(r.Method, network)
	if methodConfig == nil {
		common.SetTraceSpanError(span, errors.New("method config not found for "+r.Method+" when extracting block reference from json-rpc request"))
		// For unknown methods blockRef and/or blockNumber will be empty therefore such data will not be cached despite any caching policies.
		// This strict behavior avoids caching data that we're not aware of its nature.
		return "", 0, nil
	}

	r.RLockWithTrace(ctx)
	defer r.RUnlock()

	if methodConfig.Finalized {
		// Static methods are not expected to change over time so we can cache them forever
		// We use block number 1 as a signal to indicate data is finalized on first ever block
		return "*", 1, nil
	} else if methodConfig.Realtime {
		// Certain methods are expected to always return new data on every new block.
		// For these methods we can always return "*" as blockRef to indicate data it can be cached
		// if there's a 'realtime' cache policy specifically targeting these methods.
		return "*", 0, nil
	}

	var blockRef string
	var blockNumber int64

	if len(methodConfig.ReqRefs) > 0 {
		if len(methodConfig.ReqRefs) == 1 && len(methodConfig.ReqRefs[0]) == 1 && methodConfig.ReqRefs[0][0] == "*" {
			// This special case is for methods that can be cached regardless of their block number or hash
			// e.g. eth_getTransactionReceipt, eth_getTransactionByHash, etc.
			blockRef = "*"
		} else {
			for _, ref := range methodConfig.ReqRefs {
				if val, err := r.PeekByPath(ref...); err == nil {
					bref, bn, err := parseCompositeBlockParam(val)
					if err != nil {
						return "", 0, err
					}
					if bref != "" {
						if blockRef == "" {
							blockRef = bref
						} else if blockRef != bref {
							// This special case is when a method has multiple block parameters (eth_getLogs)
							// We can't use a specific block reference because later if reorg invalidation is added
							// it is not easy to check if this cache entry includes that reorged block.
							// Therefore users must ONLY cache finalized data for these methods (or short TTLs for unfinalized data).
							// Later we'll either need bespoke caching logic for these methods, or find a nice way
							// to invalidate these cache entries when ANY of their blocks are reorged.
							blockRef = "*"
						}
					}
					if bn > blockNumber {
						blockNumber = bn
					}
				}
			}
		}
	}

	return blockRef, blockNumber, nil
}

func extractRefFromJsonRpcResponse(ctx context.Context, req *common.NormalizedRequest, rpcReq *common.JsonRpcRequest, rpcResp *common.JsonRpcResponse) (string, int64, error) {
	ctx, span := common.StartDetailSpan(ctx, "Evm.extractRefFromJsonRpcResponse")
	defer span.End()

	if rpcReq == nil {
		err := errors.New("cannot extract block reference when json-rpc request is nil")
		common.SetTraceSpanError(span, err)
		return "", 0, err
	}
	if rpcResp == nil {
		err := errors.New("cannot extract block reference when json-rpc response is nil")
		common.SetTraceSpanError(span, err)
		return "", 0, err
	}

	var network common.Network
	if req != nil {
		network = req.Network()
	}
	methodConfig := getMethodConfig(rpcReq.Method, network)
	if methodConfig == nil {
		common.SetTraceSpanError(span, errors.New("method config not found for "+rpcReq.Method+" when extracting block reference from json-rpc response"))
		return "", 0, nil
	}
	if rpcResp.IsResultEmptyish(ctx) {
		common.SetTraceSpanError(span, errors.New("no result found for method "+rpcReq.Method+" in json-rpc response when extracting block reference"))
		return "", 0, nil
	}

	// Use response refs from method config to extract block reference
	if len(methodConfig.RespRefs) > 0 {
		blockRef := ""
		blockNumber := int64(0)

		for _, ref := range methodConfig.RespRefs {
			if val, err := rpcResp.PeekStringByPath(ctx, ref...); err == nil {
				bref, bn, err := parseCompositeBlockParam(val)
				if err != nil {
					return "", 0, err
				}
				if bref != "" && (blockRef == "" || blockRef == "*") {
					blockRef = bref
				}
				if bn > blockNumber {
					blockNumber = bn
				}
			}
		}

		// Only use numeric ref if a specific ref has not been determined yet or was composite "*"
		if blockNumber > 0 && (blockRef == "" || blockRef == "*") {
			blockRef = strconv.FormatInt(blockNumber, 10)
		}

		return blockRef, blockNumber, nil
	}

	return "", 0, nil
}

func getMethodConfig(method string, network common.Network) (cfg *common.CacheMethodConfig) {
	// Try to get method config from network if available
	if network != nil {
		networkCfg := network.Config()
		if networkCfg != nil && networkCfg.Methods != nil && networkCfg.Methods.Definitions != nil {
			if methodCfg, ok := networkCfg.Methods.Definitions[method]; ok {
				cfg = methodCfg
			}
		}
	}

	if cfg == nil {
		// If network config is not available or missing the method, we should get the method config from the default set of known methods.
		// This is necessary so that usual blockNumber detection used in various flows still resolves correctly.
		cfg = common.DefaultWithBlockCacheMethods[method]
		if cfg == nil {
			cfg = common.DefaultSpecialCacheMethods[method]
		}
		if cfg == nil {
			cfg = common.DefaultRealtimeCacheMethods[method]
		}
		if cfg == nil {
			cfg = common.DefaultStaticCacheMethods[method]
		}
	}

	return
}

func parseCompositeBlockParam(param interface{}) (string, int64, error) {
	blockNumberStr, blockHash, err := util.ParseBlockParameter(param)
	if err != nil {
		return "", 0, err
	}

	var blockRef string
	var blockNumber int64

	// If we got a block hash, use it as the block reference
	if blockHash != nil {
		blockRef = fmt.Sprintf("0x%x", blockHash)
		return blockRef, blockNumber, nil
	}

	// If we got a block number string, process it
	if blockNumberStr != "" {
		if strings.HasPrefix(blockNumberStr, "0x") {
			// Parse hex block number to int64
			bni, err := common.HexToInt64(blockNumberStr)
			if err != nil {
				return blockRef, blockNumber, err
			}
			blockNumber = bni
		} else {
			// Block tag ('latest', 'earliest', 'pending')
			blockRef = blockNumberStr
		}
	}

	if blockRef == "" && blockNumber > 0 {
		blockRef = strconv.FormatInt(blockNumber, 10)
	}

	return blockRef, blockNumber, nil
}
