package common

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

const (
	CompositeTypeNone                      = "none"
	CompositeTypeLogsSplitOnError          = "logs-split-on-error"
	CompositeTypeLogsSplitProactive        = "logs-split-proactive"
	CompositeTypeTraceFilterSplitOnError   = "trace-filter-split-on-error"
	CompositeTypeTraceFilterSplitProactive = "trace-filter-split-proactive"
	CompositeTypeMulticall3                = "multicall3"
)

const RequestContextKey ContextKey = "rq"
const UpstreamsContextKey ContextKey = "ups"

// Context markers for selection reasons only the spawning executor knows.
// The attempt recorder (upstream tryForward's deferred record) runs at the
// bottom of the failsafe chain and cannot see WHICH executor caused this
// attempt to exist, so fan-out executors tag the context they hand each
// attempt: the consensus executor marks every participant slot
// (consensus_slot) and the network sweep marks its non-first picks (sweep).
// They live here with the other ContextKey constants — request.go is
// excluded from tygo generation, and these keys are server-internal (the
// ContextKey type has no TypeScript counterpart).

// ConsensusSlotContextKey marks a context executing inside one consensus
// participant slot.
const ConsensusSlotContextKey ContextKey = "consensusSlot"

// SweepIterationContextKey marks a context executing a non-first pick of
// the try-all-upstreams sweep within one execution.
const SweepIterationContextKey ContextKey = "sweepIteration"

// WithConsensusSlot returns ctx marked as a consensus participant slot.
func WithConsensusSlot(ctx context.Context) context.Context {
	return context.WithValue(ctx, ConsensusSlotContextKey, true)
}

// IsConsensusSlot reports whether ctx executes inside a consensus
// participant slot.
func IsConsensusSlot(ctx context.Context) bool {
	v, _ := ctx.Value(ConsensusSlotContextKey).(bool)
	return v
}

// WithSweepIteration returns ctx marked as a non-first sweep pick.
func WithSweepIteration(ctx context.Context) context.Context {
	return context.WithValue(ctx, SweepIterationContextKey, true)
}

// IsSweepIteration reports whether ctx executes a non-first pick of the
// try-all-upstreams sweep.
func IsSweepIteration(ctx context.Context) bool {
	v, _ := ctx.Value(SweepIterationContextKey).(bool)
	return v
}

// wireParamsContextKey carries the JSON-RPC params one upstream attempt sends
// for one request in place of its own. The request keeps its params, so the
// cache key, response binding and every other upstream see the original question.
type wireParamsContextKey struct{}

// wireParams binds the override to its request: any other request sent with a
// derived ctx on the same attempt (a probe, a state poll) keeps its own params.
type wireParams struct {
	req    *NormalizedRequest
	params []interface{}
}

// WithWireParams returns ctx whose upstream attempt sends params for req on the wire.
func WithWireParams(ctx context.Context, req *NormalizedRequest, params []interface{}) context.Context {
	return context.WithValue(ctx, wireParamsContextKey{}, wireParams{req: req, params: params})
}

// WireParams returns the params an upstream client sends for req under ctx:
// the attempt's override for exactly req when set, else req's own params.
func WireParams(ctx context.Context, req *NormalizedRequest, params []interface{}) []interface{} {
	if w, ok := ctx.Value(wireParamsContextKey{}).(wireParams); ok && w.req == req {
		return w.params
	}
	return params
}

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
	headerDirectiveSkipConsensus              = "X-ERPC-Skip-Consensus"
	headerDirectiveEnforceHighestBlock        = "X-ERPC-Enforce-Highest-Block"
	headerDirectiveEnforceGetLogsRange        = "X-ERPC-Enforce-GetLogs-Range"
	headerDirectiveEnforceNonNullTaggedBlocks = "X-ERPC-Enforce-Non-Null-Tagged-Blocks"
	headerDirectiveIntegrity                  = "X-ERPC-Integrity"
)

// HeaderUserId is the request header erpc reads as the caller's user identity
// when a project enables ProjectConfig.TrustUserIdHeader. It lets a deployment
// that authenticates callers *in front of* erpc (e.g. an API gateway) attribute
// erpc's per-user metrics and logs without erpc performing auth itself. erpc
// does NOT validate the value — the gateway is trusted to set it and to strip
// any client-supplied copy. See [NormalizedRequest.SetUserFromTrustedHeader].
const HeaderUserId = "X-ERPC-User-Id"

const (
	queryDirectiveRetryEmpty                 = "retry-empty"
	queryDirectiveRetryPending               = "retry-pending"
	queryDirectiveSkipCacheRead              = "skip-cache-read"
	queryDirectiveUseUpstream                = "use-upstream"
	queryDirectiveSkipInterpolation          = "skip-interpolation"
	queryDirectiveSkipConsensus              = "skip-consensus"
	queryDirectiveEnforceHighestBlock        = "enforce-highest-block"
	queryDirectiveEnforceGetLogsRange        = "enforce-getlogs-range"
	queryDirectiveEnforceNonNullTaggedBlocks = "enforce-non-null-tagged-blocks"
	queryDirectiveIntegrity                  = "integrity"
)

var directiveKeyRegistry = []directiveKeyNames{
	{header: headerDirectiveRetryEmpty, query: queryDirectiveRetryEmpty},
	{header: headerDirectiveRetryPending, query: queryDirectiveRetryPending},
	{header: headerDirectiveSkipCacheRead, query: queryDirectiveSkipCacheRead},
	{header: headerDirectiveUseUpstream, query: queryDirectiveUseUpstream},
	{header: headerDirectiveSkipInterpolation, query: queryDirectiveSkipInterpolation},
	{header: headerDirectiveSkipConsensus, query: queryDirectiveSkipConsensus},
	{header: headerDirectiveEnforceHighestBlock, query: queryDirectiveEnforceHighestBlock},
	{header: headerDirectiveEnforceGetLogsRange, query: queryDirectiveEnforceGetLogsRange},
	{header: headerDirectiveEnforceNonNullTaggedBlocks, query: queryDirectiveEnforceNonNullTaggedBlocks},
	{header: headerDirectiveIntegrity, query: queryDirectiveIntegrity},
}

var DenyAllClientDirectives MatcherFunc = func(_ string) bool { return false }

// headerToQueryKey is used to normalize header keys to query keys
var headerToQueryKey map[string]string

func init() {
	headerToQueryKey = make(map[string]string, len(directiveKeyRegistry))
	for _, k := range directiveKeyRegistry {
		headerToQueryKey[k.header] = k.query
	}
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
	// Accepts "true" to skip all, "false" or "" to skip none,
	// or a connector ID pattern (e.g. "redis*", "memory*|dynamo*") to skip specific cache drivers.
	SkipCacheRead string `json:"skipCacheRead"`

	// Instruct the proxy to forward the request to a specific upstream(s) only.
	// Value can use "*" star char as a wildcard to target multiple upstreams.
	// For example "alchemy" or "my-own-*", etc.
	UseUpstream string `json:"useUpstream,omitempty"`

	// Instruct the proxy to bypass method exclusion checks.
	ByPassMethodExclusion bool `json:"-"`

	// UpstreamPolicies says whose request it is and which upstream failsafe
	// policies apply: a client's (UpstreamPoliciesConfigured) or eRPC's own
	// (any other), which matches failsafe request kind "internal" and whose
	// answer the integrity engine checks against held data only. Never set
	// from HTTP headers.
	UpstreamPolicies UpstreamPolicies `json:"-"`

	// DeferIntegrity: the upstream attempt skips the integrity checks; the
	// caller runs them on the answer it takes (a chain tracker receipts
	// fetch hands the answer to waiting client checks first, spec §11).
	// Never set from HTTP headers.
	DeferIntegrity bool `json:"-"`

	// Instruct the normalization layer to avoid mutating JSON-RPC params for block tag interpolation.
	// When true, the system will still compute and cache block references (for finality/metrics),
	// but will NOT replace tags like "latest"/"finalized" with hex numbers in outbound requests.
	SkipInterpolation bool `json:"skipInterpolation"`

	// Instruct the proxy to bypass the consensus policy for this request and
	// route through the standard retry+hedge+breaker+timeout path instead.
	// Retry, hedge, breaker, and timeout policies still apply — only the
	// consensus dispute / agreement step is skipped. Useful when the caller
	// has its own correctness checks downstream and prefers first-response
	// latency over multi-upstream agreement.
	SkipConsensus bool `json:"skipConsensus"`

	// Validation: Block Integrity (consumed by the EVM block/getLogs hooks; not
	// part of the data-integrity module).
	EnforceHighestBlock        bool `json:"enforceHighestBlock,omitempty"`
	EnforceGetLogsBlockRange   bool `json:"enforceGetLogsBlockRange,omitempty"`
	EnforceNonNullTaggedBlocks bool `json:"enforceNonNullTaggedBlocks,omitempty"`

	// IntegritySelector is the per-request data-integrity selection from the
	// X-ERPC-Integrity header / integrity query param. It is a bare word — a
	// level (off/intrinsic/corroborated/authoritative) or a configured profile
	// name. Honored only when the effective integrity headerMode permits it.
	IntegritySelector string `json:"integritySelector,omitempty"`
}

// UpstreamPolicies selects the upstream failsafe policies a request takes.
type UpstreamPolicies uint8

const (
	// UpstreamPoliciesConfigured: every configured policy; client traffic.
	UpstreamPoliciesConfigured UpstreamPolicies = iota
	// UpstreamPoliciesTimeoutOnly: subscription calls, state-poller and
	// chainId probes, integrity state probes. No network or upstream retry
	// or hedge, which would send a subscribe twice, and no breaker, so
	// probing a down vendor never opens one clients depend on; the
	// per-attempt timeout applies.
	UpstreamPoliciesTimeoutOnly
	// UpstreamPoliciesTracker: chain tracker fetches. No configured policy
	// applies; an open breaker refuses them, and they never feed it, so
	// tracker load cannot open a breaker that blocks clients.
	UpstreamPoliciesTracker
	// UpstreamPoliciesTrackerFill: catch-up tracker fetches. As
	// UpstreamPoliciesTracker, and they take an upstream rate permit only
	// while the budget stays more than half free, so they yield to clients.
	UpstreamPoliciesTrackerFill
	// UpstreamPoliciesHeadAsk: a head Ask's fetch of latest. As
	// UpstreamPoliciesTimeoutOnly, and it takes no upstream rate permit:
	// client load at a budget never stalls a head (spec §6.8).
	UpstreamPoliciesHeadAsk
)

// NewProbeRequest is a request an upstream's own state poller or chainId
// probe sends: UpstreamPoliciesTimeoutOnly.
func NewProbeRequest(body []byte) *NormalizedRequest {
	r := NewNormalizedRequest(body)
	r.SetDirectives(&RequestDirectives{UpstreamPolicies: UpstreamPoliciesTimeoutOnly})
	return r
}

func (d *RequestDirectives) Clone() *RequestDirectives {
	if d == nil {
		return &RequestDirectives{}
	}
	return &RequestDirectives{
		RetryEmpty:                 d.RetryEmpty,
		RetryPending:               d.RetryPending,
		SkipCacheRead:              d.SkipCacheRead,
		UseUpstream:                d.UseUpstream,
		ByPassMethodExclusion:      d.ByPassMethodExclusion,
		SkipInterpolation:          d.SkipInterpolation,
		SkipConsensus:              d.SkipConsensus,
		EnforceHighestBlock:        d.EnforceHighestBlock,
		EnforceGetLogsBlockRange:   d.EnforceGetLogsBlockRange,
		EnforceNonNullTaggedBlocks: d.EnforceNonNullTaggedBlocks,
		IntegritySelector:          d.IntegritySelector,
	}
}

type NormalizedRequest struct {
	sync.RWMutex

	network        Network
	cacheDal       CacheDAL
	body           []byte
	ForwardHeaders http.Header

	directives                  *RequestDirectives
	allowClientDirectiveMatcher MatcherFunc
	jsonRpcRequest              atomic.Pointer[JsonRpcRequest]
	// parseMu serializes the one parse of body into jsonRpcRequest. Hedge and
	// consensus fan-out share a request, so without it two goroutines can both
	// miss the cache and read body while a third releases it.
	parseMu     sync.Mutex
	bodyCleared atomic.Bool

	// Upstream selection fields - protected by upstreamMutex
	upstreamMutex    sync.Mutex
	UpstreamIdx      uint32
	ErrorsByUpstream sync.Map
	// Upstream -> its latest block when the ErrorsByUpstream entry was stored.
	// Wrong-empty accounting needs the head at answer time: by the time the
	// request ends, a lagging upstream may have caught up.
	headsAtError sync.Map

	// Fields for centralized upstream management
	upstreamList        []Upstream // Available upstreams for this request (post-selection)
	networkUpstreamList []Upstream // Unfiltered network registry — consensus-policy fail-closed helpers
	networkUpstreamsSet bool       // true after SetNetworkUpstreams (even if the snapshot is empty)
	ConsumedUpstreams   *sync.Map  // Tracks upstreams that provided valid responses

	// escalatedToFallbacks makes the per-request fallback escape happen at
	// most once across retries and hedges.
	escalatedToFallbacks atomic.Bool

	lastValidResponse         atomic.Pointer[NormalizedResponse]
	integrityCaught           atomic.Bool  // an integrity check rejected a response during this request
	integrityRejectedCheck    atomic.Value // id of the last check that rejected (the "why")
	integrityRejectedFinality atomic.Value // finality of the last rejected block (for saved/failed metric)
	// integrityFallback holds the newest FALLBACK-ELIGIBLE original: a response
	// a recordOnly verdict flagged, escalated to a rejection only so the
	// failsafe could hunt a validated replacement. If the hunt exhausts,
	// project.Forward serves this instead of an error.
	integrityFallback   atomic.Pointer[IntegrityFallback]
	integrityOverheadNs atomic.Int64 // ns the request waited on integrity checks + aux force-fetches
	lastUpstream        atomic.Value
	evmBlockRef         atomic.Value
	evmBlockNumber      atomic.Value
	// evmHead is the lane's Head this request took once (spec §7).
	evmHead atomic.Pointer[EvmHead]
	// headRefusal is the refusal rule's verdict for its block (spec §6.4).
	headRefusal atomic.Pointer[HeadRefusal]
	// tagStateSkips are the upstreams the tag-state gate skips for this
	// request (spec §6.11); nil unless it is a family-3 tag request.
	tagStateSkips atomic.Pointer[map[string]bool]

	compositeType   atomic.Value // Type of composite request (e.g., "logs-split")
	parentRequestId atomic.Value // ID of the parent request (for sub-requests)

	finality atomic.Value // Cached finality state

	user atomic.Value

	// Cached agent information to avoid recalculation
	agentName atomic.Value // Cached agent name from User-Agent

	// Resolved client IP (set by HTTP ingress using trusted forwarders)
	clientIP atomic.Value

	// Per-request execution counters; lazy-init via execStateHolder.
	execStateHolder execStateHolder
}

// ExecState returns the per-request execution counters. Lazy-init on
// first access — callers may invoke this concurrently.
func (r *NormalizedRequest) ExecState() *ExecState {
	if r == nil {
		return nil
	}
	return r.execStateHolder.get()
}

// CreditUnitsTotal returns the total vendor credit units this request has
// accrued across every upstream attempt (retries / hedges / consensus
// included). Thread-safe. See ExecState.CreditUnitsTotal.
func (r *NormalizedRequest) CreditUnitsTotal() int64 {
	if r == nil {
		return 0
	}
	return r.ExecState().CreditUnitsTotal()
}

// CreditUnitsByVendor returns this request's per-vendor credit-unit totals,
// summed across all attempts against each vendor. Nil when nothing has
// accrued. Thread-safe. See ExecState.CreditUnitsByVendor.
func (r *NormalizedRequest) CreditUnitsByVendor() map[string]int64 {
	if r == nil {
		return nil
	}
	return r.ExecState().CreditUnitsByVendor()
}

// UpstreamPolicies returns which upstream failsafe policies apply to r.
func (r *NormalizedRequest) UpstreamPolicies() UpstreamPolicies {
	if r == nil {
		return UpstreamPoliciesConfigured
	}
	d := r.Directives()
	if d == nil {
		return UpstreamPoliciesConfigured
	}
	return d.UpstreamPolicies
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

// SetUserFromTrustedHeader assigns the request's user identity from a value
// supplied by a trusted upstream (the [HeaderUserId] header), for deployments
// that authenticate callers in front of erpc and want per-user metrics/logs
// without erpc doing auth. It is a no-op when value is empty (after trimming)
// or when an auth strategy already resolved a user — auth always wins. Only the
// Id is set; no rate-limit budget is attached, so this never enables user-level
// rate limiting. erpc does not validate value; gate it behind
// ProjectConfig.TrustUserIdHeader and only trust the header from a proxy that
// strips any client-supplied copy.
func (r *NormalizedRequest) SetUserFromTrustedHeader(value string) {
	if r == nil || r.User() != nil {
		return
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	r.user.Store(&User{Id: value})
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

// SetLastValidResponse stores a borrowed reference to the most recent valid response.
// This is a non-owning pointer: the caller that produced the response (upstream Forward,
// consensus executor, etc.) is responsible for its Release(). Overwriting a previous
// LVR does NOT release the old one — the original producer still owns it.
func (r *NormalizedRequest) SetLastValidResponse(ctx context.Context, nrs *NormalizedResponse) bool {
	if r == nil || nrs == nil {
		return false
	}
	// A response a content-integrity check rejected must never (re-)enter the
	// slot: attempts store their response here BEFORE post-forward validation,
	// so without this a hedged corrupt body could be re-served after a reject.
	if nrs.IsIntegrityRejected() {
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

// ClearLastValidResponse drops the borrowed reference without releasing the response.
// The original producer is responsible for calling Release().
func (r *NormalizedRequest) ClearLastValidResponse() {
	if r == nil {
		return
	}
	r.lastValidResponse.Store(nil)
}

// ClearLastValidResponseIf drops the last-valid-response only when it IS the
// given (rejected) response. Reject paths must use this instead of the
// unconditional clear: with hedged attempts in flight, an unconditional clear
// can drop a concurrently-stored VALID response from another attempt.
func (r *NormalizedRequest) ClearLastValidResponseIf(rejected *NormalizedResponse) {
	if r == nil || rejected == nil {
		return
	}
	r.lastValidResponse.CompareAndSwap(rejected, nil)
}

// MarkIntegrityCaught records that integrity check `checkID` rejected a response
// during this request. Read at the end via IntegrityCaught (saved vs failed) and
// IntegrityRejectedCheck (the "why").
func (r *NormalizedRequest) MarkIntegrityCaught(checkID, finality string) {
	if r != nil {
		r.integrityCaught.Store(true)
		if checkID != "" {
			r.integrityRejectedCheck.Store(checkID)
		}
		if finality != "" {
			r.integrityRejectedFinality.Store(finality)
		}
	}
}

// IntegrityCaught reports whether an integrity check rejected at least one
// response during this request.
func (r *NormalizedRequest) IntegrityCaught() bool {
	return r != nil && r.integrityCaught.Load()
}

// IntegrityFallback is a flagged-but-serveable original response stashed by a
// fallback-eligible integrity rejection (recordOnly verdict escalated by
// autoCorrectWhenPossible), plus the violation metadata a fallback serve is
// recorded with.
type IntegrityFallback struct {
	Response *NormalizedResponse
	CheckID  string
	Finality string
	Reason   string
}

// SetIntegrityFallbackResponse stashes resp as the fallback to serve if no
// upstream produces a validated replacement. The newest stash wins: every
// eligible original satisfies the recordOnly policy equally, and the newest
// is the most likely to be post-reorg.
func (r *NormalizedRequest) SetIntegrityFallbackResponse(resp *NormalizedResponse, checkID, finality, reason string) {
	if r == nil || resp == nil {
		return
	}
	r.integrityFallback.Store(&IntegrityFallback{Response: resp, CheckID: checkID, Finality: finality, Reason: reason})
}

// TakeIntegrityFallbackResponse returns the stashed fallback (nil if none) and
// clears the slot so a fallback is served at most once.
func (r *NormalizedRequest) TakeIntegrityFallbackResponse() *IntegrityFallback {
	if r == nil {
		return nil
	}
	return r.integrityFallback.Swap(nil)
}

// IntegrityRejectedCheck returns the id of the last integrity check that
// rejected during this request (the failure reason), or "" if none.
func (r *NormalizedRequest) IntegrityRejectedCheck() string {
	if r == nil {
		return ""
	}
	if v := r.integrityRejectedCheck.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// IntegrityRejectedFinality returns the finality ("finalized"/"unfinalized"/
// "unknown") of the last block an integrity check rejected during this request,
// or "" if none.
func (r *NormalizedRequest) IntegrityRejectedFinality() string {
	if r == nil {
		return ""
	}
	if v := r.integrityRejectedFinality.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// AddIntegrityOverhead accumulates time the request waited on integrity work
// (data-checks + aux force-fetches). Called once per upstream attempt; the total
// is observed at the end of the request.
func (r *NormalizedRequest) AddIntegrityOverhead(d time.Duration) {
	if r != nil && d > 0 {
		r.integrityOverheadNs.Add(int64(d))
	}
}

// IntegrityOverhead returns the total time the request spent on integrity work.
func (r *NormalizedRequest) IntegrityOverhead() time.Duration {
	if r == nil {
		return 0
	}
	return time.Duration(r.integrityOverheadNs.Load())
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

// CacheKeySuffix returns the optional JSON-RPC cache partition-key suffix
// configured on this request's network, or "" when unset / no network.
func (r *NormalizedRequest) CacheKeySuffix() string {
	if r == nil {
		return ""
	}
	r.RLock()
	defer r.RUnlock()
	if r.network == nil {
		return ""
	}
	cfg := r.network.Config()
	if cfg == nil {
		return ""
	}
	return cfg.CacheKeySuffix
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

func (r *NormalizedRequest) SetAllowClientDirectiveMatcher(matcher MatcherFunc) {
	if r == nil {
		return
	}
	r.allowClientDirectiveMatcher = matcher
}

// ApplyDirectiveDefaults applies the default directives from the network configuration.
// It is a no-op if directives have already been populated (by a prior call to
// ApplyDirectiveDefaults, SetDirectives, or EnrichFromHttp). This prevents the
// defensive call in Network.Forward() from overwriting directives that were
// explicitly set via HTTP headers/query params between the http_server's
// ApplyDirectiveDefaults and Network.Forward.
func (r *NormalizedRequest) ApplyDirectiveDefaults(directiveDefaults *DirectiveDefaultsConfig) {
	if directiveDefaults == nil {
		return
	}
	r.Lock()
	defer r.Unlock()

	if r.directives != nil {
		return
	}
	r.directives = &RequestDirectives{}

	if directiveDefaults.RetryEmpty != nil {
		r.directives.RetryEmpty = *directiveDefaults.RetryEmpty
	}
	if directiveDefaults.RetryPending != nil {
		r.directives.RetryPending = *directiveDefaults.RetryPending
	}
	if directiveDefaults.SkipCacheRead != nil {
		switch v := directiveDefaults.SkipCacheRead.(type) {
		case string:
			r.directives.SkipCacheRead = v
		default:
			r.directives.SkipCacheRead = fmt.Sprintf("%v", v)
		}
	}
	if directiveDefaults.UseUpstream != nil {
		r.directives.UseUpstream = *directiveDefaults.UseUpstream
	}
	if directiveDefaults.SkipInterpolation != nil {
		r.directives.SkipInterpolation = *directiveDefaults.SkipInterpolation
	}
	if directiveDefaults.SkipConsensus != nil {
		r.directives.SkipConsensus = *directiveDefaults.SkipConsensus
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
	// Data-integrity validation defaults are not applied to request directives:
	// they are translated into the network's integrity config at config-load
	// time (see common/defaults.go).
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

func (r *NormalizedRequest) isDirectiveAllowed(queryKey string) bool {
	if r.allowClientDirectiveMatcher == nil {
		return true
	}
	if queryKey == "" {
		return false
	}
	return r.allowClientDirectiveMatcher(queryKey)
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

	getHeader := func(key string) string {
		if hv := headers.Get(key); hv != "" && r.isDirectiveAllowed(headerToQueryKey[key]) {
			return hv
		}
		return ""
	}

	if hv := getHeader(headerDirectiveRetryEmpty); hv != "" {
		r.directives.RetryEmpty = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}
	if hv := getHeader(headerDirectiveRetryPending); hv != "" {
		r.directives.RetryPending = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}
	if hv := getHeader(headerDirectiveSkipCacheRead); hv != "" {
		r.directives.SkipCacheRead = strings.TrimSpace(hv)
	}
	if hv := getHeader(headerDirectiveUseUpstream); hv != "" {
		r.directives.UseUpstream = hv
	}
	if hv := getHeader(headerDirectiveSkipInterpolation); hv != "" {
		r.directives.SkipInterpolation = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}
	if hv := getHeader(headerDirectiveSkipConsensus); hv != "" {
		r.directives.SkipConsensus = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}

	// Validation Headers
	if hv := getHeader(headerDirectiveEnforceHighestBlock); hv != "" {
		r.directives.EnforceHighestBlock = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}
	if hv := getHeader(headerDirectiveEnforceGetLogsRange); hv != "" {
		r.directives.EnforceGetLogsBlockRange = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}
	if hv := getHeader(headerDirectiveEnforceNonNullTaggedBlocks); hv != "" {
		r.directives.EnforceNonNullTaggedBlocks = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}
	if hv := getHeader(headerDirectiveIntegrity); hv != "" {
		r.directives.IntegritySelector = strings.TrimSpace(hv)
	}

	// Query parameters come after headers so they can still override when explicitly present in URL.

	getQueryArg := func(key string) string {
		if hv := queryArgs.Get(key); hv != "" && r.isDirectiveAllowed(key) {
			return hv
		}
		return ""
	}

	if hv := getQueryArg(queryDirectiveIntegrity); hv != "" {
		r.directives.IntegritySelector = strings.TrimSpace(hv)
	}
	if useUpstream := getQueryArg(queryDirectiveUseUpstream); useUpstream != "" {
		r.directives.UseUpstream = strings.TrimSpace(useUpstream)
	}

	if retryEmpty := getQueryArg(queryDirectiveRetryEmpty); retryEmpty != "" {
		r.directives.RetryEmpty = strings.ToLower(strings.TrimSpace(retryEmpty)) == "true"
	}

	if retryPending := getQueryArg(queryDirectiveRetryPending); retryPending != "" {
		r.directives.RetryPending = strings.ToLower(strings.TrimSpace(retryPending)) == "true"
	}

	if skipCacheRead := getQueryArg(queryDirectiveSkipCacheRead); skipCacheRead != "" {
		r.directives.SkipCacheRead = strings.TrimSpace(skipCacheRead)
	}

	if skipInterpolation := getQueryArg(queryDirectiveSkipInterpolation); skipInterpolation != "" {
		r.directives.SkipInterpolation = strings.ToLower(strings.TrimSpace(skipInterpolation)) == "true"
	}

	if skipConsensus := getQueryArg(queryDirectiveSkipConsensus); skipConsensus != "" {
		r.directives.SkipConsensus = strings.ToLower(strings.TrimSpace(skipConsensus)) == "true"
	}

	// Validation query parameters
	if v := getQueryArg(queryDirectiveEnforceHighestBlock); v != "" {
		r.directives.EnforceHighestBlock = strings.ToLower(strings.TrimSpace(v)) == "true"
	}
	if v := getQueryArg(queryDirectiveEnforceGetLogsRange); v != "" {
		r.directives.EnforceGetLogsBlockRange = strings.ToLower(strings.TrimSpace(v)) == "true"
	}
	if v := getQueryArg(queryDirectiveEnforceNonNullTaggedBlocks); v != "" {
		r.directives.EnforceNonNullTaggedBlocks = strings.ToLower(strings.TrimSpace(v)) == "true"
	}
}

// ShouldSkipCacheRead reports whether a cache read should be skipped for this request.
// The directive may be "true" (skip all connectors) or a pattern (e.g. "redis-*") to skip only matching connectors.
// connectorId is the cache connector being considered; pass "" when not evaluating a specific connector (e.g. before consulting any cache).
func (r *NormalizedRequest) ShouldSkipCacheRead(connectorId string) bool {
	if r == nil || r.directives == nil {
		return false
	}
	v := r.directives.SkipCacheRead
	if v == "" || strings.EqualFold(v, "false") {
		return false
	}
	if strings.EqualFold(v, "true") {
		return true
	}
	if connectorId == "" {
		return false
	}
	matched, _ := WildcardMatch(v, connectorId)
	return matched
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

// resolveJsonRpc returns this request's parsed envelope, parsing the raw body
// on first call and caching the result for every later caller. It is the only
// place NormalizedRequest turns a body into a JsonRpcRequest, so the method
// erpc authenticates, rate-limits, and caches on is by construction the method
// it sends upstream.
//
// It leaves the raw body in place; JsonRpcRequest is the caller that decides
// the body is no longer needed.
func (r *NormalizedRequest) resolveJsonRpc(ctx ...context.Context) (*JsonRpcRequest, error) {
	// Fast path: every caller after the first reads the published envelope
	// without touching the lock.
	if jrq := r.jsonRpcRequest.Load(); jrq != nil {
		return jrq, nil
	}

	if len(ctx) > 0 {
		_, span := StartDetailSpan(ctx[0], "Request.ResolveJsonRpc")
		defer span.End()
	}

	r.parseMu.Lock()
	defer r.parseMu.Unlock()

	if jrq := r.jsonRpcRequest.Load(); jrq != nil {
		return jrq, nil
	}

	rpcReq := new(JsonRpcRequest)
	if err := SonicCfg.Unmarshal(r.body, rpcReq); err != nil {
		return nil, NewErrJsonRpcRequestUnmarshal(err, r.body)
	}

	if rpcReq.Method == "" {
		return nil, NewErrJsonRpcRequestUnresolvableMethod(rpcReq)
	}

	// Publish last: callers compare this pointer and mutate what it points at
	// (params rewrites, cache-hash memoization), so one request must hand out
	// one envelope.
	r.jsonRpcRequest.Store(rpcReq)

	return rpcReq, nil
}

// Extract and prepare the request for forwarding.
func (r *NormalizedRequest) JsonRpcRequest(ctx ...context.Context) (*JsonRpcRequest, error) {
	if r == nil {
		return nil, nil
	}

	rpcReq, err := r.resolveJsonRpc(ctx...)
	if err != nil {
		return nil, err
	}

	// Safe to drop the raw body after successful parse to reduce retention of
	// ReadAll buffers. Take parseMu so the release cannot land while another
	// goroutine is still parsing that same body, and CAS so fan-out callers
	// write it once rather than once per attempt.
	if r.bodyCleared.CompareAndSwap(false, true) {
		r.parseMu.Lock()
		r.body = nil
		r.parseMu.Unlock()
	}

	return rpcReq, nil
}

// Method returns the method erpc authenticates, rate-limits, routes, and meters
// on. It reads the shared envelope rather than peeking at the raw body, so it
// reports the same method the upstream is asked to run. The envelope is the
// only cache: a second copy on the request would be a plain field read and
// written by every hedge and consensus goroutine at once.
func (r *NormalizedRequest) Method() (string, error) {
	if r == nil {
		return "", nil
	}

	if jrq := r.jsonRpcRequest.Load(); jrq != nil {
		return jrq.Method, nil
	}

	if len(r.body) > 0 {
		// resolveJsonRpc rather than JsonRpcRequest: this runs before routing,
		// which still reads the raw body.
		jrq, err := r.resolveJsonRpc()
		if err != nil {
			return "", err
		}
		return jrq.Method, nil
	}

	return "", NewErrJsonRpcRequestUnresolvableMethod(r.body)
}

// NetworkIdHint returns the "networkId" member carried in the request body,
// which routes a request that gave no /<architecture>/<chainId> path segments.
// Empty when the body omitted it or could not be parsed — an unparseable body
// is reported by Validate, well before routing asks.
func (r *NormalizedRequest) NetworkIdHint() string {
	if r == nil {
		return ""
	}
	if jrq := r.jsonRpcRequest.Load(); jrq != nil {
		return jrq.NetworkIdHint()
	}
	if len(r.body) == 0 {
		return ""
	}
	jrq, err := r.resolveJsonRpc()
	if err != nil {
		return ""
	}
	return jrq.NetworkIdHint()
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

// SetAgentName stores the agent name directly without HTTP-specific parsing
func (r *NormalizedRequest) SetAgentName(name string) {
	if r == nil || name == "" {
		return
	}
	r.agentName.Store(name)
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

// EvmHead is the Head r took, nil before it took one.
func (r *NormalizedRequest) EvmHead() *EvmHead {
	if r == nil {
		return nil
	}
	return r.evmHead.Load()
}

// TakeEvmHead makes h r's Head unless r took one already, and returns r's
// Head: concurrent first readers of one request all get the same value.
func (r *NormalizedRequest) TakeEvmHead(h *EvmHead) *EvmHead {
	if r.evmHead.CompareAndSwap(nil, h) {
		return h
	}
	return r.evmHead.Load()
}

// HeadRefusal is the refusal verdict r took, nil before it took one.
func (r *NormalizedRequest) HeadRefusal() *HeadRefusal {
	if r == nil {
		return nil
	}
	return r.headRefusal.Load()
}

// TakeHeadRefusal makes v r's verdict unless r took one already, and
// returns r's verdict.
func (r *NormalizedRequest) TakeHeadRefusal(v *HeadRefusal) *HeadRefusal {
	if r.headRefusal.CompareAndSwap(nil, v) {
		return v
	}
	return r.headRefusal.Load()
}

// TagStateSkips is r's tag-state decision: the upstream ids the gate skips,
// nil unless r is a family-3 request whose "latest" reaches upstreams as the
// tag (spec §6.11).
func (r *NormalizedRequest) TagStateSkips() map[string]bool {
	if r == nil {
		return nil
	}
	if m := r.tagStateSkips.Load(); m != nil {
		return *m
	}
	return nil
}

// TakeTagStateSkips makes skips r's tag-state decision unless r took one
// already, and returns r's decision: one per request, every attempt and
// hedge reads it.
func (r *NormalizedRequest) TakeTagStateSkips(skips map[string]bool) map[string]bool {
	if r.tagStateSkips.CompareAndSwap(nil, &skips) {
		return skips
	}
	return *r.tagStateSkips.Load()
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

	// Carry the auth strategy stamp so a derived request (e.g. an
	// eth_getLogs split sub-request) selects the same consensus policy as its
	// parent instead of falling back to default (Copilot finding: derived
	// requests got a fresh exec state and lost the strategy name).
	if name, ok := source.ExecState().AuthStrategy(); ok {
		if p := source.ExecState().authStrategyPtr(); p != nil {
			r.ExecState().SetAuthStrategy(p)
		} else {
			r.ExecState().SetAuthStrategy(&name)
		}
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

// SetNetworkUpstreams stores the network-registry upstream list for this
// request (cordon-inclusive, method-eligible). The consensus-policy selector
// uses it to observe punishment / operator cordons that selection already
// dropped from Upstreams(). Consensus reorder must not overwrite this list.
// An empty slice is a real snapshot (e.g. nothing method-eligible) and must
// not be treated as "never set".
func (r *NormalizedRequest) SetNetworkUpstreams(upstreams []Upstream) {
	if r == nil {
		return
	}
	r.upstreamMutex.Lock()
	defer r.upstreamMutex.Unlock()
	r.networkUpstreamList = upstreams
	r.networkUpstreamsSet = true
}

// HasNetworkUpstreams reports whether SetNetworkUpstreams was called, even
// when the stored snapshot is empty. The selector falls back to Upstreams()
// only when this is false (unit tests / non-network paths).
func (r *NormalizedRequest) HasNetworkUpstreams() bool {
	if r == nil {
		return false
	}
	r.upstreamMutex.Lock()
	defer r.upstreamMutex.Unlock()
	return r.networkUpstreamsSet
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

// Upstreams returns a snapshot copy of the ordered upstream list set for
// this request. Safe for concurrent use; mutating the returned slice does
// not affect the request (use SetUpstreams to install a new order).
func (r *NormalizedRequest) Upstreams() []Upstream {
	if r == nil {
		return nil
	}
	r.upstreamMutex.Lock()
	defer r.upstreamMutex.Unlock()
	if len(r.upstreamList) == 0 {
		return nil
	}
	out := make([]Upstream, len(r.upstreamList))
	copy(out, r.upstreamList)
	return out
}

// MarkEscalatedToFallbacks reports true only for the first caller, so a
// request escalates to the fallback tier at most once.
func (r *NormalizedRequest) MarkEscalatedToFallbacks() bool {
	if r == nil {
		return false
	}
	return r.escalatedToFallbacks.CompareAndSwap(false, true)
}

// NetworkUpstreams returns a snapshot of the network-registry list (cordon-
// inclusive, method-eligible) set via SetNetworkUpstreams. Returns nil when
// unset; returns an empty (non-nil) slice when set to empty so callers can
// distinguish "no registry snapshot" from "registry had zero eligible
// upstreams". Safe for concurrent use.
func (r *NormalizedRequest) NetworkUpstreams() []Upstream {
	if r == nil {
		return nil
	}
	r.upstreamMutex.Lock()
	defer r.upstreamMutex.Unlock()
	if !r.networkUpstreamsSet {
		return nil
	}
	out := make([]Upstream, len(r.networkUpstreamList))
	copy(out, r.networkUpstreamList)
	return out
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

	upstreamCount := len(r.upstreamList)

	// Simple round-robin selection. The outer loop in networks.go is capped to prevent
	// infinite loops. Retryable errors are removed from ConsumedUpstreams by
	// MarkUpstreamCompleted, allowing re-selection on subsequent iterations.
	for range upstreamCount {
		idx := r.UpstreamIdx % uint32(upstreamCount)
		r.UpstreamIdx++ // Guaranteed increment for next caller

		if upstream := r.upstreamList[idx]; r.reserveLocked(upstream) {
			return upstream, nil
		}
	}

	return nil, NewErrNoUpstreamsLeftToSelect(
		r,
		"no more non-consumed or working upstreams left",
	)
}

// ReserveUpstream reserves upstream for an attempt when NextUpstream could
// select it: a caller with its own order (the refusal band's head-first
// retry) picks only what NextUpstream would.
func (r *NormalizedRequest) ReserveUpstream(upstream Upstream) bool {
	if r == nil {
		return false
	}
	r.upstreamMutex.Lock()
	defer r.upstreamMutex.Unlock()
	return r.reserveLocked(upstream)
}

// reserveLocked reserves upstream when it is selectable and not consumed
// (in flight, or completed this round).
func (r *NormalizedRequest) reserveLocked(upstream Upstream) bool {
	if !r.matchesUseUpstreamLocked(upstream) || r.failedPermanentlyLocked(upstream) {
		return false
	}
	_, loaded := r.ConsumedUpstreams.LoadOrStore(upstream, true)
	return !loaded
}

// matchesUseUpstreamLocked reports whether upstream matches the request's
// UseUpstream selector (its id or any of its tags, e.g.
// `use-upstream=family:systx`); every upstream does without one.
func (r *NormalizedRequest) matchesUseUpstreamLocked(upstream Upstream) bool {
	if r.directives == nil || r.directives.UseUpstream == "" {
		return true
	}
	match, err := UpstreamMatchesSelector(r.directives.UseUpstream, upstream)
	return err == nil && match
}

// failedPermanentlyLocked reports whether upstream already answered with an
// error no retry changes (method ignored, execution exception, etc.).
// Retryable errors, missing data included, leave it selectable.
func (r *NormalizedRequest) failedPermanentlyLocked(upstream Upstream) bool {
	prevErr, exists := r.ErrorsByUpstream.Load(upstream)
	if !exists {
		return false
	}
	pe, ok := prevErr.(error)
	return ok && !IsRetryableTowardsUpstream(pe)
}

// AllUpstreamsFailedPermanently reports whether every upstream this request
// may select has already answered with an error that NextUpstream will not
// re-select (an execution exception, an unsupported method). A retry or hedge
// can then draw no upstream at all, so it cannot change the outcome.
func (r *NormalizedRequest) AllUpstreamsFailedPermanently() bool {
	if r == nil {
		return false
	}
	r.upstreamMutex.Lock()
	defer r.upstreamMutex.Unlock()

	selectable := 0
	for _, upstream := range r.upstreamList {
		if !r.matchesUseUpstreamLocked(upstream) {
			continue
		}
		selectable++
		if !r.failedPermanentlyLocked(upstream) {
			return false
		}
	}
	return selectable > 0
}

func (r *NormalizedRequest) MarkUpstreamCompleted(ctx context.Context, upstream Upstream, resp *NormalizedResponse, err error) {
	r.upstreamMutex.Lock()
	defer r.upstreamMutex.Unlock()

	hasResponse := resp != nil && !resp.IsObjectNull(ctx)

	// Cancelled hedge — upstream wasn't really tried, just free reservation.
	if err != nil && HasErrorCode(err, ErrCodeEndpointRequestCanceled) {
		r.ConsumedUpstreams.Delete(upstream)
		return
	}

	// Store errors/empty-results for reporting (error messages, metrics).
	// Used by ErrUpstreamsExhausted, the "wrong empty" metric, and error
	// state labels. Also used as a within-round gate in NextUpstream for
	// permanent errors (method ignored, execution exception).
	if err != nil {
		r.ErrorsByUpstream.Store(upstream, err)
		r.headsAtError.Store(upstream, latestBlockOf(upstream))
	} else if resp != nil && resp.IsResultEmptyish(ctx) {
		jr, jrErr := resp.JsonRpcResponse(ctx)
		if jr == nil {
			r.ErrorsByUpstream.Store(upstream, NewErrEndpointMissingData(fmt.Errorf("upstream responded emptyish but cannot extract json-rpc response: %v", jrErr), upstream))
		} else {
			r.ErrorsByUpstream.Store(upstream, NewErrEndpointMissingData(fmt.Errorf("upstream responded emptyish: %v", jr.GetResultString()), upstream))
		}
		r.headsAtError.Store(upstream, latestBlockOf(upstream))
	}

	// Free from ConsumedUpstreams if the upstream can be retried on a
	// subsequent failsafe round:
	// - No response (pre-flight skip like block unavailable): retryable
	// - Error that's retryable toward the upstream: transient failure
	// - Empty result: data may appear after emptyResultDelay
	// Permanent errors (method ignored, execution exception with response)
	// stay consumed. The ErrorsByUpstream gate in NextUpstream provides
	// additional within-round protection for those.
	isEmptyResult := hasResponse && err == nil && resp != nil && resp.IsResultEmptyish(ctx)
	canReUse := !hasResponse || (err != nil && IsRetryableTowardsUpstream(err)) || isEmptyResult
	if canReUse {
		r.ConsumedUpstreams.Delete(upstream)
	}
}

// HeadAtError returns the upstream's latest block as eRPC knew it when the
// upstream's ErrorsByUpstream entry was stored; 0 when unknown.
func (r *NormalizedRequest) HeadAtError(upstream Upstream) int64 {
	if v, ok := r.headsAtError.Load(upstream); ok {
		return v.(int64)
	}
	return 0
}

func latestBlockOf(upstream Upstream) int64 {
	eu, ok := upstream.(EvmUpstream)
	if !ok {
		return 0
	}
	sp := eu.EvmStatePoller()
	if sp == nil || sp.IsObjectNull() {
		return 0
	}
	return sp.LatestBlock()
}
