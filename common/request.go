package common

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bytedance/sonic"
	"github.com/rs/zerolog"
)

const (
	CompositeTypeNone               = "none"
	CompositeTypeLogsSplitOnError   = "logs-split-on-error"
	CompositeTypeLogsSplitProactive = "logs-split-proactive"
	CompositeTypeMulticall3         = "multicall3"
)

const RequestContextKey ContextKey = "rq"
const UpstreamsContextKey ContextKey = "ups"

type directiveKeyNames struct {
	header string
	query  string
}

const (
	headerDirectiveRetryEmpty                 = "X-ERPC-Retry-Empty"
	headerDirectiveRetryPending               = "X-ERPC-Retry-Pending"
	headerDirectiveSkipCacheRead              = "X-ERPC-Skip-Cache-Read"
	headerDirectiveUseUpstream                = "X-ERPC-Use-Upstream"
	headerDirectiveSkipInterpolation          = "X-ERPC-Skip-Interpolation"
	headerDirectiveEnforceHighestBlock        = "X-ERPC-Enforce-Highest-Block"
	headerDirectiveEnforceGetLogsRange        = "X-ERPC-Enforce-GetLogs-Range"
	headerDirectiveEnforceNonNullTaggedBlocks = "X-ERPC-Enforce-Non-Null-Tagged-Blocks"
	headerDirectiveEnforceLogIndexStrict      = "X-ERPC-Enforce-Log-Index-Strict-Increments"
	headerDirectiveValidateLogsBloomEmpty     = "X-ERPC-Validate-Logs-Bloom-Emptiness"
	headerDirectiveValidateLogsBloomMatch     = "X-ERPC-Validate-Logs-Bloom-Match"
	headerDirectiveValidateTxHashUniq         = "X-ERPC-Validate-Tx-Hash-Uniqueness"
	headerDirectiveValidateTxIndex            = "X-ERPC-Validate-Transaction-Index"
	headerDirectiveReceiptsCountExact         = "X-ERPC-Receipts-Count-Exact"
	headerDirectiveReceiptsCountAtLeast       = "X-ERPC-Receipts-Count-At-Least"
	headerDirectiveValidationBlockHash        = "X-ERPC-Validation-Expected-Block-Hash"
	headerDirectiveValidationBlockNumber      = "X-ERPC-Validation-Expected-Block-Number"
	headerDirectiveValidateHeaderFieldLengths = "X-ERPC-Validate-Header-Field-Lengths"
	headerDirectiveValidateTxFields           = "X-ERPC-Validate-Transaction-Fields"
	headerDirectiveValidateTxBlockInfo        = "X-ERPC-Validate-Transaction-Block-Info"
	headerDirectiveValidateLogFields          = "X-ERPC-Validate-Log-Fields"
)

const (
	queryDirectiveRetryEmpty                 = "retry-empty"
	queryDirectiveRetryPending               = "retry-pending"
	queryDirectiveSkipCacheRead              = "skip-cache-read"
	queryDirectiveUseUpstream                = "use-upstream"
	queryDirectiveSkipInterpolation          = "skip-interpolation"
	queryDirectiveEnforceHighestBlock        = "enforce-highest-block"
	queryDirectiveEnforceGetLogsRange        = "enforce-getlogs-range"
	queryDirectiveEnforceNonNullTaggedBlocks = "enforce-non-null-tagged-blocks"
	queryDirectiveEnforceLogIndexStrict      = "enforce-log-index-strict-increments"
	queryDirectiveValidateLogsBloomEmpty     = "validate-logs-bloom-emptiness"
	queryDirectiveValidateLogsBloomMatch     = "validate-logs-bloom-match"
	queryDirectiveValidateTxHashUniq         = "validate-tx-hash-uniqueness"
	queryDirectiveValidateTxIndex            = "validate-transaction-index"
	queryDirectiveReceiptsCountExact         = "receipts-count-exact"
	queryDirectiveReceiptsCountAtLeast       = "receipts-count-at-least"
	queryDirectiveValidationBlockHash        = "validation-expected-block-hash"
	queryDirectiveValidationBlockNumber      = "validation-expected-block-number"
	queryDirectiveValidateHeaderFieldLengths = "validate-header-field-lengths"
	queryDirectiveValidateTxFields           = "validate-transaction-fields"
	queryDirectiveValidateTxBlockInfo        = "validate-transaction-block-info"
	queryDirectiveValidateLogFields          = "validate-log-fields"
)

var directiveKeyRegistry = []directiveKeyNames{
	{header: headerDirectiveRetryEmpty, query: queryDirectiveRetryEmpty},
	{header: headerDirectiveRetryPending, query: queryDirectiveRetryPending},
	{header: headerDirectiveSkipCacheRead, query: queryDirectiveSkipCacheRead},
	{header: headerDirectiveUseUpstream, query: queryDirectiveUseUpstream},
	{header: headerDirectiveSkipInterpolation, query: queryDirectiveSkipInterpolation},
	{header: headerDirectiveEnforceHighestBlock, query: queryDirectiveEnforceHighestBlock},
	{header: headerDirectiveEnforceGetLogsRange, query: queryDirectiveEnforceGetLogsRange},
	{header: headerDirectiveEnforceNonNullTaggedBlocks, query: queryDirectiveEnforceNonNullTaggedBlocks},
	{header: headerDirectiveEnforceLogIndexStrict, query: queryDirectiveEnforceLogIndexStrict},
	{header: headerDirectiveValidateLogsBloomEmpty, query: queryDirectiveValidateLogsBloomEmpty},
	{header: headerDirectiveValidateLogsBloomMatch, query: queryDirectiveValidateLogsBloomMatch},
	{header: headerDirectiveValidateTxHashUniq, query: queryDirectiveValidateTxHashUniq},
	{header: headerDirectiveValidateTxIndex, query: queryDirectiveValidateTxIndex},
	{header: headerDirectiveReceiptsCountExact, query: queryDirectiveReceiptsCountExact},
	{header: headerDirectiveReceiptsCountAtLeast, query: queryDirectiveReceiptsCountAtLeast},
	{header: headerDirectiveValidationBlockHash, query: queryDirectiveValidationBlockHash},
	{header: headerDirectiveValidationBlockNumber, query: queryDirectiveValidationBlockNumber},
	{header: headerDirectiveValidateHeaderFieldLengths, query: queryDirectiveValidateHeaderFieldLengths},
	{header: headerDirectiveValidateTxFields, query: queryDirectiveValidateTxFields},
	{header: headerDirectiveValidateTxBlockInfo, query: queryDirectiveValidateTxBlockInfo},
	{header: headerDirectiveValidateLogFields, query: queryDirectiveValidateLogFields},
}

type RequestDirectives struct {
	// Instruct the proxy to retry if response from the upstream appears to be empty
	// indicating missing or non-synced data (empty array for logs, null for block, null for tx receipt, etc).
	// By default this value is "false" – meaning empty responses will NOT be retried unless explicitly
	// enabled via project/network directive defaults or incoming HTTP header / query parameter.
	RetryEmpty bool `json:"retryEmpty"`

	// Instruct the proxy to retry if response from the upstream appears to be a pending tx,
	// for example when blockNumber/blockHash are still null.
	// This value is "true" by default, to give a chance to receive the full TX.
	// If you intentionally require to get pending TX data immediately without waiting,
	// you can set this value to "false" via Headers.
	RetryPending bool `json:"retryPending"`

	// Instruct the proxy to skip cache reads for example to force freshness,
	// or override some cache corruption.
	SkipCacheRead bool `json:"skipCacheRead"`

	// Instruct the proxy to forward the request to a specific upstream(s) only.
	// Value can use "*" star char as a wildcard to target multiple upstreams.
	// For example "alchemy" or "my-own-*", etc.
	UseUpstream string `json:"useUpstream,omitempty"`

	// Instruct the proxy to bypass method exclusion checks.
	ByPassMethodExclusion bool `json:"-"`

	// Instruct the normalization layer to avoid mutating JSON-RPC params for block tag interpolation.
	// When true, the system will still compute and cache block references (for finality/metrics),
	// but will NOT replace tags like "latest"/"finalized" with hex numbers in outbound requests.
	SkipInterpolation bool `json:"skipInterpolation"`

	// Validation: Block Integrity
	EnforceHighestBlock        bool `json:"enforceHighestBlock,omitempty"`
	EnforceGetLogsBlockRange   bool `json:"enforceGetLogsBlockRange,omitempty"`
	EnforceNonNullTaggedBlocks bool `json:"enforceNonNullTaggedBlocks,omitempty"`

	// Validation: Header Field Lengths (only via config/library, not HTTP headers)
	ValidateHeaderFieldLengths bool `json:"validateHeaderFieldLengths,omitempty"`

	// Validation: Transactions (for eth_getBlockByNumber/Hash with full txs)
	ValidateTransactionFields    bool `json:"validateTransactionFields,omitempty"`
	ValidateTransactionBlockInfo bool `json:"validateTransactionBlockInfo,omitempty"`

	// Validation: Receipts & Logs
	EnforceLogIndexStrictIncrements bool `json:"enforceLogIndexStrictIncrements,omitempty"`
	ValidateTxHashUniqueness        bool `json:"validateTxHashUniqueness,omitempty"`
	ValidateTransactionIndex        bool `json:"validateTransactionIndex,omitempty"`
	ValidateLogFields               bool `json:"validateLogFields,omitempty"`

	// ValidateLogsBloomEmptiness: if logs exist, bloom must not be zero; if bloom is non-zero, logs must exist
	ValidateLogsBloomEmptiness bool `json:"validateLogsBloomEmptiness,omitempty"`
	// ValidateLogsBloomMatch: recalculate bloom from logs and verify it matches the provided bloom
	// For methods without logs in response (e.g., eth_getBlockByNumber), use GroundTruthLogs
	ValidateLogsBloomMatch bool `json:"validateLogsBloomMatch,omitempty"`

	// Validation: Receipt-to-Transaction Cross-Validation
	// When true, validates that receipt[i].transactionHash == tx[i].hash (requires GroundTruthTransactions)
	ValidateReceiptTransactionMatch bool `json:"validateReceiptTransactionMatch,omitempty"`
	// When true, validates contract creation consistency (no tx.to → receipt must have contractAddress)
	ValidateContractCreation bool `json:"validateContractCreation,omitempty"`

	// Validation: numeric checks (nil means unset/don't check)
	ReceiptsCountExact   *int64 `json:"receiptsCountExact,omitempty"`
	ReceiptsCountAtLeast *int64 `json:"receiptsCountAtLeast,omitempty"`

	// Validation: Expected Ground Truths (nil means unset/don't check)
	ValidationExpectedBlockHash   string `json:"validationExpectedBlockHash,omitempty"`
	ValidationExpectedBlockNumber *int64 `json:"validationExpectedBlockNumber,omitempty"`

	// Ground Truth Data (library-mode only, NOT settable via HTTP headers/query params)
	// These fields allow library users to pass full objects for cross-entity validation.
	//
	// GroundTruthTransactions: expected transactions for receipt validation (uses manifesto evm.Transaction)
	// When set with ValidateReceiptTransactionMatch, receipts are validated against these transactions
	GroundTruthTransactions []*GroundTruthTransaction `json:"-"`
	//
	// GroundTruthLogs: expected logs for bloom validation when logs aren't in the response
	// Used with ValidateLogsBloomMatch for methods like eth_getBlockByNumber that don't return logs
	GroundTruthLogs []*GroundTruthLog `json:"-"`
}

// GroundTruthTransaction represents expected transaction data for cross-validation.
// Uses manifesto-compatible structure for library-mode validation.
type GroundTruthTransaction struct {
	// Hash is the transaction hash (required for matching)
	Hash []byte
	// To is the recipient address (nil/empty for contract creation)
	To []byte
	// TransactionIndex is the expected position in the block
	TransactionIndex *uint32
}

// GroundTruthLog represents expected log data for bloom validation.
// Uses manifesto-compatible structure for library-mode validation.
type GroundTruthLog struct {
	// Address is the contract address that emitted the log
	Address []byte
	// Topics are the indexed event parameters
	Topics [][]byte
}

func (d *RequestDirectives) Clone() *RequestDirectives {
	if d == nil {
		return &RequestDirectives{}
	}
	cloned := &RequestDirectives{
		RetryEmpty:                      d.RetryEmpty,
		RetryPending:                    d.RetryPending,
		SkipCacheRead:                   d.SkipCacheRead,
		UseUpstream:                     d.UseUpstream,
		ByPassMethodExclusion:           d.ByPassMethodExclusion,
		SkipInterpolation:               d.SkipInterpolation,
		EnforceHighestBlock:             d.EnforceHighestBlock,
		EnforceGetLogsBlockRange:        d.EnforceGetLogsBlockRange,
		EnforceNonNullTaggedBlocks:      d.EnforceNonNullTaggedBlocks,
		ValidateHeaderFieldLengths:      d.ValidateHeaderFieldLengths,
		ValidateTransactionFields:       d.ValidateTransactionFields,
		ValidateTransactionBlockInfo:    d.ValidateTransactionBlockInfo,
		EnforceLogIndexStrictIncrements: d.EnforceLogIndexStrictIncrements,
		ValidateTxHashUniqueness:        d.ValidateTxHashUniqueness,
		ValidateTransactionIndex:        d.ValidateTransactionIndex,
		ValidateLogFields:               d.ValidateLogFields,
		ValidateLogsBloomEmptiness:      d.ValidateLogsBloomEmptiness,
		ValidateLogsBloomMatch:          d.ValidateLogsBloomMatch,
		ValidateReceiptTransactionMatch: d.ValidateReceiptTransactionMatch,
		ValidateContractCreation:        d.ValidateContractCreation,
		ValidationExpectedBlockHash:     d.ValidationExpectedBlockHash,
	}
	// Deep copy pointer fields
	if d.ReceiptsCountExact != nil {
		v := *d.ReceiptsCountExact
		cloned.ReceiptsCountExact = &v
	}
	if d.ReceiptsCountAtLeast != nil {
		v := *d.ReceiptsCountAtLeast
		cloned.ReceiptsCountAtLeast = &v
	}
	if d.ValidationExpectedBlockNumber != nil {
		v := *d.ValidationExpectedBlockNumber
		cloned.ValidationExpectedBlockNumber = &v
	}
	// Deep copy GroundTruthTransactions slice (shallow copy of byte slices is fine - they're immutable)
	if len(d.GroundTruthTransactions) > 0 {
		cloned.GroundTruthTransactions = make([]*GroundTruthTransaction, len(d.GroundTruthTransactions))
		for i, tx := range d.GroundTruthTransactions {
			if tx == nil {
				continue
			}
			clonedTx := &GroundTruthTransaction{
				Hash: tx.Hash,
				To:   tx.To,
			}
			if tx.TransactionIndex != nil {
				v := *tx.TransactionIndex
				clonedTx.TransactionIndex = &v
			}
			cloned.GroundTruthTransactions[i] = clonedTx
		}
	}
	// Deep copy GroundTruthLogs slice
	if len(d.GroundTruthLogs) > 0 {
		cloned.GroundTruthLogs = make([]*GroundTruthLog, len(d.GroundTruthLogs))
		for i, log := range d.GroundTruthLogs {
			if log == nil {
				continue
			}
			clonedLog := &GroundTruthLog{
				Address: log.Address,
			}
			if len(log.Topics) > 0 {
				clonedLog.Topics = make([][]byte, len(log.Topics))
				copy(clonedLog.Topics, log.Topics)
			}
			cloned.GroundTruthLogs[i] = clonedLog
		}
	}
	return cloned
}

type NormalizedRequest struct {
	sync.RWMutex

	network  Network
	cacheDal CacheDAL
	body     []byte

	method         string
	directives     *RequestDirectives
	jsonRpcRequest atomic.Pointer[JsonRpcRequest]

	// Upstream selection fields - protected by upstreamMutex
	upstreamMutex    sync.Mutex
	UpstreamIdx      uint32
	ErrorsByUpstream sync.Map
	EmptyResponses   sync.Map // TODO we can potentially remove this map entirely, only use is to report "wrong empty responses"

	// Fields for centralized upstream management
	upstreamList      []Upstream // Available upstreams for this request
	ConsumedUpstreams *sync.Map  // Tracks upstreams that provided valid responses

	lastValidResponse atomic.Pointer[NormalizedResponse]
	lastUpstream      atomic.Value
	evmBlockRef       atomic.Value
	evmBlockNumber    atomic.Value

	compositeType   atomic.Value // Type of composite request (e.g., "logs-split")
	parentRequestId atomic.Value // ID of the parent request (for sub-requests)

	finality atomic.Value // Cached finality state

	user atomic.Value

	// Cached agent information to avoid recalculation
	agentName atomic.Value // Cached agent name from User-Agent

	// Resolved client IP (set by HTTP ingress using trusted forwarders)
	clientIP atomic.Value
}

func NewNormalizedRequest(body []byte) *NormalizedRequest {
	nr := &NormalizedRequest{
		body:       body,
		directives: nil,
	}
	nr.compositeType.Store(CompositeTypeNone)
	return nr
}

func NewNormalizedRequestFromJsonRpcRequest(jsonRpcRequest *JsonRpcRequest) *NormalizedRequest {
	nr := &NormalizedRequest{
		directives: nil,
	}
	nr.jsonRpcRequest.Store(jsonRpcRequest)
	nr.compositeType.Store(CompositeTypeNone)
	return nr
}

func (r *NormalizedRequest) SetUser(user *User) {
	if r == nil || user == nil {
		return
	}
	r.user.Store(user)
}

func (r *NormalizedRequest) User() *User {
	if r == nil {
		return nil
	}
	if u, ok := r.user.Load().(*User); ok {
		return u
	}
	return nil
}

func (r *NormalizedRequest) SetLastUpstream(upstream Upstream) *NormalizedRequest {
	if r == nil || upstream == nil {
		return r
	}
	r.lastUpstream.Store(upstream)
	return r
}

func (r *NormalizedRequest) LastUpstream() Upstream {
	if r == nil {
		return nil
	}
	if lu := r.lastUpstream.Load(); lu != nil {
		return lu.(Upstream)
	}
	return nil
}

func (r *NormalizedRequest) SetLastValidResponse(ctx context.Context, nrs *NormalizedResponse) bool {
	if r == nil || nrs == nil {
		return false
	}
	prevLV := r.lastValidResponse.Load()
	prevIsEmpty := prevLV == nil || prevLV.IsObjectNull(ctx) || prevLV.IsResultEmptyish(ctx)
	newIsEmpty := nrs.IsObjectNull(ctx) || nrs.IsResultEmptyish(ctx)
	if prevIsEmpty || !newIsEmpty {
		r.lastValidResponse.Store(nrs)
		if !nrs.IsObjectNull(ctx) {
			ups := nrs.Upstream()
			if ups != nil {
				r.SetLastUpstream(ups)
			}
		}
		return true
	}
	return false
}

func (r *NormalizedRequest) LastValidResponse() *NormalizedResponse {
	if r == nil {
		return nil
	}
	return r.lastValidResponse.Load()
}

// ClearLastValidResponse clears the stored last valid response pointer.
func (r *NormalizedRequest) ClearLastValidResponse() {
	if r == nil {
		return
	}
	r.lastValidResponse.Store(nil)
}

func (r *NormalizedRequest) Network() Network {
	if r == nil {
		return nil
	}
	return r.network
}

func (r *NormalizedRequest) ID() interface{} {
	if r == nil {
		return nil
	}

	if jrr, _ := r.JsonRpcRequest(); jrr != nil {
		return jrr.ID
	}

	return nil
}

func (r *NormalizedRequest) NetworkId() string {
	if r == nil {
		// For certain requests such as internal eth_chainId requests, network might not be available yet.
		return "n/a"
	}
	r.RLock()
	defer r.RUnlock()
	if r.network == nil {
		return "n/a"
	}
	return r.network.Id()
}

// NetworkLabel returns a user-friendly label for the network suitable for metrics.
// It prefers the network alias when available, otherwise falls back to the canonical ID.
func (r *NormalizedRequest) NetworkLabel() string {
	if r == nil {
		return "n/a"
	}
	r.RLock()
	defer r.RUnlock()
	if r.network == nil {
		return "n/a"
	}
	lbl := r.network.Label()
	if lbl != "" {
		return lbl
	}
	return r.network.Id()
}

func (r *NormalizedRequest) SetNetwork(network Network) {
	r.Lock()
	defer r.Unlock()
	r.network = network
}

func (r *NormalizedRequest) SetCacheDal(cacheDal CacheDAL) {
	r.Lock()
	defer r.Unlock()
	r.cacheDal = cacheDal
}

func (r *NormalizedRequest) CacheDal() CacheDAL {
	if r == nil {
		return nil
	}
	r.RLock()
	defer r.RUnlock()
	return r.cacheDal
}

func (r *NormalizedRequest) SetDirectives(directives *RequestDirectives) {
	if r == nil {
		return
	}
	r.Lock()
	defer r.Unlock()
	r.directives = directives
}

// ApplyDirectiveDefaults applies the default directives from the network configuration.
func (r *NormalizedRequest) ApplyDirectiveDefaults(directiveDefaults *DirectiveDefaultsConfig) {
	if directiveDefaults == nil {
		return
	}
	r.Lock()
	defer r.Unlock()

	if r.directives == nil {
		r.directives = &RequestDirectives{}
	}

	if directiveDefaults.RetryEmpty != nil {
		r.directives.RetryEmpty = *directiveDefaults.RetryEmpty
	}
	if directiveDefaults.RetryPending != nil {
		r.directives.RetryPending = *directiveDefaults.RetryPending
	}
	if directiveDefaults.SkipCacheRead != nil {
		r.directives.SkipCacheRead = *directiveDefaults.SkipCacheRead
	}
	if directiveDefaults.UseUpstream != nil {
		r.directives.UseUpstream = *directiveDefaults.UseUpstream
	}
	if directiveDefaults.SkipInterpolation != nil {
		r.directives.SkipInterpolation = *directiveDefaults.SkipInterpolation
	}

	// Validation: Block Integrity
	if directiveDefaults.EnforceHighestBlock != nil {
		r.directives.EnforceHighestBlock = *directiveDefaults.EnforceHighestBlock
	}
	if directiveDefaults.EnforceGetLogsBlockRange != nil {
		r.directives.EnforceGetLogsBlockRange = *directiveDefaults.EnforceGetLogsBlockRange
	}
	if directiveDefaults.EnforceNonNullTaggedBlocks != nil {
		r.directives.EnforceNonNullTaggedBlocks = *directiveDefaults.EnforceNonNullTaggedBlocks
	}

	// Validation: Header Field Lengths
	if directiveDefaults.ValidateHeaderFieldLengths != nil {
		r.directives.ValidateHeaderFieldLengths = *directiveDefaults.ValidateHeaderFieldLengths
	}

	// Validation: Transactions
	if directiveDefaults.ValidateTransactionFields != nil {
		r.directives.ValidateTransactionFields = *directiveDefaults.ValidateTransactionFields
	}
	if directiveDefaults.ValidateTransactionBlockInfo != nil {
		r.directives.ValidateTransactionBlockInfo = *directiveDefaults.ValidateTransactionBlockInfo
	}

	// Validation: Receipts & Logs
	if directiveDefaults.EnforceLogIndexStrictIncrements != nil {
		r.directives.EnforceLogIndexStrictIncrements = *directiveDefaults.EnforceLogIndexStrictIncrements
	}
	if directiveDefaults.ValidateTxHashUniqueness != nil {
		r.directives.ValidateTxHashUniqueness = *directiveDefaults.ValidateTxHashUniqueness
	}
	if directiveDefaults.ValidateTransactionIndex != nil {
		r.directives.ValidateTransactionIndex = *directiveDefaults.ValidateTransactionIndex
	}
	if directiveDefaults.ValidateLogFields != nil {
		r.directives.ValidateLogFields = *directiveDefaults.ValidateLogFields
	}

	// Validation: Bloom Filter
	if directiveDefaults.ValidateLogsBloomEmptiness != nil {
		r.directives.ValidateLogsBloomEmptiness = *directiveDefaults.ValidateLogsBloomEmptiness
	}
	if directiveDefaults.ValidateLogsBloomMatch != nil {
		r.directives.ValidateLogsBloomMatch = *directiveDefaults.ValidateLogsBloomMatch
	}

	// Validation: Receipt-to-Transaction Cross-Validation
	if directiveDefaults.ValidateReceiptTransactionMatch != nil {
		r.directives.ValidateReceiptTransactionMatch = *directiveDefaults.ValidateReceiptTransactionMatch
	}
	if directiveDefaults.ValidateContractCreation != nil {
		r.directives.ValidateContractCreation = *directiveDefaults.ValidateContractCreation
	}

	// Validation: numeric checks (copy pointer values)
	if directiveDefaults.ReceiptsCountExact != nil {
		v := *directiveDefaults.ReceiptsCountExact
		r.directives.ReceiptsCountExact = &v
	}
	if directiveDefaults.ReceiptsCountAtLeast != nil {
		v := *directiveDefaults.ReceiptsCountAtLeast
		r.directives.ReceiptsCountAtLeast = &v
	}

	// Validation: Expected Ground Truths
	if directiveDefaults.ValidationExpectedBlockHash != nil {
		r.directives.ValidationExpectedBlockHash = *directiveDefaults.ValidationExpectedBlockHash
	}
	if directiveDefaults.ValidationExpectedBlockNumber != nil {
		v := *directiveDefaults.ValidationExpectedBlockNumber
		r.directives.ValidationExpectedBlockNumber = &v
	}
}

func hasDirectiveInHeaders(headers http.Header) bool {
	if headers == nil {
		return false
	}
	for _, keys := range directiveKeyRegistry {
		if keys.header != "" && headers.Get(keys.header) != "" {
			return true
		}
	}
	return false
}

func hasDirectiveInQueryParams(queryArgs url.Values) bool {
	if queryArgs == nil {
		return false
	}
	for _, keys := range directiveKeyRegistry {
		if keys.query != "" && queryArgs.Get(keys.query) != "" {
			return true
		}
	}
	return false
}

func (r *NormalizedRequest) EnrichFromHttp(headers http.Header, queryArgs url.Values, mode UserAgentTrackingMode) {
	hasDirectives := hasDirectiveInHeaders(headers) || hasDirectiveInQueryParams(queryArgs)

	// Extract user agent (always needed)
	userAgent := r.getUserAgent(headers, queryArgs)
	if userAgent != "" {
		if mode == UserAgentTrackingModeRaw {
			r.agentName.Store(userAgent)
		} else {
			r.agentName.Store(r.simplifyAgentName(userAgent))
		}
	}

	if !hasDirectives {
		return
	}

	r.Lock()
	defer r.Unlock()

	// Copy-On-Write: If we have existing (likely shared) directives, clone them.
	// If nil, create new.
	if r.directives == nil {
		r.directives = &RequestDirectives{}
	} else {
		r.directives = r.directives.Clone()
	}

	// Headers have precedence over directive defaults, but should only override when explicitly provided.
	if hv := headers.Get(headerDirectiveRetryEmpty); hv != "" {
		r.directives.RetryEmpty = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}
	if hv := headers.Get(headerDirectiveRetryPending); hv != "" {
		r.directives.RetryPending = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}
	if hv := headers.Get(headerDirectiveSkipCacheRead); hv != "" {
		r.directives.SkipCacheRead = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}
	if hv := headers.Get(headerDirectiveUseUpstream); hv != "" {
		r.directives.UseUpstream = hv
	}
	if hv := headers.Get(headerDirectiveSkipInterpolation); hv != "" {
		r.directives.SkipInterpolation = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}

	// Validation Headers
	if hv := headers.Get(headerDirectiveEnforceHighestBlock); hv != "" {
		r.directives.EnforceHighestBlock = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}
	if hv := headers.Get(headerDirectiveEnforceGetLogsRange); hv != "" {
		r.directives.EnforceGetLogsBlockRange = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}
	if hv := headers.Get(headerDirectiveEnforceNonNullTaggedBlocks); hv != "" {
		r.directives.EnforceNonNullTaggedBlocks = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}
	if hv := headers.Get(headerDirectiveEnforceLogIndexStrict); hv != "" {
		r.directives.EnforceLogIndexStrictIncrements = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}
	if hv := headers.Get(headerDirectiveValidateLogsBloomEmpty); hv != "" {
		r.directives.ValidateLogsBloomEmptiness = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}
	if hv := headers.Get(headerDirectiveValidateLogsBloomMatch); hv != "" {
		r.directives.ValidateLogsBloomMatch = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}
	if hv := headers.Get(headerDirectiveValidateTxHashUniq); hv != "" {
		r.directives.ValidateTxHashUniqueness = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}
	if hv := headers.Get(headerDirectiveValidateTxIndex); hv != "" {
		r.directives.ValidateTransactionIndex = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}

	if hv := headers.Get(headerDirectiveReceiptsCountExact); hv != "" {
		if v, err := strconv.ParseInt(hv, 10, 64); err == nil {
			r.directives.ReceiptsCountExact = &v
		}
	}
	if hv := headers.Get(headerDirectiveReceiptsCountAtLeast); hv != "" {
		if v, err := strconv.ParseInt(hv, 10, 64); err == nil {
			r.directives.ReceiptsCountAtLeast = &v
		}
	}
	if hv := headers.Get(headerDirectiveValidationBlockHash); hv != "" {
		r.directives.ValidationExpectedBlockHash = hv
	}
	if hv := headers.Get(headerDirectiveValidationBlockNumber); hv != "" {
		if v, err := strconv.ParseInt(hv, 10, 64); err == nil {
			r.directives.ValidationExpectedBlockNumber = &v
		}
	}
	if hv := headers.Get(headerDirectiveValidateHeaderFieldLengths); hv != "" {
		r.directives.ValidateHeaderFieldLengths = strings.ToLower(hv) == "true"
	}
	if hv := headers.Get(headerDirectiveValidateTxFields); hv != "" {
		r.directives.ValidateTransactionFields = strings.ToLower(hv) == "true"
	}
	if hv := headers.Get(headerDirectiveValidateTxBlockInfo); hv != "" {
		r.directives.ValidateTransactionBlockInfo = strings.ToLower(hv) == "true"
	}
	if hv := headers.Get(headerDirectiveValidateLogFields); hv != "" {
		r.directives.ValidateLogFields = strings.ToLower(hv) == "true"
	}

	// Query parameters come after headers so they can still override when explicitly present in URL.
	if useUpstream := queryArgs.Get(queryDirectiveUseUpstream); useUpstream != "" {
		r.directives.UseUpstream = strings.TrimSpace(useUpstream)
	}

	if retryEmpty := queryArgs.Get(queryDirectiveRetryEmpty); retryEmpty != "" {
		r.directives.RetryEmpty = strings.ToLower(strings.TrimSpace(retryEmpty)) == "true"
	}

	if retryPending := queryArgs.Get(queryDirectiveRetryPending); retryPending != "" {
		r.directives.RetryPending = strings.ToLower(strings.TrimSpace(retryPending)) == "true"
	}

	if skipCacheRead := queryArgs.Get(queryDirectiveSkipCacheRead); skipCacheRead != "" {
		r.directives.SkipCacheRead = strings.ToLower(strings.TrimSpace(skipCacheRead)) == "true"
	}

	if skipInterpolation := queryArgs.Get(queryDirectiveSkipInterpolation); skipInterpolation != "" {
		r.directives.SkipInterpolation = strings.ToLower(strings.TrimSpace(skipInterpolation)) == "true"
	}

	// Validation query parameters
	if v := queryArgs.Get(queryDirectiveEnforceHighestBlock); v != "" {
		r.directives.EnforceHighestBlock = strings.ToLower(strings.TrimSpace(v)) == "true"
	}
	if v := queryArgs.Get(queryDirectiveEnforceGetLogsRange); v != "" {
		r.directives.EnforceGetLogsBlockRange = strings.ToLower(strings.TrimSpace(v)) == "true"
	}
	if v := queryArgs.Get(queryDirectiveEnforceNonNullTaggedBlocks); v != "" {
		r.directives.EnforceNonNullTaggedBlocks = strings.ToLower(strings.TrimSpace(v)) == "true"
	}
	if v := queryArgs.Get(queryDirectiveEnforceLogIndexStrict); v != "" {
		r.directives.EnforceLogIndexStrictIncrements = strings.ToLower(strings.TrimSpace(v)) == "true"
	}
	if v := queryArgs.Get(queryDirectiveValidateLogsBloomEmpty); v != "" {
		r.directives.ValidateLogsBloomEmptiness = strings.ToLower(strings.TrimSpace(v)) == "true"
	}
	if v := queryArgs.Get(queryDirectiveValidateLogsBloomMatch); v != "" {
		r.directives.ValidateLogsBloomMatch = strings.ToLower(strings.TrimSpace(v)) == "true"
	}
	if v := queryArgs.Get(queryDirectiveValidateTxHashUniq); v != "" {
		r.directives.ValidateTxHashUniqueness = strings.ToLower(strings.TrimSpace(v)) == "true"
	}
	if v := queryArgs.Get(queryDirectiveValidateTxIndex); v != "" {
		r.directives.ValidateTransactionIndex = strings.ToLower(strings.TrimSpace(v)) == "true"
	}
	if v := queryArgs.Get(queryDirectiveReceiptsCountExact); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			r.directives.ReceiptsCountExact = &n
		}
	}
	if v := queryArgs.Get(queryDirectiveReceiptsCountAtLeast); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			r.directives.ReceiptsCountAtLeast = &n
		}
	}
	if v := queryArgs.Get(queryDirectiveValidationBlockHash); v != "" {
		r.directives.ValidationExpectedBlockHash = v
	}
	if v := queryArgs.Get(queryDirectiveValidationBlockNumber); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			r.directives.ValidationExpectedBlockNumber = &n
		}
	}
	if v := queryArgs.Get(queryDirectiveValidateHeaderFieldLengths); v != "" {
		r.directives.ValidateHeaderFieldLengths = strings.ToLower(strings.TrimSpace(v)) == "true"
	}
	if v := queryArgs.Get(queryDirectiveValidateTxFields); v != "" {
		r.directives.ValidateTransactionFields = strings.ToLower(strings.TrimSpace(v)) == "true"
	}
	if v := queryArgs.Get(queryDirectiveValidateTxBlockInfo); v != "" {
		r.directives.ValidateTransactionBlockInfo = strings.ToLower(strings.TrimSpace(v)) == "true"
	}
	if v := queryArgs.Get(queryDirectiveValidateLogFields); v != "" {
		r.directives.ValidateLogFields = strings.ToLower(strings.TrimSpace(v)) == "true"
	}
}

func (r *NormalizedRequest) SkipCacheRead() bool {
	if r == nil {
		return false
	}
	if r.directives == nil {
		return false
	}
	return r.directives.SkipCacheRead
}

func (r *NormalizedRequest) Directives() *RequestDirectives {
	if r == nil {
		return nil
	}

	return r.directives
}

func (r *NormalizedRequest) LockWithTrace(ctx context.Context) {
	_, span := StartDetailSpan(ctx, "Request.Lock")
	defer span.End()
	r.Lock()
}

func (r *NormalizedRequest) RLockWithTrace(ctx context.Context) {
	_, span := StartDetailSpan(ctx, "Request.RLock")
	defer span.End()
	r.RLock()
}

// Extract and prepare the request for forwarding.
func (r *NormalizedRequest) JsonRpcRequest(ctx ...context.Context) (*JsonRpcRequest, error) {
	if len(ctx) > 0 {
		_, span := StartDetailSpan(ctx[0], "Request.ResolveJsonRpc")
		defer span.End()
	}

	if r == nil {
		return nil, nil
	}
	if jrq := r.jsonRpcRequest.Load(); jrq != nil {
		return jrq, nil
	}

	rpcReq := new(JsonRpcRequest)
	if err := SonicCfg.Unmarshal(r.body, rpcReq); err != nil {
		return nil, NewErrJsonRpcRequestUnmarshal(err, r.body)
	}

	method := rpcReq.Method
	if method == "" {
		return nil, NewErrJsonRpcRequestUnresolvableMethod(rpcReq)
	}

	r.jsonRpcRequest.Store(rpcReq)
	// Safe to drop the raw body after successful parse to reduce retention of ReadAll buffers.
	r.body = nil

	return rpcReq, nil
}

func (r *NormalizedRequest) Method() (string, error) {
	if r == nil {
		return "", nil
	}

	if r.method != "" {
		return r.method, nil
	}

	if jrq := r.jsonRpcRequest.Load(); jrq != nil {
		r.method = jrq.Method
		return jrq.Method, nil
	}

	if len(r.body) > 0 {
		method, err := sonic.Get(r.body, "method")
		if err != nil {
			return "", NewErrJsonRpcRequestUnmarshal(err, r.body)
		}
		m, err := method.String()
		r.method = m
		return m, err
	}

	return "", NewErrJsonRpcRequestUnresolvableMethod(r.body)
}

func (r *NormalizedRequest) Body() []byte {
	return r.body
}

func (r *NormalizedRequest) MarshalZerologObject(e *zerolog.Event) {
	if r == nil {
		return
	}

	if lu := r.lastUpstream.Load(); lu != nil {
		if lup := lu.(Upstream); lup != nil {
			if lup.Config() != nil {
				e.Str("lastUpstream", lup.Id())
			} else {
				e.Str("lastUpstream", fmt.Sprintf("%p", lup))
			}
		} else {
			e.Str("lastUpstream", "nil")
		}
	} else {
		e.Str("lastUpstream", "nil")
	}

	if r.network != nil {
		e.Str("networkId", r.network.Id())
	}

	if jrq := r.jsonRpcRequest.Load(); jrq != nil {
		e.Object("jsonRpc", jrq)
	} else if r.body != nil {
		if IsSemiValidJson(r.body) {
			e.RawJSON("body", r.body)
		} else {
			e.Str("body", string(r.body))
		}
	}
}

// SetClientIP stores the resolved client IP address
func (r *NormalizedRequest) SetClientIP(ip string) {
	if r == nil {
		return
	}
	r.clientIP.Store(ip)
}

// ClientIP returns the resolved client IP address or "n/a" if not available
func (r *NormalizedRequest) ClientIP() string {
	if r == nil {
		return "n/a"
	}
	if v := r.clientIP.Load(); v != nil {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return "n/a"
}

// TODO Move evm specific data to RequestMetadata struct so we can have multiple architectures besides evm
func (r *NormalizedRequest) EvmBlockRef() interface{} {
	if r == nil {
		return nil
	}
	return r.evmBlockRef.Load()
}

func (r *NormalizedRequest) SetEvmBlockRef(blockRef interface{}) {
	if r == nil || blockRef == nil {
		return
	}
	r.evmBlockRef.Store(blockRef)
}

func (r *NormalizedRequest) EvmBlockNumber() interface{} {
	if r == nil {
		return nil
	}
	return r.evmBlockNumber.Load()
}

func (r *NormalizedRequest) SetEvmBlockNumber(blockNumber interface{}) {
	if r == nil || blockNumber == nil {
		return
	}
	r.evmBlockNumber.Store(blockNumber)
}

func (r *NormalizedRequest) MarshalJSON() ([]byte, error) {
	if r.body != nil {
		return r.body, nil
	}

	if jrq := r.jsonRpcRequest.Load(); jrq != nil {
		return SonicCfg.Marshal(map[string]interface{}{
			"method": jrq.Method,
		})
	}

	if m, _ := r.Method(); m != "" {
		return SonicCfg.Marshal(map[string]interface{}{
			"method": m,
		})
	}

	return nil, nil
}

func (r *NormalizedRequest) CacheHash(ctx ...context.Context) (string, error) {
	rq, err := r.JsonRpcRequest(ctx...)
	if err != nil {
		return "", err
	}

	if rq != nil {
		return rq.CacheHash(ctx...)
	}

	return "", fmt.Errorf("request is not valid to generate cache hash")
}

func (r *NormalizedRequest) Validate() error {
	if r == nil {
		return NewErrInvalidRequest(fmt.Errorf("request is nil"))
	}

	if r.body == nil && r.jsonRpcRequest.Load() == nil {
		return NewErrInvalidRequest(fmt.Errorf("request body is nil"))
	}

	method, err := r.Method()
	if err != nil {
		return NewErrInvalidRequest(err)
	}

	if method == "" {
		return NewErrInvalidRequest(fmt.Errorf("method is required"))
	}

	return nil
}

// IsCompositeRequest returns whether this request is a top-level composite request
func (r *NormalizedRequest) IsCompositeRequest() bool {
	if r == nil {
		return false
	}
	return r.CompositeType() != CompositeTypeNone
}

func (r *NormalizedRequest) CompositeType() string {
	if r == nil {
		return ""
	}
	if ct := r.compositeType.Load(); ct != nil {
		return ct.(string)
	}
	return ""
}

func (r *NormalizedRequest) SetCompositeType(compositeType string) {
	if r == nil || compositeType == "" {
		return
	}
	r.compositeType.Store(compositeType)
}

func (r *NormalizedRequest) ParentRequestId() interface{} {
	if r == nil {
		return nil
	}
	return r.parentRequestId.Load()
}

func (r *NormalizedRequest) SetParentRequestId(parentId interface{}) {
	if r == nil || parentId == nil {
		return
	}
	r.parentRequestId.Store(parentId)
}

func (r *NormalizedRequest) Finality(ctx context.Context) DataFinalityState {
	if r == nil {
		return DataFinalityStateUnknown
	}

	// Check if we have a cached value
	if f := r.finality.Load(); f != nil {
		return f.(DataFinalityState)
	}

	// Calculate and cache the finality
	if r.network != nil {
		finality := r.network.GetFinality(ctx, r, nil)
		// Only cache if we got a definitive answer (not unknown)
		if finality != DataFinalityStateUnknown {
			r.finality.Store(finality)
		}
		return finality
	}

	return DataFinalityStateUnknown
}

// CopyHttpContextFrom copies HTTP context (headers and query parameters) from another request
// This ensures user agent information is preserved for metrics when creating derived requests
func (r *NormalizedRequest) CopyHttpContextFrom(source *NormalizedRequest) {
	if r == nil || source == nil {
		return
	}

	r.Lock()
	defer r.Unlock()

	source.RLock()
	defer source.RUnlock()

	// Copy the user agent information
	if agentName := source.agentName.Load(); agentName != nil {
		r.agentName.Store(agentName)
	}

	// Also copy the user if it exists
	if user := source.User(); user != nil {
		r.SetUser(user)
	}
}

func (r *NormalizedRequest) SetUpstreams(upstreams []Upstream) {
	if r == nil {
		return
	}
	r.upstreamMutex.Lock()
	defer r.upstreamMutex.Unlock()

	r.upstreamList = upstreams
	if r.ConsumedUpstreams == nil {
		r.ConsumedUpstreams = &sync.Map{}
	}
}

// UpstreamsCount returns the number of available upstreams for this request
func (r *NormalizedRequest) UpstreamsCount() int {
	if r == nil {
		return 0
	}
	r.upstreamMutex.Lock()
	defer r.upstreamMutex.Unlock()
	return len(r.upstreamList)
}

// UserId returns the user ID from the user object, or "n/a" if not available
func (r *NormalizedRequest) UserId() string {
	if r == nil {
		return "n/a"
	}

	user := r.User()
	if user != nil && user.Id != "" {
		return user.Id
	}

	return "n/a"
}

// AgentName returns the cached agent name. The agent name should be populated via EnrichFromHttp().
func (r *NormalizedRequest) AgentName() string {
	if r == nil {
		return "unknown"
	}

	// Check if cached agent name is available
	if cachedAgentName := r.agentName.Load(); cachedAgentName != nil {
		return cachedAgentName.(string)
	}

	// If not cached, return unknown (EnrichFromHttp should be called to populate this)
	return "unknown"
}

// getUserAgent returns the user agent string, with query parameter taking precedence over header
func (r *NormalizedRequest) getUserAgent(headers http.Header, queryArgs url.Values) string {
	// Query parameter takes precedence
	if queryArgs != nil {
		if userAgent := queryArgs.Get("user-agent"); userAgent != "" {
			return userAgent
		}
	}

	// Fallback to User-Agent header
	if headers != nil {
		return headers.Get("User-Agent")
	}

	return ""
}

// simplifyAgentName extracts and simplifies the agent name for low cardinality metrics
func (r *NormalizedRequest) simplifyAgentName(userAgent string) string {
	userAgent = strings.ToLower(strings.TrimSpace(userAgent))

	// Common patterns for agent name extraction
	if strings.Contains(userAgent, "curl") {
		return "curl"
	}
	if strings.Contains(userAgent, "wget") {
		return "wget"
	}
	if strings.Contains(userAgent, "postman") {
		return "postman"
	}
	if strings.Contains(userAgent, "insomnia") {
		return "insomnia"
	}
	if strings.Contains(userAgent, "httpie") {
		return "httpie"
	}
	if strings.Contains(userAgent, "python") {
		return "python"
	}
	if strings.Contains(userAgent, "node") || strings.Contains(userAgent, "javascript") {
		return "nodejs"
	}
	if strings.Contains(userAgent, "go-http-client") || strings.Contains(userAgent, "go/") {
		return "go"
	}
	if strings.Contains(userAgent, "java") {
		return "java"
	}
	if strings.Contains(userAgent, "rust") {
		return "rust"
	}
	if strings.Contains(userAgent, "ruby") {
		return "ruby"
	}
	if strings.Contains(userAgent, "php") {
		return "php"
	}
	if strings.Contains(userAgent, "chrome") {
		return "chrome"
	}
	if strings.Contains(userAgent, "firefox") {
		return "firefox"
	}
	if strings.Contains(userAgent, "safari") {
		return "safari"
	}
	if strings.Contains(userAgent, "edge") {
		return "edge"
	}
	if strings.Contains(userAgent, "viem") {
		return "viem"
	}
	if strings.Contains(userAgent, "ethers") {
		return "ethers"
	}
	if strings.Contains(userAgent, "web3") {
		return "web3"
	}
	if strings.Contains(userAgent, "alchemy") {
		return "alchemy-sdk"
	}
	if strings.Contains(userAgent, "infura") {
		return "infura-sdk"
	}
	if strings.Contains(userAgent, "quicknode") {
		return "quicknode-sdk"
	}

	// Try to extract first word before version or space
	parts := strings.Fields(userAgent)
	if len(parts) > 0 {
		name := parts[0]
		// Remove version info from the name part
		if idx := strings.IndexAny(name, "/()"); idx != -1 {
			name = name[:idx]
		}
		// Limit length to prevent high cardinality
		if len(name) > 20 {
			name = name[:20]
		}
		if name != "" {
			return name
		}
	}

	return "other"
}

func (r *NormalizedRequest) NextUpstream() (Upstream, error) {
	if r == nil {
		return nil, fmt.Errorf("unexpected uninitialized request")
	}

	// Lock for 100% guaranteed sequential upstream selection
	r.upstreamMutex.Lock()
	defer r.upstreamMutex.Unlock()

	if len(r.upstreamList) == 0 {
		return nil, NewErrNoUpstreamsLeftToSelect(
			r,
			"no upstreams are set for this request",
		)
	}

	// Capture any UseUpstream directive to be applied inside the generic selection loop
	var useUpstreamPattern string
	if r.directives != nil && r.directives.UseUpstream != "" {
		useUpstreamPattern = r.directives.UseUpstream
	}

	upstreamCount := len(r.upstreamList)

	// Simple round-robin selection. The outer loop in networks.go is capped to prevent
	// infinite loops. Retryable errors are removed from ConsumedUpstreams by
	// MarkUpstreamCompleted, allowing re-selection on subsequent iterations.
	for attempts := 0; attempts < upstreamCount; attempts++ {
		idx := r.UpstreamIdx % uint32(upstreamCount) // #nosec G115
		r.UpstreamIdx++                              // Guaranteed increment for next caller

		upstream := r.upstreamList[idx]

		// If a UseUpstream directive is provided, only consider matching upstreams
		if useUpstreamPattern != "" {
			match, err := WildcardMatch(useUpstreamPattern, upstream.Id())
			if err != nil || !match {
				continue
			}
		}

		// Skip if already consumed (in-flight or already completed this round)
		if _, consumed := r.ConsumedUpstreams.Load(upstream); consumed {
			continue
		}

		// Skip if already responded with non-retryable error (permanent blacklist)
		if prevErr, exists := r.ErrorsByUpstream.Load(upstream); exists {
			if pe, ok := prevErr.(error); ok && !IsRetryableTowardsUpstream(pe) {
				continue
			}
		}

		// Skip if already responded empty
		if _, isEmpty := r.EmptyResponses.Load(upstream); isEmpty {
			continue
		}

		// Try to reserve this upstream atomically
		// If LoadOrStore returns loaded=false, we successfully reserved it
		if _, loaded := r.ConsumedUpstreams.LoadOrStore(upstream, true); !loaded {
			// Successfully reserved this upstream
			return upstream, nil
		}
		// If we reach here, another goroutine reserved this upstream, continue to next
	}

	return nil, NewErrNoUpstreamsLeftToSelect(
		r,
		"no more non-consumed or working upstreams left",
	)
}

func (r *NormalizedRequest) MarkUpstreamCompleted(ctx context.Context, upstream Upstream, resp *NormalizedResponse, err error) {
	r.upstreamMutex.Lock()
	defer r.upstreamMutex.Unlock()

	hasResponse := resp != nil && !resp.IsObjectNull(ctx)

	// Check if this is a cancelled hedge - if so, just release and return
	if err != nil && HasErrorCode(err, ErrCodeEndpointRequestCanceled) {
		r.ConsumedUpstreams.Delete(upstream)
		// Don't store the error - this upstream wasn't really "tried"
		return
	}

	// Store errors for reporting and for NextUpstream to skip permanent errors.
	if err != nil {
		r.ErrorsByUpstream.Store(upstream, err)
	} else if resp != nil && resp.IsResultEmptyish(ctx) {
		jr, jrErr := resp.JsonRpcResponse(ctx)
		if jr == nil {
			r.ErrorsByUpstream.Store(upstream, NewErrEndpointMissingData(fmt.Errorf("upstream responded emptyish but cannot extract json-rpc response: %v", jrErr), upstream))
		} else {
			r.ErrorsByUpstream.Store(upstream, NewErrEndpointMissingData(fmt.Errorf("upstream responded emptyish: %v", jr.GetResultString()), upstream))
		}
		r.EmptyResponses.Store(upstream, true)
	}

	// Remove from ConsumedUpstreams if upstream can be retried:
	// - No response yet (pre-flight error like block unavailable)
	// - Or error is retryable (transient failure)
	// The outer loop in networks.go is capped to prevent infinite loops.
	canReUse := !hasResponse || (err != nil && IsRetryableTowardsUpstream(err))
	if canReUse {
		r.ConsumedUpstreams.Delete(upstream)
	}
}
