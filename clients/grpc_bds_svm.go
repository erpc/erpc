package clients

import (
	"context"
	"fmt"
	"strconv"

	"github.com/blockchain-data-standards/manifesto/svm"
	"github.com/blockchain-data-standards/manifesto/svm/parse"
	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// svmMaxBlocksLimit is Agave's getBlocksWithLimit cap; above it the node
// answers -32602, which only the live upstream renders.
const svmMaxBlocksLimit = 500_000

// svmUnservable reports a request this path will not answer. It is an
// "unsupported" error, never a failure: the cache treats it as a miss and the
// request goes to a live upstream, which renders whatever Agave would.
func svmUnservable(format string, args ...interface{}) error {
	return common.NewErrEndpointUnsupported(fmt.Errorf(format, args...))
}

// svmParams decodes the positional params array.
func svmParams(jrReq *common.JsonRpcRequest) ([]interface{}, error) {
	jrReq.RLock()
	paramsBytes, err := sonic.Marshal(jrReq.Params)
	jrReq.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal params: %w", err)
	}
	var params []interface{}
	if err := sonic.Unmarshal(paramsBytes, &params); err != nil {
		return nil, fmt.Errorf("failed to parse params: %w", err)
	}
	return params, nil
}

// svmConfigObject returns params[i] as a config object; absent or null is an
// empty config.
func svmConfigObject(params []interface{}, i int) (map[string]interface{}, error) {
	if len(params) <= i || params[i] == nil {
		return nil, nil
	}
	cfg, ok := params[i].(map[string]interface{})
	if !ok {
		return nil, svmUnservable("param %d is not a config object", i)
	}
	return cfg, nil
}

// svmStrictCommitment maps a request commitment the reader can honour.
// "processed" and anything unknown fall through: the reader serves nothing
// below confirmed, and the node renders its own answer for a bad value.
func svmStrictCommitment(v interface{}) (svm.Commitment, error) {
	switch v {
	case nil, "finalized":
		return svm.Commitment_COMMITMENT_FINALIZED, nil
	case "confirmed":
		return svm.Commitment_COMMITMENT_CONFIRMED, nil
	default:
		return 0, svmUnservable("commitment %v not served by BDS", v)
	}
}

// svmReaderCommitment is the commitment to ask the reader at for a slot-pinned
// read, and false when the slot is above the network's root at finalized.
//
// The reader's own FINALIZED ceiling is its checkpoint: a batch is stored
// once 32 slots deep, and the checkpoint lands ~10 s later, so it trails the
// network's root by tens of seconds. Since Alpenglow (epoch 1064) confirmed
// and finalized are the same state, so a confirmed block at or below the
// root the vendor pollers report is the finalized block. With no root known
// the reader's ceiling stays the bound.
func svmReaderCommitment(ctx context.Context, req *common.NormalizedRequest, want svm.Commitment, slot uint64) (svm.Commitment, bool) {
	if want != svm.Commitment_COMMITMENT_FINALIZED {
		return want, true
	}
	root, ok := svmFinalizedSlot(ctx, req)
	if !ok {
		return want, true
	}
	if slot > root {
		return want, false
	}
	return svm.Commitment_COMMITMENT_CONFIRMED, true
}

// svmFinalizedSlot is the network's majority finalized root, or ok=false when
// the request carries no SVM network or no upstream has reported one. The
// reader stores blocks at confirmed and does not read commitment for these
// calls, so this is what proves an answer rooted.
func svmFinalizedSlot(ctx context.Context, req *common.NormalizedRequest) (uint64, bool) {
	n, ok := req.Network().(common.SvmNetwork)
	if !ok {
		return 0, false
	}
	tip := n.SvmHighestFinalizedSlot(ctx)
	if tip <= 0 {
		return 0, false
	}
	return uint64(tip), true
}

// svmHistoryOf is the Agave history of the request's cluster; the zero value
// (every field recorded) when the request carries no SVM network config.
func svmHistoryOf(req *common.NormalizedRequest) common.SvmHistory {
	n := req.Network()
	if n == nil {
		return common.SvmHistory{}
	}
	cfg := n.Config()
	if cfg == nil || cfg.Svm == nil {
		return common.SvmHistory{}
	}
	return common.KnownSvmHistory(cfg.Svm.Chain, cfg.Svm.Cluster)
}

// svmResult wraps a rendered result in the request's JSON-RPC envelope.
func svmResult(req *common.NormalizedRequest, jrReq *common.JsonRpcRequest, result interface{}) (*common.NormalizedResponse, error) {
	resultBytes, err := sonic.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal result: %w", err)
	}
	jsonRpcResp := &common.JsonRpcResponse{}
	if err := jsonRpcResp.SetID(jrReq.ID); err != nil {
		return nil, fmt.Errorf("failed to set ID: %w", err)
	}
	jsonRpcResp.SetResult(resultBytes)
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jsonRpcResp), nil
}

// svmVersionServable is Agave's validate_version: legacy always, a numeric
// version only at or below the request's cap. An absent or unparsable
// version cannot be proven servable.
func svmVersionServable(version *string, max *uint32) bool {
	if version == nil {
		return false
	}
	if *version == "legacy" {
		return true
	}
	n, err := strconv.ParseUint(*version, 10, 32)
	return err == nil && max != nil && n <= uint64(*max)
}

// handleSvmGetTransaction serves Solana `getTransaction` from the reader's
// signature index.
//
// An unset transaction means "this reader has no entry", never "the chain has
// no such transaction": the backfilled history carries no index entries. So
// every answer the reader cannot prove — no entry, a different first
// signature, no transactionIndex, a version the caller did not opt into —
// falls through to a live upstream rather than rendering null.
func (c *GenericGrpcBdsClient) handleSvmGetTransaction(ctx context.Context, conn *bdsConn, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	params, err := svmParams(jrReq)
	if err != nil {
		return nil, err
	}
	if len(params) < 1 || len(params) > 2 {
		return nil, svmUnservable("getTransaction takes 1 or 2 params, got %d", len(params))
	}
	sigStr, ok := params[0].(string)
	if !ok {
		return nil, svmUnservable("getTransaction signature is not a string")
	}
	signature, err := svm.Base58Decode(sigStr)
	if err != nil || len(signature) != 64 {
		return nil, svmUnservable("getTransaction signature is not 64 base58 bytes")
	}

	grpcReq := &svm.GetTransactionRequest{Signature: signature}
	var wantParsed bool
	encoding := func(v interface{}) error {
		switch v {
		case nil, "json":
			return nil
		case "jsonParsed":
			wantParsed = true
			return nil
		default:
			return svmUnservable("getTransaction encoding %v not served by BDS", v)
		}
	}
	if len(params) == 2 {
		// Agave's RpcEncodingConfigWrapper: a config object, or the
		// deprecated bare encoding string.
		if s, isString := params[1].(string); isString {
			if err := encoding(s); err != nil {
				return nil, err
			}
		} else {
			cfg, err := svmConfigObject(params, 1)
			if err != nil {
				return nil, err
			}
			for key, value := range cfg {
				if value == nil {
					continue
				}
				switch key {
				case "encoding":
					if err := encoding(value); err != nil {
						return nil, err
					}
				case "commitment":
					if grpcReq.Commitment, err = svmStrictCommitment(value); err != nil {
						return nil, err
					}
				case "maxSupportedTransactionVersion":
					n, isNum := value.(float64)
					if !isNum || n < 0 || n > 255 || n != float64(uint8(n)) {
						return nil, svmUnservable("maxSupportedTransactionVersion %v is not a u8", value)
					}
					v := uint32(n)
					grpcReq.MaxSupportedTransactionVersion = &v
				default:
					return nil, svmUnservable("getTransaction config key %q not served by BDS", key)
				}
			}
		}
	}

	// A finalized read is asked at CONFIRMED and bounded by the network root
	// after the answer: the slot is unknown until the reader names it (see
	// svmReaderCommitment for why the reader's own ceiling is not used).
	root, bounded := uint64(0), false
	if grpcReq.Commitment == svm.Commitment_COMMITMENT_FINALIZED {
		if root, bounded = svmFinalizedSlot(ctx, req); bounded {
			grpcReq.Commitment = svm.Commitment_COMMITMENT_CONFIRMED
		}
	}

	ctx, grpcSpan := common.StartDetailSpan(ctx, "GrpcBdsClient.SvmGetTransaction",
		trace.WithAttributes(attribute.String("commitment", grpcReq.Commitment.String())),
	)
	grpcResp, err := callBoundedT(ctx, func(ctx context.Context) (*svm.GetTransactionResponse, error) {
		return conn.svmClient.GetTransaction(ctx, grpcReq)
	})
	if err != nil {
		common.SetTraceSpanError(grpcSpan, err)
		grpcSpan.End()
		return nil, svmMapError(err)
	}
	ct := grpcResp.GetTransaction()
	grpcSpan.SetAttributes(attribute.Bool("found", ct != nil))
	grpcSpan.End()

	// The index maps a hash of the first signature; a transaction whose first
	// signature is not the one asked for is not the answer, whatever the
	// reader says.
	sigs := ct.GetTransaction().GetSignatures()
	if len(sigs) == 0 || string(sigs[0]) != string(signature) {
		return nil, svmUnservable("transaction not indexed by BDS")
	}
	if grpcResp.TransactionIndex == nil {
		return nil, svmUnservable("BDS returned no transactionIndex")
	}
	if !svmVersionServable(ct.Version, grpcReq.MaxSupportedTransactionVersion) {
		return nil, svmUnservable("transaction version not servable to this request")
	}

	var result map[string]interface{}
	if wantParsed {
		parse.AttachToTransaction(ct)
		result = svm.ConfirmedTransactionToJsonRpcParsed(ct)
	} else {
		result = svm.ConfirmedTransactionToJsonRpc(ct)
	}
	if grpcReq.MaxSupportedTransactionVersion == nil {
		// Agave renders `version` only for a caller that opted in.
		delete(result, "version")
	}
	if bounded && grpcResp.Slot > root {
		return nil, svmUnservable("slot %d is not finalized on this network", grpcResp.Slot)
	}
	result["slot"] = grpcResp.Slot
	result["transactionIndex"] = *grpcResp.TransactionIndex
	if grpcResp.BlockTime != nil && svmHistoryOf(req).RecordsBlockTime(grpcResp.Slot) {
		result["blockTime"] = *grpcResp.BlockTime
	} else {
		result["blockTime"] = nil
	}
	return svmResult(req, jrReq, result)
}

// handleSvmGetBlockTime serves Solana `getBlockTime`. The call takes no
// commitment: Agave reads a rooted time at or below its root and a bank's
// clock above it, so only a slot the network has finalized is answered here.
func (c *GenericGrpcBdsClient) handleSvmGetBlockTime(ctx context.Context, conn *bdsConn, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	params, err := svmParams(jrReq)
	if err != nil {
		return nil, err
	}
	if len(params) != 1 {
		return nil, svmUnservable("getBlockTime takes 1 param, got %d", len(params))
	}
	slot, err := svmParseSlot(params[0])
	if err != nil {
		return nil, svmUnservable("%v", err)
	}
	finalized, ok := svmFinalizedSlot(ctx, req)
	if !ok || slot > finalized {
		return nil, svmUnservable("slot %d is not finalized on this network", slot)
	}
	history := svmHistoryOf(req)
	if history.GenesisTimeOnly(slot) {
		// Agave answers slot 0 with the genesis creation time, which no
		// archive row carries.
		return nil, svmUnservable("slot 0 block time is the genesis creation time")
	}

	ctx, grpcSpan := common.StartDetailSpan(ctx, "GrpcBdsClient.SvmGetBlockTime",
		trace.WithAttributes(attribute.Int64("slot", int64(slot))),
	)
	grpcResp, err := callBoundedT(ctx, func(ctx context.Context) (*svm.GetBlockTimeResponse, error) {
		return conn.svmClient.GetBlockTime(ctx, &svm.GetBlockTimeRequest{Slot: slot})
	})
	if err != nil {
		common.SetTraceSpanError(grpcSpan, err)
		grpcSpan.End()
		return nil, svmMapError(err)
	}
	grpcSpan.SetAttributes(attribute.String("slot_status", grpcResp.SlotStatus.String()))
	grpcSpan.End()

	// A skip is -32007 inside the node's ledger and -32009 below it; that
	// window differs by vendor, so only the live upstream can answer it.
	if grpcResp.SlotStatus == svm.SlotStatus_SLOT_SKIPPED {
		return nil, svmUnservable("slot %d is skipped; not served from BDS", slot)
	}
	if !history.RecordsBlockTime(slot) {
		return svmResult(req, jrReq, nil)
	}
	if grpcResp.BlockTime == nil {
		// A present slot Agave records a time for: the reader holding none
		// is not Agave's null.
		return nil, svmUnservable("BDS holds no block time for slot %d", slot)
	}
	return svmResult(req, jrReq, *grpcResp.BlockTime)
}

// handleSvmGetBlocksWithLimit serves Solana `getBlocksWithLimit`. Agave lists
// the first `limit` rooted blocks from `start`; the reader stops early at its
// tip or a skip-dense range, so only a list of exactly `limit` ascending slots
// ending at a finalized slot is that list.
func (c *GenericGrpcBdsClient) handleSvmGetBlocksWithLimit(ctx context.Context, conn *bdsConn, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	params, err := svmParams(jrReq)
	if err != nil {
		return nil, err
	}
	if len(params) < 2 || len(params) > 3 {
		return nil, svmUnservable("getBlocksWithLimit takes 2 or 3 params, got %d", len(params))
	}
	start, err := svmParseSlot(params[0])
	if err != nil {
		return nil, svmUnservable("%v", err)
	}
	limit, err := svmParseSlot(params[1])
	if err != nil || limit > svmMaxBlocksLimit {
		return nil, svmUnservable("limit %v not served by BDS", params[1])
	}
	grpcReq := &svm.GetBlocksWithLimitRequest{StartSlot: start, Limit: limit}
	cfg, err := svmConfigObject(params, 2)
	if err != nil {
		return nil, err
	}
	for key, value := range cfg {
		if value == nil {
			continue
		}
		if key != "commitment" {
			return nil, svmUnservable("getBlocksWithLimit config key %q not served by BDS", key)
		}
		if grpcReq.Commitment, err = svmStrictCommitment(value); err != nil {
			return nil, err
		}
	}
	if limit == 0 {
		return svmResult(req, jrReq, []uint64{})
	}
	finalized, ok := svmFinalizedSlot(ctx, req)
	if !ok || start > finalized {
		return nil, svmUnservable("start slot %d is not finalized on this network", start)
	}
	// The check below bounds the list by the network's root, so the reader
	// need not apply its own: its FINALIZED ceiling is its checkpoint, tens
	// of seconds behind the root, and would short every list near the tip.
	grpcReq.Commitment = svm.Commitment_COMMITMENT_CONFIRMED

	ctx, grpcSpan := common.StartDetailSpan(ctx, "GrpcBdsClient.SvmGetBlocksWithLimit",
		trace.WithAttributes(
			attribute.Int64("start_slot", int64(start)),
			attribute.Int64("limit", int64(limit)),
		),
	)
	grpcResp, err := callBoundedT(ctx, func(ctx context.Context) (*svm.GetBlocksResponse, error) {
		return conn.svmClient.GetBlocksWithLimit(ctx, grpcReq)
	})
	if err != nil {
		common.SetTraceSpanError(grpcSpan, err)
		grpcSpan.End()
		return nil, svmMapError(err)
	}
	slots := grpcResp.GetSlots()
	grpcSpan.SetAttributes(attribute.Int("slots", len(slots)))
	grpcSpan.End()

	if uint64(len(slots)) != limit || slots[0] < start || slots[len(slots)-1] > finalized {
		return nil, svmUnservable("BDS list is not the rooted list Agave returns")
	}
	for i := 1; i < len(slots); i++ {
		if slots[i] <= slots[i-1] {
			return nil, svmUnservable("BDS list is not ascending")
		}
	}
	return svmResult(req, jrReq, slots)
}
