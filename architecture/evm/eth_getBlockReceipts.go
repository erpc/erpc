package evm

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/erpc/erpc/common"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// upstreamPostForward_eth_getBlockReceipts validates receipts structure per Upstream integrity config.
// It runs as a post-upstream hook and never imports go-ethereum. It inspects the JSON result directly.
func upstreamPostForward_eth_getBlockReceipts(ctx context.Context, n common.Network, u common.Upstream, rq *common.NormalizedRequest, rs *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "Upstream.PostForwardHook.eth_getBlockReceipts", trace.WithAttributes(
		attribute.String("network.id", n.Id()),
		attribute.String("upstream.id", u.Id()),
	))
	defer span.End()

	if re != nil || rs == nil {
		return rs, re
	}

	cfg := u.Config()
	if cfg == nil || cfg.Evm == nil || cfg.Evm.Integrity == nil || cfg.Evm.Integrity.EthGetBlockReceipts == nil || !cfg.Evm.Integrity.EthGetBlockReceipts.Enabled {
		return rs, re
	}
	mcfg := cfg.Evm.Integrity.EthGetBlockReceipts

	// Parse JSON-RPC response
	jrr, err := rs.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return rs, err
	}
	if jrr.Error != nil {
		return rs, re
	}
	if rs.IsResultEmptyish(ctx) || jrr.IsResultEmptyish() {
		return rs, re
	}

	// Minimal JSON model for validation
	type logLite struct {
		LogIndex string `json:"logIndex"`
	}
	type receiptLite struct {
		BlockHash string    `json:"blockHash"`
		LogsBloom string    `json:"logsBloom"`
		Logs      []logLite `json:"logs"`
	}

	var receipts []receiptLite
	if err := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &receipts); err != nil {
		return nil, common.NewErrUpstreamMalformedResponse(fmt.Errorf("invalid JSON result for eth_getBlockReceipts: %w", err), u)
	}

	// Quick sanity: ensure decoded array
	if receipts == nil {
		receipts = []receiptLite{}
	}

	// Validate blockHash consistency across receipts (when present)
	var expectedBlockHash string
	for i := range receipts {
		bh := strings.ToLower(strings.TrimPrefix(receipts[i].BlockHash, "0x"))
		if bh == "" {
			continue
		}
		if expectedBlockHash == "" {
			expectedBlockHash = bh
		} else if bh != expectedBlockHash {
			return nil, common.NewErrUpstreamMalformedResponse(fmt.Errorf("inconsistent receipt.blockHash in array"), u)
		}
	}

	// Validate global logIndex strict increments if enabled
	if mcfg.CheckLogIndexStrictIncrements != nil && *mcfg.CheckLogIndexStrictIncrements {
		var last uint64
		hasLast := false
		for i := range receipts {
			for j := range receipts[i].Logs {
				lix := receipts[i].Logs[j].LogIndex
				if lix == "" {
					continue
				}
				// Ensure lix is ASCII to avoid allocations on weird encodings
				if !utf8.ValidString(lix) {
					return nil, common.NewErrUpstreamMalformedResponse(fmt.Errorf("invalid UTF-8 in logIndex"), u)
				}
				if !strings.HasPrefix(lix, "0x") {
					return nil, common.NewErrUpstreamMalformedResponse(fmt.Errorf("logIndex must be hex string"), u)
				}
				v, herr := common.HexToUint64(lix)
				if herr != nil {
					return nil, common.NewErrUpstreamMalformedResponse(fmt.Errorf("invalid logIndex hex: %w", herr), u)
				}
				if hasLast && v <= last {
					return nil, common.NewErrUpstreamMalformedResponse(fmt.Errorf("logIndex not strictly increasing"), u)
				}
				last = v
				hasLast = true
			}
		}
	}

	// Optional receipt-level bloom vs logs sanity (cannot check block header bloom here)
	if mcfg.CheckLogsBloom != nil && *mcfg.CheckLogsBloom {
		for i := range receipts {
			lb := strings.TrimLeft(strings.TrimPrefix(receipts[i].LogsBloom, "0x"), "0")
			if lb != "" && len(receipts[i].Logs) == 0 {
				return nil, common.NewErrUpstreamMalformedResponse(fmt.Errorf("non-zero receipt.logsBloom but 0 logs"), u)
			}
		}
	}

	return rs, re
}
