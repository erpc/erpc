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

// upstreamPostForward_eth_getBlockReceipts validates receipts based on request directives.
// It runs as a post-upstream hook after the response is received.
func upstreamPostForward_eth_getBlockReceipts(ctx context.Context, n common.Network, u common.Upstream, rq *common.NormalizedRequest, rs *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
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
	ctx, span := common.StartDetailSpan(ctx, "Upstream.PostForwardHook.eth_getBlockReceipts", trace.WithAttributes(
		attribute.String("network.id", networkId),
		attribute.String("upstream.id", upstreamId),
	))
	defer span.End()

	// Skip validation if response is empty
	if rs.IsObjectNull() || rs.IsResultEmptyish() {
		return rs, re
	}

	// Get directives - if nil, no validation needed
	dirs := rq.Directives()
	if dirs == nil {
		return rs, re
	}

	// Run directive-based validation
	if err := validateGetBlockReceipts(ctx, u, dirs, rs); err != nil {
		return rs, err
	}

	return rs, re
}

// validateGetBlockReceipts validates eth_getBlockReceipts responses based on request directives.
// It checks:
// - ReceiptsCountExact / ReceiptsCountAtLeast
// - ValidationExpectedBlockHash / ValidationExpectedBlockNumber
// - ValidateTxHashUniqueness
// - ValidateTransactionIndex
// - EnforceNonEmptyLogsBloom
// - EnforceLogIndexStrictIncrements
// - ValidateLogsBloom
func validateGetBlockReceipts(ctx context.Context, u common.Upstream, dirs *common.RequestDirectives, rs *common.NormalizedResponse) error {
	jrr, err := rs.JsonRpcResponse(ctx)
	if err != nil {
		return err
	}

	// Minimal JSON model for validation
	type logLite struct {
		LogIndex string   `json:"logIndex"`
		Address  string   `json:"address"`
		Topics   []string `json:"topics"`
	}
	type receiptLite struct {
		BlockHash        string    `json:"blockHash"`
		BlockNumber      string    `json:"blockNumber"`
		TransactionHash  string    `json:"transactionHash"`
		TransactionIndex string    `json:"transactionIndex"`
		LogsBloom        string    `json:"logsBloom"`
		Logs             []logLite `json:"logs"`
	}

	var receipts []receiptLite
	if err := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &receipts); err != nil {
		return common.NewErrEndpointContentValidation(fmt.Errorf("invalid JSON result for receipts validation: %w", err), u)
	}

	if receipts == nil {
		receipts = []receiptLite{}
	}

	count := int64(len(receipts))

	// 1. Count Validations
	if dirs.ReceiptsCountExact != -1 {
		if count != dirs.ReceiptsCountExact {
			return common.NewErrEndpointContentValidation(
				fmt.Errorf("receipts count mismatch: expected %d got %d", dirs.ReceiptsCountExact, count),
				u,
			)
		}
	}
	if dirs.ReceiptsCountAtLeast != -1 {
		if count < dirs.ReceiptsCountAtLeast {
			return common.NewErrEndpointContentValidation(
				fmt.Errorf("receipts count insufficient: expected at least %d got %d", dirs.ReceiptsCountAtLeast, count),
				u,
			)
		}
	}

	// 2. Expected Ground Truths
	if expectedHash := dirs.ValidationExpectedBlockHash; expectedHash != "" {
		expectedHash = strings.ToLower(strings.TrimPrefix(expectedHash, "0x"))
		for i, r := range receipts {
			gotHash := strings.ToLower(strings.TrimPrefix(r.BlockHash, "0x"))
			if gotHash != "" && gotHash != expectedHash {
				return common.NewErrEndpointContentValidation(
					fmt.Errorf("receipts block hash mismatch at index %d: expected %s got %s", i, expectedHash, gotHash),
					u,
				)
			}
		}
	}

	if expectedNum := dirs.ValidationExpectedBlockNumber; expectedNum != -1 {
		for i, r := range receipts {
			if r.BlockNumber == "" {
				continue
			}
			bn, err := common.HexToInt64(r.BlockNumber)
			if err != nil {
				return common.NewErrEndpointContentValidation(fmt.Errorf("invalid blockNumber hex at index %d: %w", i, err), u)
			}
			if bn != expectedNum {
				return common.NewErrEndpointContentValidation(
					fmt.Errorf("receipts block number mismatch at index %d: expected %d got %d", i, expectedNum, bn),
					u,
				)
			}
		}
	}

	// 3. Tx Hash Uniqueness
	if dirs.ValidateTxHashUniqueness {
		seen := make(map[string]struct{}, count)
		for i, r := range receipts {
			if r.TransactionHash == "" {
				continue
			}
			if _, exists := seen[r.TransactionHash]; exists {
				return common.NewErrEndpointContentValidation(fmt.Errorf("duplicate transaction hash %s at index %d", r.TransactionHash, i), u)
			}
			seen[r.TransactionHash] = struct{}{}
		}
	}

	// 4. Transaction Index Consistency
	if dirs.ValidateTransactionIndex {
		for i, r := range receipts {
			if r.TransactionIndex == "" {
				continue
			}
			idx, err := common.HexToInt64(r.TransactionIndex)
			if err != nil {
				return common.NewErrEndpointContentValidation(fmt.Errorf("invalid transactionIndex hex at index %d: %w", i, err), u)
			}
			if idx != int64(i) {
				return common.NewErrEndpointContentValidation(
					fmt.Errorf("transaction index mismatch at array index %d: got %d", i, idx),
					u,
				)
			}
		}
	}

	// 5. Deep Receipt Validation (Logs, Bloom)
	var expectedLogIndex uint64 = 0

	for i, r := range receipts {
		// EnforceNonEmptyLogsBloom
		if dirs.EnforceNonEmptyLogsBloom {
			lb := strings.TrimLeft(strings.TrimPrefix(r.LogsBloom, "0x"), "0")
			if lb != "" && len(r.Logs) == 0 {
				return common.NewErrEndpointContentValidation(fmt.Errorf("non-zero receipt.logsBloom but 0 logs at receipt %d", i), u)
			}
		}

		// EnforceLogIndexStrictIncrements
		if dirs.EnforceLogIndexStrictIncrements {
			for j, log := range r.Logs {
				lix := log.LogIndex
				if lix == "" {
					return common.NewErrEndpointContentValidation(fmt.Errorf("missing logIndex at receipt %d log %d", i, j), u)
				}
				if !utf8.ValidString(lix) {
					return common.NewErrEndpointContentValidation(fmt.Errorf("invalid UTF-8 in logIndex at receipt %d log %d", i, j), u)
				}
				v, herr := common.HexToUint64(lix)
				if herr != nil {
					return common.NewErrEndpointContentValidation(fmt.Errorf("invalid logIndex hex at receipt %d log %d: %w", i, j, herr), u)
				}
				if v != expectedLogIndex {
					return common.NewErrEndpointContentValidation(fmt.Errorf("logIndex not contiguous: expected %d got %d at receipt %d log %d", expectedLogIndex, v, i, j), u)
				}
				expectedLogIndex++
			}
		}

		// ValidateLogsBloom
		if dirs.ValidateLogsBloom && len(r.Logs) > 0 {
			providedBloom, perr := evm.HexToBytes(r.LogsBloom)
			if perr != nil {
				return common.NewErrEndpointContentValidation(fmt.Errorf("invalid receipt.logsBloom hex: %w", perr), u)
			}
			if len(providedBloom) != evm.BloomLength {
				return common.NewErrEndpointContentValidation(fmt.Errorf("invalid receipt.logsBloom length: %d", len(providedBloom)), u)
			}

			expected := make([]byte, evm.BloomLength)
			for j, log := range r.Logs {
				if addr := strings.TrimSpace(log.Address); addr != "" {
					ab, aerr := evm.HexToBytes(addr)
					if aerr != nil {
						return common.NewErrEndpointContentValidation(fmt.Errorf("invalid log.address hex in receipt %d log %d: %w", i, j, aerr), u)
					}
					if len(ab) != evm.AddressLength {
						return common.NewErrEndpointContentValidation(fmt.Errorf("invalid log.address length in receipt %d log %d: %d", i, j, len(ab)), u)
					}
					bloomAdd(expected, ab)
				}
				for k, topic := range log.Topics {
					tb, terr := evm.HexToBytes(topic)
					if terr != nil {
						return common.NewErrEndpointContentValidation(fmt.Errorf("invalid log.topic hex in receipt %d log %d idx %d: %w", i, j, k, terr), u)
					}
					if len(tb) != evm.TopicLength {
						return common.NewErrEndpointContentValidation(fmt.Errorf("invalid log.topic length in receipt %d log %d idx %d: %d", i, j, k, len(tb)), u)
					}
					bloomAdd(expected, tb)
				}
			}

			if !bytes.Equal(expected, providedBloom) {
				return common.NewErrEndpointContentValidation(fmt.Errorf("receipt.logsBloom does not match logs content at receipt %d", i), u)
			}
		}
	}

	return nil
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
