package evm

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
)

// DirectivesKeyVersion should be bumped when the set of directives
// included in the key changes. This prevents cross-node key mismatches.
const DirectivesKeyVersion = 1

// BatchingKey uniquely identifies a batch for grouping eth_call requests.
type BatchingKey struct {
	ProjectId     string
	NetworkId     string
	BlockRef      string
	DirectivesKey string
	UserId        string // empty if cross-user batching is allowed
}

func (k BatchingKey) String() string {
	return fmt.Sprintf("%s|%s|%s|%s|%s", k.ProjectId, k.NetworkId, k.BlockRef, k.DirectivesKey, k.UserId)
}

// DeriveDirectivesKey creates a stable, versioned key from relevant directives.
// Only includes directives that affect batching behavior.
func DeriveDirectivesKey(dirs *common.RequestDirectives) string {
	if dirs == nil {
		return fmt.Sprintf("v%d:", DirectivesKeyVersion)
	}

	parts := make([]string, 0, 5)
	if dirs.UseUpstream != "" {
		parts = append(parts, fmt.Sprintf("use-upstream=%s", dirs.UseUpstream))
	}
	if dirs.SkipInterpolation {
		parts = append(parts, "skip-interpolation=true")
	}
	if dirs.RetryEmpty {
		parts = append(parts, "retry-empty=true")
	}
	if dirs.RetryPending {
		parts = append(parts, "retry-pending=true")
	}
	if dirs.SkipCacheRead {
		parts = append(parts, "skip-cache-read=true")
	}

	sort.Strings(parts)
	return fmt.Sprintf("v%d:%s", DirectivesKeyVersion, strings.Join(parts, ","))
}

// DeriveCallKey creates a unique key for deduplication within a batch.
// Uses the same derivation as cache keys for consistency.
func DeriveCallKey(req *common.NormalizedRequest) (string, error) {
	if req == nil {
		return "", fmt.Errorf("request is nil")
	}
	jrq, err := req.JsonRpcRequest()
	if err != nil {
		return "", err
	}

	jrq.RLock()
	method := jrq.Method
	params := jrq.Params
	jrq.RUnlock()

	// Use method + params as key (same as cache key derivation)
	paramsJSON, err := common.SonicCfg.Marshal(params)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%s", method, string(paramsJSON)), nil
}

// BatchEntry represents a request waiting in a batch.
type BatchEntry struct {
	Ctx       context.Context
	Request   *common.NormalizedRequest
	CallKey   string
	Target    []byte
	CallData  []byte
	ResultCh  chan BatchResult
	CreatedAt time.Time
	Deadline  time.Time
}

// BatchResult is the outcome delivered to a waiting request.
type BatchResult struct {
	Response *common.NormalizedResponse
	Error    error
}

// Batch holds pending requests for a single batching key.
type Batch struct {
	Key       BatchingKey
	Entries   []*BatchEntry
	CallKeys  map[string][]*BatchEntry // for deduplication
	FlushTime time.Time
	Flushing  bool
	mu        sync.Mutex
}

func NewBatch(key BatchingKey, flushTime time.Time) *Batch {
	return &Batch{
		Key:       key,
		Entries:   make([]*BatchEntry, 0, 16),
		CallKeys:  make(map[string][]*BatchEntry),
		FlushTime: flushTime,
	}
}

// ineligibleCallFields are fields that make an eth_call ineligible for batching.
// Multicall3 aggregate3 only supports target + calldata, not gas/value/etc.
var ineligibleCallFields = []string{
	"from", "gas", "gasPrice", "maxFeePerGas", "maxPriorityFeePerGas", "value",
}

// allowedBlockTags are block tags that can be batched by default.
var allowedBlockTags = map[string]bool{
	"latest":    true,
	"finalized": true,
	"safe":      true,
	"earliest":  true,
}

// IsEligibleForBatching checks if a request can be batched via Multicall3.
// Returns (eligible, reason) where reason explains why not eligible.
func IsEligibleForBatching(req *common.NormalizedRequest, cfg *common.Multicall3AggregationConfig) (bool, string) {
	if req == nil {
		return false, "request is nil"
	}
	if cfg == nil || !cfg.Enabled {
		return false, "batching disabled"
	}

	jrq, err := req.JsonRpcRequest()
	if err != nil {
		return false, fmt.Sprintf("json-rpc error: %v", err)
	}

	jrq.RLock()
	method := strings.ToLower(jrq.Method)
	params := jrq.Params
	jrq.RUnlock()

	// Must be eth_call
	if method != "eth_call" {
		return false, "not eth_call"
	}

	// Must have 1-3 params (call object, optional block, optional state override)
	if len(params) < 1 {
		return false, fmt.Sprintf("invalid param count: %d", len(params))
	}

	// Check for state override (3rd param) - not supported with multicall3
	if len(params) > 2 {
		return false, "has state override"
	}

	// Parse call object
	callObj, ok := params[0].(map[string]interface{})
	if !ok {
		return false, "invalid call object type"
	}

	// Must have 'to' address
	toVal, hasTo := callObj["to"]
	if !hasTo {
		return false, "missing to address"
	}
	toStr, ok := toVal.(string)
	if !ok || toStr == "" {
		return false, "invalid to address"
	}

	// Check for ineligible fields
	for _, field := range ineligibleCallFields {
		if _, has := callObj[field]; has {
			return false, fmt.Sprintf("has %s field", field)
		}
	}

	// Recursion guard: don't batch calls to multicall3 contract
	if strings.EqualFold(toStr, multicall3Address) {
		return false, "already multicall"
	}

	// Check block tag
	blockTag := "latest"
	if len(params) >= 2 && params[1] != nil {
		normalized, err := NormalizeBlockParam(params[1])
		if err != nil {
			return false, fmt.Sprintf("invalid block param: %v", err)
		}
		blockTag = strings.ToLower(normalized)
	}

	// Check if pending tag is allowed
	if blockTag == "pending" && !cfg.AllowPendingTagBatching {
		return false, "pending tag not allowed"
	}

	// Check if block tag is eligible for batching:
	// - Known named tags (latest, finalized, safe, earliest) are always allowed
	// - pending is allowed if AllowPendingTagBatching is true (checked above)
	// - Numeric block numbers (decimal strings after normalization) are allowed
	// - Block hashes (0x + 64 hex chars) are allowed
	if !isBlockRefEligibleForBatching(blockTag) {
		return false, fmt.Sprintf("block tag not allowed: %s", blockTag)
	}

	return true, ""
}

// isBlockRefEligibleForBatching checks if a normalized block reference is eligible for batching.
// It allows: known block tags, numeric block numbers, and block hashes.
func isBlockRefEligibleForBatching(blockRef string) bool {
	// Check known block tags (including pending, which is handled separately)
	if allowedBlockTags[blockRef] || blockRef == "pending" {
		return true
	}

	// Check if it's a numeric block number (decimal string after normalization)
	if len(blockRef) > 0 && blockRef[0] >= '0' && blockRef[0] <= '9' {
		return true
	}

	// Check if it's a block hash (0x + 64 hex chars = 66 chars total for 32 bytes)
	if strings.HasPrefix(blockRef, "0x") && len(blockRef) == 66 {
		return true
	}

	return false
}

// ExtractCallInfo extracts target and calldata from an eligible eth_call request.
func ExtractCallInfo(req *common.NormalizedRequest) (target []byte, callData []byte, blockRef string, err error) {
	jrq, err := req.JsonRpcRequest()
	if err != nil {
		return nil, nil, "", err
	}

	jrq.RLock()
	params := jrq.Params
	jrq.RUnlock()

	callObj := params[0].(map[string]interface{})
	toStr := callObj["to"].(string)

	target, err = common.HexToBytes(toStr)
	if err != nil {
		return nil, nil, "", err
	}

	dataHex := "0x"
	if dataVal, ok := callObj["data"]; ok {
		dataHex = dataVal.(string)
	} else if inputVal, ok := callObj["input"]; ok {
		dataHex = inputVal.(string)
	}

	callData, err = common.HexToBytes(dataHex)
	if err != nil {
		return nil, nil, "", err
	}

	blockRef = "latest"
	if len(params) >= 2 && params[1] != nil {
		blockRef, err = NormalizeBlockParam(params[1])
		if err != nil {
			return nil, nil, "", err
		}
	}

	return target, callData, blockRef, nil
}
