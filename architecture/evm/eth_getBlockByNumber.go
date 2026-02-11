package evm

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func BuildGetBlockByNumberRequest(blockNumberOrTag interface{}, includeTransactions bool) (*common.JsonRpcRequest, error) {
	var bkt string
	var err error

	switch v := blockNumberOrTag.(type) {
	case string:
		bkt = v
		if !strings.HasPrefix(bkt, "0x") {
			switch bkt {
			case "latest", "finalized", "safe", "pending", "earliest":
				// Acceptable tags
			default:
				return nil, fmt.Errorf("invalid block number or tag for eth_getBlockByNumber: %v", v)
			}
		}
	case int, int64, float64:
		bkt, err = common.NormalizeHex(v)
		if err != nil {
			return nil, fmt.Errorf("invalid block number or tag for eth_getBlockByNumber: %v", v)
		}
	default:
		return nil, fmt.Errorf("invalid block number or tag for eth_getBlockByNumber: %v", v)
	}

	return common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{bkt, includeTransactions}), nil
}

func networkPostForward_eth_getBlockByNumber(ctx context.Context, network common.Network, nq *common.NormalizedRequest, nr *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "Network.PostForward.eth_getBlockByNumber", trace.WithAttributes(
		attribute.String("request.id", fmt.Sprintf("%v", nq.ID())),
		attribute.String("network.id", network.Id()),
	))
	defer span.End()

	nr, err := enforceHighestBlock(ctx, network, nq, nr, re)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nr, err
	}

	// Track timestamp distance for "latest" blocks and add lag metrics to detailed spans
	if nr != nil && re == nil {
		rqj, rqErr := nq.JsonRpcRequest(ctx)
		if rqErr == nil && rqj != nil {
			rqj.RLock()
			if len(rqj.Params) >= 1 {
				if bnp, ok := rqj.Params[0].(string); ok && bnp == "latest" {
					// Extract block number and timestamp from the response
					_, respBlockNumber, bnErr := ExtractBlockReferenceFromResponse(ctx, nr)
					blockTimestamp, tsErr := ExtractBlockTimestampFromResponse(ctx, nr)

					// Calculate block number lag
					if common.IsTracingDetailed && bnErr == nil && respBlockNumber > 0 {
						highestBlock := network.EvmHighestLatestBlockNumber(ctx)
						blockNumberLag := highestBlock - respBlockNumber
						if blockNumberLag < 0 {
							blockNumberLag = 0
						}
						span.SetAttributes(
							attribute.Int64("block.number", respBlockNumber),
							attribute.Int64("highest_block", highestBlock),
							attribute.Int64("block.number_lag", blockNumberLag),
						)
					}

					// Calculate timestamp lag and record metric
					if tsErr == nil && blockTimestamp > 0 {
						currentTime := time.Now().Unix()
						timestampLag := currentTime - blockTimestamp

						// Add to detailed span
						if common.IsTracingDetailed {
							span.SetAttributes(
								attribute.Int64("block.timestamp", blockTimestamp),
								attribute.Int64("current.timestamp", currentTime),
								attribute.Int64("block.timestamp_lag", timestampLag),
							)
						}

						// Record prometheus metric
						telemetry.MetricNetworkLatestBlockTimestampDistance.WithLabelValues(
							network.ProjectId(),
							network.Label(),
							"network_response",
						).Set(float64(timestampLag))
					}
				}
			}
			rqj.RUnlock()
		}
	}

	return enforceNonNullBlock(nq, nr)
}

func enforceHighestBlock(ctx context.Context, network common.Network, nq *common.NormalizedRequest, nr *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	if re != nil {
		return nr, re
	}

	// Check directive - this is the new way to control this behavior
	dirs := nq.Directives()
	if dirs == nil || !dirs.EnforceHighestBlock {
		return nr, re
	}

	logger := network.Logger().With().Str("method", "eth_getBlockByNumber").Logger()

	// If response is from cache, skip enforcement otherwise there's no point in caching.
	// As we'll definetely have higher latest block number vs what we have in cache.
	// The correct way to deal with this situation is to set proper TTL for "realtime" cache policy.
	if nr.FromCache() {
		logger.Trace().
			Object("request", nq).
			Object("response", nr).
			Msg("skipping enforcement of highest block number as response is from cache")
		return nr, re
	}

	rqj, err := nq.JsonRpcRequest(ctx)
	if err != nil {
		return nil, err
	}
	rqj.RLock()
	defer rqj.RUnlock()

	if len(rqj.Params) < 1 {
		return nr, re
	}
	bnp, ok := rqj.Params[0].(string)
	if !ok {
		return nr, re
	}
	if bnp != "latest" && bnp != "finalized" {
		return nr, re
	}

	switch bnp {
	case "latest":
		highestBlockNumber := network.EvmHighestLatestBlockNumber(ctx)
		_, respBlockNumber, err := ExtractBlockReferenceFromResponse(ctx, nr)
		if err != nil {
			return nil, err
		}
		if highestBlockNumber > respBlockNumber {
			logger.Debug().
				Str("blockTag", bnp).
				Object("request", nq).
				Object("response", nr).
				Interface("highestBlockNumber", highestBlockNumber).
				Interface("respBlockNumber", respBlockNumber).
				Interface("err", err).
				Msg("enforcing highest latest block")
			if respBlockNumber > 0 {
				// When extracted block number is 0, it mostly means response is actually a json-rpc error
				// therefore we better fetch the highest block number again.
				ups := nr.Upstream()
				telemetry.MetricUpstreamStaleLatestBlock.WithLabelValues(
					network.ProjectId(),
					ups.VendorName(),
					network.Label(),
					ups.Id(),
					"eth_getBlockByNumber",
				).Inc()
			}
			var itx bool
			if len(rqj.Params) > 1 {
				itx, _ = rqj.Params[1].(bool)
			}
			request, err := BuildGetBlockByNumberRequest(highestBlockNumber, itx)
			if err != nil {
				return nil, err
			}
			err = request.SetID(nq.ID())
			if err != nil {
				return nil, err
			}
			newReq := common.NewNormalizedRequestFromJsonRpcRequest(request)
			dr := nq.Directives().Clone()
			dr.SkipCacheRead = true
			// In case a block number is extracted, it means the node actually has an older latest block.
			// Therefore we exclude the current upstream from the request (as high likely it doesn't have this block).
			// Otherwise we still allow the current upstream to be used in case json-rpc error was an intermittent issue.
			if respBlockNumber > 0 {
				dr.UseUpstream = fmt.Sprintf("!%s", nr.UpstreamId())
			}
			newReq.SetDirectives(dr)
			newReq.SetNetwork(network)

			// Copy HTTP context (headers, query parameters, user) for proper metrics tracking
			newReq.CopyHttpContextFrom(nq)

			nnr, err := network.Forward(ctx, newReq)
			// This is needed in case highest block number is corrupted somehow and for example
			// it is requesting a very high non-existent block number.
			return pickHighestBlock(ctx, nnr, nr, err)
		} else {
			return nr, re
		}
	case "finalized":
		highestBlockNumber := network.EvmHighestFinalizedBlockNumber(ctx)
		_, respBlockNumber, err := ExtractBlockReferenceFromResponse(ctx, nr)
		if err != nil {
			return nil, err
		}
		if highestBlockNumber > respBlockNumber {
			logger.Debug().
				Str("blockTag", bnp).
				Interface("highestBlockNumber", highestBlockNumber).
				Interface("respBlockNumber", respBlockNumber).
				Interface("err", err).
				Msg("enforcing highest finalized block")
			if respBlockNumber > 0 {
				// When extracted block number is 0, it mostly means response is actually a json-rpc error
				// therefore we better fetch the highest block number again.
				ups := nr.Upstream()
				telemetry.MetricUpstreamStaleFinalizedBlock.WithLabelValues(
					network.ProjectId(),
					ups.VendorName(),
					network.Label(),
					ups.Id(),
				).Inc()
			}
			var itx bool
			if len(rqj.Params) > 1 {
				itx, _ = rqj.Params[1].(bool)
			}
			request, err := BuildGetBlockByNumberRequest(highestBlockNumber, itx)
			if err != nil {
				return nil, err
			}
			err = request.SetID(nq.ID())
			if err != nil {
				return nil, err
			}
			newReq2 := common.NewNormalizedRequestFromJsonRpcRequest(request)
			dr := nq.Directives().Clone()
			dr.SkipCacheRead = true
			if respBlockNumber > 0 {
				// In case a block number is extracted, it means the node actually has an older latest block.
				// Therefore we exclude the current upstream from the request (as high likely it doesn't have this block).
				// Otherwise we still allow the current upstream to be used in case json-rpc error was an intermittent issue.
				// Also, if response from cache we don't need to exclude the current upstream.
				dr.UseUpstream = fmt.Sprintf("!%s", nr.UpstreamId())
			}
			newReq2.SetDirectives(dr)
			newReq2.SetNetwork(network)

			// Copy HTTP context (headers, query parameters, user) for proper metrics tracking
			newReq2.CopyHttpContextFrom(nq)

			nnr, err := network.Forward(ctx, newReq2)
			// This is needed in case highest block number is corrupted somehow and for example
			// it is requesting a very high non-existent block number.
			return pickHighestBlock(ctx, nnr, nr, err)
		} else {
			return nr, re
		}
	default:
		return nr, re
	}
}

// enforceNonNullBlock checks if the block result is null/empty and returns an appropriate error
// This is now controlled by the EnforceNonNullTaggedBlocks directive
func enforceNonNullBlock(nq *common.NormalizedRequest, nr *common.NormalizedResponse) (*common.NormalizedResponse, error) {
	if nr != nil && !nr.IsObjectNull() && !nr.IsResultEmptyish() {
		return nr, nil
	}

	// Response is null/empty - extract block parameter to determine if it's a tag or numeric
	rq := nr.Request()
	var bnp string
	var isTag bool
	if rq != nil {
		rqj, _ := rq.JsonRpcRequest()
		if rqj != nil && len(rqj.Params) > 0 {
			bnp, _ = rqj.Params[0].(string)
			// Check if it's a block tag (not a hex number)
			// Tags: "latest", "pending", "finalized", "safe", "earliest"
			// Numeric: starts with "0x"
			isTag = bnp != "" && !strings.HasPrefix(bnp, "0x")
		}
	}

	// For tagged blocks, check directive
	if isTag {
		dirs := nq.Directives()
		if dirs == nil || !dirs.EnforceNonNullTaggedBlocks {
			// Directive not set or disabled - allow null tagged blocks
			return nr, nil
		}
	}

	// Create error for:
	// 1. Numeric blocks with null result (always an error - indicates missing/pruned data)
	// 2. Tagged blocks with null result when enforcement is enabled
	details := make(map[string]interface{})
	details["blockNumber"] = bnp
	return nil, common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(
			0,
			common.JsonRpcErrorMissingData,
			"block not found with number "+bnp,
			nil,
			details,
		),
		nr.Upstream(),
	)
}

func pickHighestBlock(ctx context.Context, x *common.NormalizedResponse, y *common.NormalizedResponse, err error) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "Evm.PickHighestBlock")
	defer span.End()

	xnull := x == nil || x.IsObjectNull() || x.IsResultEmptyish()
	ynull := y == nil || y.IsObjectNull() || y.IsResultEmptyish()
	if xnull && ynull && err != nil {
		// both emptyish; nothing to keep
		if x != nil {
			x.Release()
		}
		if y != nil {
			y.Release()
		}
		return nil, err
	} else if xnull && !ynull {
		if x != nil {
			x.Release()
		}
		return y, nil
	} else if !xnull && ynull {
		if y != nil {
			y.Release()
		}
		return x, nil
	}
	xjrr, err := x.JsonRpcResponse(ctx)
	if err != nil || xjrr == nil {
		if x != nil && x != y {
			x.Release()
		}
		return y, nil
	}
	yjrr, err := y.JsonRpcResponse(ctx)
	if err != nil || yjrr == nil {
		if y != nil && y != x {
			y.Release()
		}
		return x, nil
	}
	xbn, err := xjrr.PeekStringByPath(ctx, "number")
	if err != nil {
		if x != nil && x != y {
			x.Release()
		}
		return y, nil
	}
	span.SetAttributes(attribute.String("block_number_1", xbn))
	ybn, err := yjrr.PeekStringByPath(ctx, "number")
	if err != nil {
		if y != nil && y != x {
			y.Release()
		}
		return x, nil
	}
	span.SetAttributes(attribute.String("block_number_2", ybn))
	xbnInt, err := strconv.ParseInt(xbn, 0, 64)
	if err != nil {
		if x != nil && x != y {
			x.Release()
		}
		return y, nil
	}
	ybnInt, err := strconv.ParseInt(ybn, 0, 64)
	if err != nil {
		if y != nil && y != x {
			y.Release()
		}
		return x, nil
	}
	if xbnInt > ybnInt {
		if y != nil && y != x {
			y.Release()
		}
		return x, nil
	}
	if x != nil && x != y {
		x.Release()
	}
	return y, nil
}

// upstreamPostForward_eth_getBlockByNumber validates block responses based on request directives.
// It performs validation for:
// - ValidateHeaderFieldLengths (hash, parentHash, stateRoot, etc.)
// - ValidateTransactionFields (tx hash length, uniqueness)
// - ValidateTransactionBlockInfo (tx block hash/number/index match)
// - ValidateBlockLogsBloom (non-zero bloom implies logs in receipts)
func upstreamPostForward_eth_getBlockByNumber(ctx context.Context, n common.Network, u common.Upstream, rq *common.NormalizedRequest, rs *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	if re != nil || rs == nil {
		return rs, re
	}

	var networkId, upstreamId string
	if n != nil {
		networkId = n.Id()
	}
	if u != nil {
		upstreamId = u.Id()
	}
	ctx, span := common.StartDetailSpan(ctx, "Upstream.PostForwardHook.eth_getBlockByNumber", trace.WithAttributes(
		attribute.String("network.id", networkId),
		attribute.String("upstream.id", upstreamId),
	))
	defer span.End()

	// Skip validation if response is empty
	if rs.IsObjectNull() || rs.IsResultEmptyish() {
		return rs, re
	}

	// Only run validation when directives are set (avoids JSON parsing overhead otherwise).
	// Always-on integrity checks piggyback on the same parse as directive-gated checks.
	dirs := rq.Directives()
	if dirs == nil {
		return rs, re
	}
	if err := validateBlock(ctx, u, dirs, rs); err != nil {
		return rs, err
	}

	return rs, re
}

// blockValidationTxLite is a minimal transaction model for block validation
type blockValidationTxLite struct {
	Hash             string `json:"hash"`
	BlockHash        string `json:"blockHash"`
	BlockNumber      string `json:"blockNumber"`
	TransactionIndex string `json:"transactionIndex"`
}

// emptyTrieRoot is the keccak256 of the RLP-encoded empty trie.
// Blocks with zero transactions have this as their transactionsRoot.
const emptyTrieRoot = "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421"

// zeroHash32 is the 32-byte all-zeros hash. Some non-standard chains (e.g. ZKSync Era)
// use this instead of the canonical empty trie root for blocks with zero transactions.
const zeroHash32 = "0x0000000000000000000000000000000000000000000000000000000000000000"

// blockValidationBlockLite is a minimal block model for validation
type blockValidationBlockLite struct {
	Hash             string `json:"hash"`
	ParentHash       string `json:"parentHash"`
	StateRoot        string `json:"stateRoot"`
	TransactionsRoot string `json:"transactionsRoot"`
	ReceiptsRoot     string `json:"receiptsRoot"`
	LogsBloom        string `json:"logsBloom"`
	Number           string `json:"number"`
	Transactions     []any  `json:"transactions"` // Can be []string (hashes) or []blockValidationTxLite (full txs)
}

// validateBlock validates eth_getBlockByNumber/Hash responses.
// It runs always-on integrity checks first (fundamental Ethereum invariants),
// then directive-gated checks. Callers must ensure dirs is non-nil.
func validateBlock(ctx context.Context, u common.Upstream, dirs *common.RequestDirectives, rs *common.NormalizedResponse) error {
	jrr, err := rs.JsonRpcResponse(ctx)
	if err != nil {
		return err
	}

	var block blockValidationBlockLite
	if err := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &block); err != nil {
		return common.NewErrEndpointContentValidation(fmt.Errorf("invalid JSON result for block validation: %w", err), u)
	}

	// ── Directive-gated checks (opt-in via config/library) ───────────────

	// 1. TransactionsRoot vs Transaction Count (default: enabled)
	// The transactionsRoot is a Merkle trie root over the block's transactions.
	// The canonical empty trie root is a universal constant for blocks with zero transactions.
	// Some chains (e.g. ZKSync Era) use the all-zeros hash instead.
	// If they disagree, the upstream returned truncated/incomplete data.
	// Disable via validateTransactionsRoot: false for non-standard chains.
	if dirs.ValidateTransactionsRoot && block.TransactionsRoot != "" {
		txRootLower := strings.ToLower(block.TransactionsRoot)
		isEmptyRoot := txRootLower == emptyTrieRoot || txRootLower == zeroHash32
		txCount := len(block.Transactions)

		if !isEmptyRoot && txCount == 0 {
			return common.NewErrEndpointContentValidation(
				fmt.Errorf("transactionsRoot is %s (non-empty) but block contains 0 transactions; upstream returned incomplete block data", block.TransactionsRoot),
				u,
			)
		}
		if isEmptyRoot && txCount > 0 {
			return common.NewErrEndpointContentValidation(
				fmt.Errorf("transactionsRoot is empty trie root but block contains %d transactions; inconsistent block data", txCount),
				u,
			)
		}
	}

	// 2. Header Field Length Validation
	if dirs.ValidateHeaderFieldLengths {
		if err := validateHeaderFieldLengths(u, &block); err != nil {
			return err
		}
	}

	// 3. Transaction Validation (only if we have full transactions)
	if len(block.Transactions) > 0 {
		// Check if transactions are full objects or just hashes
		var fullTxs []blockValidationTxLite
		for i, tx := range block.Transactions {
			switch t := tx.(type) {
			case map[string]interface{}:
				// Full transaction object - re-parse it
				txBytes, err := common.SonicCfg.Marshal(t)
				if err != nil {
					return common.NewErrEndpointContentValidation(fmt.Errorf("tx %d: failed to marshal: %w", i, err), u)
				}
				var txl blockValidationTxLite
				if err := common.SonicCfg.Unmarshal(txBytes, &txl); err != nil {
					return common.NewErrEndpointContentValidation(fmt.Errorf("tx %d: failed to unmarshal: %w", i, err), u)
				}
				fullTxs = append(fullTxs, txl)
			case string:
				// Just a hash - skip full tx validation
				continue
			}
		}

		if len(fullTxs) > 0 {
			if err := validateBlockTransactions(u, dirs, &block, fullTxs); err != nil {
				return err
			}
		}
	}

	return nil
}

func validateHeaderFieldLengths(u common.Upstream, block *blockValidationBlockLite) error {
	// Hash must be 32 bytes (64 hex chars + 0x prefix)
	if block.Hash != "" {
		hashBytes, err := common.HexToBytes(block.Hash)
		if err != nil {
			return common.NewErrEndpointContentValidation(fmt.Errorf("invalid block hash hex: %w", err), u)
		}
		if len(hashBytes) != 32 {
			return common.NewErrEndpointContentValidation(fmt.Errorf("block hash length invalid: %d", len(hashBytes)), u)
		}
	}

	// ParentHash must be 32 bytes
	if block.ParentHash != "" {
		parentHashBytes, err := common.HexToBytes(block.ParentHash)
		if err != nil {
			return common.NewErrEndpointContentValidation(fmt.Errorf("invalid parentHash hex: %w", err), u)
		}
		if len(parentHashBytes) != 32 {
			return common.NewErrEndpointContentValidation(fmt.Errorf("parentHash length invalid: %d", len(parentHashBytes)), u)
		}
	}

	// StateRoot must be 32 bytes
	if block.StateRoot != "" {
		stateRootBytes, err := common.HexToBytes(block.StateRoot)
		if err != nil {
			return common.NewErrEndpointContentValidation(fmt.Errorf("invalid stateRoot hex: %w", err), u)
		}
		if len(stateRootBytes) != 32 {
			return common.NewErrEndpointContentValidation(fmt.Errorf("stateRoot length invalid: %d", len(stateRootBytes)), u)
		}
	}

	// TransactionsRoot must be 32 bytes
	if block.TransactionsRoot != "" {
		txRootBytes, err := common.HexToBytes(block.TransactionsRoot)
		if err != nil {
			return common.NewErrEndpointContentValidation(fmt.Errorf("invalid transactionsRoot hex: %w", err), u)
		}
		if len(txRootBytes) != 32 {
			return common.NewErrEndpointContentValidation(fmt.Errorf("transactionsRoot length invalid: %d", len(txRootBytes)), u)
		}
	}

	// ReceiptsRoot must be 32 bytes
	if block.ReceiptsRoot != "" {
		receiptsRootBytes, err := common.HexToBytes(block.ReceiptsRoot)
		if err != nil {
			return common.NewErrEndpointContentValidation(fmt.Errorf("invalid receiptsRoot hex: %w", err), u)
		}
		if len(receiptsRootBytes) != 32 {
			return common.NewErrEndpointContentValidation(fmt.Errorf("receiptsRoot length invalid: %d", len(receiptsRootBytes)), u)
		}
	}

	// LogsBloom must be 256 bytes
	if block.LogsBloom != "" {
		bloomBytes, err := common.HexToBytes(block.LogsBloom)
		if err != nil {
			return common.NewErrEndpointContentValidation(fmt.Errorf("invalid logsBloom hex: %w", err), u)
		}
		if len(bloomBytes) != 256 {
			return common.NewErrEndpointContentValidation(fmt.Errorf("logsBloom length invalid: %d", len(bloomBytes)), u)
		}
	}

	return nil
}

func validateBlockTransactions(u common.Upstream, dirs *common.RequestDirectives, block *blockValidationBlockLite, txs []blockValidationTxLite) error {
	// ValidateTransactionFields: hash length and uniqueness
	if dirs.ValidateTransactionFields {
		seenHashes := make(map[string]struct{}, len(txs))
		for i, tx := range txs {
			// Hash length must be 32 bytes
			if tx.Hash != "" {
				hashBytes, err := common.HexToBytes(tx.Hash)
				if err != nil {
					return common.NewErrEndpointContentValidation(fmt.Errorf("tx %d: invalid hash hex: %w", i, err), u)
				}
				if len(hashBytes) != 32 {
					return common.NewErrEndpointContentValidation(fmt.Errorf("tx %d: hash length invalid: %d", i, len(hashBytes)), u)
				}

				// Check for duplicates
				hashLower := strings.ToLower(tx.Hash)
				if _, exists := seenHashes[hashLower]; exists {
					return common.NewErrEndpointContentValidation(fmt.Errorf("duplicate transaction hash in block: %s", tx.Hash), u)
				}
				seenHashes[hashLower] = struct{}{}
			}
		}
	}

	// ValidateTransactionBlockInfo: block hash/number/index match
	if dirs.ValidateTransactionBlockInfo {
		blockHash := strings.ToLower(strings.TrimPrefix(block.Hash, "0x"))
		var blockNum int64
		if block.Number != "" {
			var err error
			blockNum, err = common.HexToInt64(block.Number)
			if err != nil {
				return common.NewErrEndpointContentValidation(fmt.Errorf("invalid block number hex: %w", err), u)
			}
		}

		for i, tx := range txs {
			// Block hash must match
			if tx.BlockHash != "" && blockHash != "" {
				txBlockHash := strings.ToLower(strings.TrimPrefix(tx.BlockHash, "0x"))
				if txBlockHash != blockHash {
					return common.NewErrEndpointContentValidation(fmt.Errorf("tx %d: block hash mismatch", i), u)
				}
			}

			// Block number must match
			if tx.BlockNumber != "" && block.Number != "" {
				txBlockNum, err := common.HexToInt64(tx.BlockNumber)
				if err != nil {
					return common.NewErrEndpointContentValidation(fmt.Errorf("tx %d: invalid blockNumber hex: %w", i, err), u)
				}
				if txBlockNum != blockNum {
					return common.NewErrEndpointContentValidation(fmt.Errorf("tx %d: block number mismatch", i), u)
				}
			}

			// Transaction index must match array position
			if tx.TransactionIndex != "" {
				txIdx, err := common.HexToInt64(tx.TransactionIndex)
				if err != nil {
					return common.NewErrEndpointContentValidation(fmt.Errorf("tx %d: invalid transactionIndex hex: %w", i, err), u)
				}
				if txIdx != int64(i) {
					return common.NewErrEndpointContentValidation(fmt.Errorf("tx %d: transactionIndex mismatch: got %d want %d", i, txIdx, i), u)
				}
			}
		}
	}

	return nil
}
