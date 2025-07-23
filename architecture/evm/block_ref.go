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
				// TODO An ideal version stores the data for all eth_getBlockByNumber(latest) and eth_getBlockByNumber(blockNumber),
				// and eth_getBlockByNumber(blockHash) where blockNumber/blockHash are the actual values returned in the response.
				// So that if user gets the latest block, then cache is populated for when they provide that specific block as well.
				// When implementing that feature remember that CacheHash() must be calculated separately for each number/hash combo.
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
		r.SetEvmBlockRef(blockRef)
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

func extractRefFromJsonRpcRequest(ctx context.Context, req *common.NormalizedRequest, r *common.JsonRpcRequest) (string, int64, error) {
	ctx, span := common.StartDetailSpan(ctx, "Evm.extractRefFromJsonRpcRequest")
	defer span.End()

	if r == nil {
		err := errors.New("cannot extract block reference when json-rpc request is nil")
		common.SetTraceSpanError(span, err)
		return "", 0, err
	}
	methodConfig := getMethodConfig(r.Method, req)
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

	methodConfig := getMethodConfig(rpcReq.Method, req)
	if methodConfig == nil {
		common.SetTraceSpanError(span, errors.New("method config not found for "+rpcReq.Method+" when extracting block reference from json-rpc response"))
		return "", 0, nil
	}
	if len(rpcResp.Result) == 0 {
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

		if blockNumber > 0 {
			blockRef = strconv.FormatInt(blockNumber, 10)
		}

		return blockRef, blockNumber, nil
	}

	return "", 0, nil
}

func getMethodConfig(method string, req *common.NormalizedRequest) (cfg *common.CacheMethodConfig) {
	// Try to get method config from network if available
	if req != nil && req.Network() != nil {
		network := req.Network()
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
