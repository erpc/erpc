package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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

// svmFinalizedSlot is the leading upstream's finalized root
// (SvmHighestFinalizedSlotMax), or ok=false when the request carries no SVM
// network or no upstream has reported one. The reader stores blocks at
// confirmed and does not read commitment for these calls, so this is what
// proves an answer rooted. A slot above it falls through to an upstream, so
// the bound is the most-advanced root: the majority root would send rooted
// blocks the reader holds to an upstream.
func svmFinalizedSlot(ctx context.Context, req *common.NormalizedRequest) (uint64, bool) {
	n, ok := req.Network().(common.SvmNetwork)
	if !ok {
		return 0, false
	}
	tip := n.SvmHighestFinalizedSlotMax(ctx)
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

// svmMaxSignaturesLimit is Agave's getSignaturesForAddress page cap and its
// default page size.
const svmMaxSignaturesLimit = 1000

// svmSignatureInfo is one getSignaturesForAddress row. Field order is the
// order Agave renders (its JSON keys are sorted).
type svmSignatureInfo struct {
	BlockTime          *int64          `json:"blockTime"`
	ConfirmationStatus string          `json:"confirmationStatus"`
	Err                json.RawMessage `json:"err"`
	Memo               *string         `json:"memo"`
	Signature          string          `json:"signature"`
	Slot               uint64          `json:"slot"`
	TransactionIndex   uint32          `json:"transactionIndex"`
}

// handleSvmGetSignaturesForAddress serves Solana `getSignaturesForAddress`
// from the reader's address index. The reader answers only a page it can
// prove complete and refuses everything else with OUT_OF_RANGE, which falls
// through to a live upstream.
//
// Every row carries Agave's confirmationStatus, which needs the network root,
// so no root known means no answer. At finalized, Agave pages from its root
// and resolves cursors among rooted slots only, so rows above the root are
// dropped and a cursor above it is not served.
func (c *GenericGrpcBdsClient) handleSvmGetSignaturesForAddress(ctx context.Context, conn *bdsConn, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	params, err := svmParams(jrReq)
	if err != nil {
		return nil, err
	}
	if len(params) < 1 || len(params) > 2 {
		return nil, svmUnservable("getSignaturesForAddress takes 1 or 2 params, got %d", len(params))
	}
	addrStr, ok := params[0].(string)
	if !ok {
		return nil, svmUnservable("getSignaturesForAddress address is not a string")
	}
	address, err := svm.Base58Decode(addrStr)
	if err != nil || len(address) != 32 {
		return nil, svmUnservable("getSignaturesForAddress address is not 32 base58 bytes")
	}
	cfg, err := svmConfigObject(params, 1)
	if err != nil {
		return nil, err
	}

	grpcReq := &svm.GetSignaturesForAddressRequest{Address: address}
	signature := func(key string, v interface{}) ([]byte, error) {
		s, isString := v.(string)
		if !isString {
			return nil, svmUnservable("getSignaturesForAddress %s is not a string", key)
		}
		sig, err := svm.Base58Decode(s)
		if err != nil || len(sig) != 64 {
			return nil, svmUnservable("getSignaturesForAddress %s is not 64 base58 bytes", key)
		}
		return sig, nil
	}
	for key, value := range cfg {
		if value == nil {
			continue
		}
		switch key {
		case "limit":
			n, isNum := value.(float64)
			if !isNum || n < 1 || n > svmMaxSignaturesLimit || n != math.Trunc(n) {
				return nil, svmUnservable("getSignaturesForAddress limit %v not served by BDS", value)
			}
			grpcReq.Limit = uint32(n)
		case "before":
			if grpcReq.Before, err = signature(key, value); err != nil {
				return nil, err
			}
		case "until":
			if grpcReq.Until, err = signature(key, value); err != nil {
				return nil, err
			}
		case "commitment":
			if grpcReq.Commitment, err = svmStrictCommitment(value); err != nil {
				return nil, err
			}
		case "minContextSlot":
			n, isNum := value.(float64)
			if !isNum || n < 0 || n != math.Trunc(n) || n >= 1<<53 {
				return nil, svmUnservable("getSignaturesForAddress minContextSlot %v is not a u64", value)
			}
			slot := uint64(n)
			grpcReq.MinContextSlot = &slot
		default:
			return nil, svmUnservable("getSignaturesForAddress config key %q not served by BDS", key)
		}
	}

	root, ok := svmFinalizedSlot(ctx, req)
	if !ok {
		return nil, svmUnservable("no finalized root known for confirmationStatus")
	}
	finalized := grpcReq.Commitment == svm.Commitment_COMMITMENT_FINALIZED
	var cursorsRooted chan error
	if finalized {
		// The reader serves confirmed to its live tip; the root bounds it below.
		grpcReq.Commitment = svm.Commitment_COMMITMENT_CONFIRMED
		if grpcReq.MinContextSlot != nil && *grpcReq.MinContextSlot > root {
			return nil, svmUnservable("minContextSlot %d is above the finalized root %d", *grpcReq.MinContextSlot, root)
		}
		var cursors [][]byte
		for _, cursor := range [][]byte{grpcReq.Before, grpcReq.Until} {
			if cursor != nil {
				cursors = append(cursors, cursor)
			}
		}
		if len(cursors) > 0 {
			cursorsRooted = make(chan error, 1)
			go func(ctx context.Context) { cursorsRooted <- svmCursorsRooted(ctx, conn, cursors, root) }(ctx)
		}
	}

	ctx, grpcSpan := common.StartDetailSpan(ctx, "GrpcBdsClient.SvmGetSignaturesForAddress",
		trace.WithAttributes(
			attribute.String("commitment", grpcReq.Commitment.String()),
			attribute.Int64("limit", int64(grpcReq.Limit)),
		),
	)
	grpcResp, err := callBoundedT(ctx, func(ctx context.Context) (*svm.GetSignaturesForAddressResponse, error) {
		return conn.svmClient.GetSignaturesForAddress(ctx, grpcReq)
	})
	if err != nil {
		common.SetTraceSpanError(grpcSpan, err)
		grpcSpan.End()
		return nil, svmMapError(err)
	}
	rows := grpcResp.GetSignatures()
	grpcSpan.SetAttributes(attribute.Int("rows", len(rows)))
	grpcSpan.End()
	if cursorsRooted != nil {
		if err := <-cursorsRooted; err != nil {
			return nil, err
		}
	}

	limit := int(grpcReq.Limit)
	if limit == 0 {
		limit = svmMaxSignaturesLimit
	}
	if len(rows) > limit {
		return nil, svmUnservable("BDS returned %d rows for limit %d", len(rows), limit)
	}
	history := svmHistoryOf(req)
	result := make([]svmSignatureInfo, 0, len(rows))
	dropped := false
	for i, row := range rows {
		if len(row.Signature) != 64 || row.TransactionIndex == nil {
			return nil, svmUnservable("BDS row %d has no 64-byte signature or transactionIndex", i)
		}
		if i > 0 {
			prev := rows[i-1]
			if row.Slot > prev.Slot || (row.Slot == prev.Slot && *row.TransactionIndex >= *prev.TransactionIndex) {
				return nil, svmUnservable("BDS rows are not newest-first")
			}
		}
		if finalized && row.Slot > root {
			dropped = true
			continue
		}
		info := svmSignatureInfo{
			ConfirmationStatus: "confirmed",
			Err:                json.RawMessage("null"),
			Memo:               row.Memo,
			Signature:          svm.Base58Encode(row.Signature),
			Slot:               row.Slot,
			TransactionIndex:   *row.TransactionIndex,
		}
		if row.Slot <= root {
			info.ConfirmationStatus = "finalized"
		}
		if row.Err != nil {
			if !json.Valid([]byte(*row.Err)) {
				return nil, svmUnservable("BDS row %d err is not JSON", i)
			}
			info.Err = json.RawMessage(*row.Err)
		}
		if row.BlockTime != nil && history.RecordsBlockTime(row.Slot) {
			info.BlockTime = row.BlockTime
		}
		result = append(result, info)
	}
	// Rows above the root were dropped from a full page: the rows under the
	// page's oldest one that Agave would list next are not in it.
	if dropped && len(rows) == limit {
		return nil, svmUnservable("page above the finalized root does not fill limit %d", limit)
	}
	return svmResult(req, jrReq, result)
}

// svmCursorsRooted reports whether every cursor signature landed at or below
// root. Agave resolves a finalized cursor among rooted slots only: an unrooted
// `before` gives an empty page (or -32020), an unrooted `until` is ignored.
func svmCursorsRooted(ctx context.Context, conn *bdsConn, cursors [][]byte, root uint64) error {
	resp, err := callBoundedT(ctx, func(ctx context.Context) (*svm.GetSignatureStatusesResponse, error) {
		return conn.svmClient.GetSignatureStatuses(ctx, &svm.GetSignatureStatusesRequest{Signatures: cursors})
	})
	if err != nil {
		return svmMapError(err)
	}
	statuses := resp.GetStatuses()
	if len(statuses) != len(cursors) {
		return svmUnservable("BDS returned %d statuses for %d cursors", len(statuses), len(cursors))
	}
	for _, st := range statuses {
		if !st.GetFound() || st.GetSlot() > root {
			return svmUnservable("cursor is not at or below the finalized root %d", root)
		}
	}
	return nil
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
