// Package evm includes Multicall3 helpers for aggregating eth_call batches.
// Multicall3 aggregate3((address,bool,bytes)[]) expects ABI-encoded calls and
// returns a dynamic array of (bool success, bytes returnData) with offsets
// relative to the array head. This file encodes calldata and decodes results.
package evm

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"golang.org/x/crypto/sha3"
)

// multicall3RequestCounter provides unique IDs for multicall3 requests to prevent collisions
var multicall3RequestCounter uint64

const multicall3Address = "0xcA11bde05977b3631167028862bE2a173976CA11"

var ErrMulticall3BatchNotEligible = errors.New("multicall3 batch not eligible")

// safeUint64ToInt converts uint64 to int with overflow protection.
// Returns an error if the value would overflow on the current platform.
func safeUint64ToInt(v uint64) (int, error) {
	if v > uint64(math.MaxInt) {
		return 0, fmt.Errorf("integer overflow: %d exceeds max int", v)
	}
	return int(v), nil
}

const (
	abiWordSize              = 32
	aggregate3ElementHeadLen = 3 * abiWordSize // address + allowFailure + data offset
	evmAddressLength         = 20
)

type Multicall3Call struct {
	Request  *common.NormalizedRequest
	Target   []byte
	CallData []byte
}

type Multicall3Result struct {
	Success    bool
	ReturnData []byte
}

func NewMulticall3Call(req *common.NormalizedRequest, targetHex, dataHex string) (Multicall3Call, error) {
	if req == nil {
		return Multicall3Call{}, ErrMulticall3BatchNotEligible
	}
	targetBytes, err := common.HexToBytes(targetHex)
	if err != nil || len(targetBytes) != evmAddressLength {
		return Multicall3Call{}, ErrMulticall3BatchNotEligible
	}
	callData, err := common.HexToBytes(dataHex)
	if err != nil {
		return Multicall3Call{}, ErrMulticall3BatchNotEligible
	}
	return Multicall3Call{
		Request:  req,
		Target:   targetBytes,
		CallData: callData,
	}, nil
}

func NormalizeBlockParam(param interface{}) (string, error) {
	if param == nil {
		return "latest", nil
	}

	blockNumberStr, blockHash, err := util.ParseBlockParameter(param)
	if err != nil {
		return "", err
	}
	if blockHash != nil {
		return fmt.Sprintf("0x%x", blockHash), nil
	}
	if blockNumberStr == "" {
		return "", errors.New("block parameter is empty")
	}
	if strings.HasPrefix(blockNumberStr, "0x") {
		bn, err := common.HexToInt64(blockNumberStr)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d", bn), nil
	}
	return blockNumberStr, nil
}

func BuildMulticall3Request(requests []*common.NormalizedRequest, blockParam interface{}) (*common.NormalizedRequest, []Multicall3Call, error) {
	if len(requests) < 1 {
		return nil, nil, ErrMulticall3BatchNotEligible
	}

	if blockParam == nil {
		blockParam = "latest"
	}

	calls := make([]Multicall3Call, 0, len(requests))
	for _, req := range requests {
		if req == nil {
			return nil, nil, ErrMulticall3BatchNotEligible
		}

		jrq, err := req.JsonRpcRequest()
		if err != nil {
			return nil, nil, err
		}

		jrq.RLock()
		method := jrq.Method
		params := jrq.Params
		jrq.RUnlock()

		if !strings.EqualFold(method, "eth_call") {
			return nil, nil, ErrMulticall3BatchNotEligible
		}
		if len(params) < 1 || len(params) > 2 {
			return nil, nil, ErrMulticall3BatchNotEligible
		}

		callObj, ok := params[0].(map[string]interface{})
		if !ok {
			return nil, nil, ErrMulticall3BatchNotEligible
		}

		targetHex, ok := callObj["to"].(string)
		if !ok || targetHex == "" {
			return nil, nil, ErrMulticall3BatchNotEligible
		}

		dataHex := "0x"
		if dataValue, ok := callObj["data"]; ok {
			dataStr, ok := dataValue.(string)
			if !ok {
				return nil, nil, ErrMulticall3BatchNotEligible
			}
			dataHex = dataStr
		} else if inputValue, ok := callObj["input"]; ok {
			inputStr, ok := inputValue.(string)
			if !ok {
				return nil, nil, ErrMulticall3BatchNotEligible
			}
			dataHex = inputStr
		}

		for key := range callObj {
			switch key {
			case "to", "data", "input":
				continue
			default:
				return nil, nil, ErrMulticall3BatchNotEligible
			}
		}

		call, err := NewMulticall3Call(req, targetHex, dataHex)
		if err != nil {
			return nil, nil, err
		}
		calls = append(calls, call)
	}

	encodedCalls, err := encodeAggregate3Calls(calls)
	if err != nil {
		return nil, nil, err
	}

	callObj := map[string]interface{}{
		"to":   multicall3Address,
		"data": "0x" + hex.EncodeToString(encodedCalls),
	}

	jrq := common.NewJsonRpcRequest("eth_call", []interface{}{callObj, blockParam})
	// Use atomic counter combined with timestamp for guaranteed unique IDs
	counter := atomic.AddUint64(&multicall3RequestCounter, 1)
	jrq.ID = fmt.Sprintf("multicall3-%d-%d", time.Now().UnixNano(), counter)

	nrq := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	nrq.CopyHttpContextFrom(requests[0])
	if dirs := requests[0].Directives(); dirs != nil {
		nrq.SetDirectives(dirs.Clone())
	}

	return nrq, calls, nil
}

func DecodeMulticall3Aggregate3Result(data []byte) ([]Multicall3Result, error) {
	if len(data) < 32 {
		return nil, errors.New("multicall3 result too short")
	}

	offset, err := readUint256(data[:32])
	if err != nil {
		return nil, err
	}
	base, err := safeUint64ToInt(offset)
	if err != nil {
		return nil, fmt.Errorf("multicall3 result offset overflow: %w", err)
	}
	if base < 0 || base+32 > len(data) {
		return nil, errors.New("multicall3 result offset out of bounds")
	}

	count, err := readUint256(data[base : base+32])
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return []Multicall3Result{}, nil
	}

	countInt, err := safeUint64ToInt(count)
	if err != nil {
		return nil, fmt.Errorf("multicall3 result count overflow: %w", err)
	}

	offsetsStart := base + 32
	// Check bounds before multiplication to prevent integer overflow
	// Each element needs 32 bytes in the offset table
	maxElements := (len(data) - offsetsStart) / 32
	if countInt > maxElements {
		return nil, errors.New("multicall3 result count exceeds available data")
	}
	offsetsEnd := offsetsStart + countInt*32
	if offsetsEnd > len(data) {
		return nil, errors.New("multicall3 result offsets out of bounds")
	}

	results := make([]Multicall3Result, countInt)
	for i := 0; i < countInt; i++ {
		offsetStart := offsetsStart + i*32
		offsetVal, err := readUint256(data[offsetStart : offsetStart+32])
		if err != nil {
			return nil, err
		}
		// Element offsets are relative to where the offset table starts (after length word)
		offsetValInt, err := safeUint64ToInt(offsetVal)
		if err != nil {
			return nil, fmt.Errorf("multicall3 result element offset overflow: %w", err)
		}
		elemStart := offsetsStart + offsetValInt
		if elemStart < offsetsStart || elemStart+64 > len(data) {
			return nil, errors.New("multicall3 result element out of bounds")
		}

		success, err := readBool(data[elemStart : elemStart+32])
		if err != nil {
			return nil, err
		}

		dataOffset, err := readUint256(data[elemStart+32 : elemStart+64])
		if err != nil {
			return nil, err
		}
		dataOffsetInt, err := safeUint64ToInt(dataOffset)
		if err != nil {
			return nil, fmt.Errorf("multicall3 result data offset overflow: %w", err)
		}
		bytesStart := elemStart + dataOffsetInt
		if bytesStart < elemStart || bytesStart+32 > len(data) {
			return nil, errors.New("multicall3 result bytes offset out of bounds")
		}

		dataLen, err := readUint256(data[bytesStart : bytesStart+32])
		if err != nil {
			return nil, err
		}
		dataLenInt, err := safeUint64ToInt(dataLen)
		if err != nil {
			return nil, fmt.Errorf("multicall3 result data length overflow: %w", err)
		}
		dataStart := bytesStart + 32
		dataEnd := dataStart + dataLenInt
		if dataStart < bytesStart || dataEnd > len(data) {
			return nil, errors.New("multicall3 result bytes length out of bounds")
		}

		returnData := append([]byte(nil), data[dataStart:dataEnd]...)
		results[i] = Multicall3Result{
			Success:    success,
			ReturnData: returnData,
		}
	}

	return results, nil
}

// ShouldFallbackMulticall3 determines if an error should trigger fallback to individual requests.
// Returns true only when the multicall3 contract is unavailable (unsupported endpoint) or when
// there are specific execution errors indicating the contract doesn't exist on this network.
// Other execution errors (like reverts) should NOT trigger fallback as they would also fail individually.
func ShouldFallbackMulticall3(err error) bool {
	if err == nil {
		return false
	}
	// Always fallback if endpoint is unsupported (e.g., network doesn't support eth_call)
	if common.HasErrorCode(err, common.ErrCodeEndpointUnsupported) {
		return true
	}
	// For execution errors, only fallback if it indicates multicall3 contract unavailability
	if common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
		errStr := strings.ToLower(err.Error())
		// Check for indicators that the multicall3 contract doesn't exist.
		// Different providers use different error messages, so we match multiple patterns.
		// NOTE: We intentionally do NOT include "execution reverted" as that pattern is too
		// broad and would match legitimate contract reverts. Legitimate reverts should NOT
		// trigger fallback - they would also revert when called individually.
		contractUnavailablePatterns := []string{
			"contract not found",
			"no code at address",
			"code is empty",
			"not a contract",
			"invalid opcode",    // can indicate missing contract
			"missing trie node", // pre-deployment block query
			"does not exist",
			"account not found",
		}
		for _, pattern := range contractUnavailablePatterns {
			if strings.Contains(errStr, pattern) {
				return true
			}
		}
		// Other execution errors (like authentication, rate limits, etc.) should not fallback
		return false
	}
	return false
}

func encodeAggregate3Calls(calls []Multicall3Call) ([]byte, error) {
	arrayData, err := encodeAggregate3Array(calls)
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, 4+abiWordSize+len(arrayData))
	out = append(out, multicall3Aggregate3Selector...)
	out = append(out, encodeUint64(abiWordSize)...)
	out = append(out, arrayData...)
	return out, nil
}

func encodeAggregate3Array(calls []Multicall3Call) ([]byte, error) {
	// ABI array: length (1 word) + offsets (1 word each) + element data.
	// Offsets are relative to start of array data (right after length word),
	// so the first element starts at offset = N*32 (after the N offset words).
	offsetTableSize := abiWordSize * len(calls)
	elements := make([][]byte, len(calls))
	offsets := make([]uint64, len(calls))
	// offsetTableSize is derived from len(calls) which is bounded by int,
	// so this conversion to uint64 is safe (always non-negative).
	cur := uint64(offsetTableSize) // #nosec G115

	for i, call := range calls {
		elem := encodeAggregate3Element(call)
		elements[i] = elem
		offsets[i] = cur
		cur += uint64(len(elem))
	}

	// cur accumulates sizes of in-memory slices, so it must fit in int.
	// Add explicit check for safety.
	capacity, err := safeUint64ToInt(cur)
	if err != nil {
		return nil, fmt.Errorf("multicall3 encoded data too large: %w", err)
	}
	out := make([]byte, 0, capacity)
	out = append(out, encodeUint64(uint64(len(calls)))...)
	for _, off := range offsets {
		out = append(out, encodeUint64(off)...)
	}
	for _, elem := range elements {
		out = append(out, elem...)
	}
	return out, nil
}

func encodeAggregate3Element(call Multicall3Call) []byte {
	head := make([]byte, 0, aggregate3ElementHeadLen)
	head = append(head, encodeAddress(call.Target)...)
	head = append(head, encodeBool(true)...)
	head = append(head, encodeUint64(aggregate3ElementHeadLen)...)
	tail := encodeBytes(call.CallData)
	return append(head, tail...)
}

func encodeAddress(addr []byte) []byte {
	out := make([]byte, 32)
	copy(out[32-len(addr):], addr)
	return out
}

func encodeBool(value bool) []byte {
	out := make([]byte, 32)
	if value {
		out[31] = 1
	}
	return out
}

func encodeUint64(value uint64) []byte {
	out := make([]byte, 32)
	binary.BigEndian.PutUint64(out[24:], value)
	return out
}

func encodeBytes(data []byte) []byte {
	out := make([]byte, 0, abiWordSize+len(data)+abiWordSize)
	out = append(out, encodeUint64(uint64(len(data)))...)
	out = append(out, data...)
	pad := (abiWordSize - (len(data) % abiWordSize)) % abiWordSize
	if pad > 0 {
		out = append(out, make([]byte, pad)...)
	}
	return out
}

func readUint256(data []byte) (uint64, error) {
	if len(data) != 32 {
		return 0, errors.New("invalid uint256 length")
	}
	val := new(big.Int).SetBytes(data)
	if !val.IsUint64() {
		return 0, errors.New("uint256 overflows uint64")
	}
	return val.Uint64(), nil
}

func readBool(data []byte) (bool, error) {
	val, err := readUint256(data)
	if err != nil {
		return false, err
	}
	return val != 0, nil
}

var multicall3Aggregate3Selector = func() []byte {
	hasher := sha3.NewLegacyKeccak256()
	hasher.Write([]byte("aggregate3((address,bool,bytes)[])"))
	sum := hasher.Sum(nil)
	return sum[:4]
}()
