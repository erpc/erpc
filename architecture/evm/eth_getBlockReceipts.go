package evm

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/crypto/sha3"
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
		LogIndex string   `json:"logIndex"`
		Address  string   `json:"address"`
		Topics   []string `json:"topics"`
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

	// Validate global logIndex contiguity (start at 0, increment by 1) if enabled
	if mcfg.CheckLogIndexStrictIncrements != nil && *mcfg.CheckLogIndexStrictIncrements {
		var expected uint64 = 0
		for i := range receipts {
			for j := range receipts[i].Logs {
				lix := receipts[i].Logs[j].LogIndex
				if lix == "" {
					return nil, common.NewErrUpstreamMalformedResponse(fmt.Errorf("missing logIndex at receipt %d log %d", i, j), u)
				}
				// Ensure lix is ASCII to avoid allocations on weird encodings
				if !utf8.ValidString(lix) {
					return nil, common.NewErrUpstreamMalformedResponse(fmt.Errorf("invalid UTF-8 in logIndex at receipt %d log %d", i, j), u)
				}
				if !strings.HasPrefix(lix, "0x") {
					return nil, common.NewErrUpstreamMalformedResponse(fmt.Errorf("logIndex must be hex string at receipt %d log %d", i, j), u)
				}
				v, herr := common.HexToUint64(lix)
				if herr != nil {
					return nil, common.NewErrUpstreamMalformedResponse(fmt.Errorf("invalid logIndex hex at receipt %d log %d: %w", i, j, herr), u)
				}
				if v != expected {
					return nil, common.NewErrUpstreamMalformedResponse(fmt.Errorf("logIndex not contiguous: expected %d got %d at receipt %d log %d", expected, v, i, j), u)
				}
				expected++
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

			// Recompute receipt bloom from logs (address + topics) and compare
			if len(receipts[i].Logs) > 0 {
				providedBloom, perr := evm.HexToBytes(receipts[i].LogsBloom)
				if perr != nil {
					return nil, common.NewErrUpstreamMalformedResponse(fmt.Errorf("invalid receipt.logsBloom hex: %w", perr), u)
				}
				if len(providedBloom) != evm.BloomLength {
					return nil, common.NewErrUpstreamMalformedResponse(fmt.Errorf("invalid receipt.logsBloom length: %d", len(providedBloom)), u)
				}

				expected := make([]byte, evm.BloomLength)
				for j := range receipts[i].Logs {
					// address
					if addr := strings.TrimSpace(receipts[i].Logs[j].Address); addr != "" {
						ab, aerr := evm.HexToBytes(addr)
						if aerr != nil {
							return nil, common.NewErrUpstreamMalformedResponse(fmt.Errorf("invalid log.address hex in receipt %d log %d: %w", i, j, aerr), u)
						}
						if len(ab) != evm.AddressLength {
							return nil, common.NewErrUpstreamMalformedResponse(fmt.Errorf("invalid log.address length in receipt %d log %d: %d", i, j, len(ab)), u)
						}
						bloomAdd(expected, ab)
					}
					// topics
					for k := range receipts[i].Logs[j].Topics {
						tb, terr := evm.HexToBytes(receipts[i].Logs[j].Topics[k])
						if terr != nil {
							return nil, common.NewErrUpstreamMalformedResponse(fmt.Errorf("invalid log.topic hex in receipt %d log %d idx %d: %w", i, j, k, terr), u)
						}
						if len(tb) != evm.TopicLength {
							return nil, common.NewErrUpstreamMalformedResponse(fmt.Errorf("invalid log.topic length in receipt %d log %d idx %d: %d", i, j, k, len(tb)), u)
						}
						bloomAdd(expected, tb)
					}
				}

				// Compare recomputed vs provided bloom
				if !bytes.Equal(expected, providedBloom) {
					return nil, common.NewErrUpstreamMalformedResponse(fmt.Errorf("receipt.logsBloom does not match logs content"), u)
				}
			}
		}
	}

	return rs, re
}

// bloomAdd sets the three keccak-derived bit positions for value into bloom (2048-bit, 256 bytes, big-endian)
func bloomAdd(bloom []byte, value []byte) {
	var h [32]byte
	hw := sha3.NewLegacyKeccak256()
	_, _ = hw.Write(value)
	_ = hw.Sum(h[:0])

	for i := 0; i < 6; i += 2 {
		bitpos := uint16(h[i])<<8 | uint16(h[i+1])
		bitpos &= 0x7FF // 2047
		byteIndex := 255 - int(bitpos>>3)
		bitMask := byte(1 << (bitpos & 7))
		bloom[byteIndex] |= bitMask
	}
}
