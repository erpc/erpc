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
// It performs comprehensive validation including:
// - ReceiptsCountExact / ReceiptsCountAtLeast
// - ValidationExpectedBlockHash / ValidationExpectedBlockNumber
// - ValidateTxHashUniqueness
// - ValidateTransactionIndex
// - EnforceNonEmptyLogsBloom
// - EnforceLogIndexStrictIncrements
// - ValidateLogsBloom
// - ValidateLogFields (address/topic lengths, block/tx hash matching)
// - ValidateReceiptTransactionMatch (cross-validation with GroundTruthTransactions)
// - ValidateContractCreation (contract creation consistency)
func validateGetBlockReceipts(ctx context.Context, u common.Upstream, dirs *common.RequestDirectives, rs *common.NormalizedResponse) error {
	jrr, err := rs.JsonRpcResponse(ctx)
	if err != nil {
		return err
	}

	// Minimal JSON model for validation
	type logLite struct {
		LogIndex         string   `json:"logIndex"`
		Address          string   `json:"address"`
		Topics           []string `json:"topics"`
		BlockHash        string   `json:"blockHash"`
		BlockNumber      string   `json:"blockNumber"`
		TransactionHash  string   `json:"transactionHash"`
		TransactionIndex string   `json:"transactionIndex"`
	}
	type receiptLite struct {
		BlockHash        string    `json:"blockHash"`
		BlockNumber      string    `json:"blockNumber"`
		TransactionHash  string    `json:"transactionHash"`
		TransactionIndex string    `json:"transactionIndex"`
		LogsBloom        string    `json:"logsBloom"`
		Logs             []logLite `json:"logs"`
		ContractAddress  string    `json:"contractAddress"`
	}

	var receipts []receiptLite
	if err := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &receipts); err != nil {
		return common.NewErrEndpointContentValidation(fmt.Errorf("invalid JSON result for receipts validation: %w", err), u)
	}

	if receipts == nil {
		receipts = []receiptLite{}
	}

	count := int64(len(receipts))

	// 1. Count Validations (nil means unset/don't check)
	if dirs.ReceiptsCountExact != nil {
		if count != *dirs.ReceiptsCountExact {
			return common.NewErrEndpointContentValidation(
				fmt.Errorf("receipts count mismatch: expected %d got %d", *dirs.ReceiptsCountExact, count),
				u,
			)
		}
	}
	if dirs.ReceiptsCountAtLeast != nil {
		if count < *dirs.ReceiptsCountAtLeast {
			return common.NewErrEndpointContentValidation(
				fmt.Errorf("receipts count insufficient: expected at least %d got %d", *dirs.ReceiptsCountAtLeast, count),
				u,
			)
		}
	}

	// 2. Expected Ground Truths
	expectedBlockHash := ""
	if dirs.ValidationExpectedBlockHash != "" {
		expectedBlockHash = strings.ToLower(strings.TrimPrefix(dirs.ValidationExpectedBlockHash, "0x"))
		for i, r := range receipts {
			gotHash := strings.ToLower(strings.TrimPrefix(r.BlockHash, "0x"))
			if gotHash != "" && gotHash != expectedBlockHash {
				return common.NewErrEndpointContentValidation(
					fmt.Errorf("receipts block hash mismatch at index %d: expected %s got %s", i, expectedBlockHash, gotHash),
					u,
				)
			}
		}
	}

	var expectedBlockNum *int64
	if dirs.ValidationExpectedBlockNumber != nil {
		expectedBlockNum = dirs.ValidationExpectedBlockNumber
		for i, r := range receipts {
			if r.BlockNumber == "" {
				continue
			}
			bn, err := common.HexToInt64(r.BlockNumber)
			if err != nil {
				return common.NewErrEndpointContentValidation(fmt.Errorf("invalid blockNumber hex at index %d: %w", i, err), u)
			}
			if bn != *expectedBlockNum {
				return common.NewErrEndpointContentValidation(
					fmt.Errorf("receipts block number mismatch at index %d: expected %d got %d", i, *expectedBlockNum, bn),
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

	// 5. Cross-validation with GroundTruthTransactions (library-mode only)
	if dirs.ValidateReceiptTransactionMatch && len(dirs.GroundTruthTransactions) > 0 {
		gtTxs := dirs.GroundTruthTransactions
		if int64(len(gtTxs)) != count {
			return common.NewErrEndpointContentValidation(
				fmt.Errorf("receipts count %d != ground truth transactions count %d", count, len(gtTxs)),
				u,
			)
		}
		for i, r := range receipts {
			gtTx := gtTxs[i]
			if gtTx == nil {
				continue
			}
			// Match transaction hash (gtTx.Hash is []byte, r.TransactionHash is hex string)
			rTxHashBytes, err := evm.HexToBytes(r.TransactionHash)
			if err != nil {
				return common.NewErrEndpointContentValidation(
					fmt.Errorf("receipt %d: invalid tx hash hex: %w", i, err),
					u,
				)
			}
			if !bytes.Equal(rTxHashBytes, gtTx.Hash) {
				return common.NewErrEndpointContentValidation(
					fmt.Errorf("receipt %d tx hash mismatch: expected %x got %x", i, gtTx.Hash, rTxHashBytes),
					u,
				)
			}

			// Contract creation consistency
			if dirs.ValidateContractCreation {
				gtHasTo := len(gtTx.To) > 0
				rHasContract := r.ContractAddress != ""
				if !gtHasTo {
					// Contract creation: receipt must have contractAddress
					if !rHasContract {
						return common.NewErrEndpointContentValidation(
							fmt.Errorf("receipt %d: contract creation tx but no contractAddress in receipt", i),
							u,
						)
					}
					// Validate contractAddress length
					contractBytes, err := evm.HexToBytes(r.ContractAddress)
					if err != nil {
						return common.NewErrEndpointContentValidation(
							fmt.Errorf("receipt %d: invalid contractAddress hex: %w", i, err),
							u,
						)
					}
					if len(contractBytes) != evm.AddressLength {
						return common.NewErrEndpointContentValidation(
							fmt.Errorf("receipt %d: contractAddress length invalid: %d", i, len(contractBytes)),
							u,
						)
					}
				} else {
					// Not contract creation: receipt should NOT have contractAddress
					if rHasContract {
						return common.NewErrEndpointContentValidation(
							fmt.Errorf("receipt %d: non-creation tx but has contractAddress", i),
							u,
						)
					}
				}
			}
		}
	}

	// 6. Deep Receipt Validation (Logs, Bloom)
	var expectedLogIndex uint64 = 0

	for i, r := range receipts {
		// ValidateLogsBloomEmptiness: consistency check between bloom and logs
		// - if bloom is non-zero, logs must exist
		// - if logs exist, bloom must not be zero
		if dirs.ValidateLogsBloomEmptiness {
			bloomIsZero := isZeroBloom(r.LogsBloom)
			hasLogs := len(r.Logs) > 0
			if !bloomIsZero && !hasLogs {
				return common.NewErrEndpointContentValidation(fmt.Errorf("non-zero receipt.logsBloom but 0 logs at receipt %d", i), u)
			}
			if bloomIsZero && hasLogs {
				return common.NewErrEndpointContentValidation(fmt.Errorf("zero receipt.logsBloom but %d logs at receipt %d", len(r.Logs), i), u)
			}
		}

		// Process logs
		for j, log := range r.Logs {
			// ValidateLogFields
			if dirs.ValidateLogFields {
				// Address length
				if log.Address != "" {
					addrBytes, err := evm.HexToBytes(log.Address)
					if err != nil {
						return common.NewErrEndpointContentValidation(fmt.Errorf("receipt %d log %d: invalid address hex: %w", i, j, err), u)
					}
					if len(addrBytes) != evm.AddressLength {
						return common.NewErrEndpointContentValidation(fmt.Errorf("receipt %d log %d: address length invalid: %d", i, j, len(addrBytes)), u)
					}
				}

				// Topic lengths and count
				if len(log.Topics) > evm.MaxTopics {
					return common.NewErrEndpointContentValidation(fmt.Errorf("receipt %d log %d: too many topics: %d", i, j, len(log.Topics)), u)
				}
				for k, topic := range log.Topics {
					topicBytes, err := evm.HexToBytes(topic)
					if err != nil {
						return common.NewErrEndpointContentValidation(fmt.Errorf("receipt %d log %d topic %d: invalid hex: %w", i, j, k, err), u)
					}
					if len(topicBytes) != evm.TopicLength {
						return common.NewErrEndpointContentValidation(fmt.Errorf("receipt %d log %d topic %d: length invalid: %d", i, j, k, len(topicBytes)), u)
					}
				}

				// Log block hash must match receipt block hash
				if log.BlockHash != "" && r.BlockHash != "" {
					logBH := strings.ToLower(strings.TrimPrefix(log.BlockHash, "0x"))
					rBH := strings.ToLower(strings.TrimPrefix(r.BlockHash, "0x"))
					if logBH != rBH {
						return common.NewErrEndpointContentValidation(fmt.Errorf("receipt %d log %d: block hash mismatch", i, j), u)
					}
				}

				// Log block number must match receipt block number
				if log.BlockNumber != "" && r.BlockNumber != "" {
					logBN, err := common.HexToInt64(log.BlockNumber)
					if err != nil {
						return common.NewErrEndpointContentValidation(fmt.Errorf("receipt %d log %d: invalid blockNumber hex: %w", i, j, err), u)
					}
					rBN, err := common.HexToInt64(r.BlockNumber)
					if err != nil {
						return common.NewErrEndpointContentValidation(fmt.Errorf("receipt %d: invalid blockNumber hex: %w", i, err), u)
					}
					if logBN != rBN {
						return common.NewErrEndpointContentValidation(fmt.Errorf("receipt %d log %d: block number mismatch", i, j), u)
					}
				}

				// Log tx hash must match receipt tx hash
				if log.TransactionHash != "" && r.TransactionHash != "" {
					logTH := strings.ToLower(strings.TrimPrefix(log.TransactionHash, "0x"))
					rTH := strings.ToLower(strings.TrimPrefix(r.TransactionHash, "0x"))
					if logTH != rTH {
						return common.NewErrEndpointContentValidation(fmt.Errorf("receipt %d log %d: tx hash mismatch", i, j), u)
					}
				}

				// Log tx index must match receipt tx index
				if log.TransactionIndex != "" && r.TransactionIndex != "" {
					logTI, err := common.HexToInt64(log.TransactionIndex)
					if err != nil {
						return common.NewErrEndpointContentValidation(fmt.Errorf("receipt %d log %d: invalid transactionIndex hex: %w", i, j, err), u)
					}
					rTI, err := common.HexToInt64(r.TransactionIndex)
					if err != nil {
						return common.NewErrEndpointContentValidation(fmt.Errorf("receipt %d: invalid transactionIndex hex: %w", i, err), u)
					}
					if logTI != rTI {
						return common.NewErrEndpointContentValidation(fmt.Errorf("receipt %d log %d: tx index mismatch", i, j), u)
					}
				}
			}

			// EnforceLogIndexStrictIncrements
			if dirs.EnforceLogIndexStrictIncrements {
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

		// ValidateLogsBloomMatch: recalculate bloom from logs and verify it matches
		if dirs.ValidateLogsBloomMatch && len(r.Logs) > 0 {
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

// isZeroBloom checks if a hex-encoded bloom filter is all zeros
func isZeroBloom(bloomHex string) bool {
	trimmed := strings.TrimLeft(strings.TrimPrefix(bloomHex, "0x"), "0")
	return trimmed == ""
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
