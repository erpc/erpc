// Package simulator implements the eRPC traffic simulator backend: it
// boots a real eRPC instance against synthetic ("fake") upstreams, drives
// it with a synthetic traffic generator, captures every request's
// lifecycle, and streams the resulting state + traces to a browser UI
// over a WebSocket.
//
// The simulator's job is to make every dot in the browser visualization
// a real eRPC request — not a model. The traffic generator builds a real
// `*common.NormalizedRequest`, calls `Network.Forward(ctx, req)`, and
// after the call returns reads `req.ExecState().UpstreamAttemptLog()`
// for the per-upstream attempt timeline. The selection-policy lives in
// the real `internal/policy` engine; the failsafe stack is real; the
// circuit-breaker / retry / hedge are real. The only synthetic pieces
// are the upstreams (loopback HTTP servers whose behaviour is driven by
// the operator's sliders).
package simulator

import (
	"sync/atomic"
	"time"
)

// UpstreamKnobs is the per-upstream synthetic behaviour the operator
// controls from the UI. All fields are read on every fake-upstream
// request, so updates take effect on the NEXT inbound call.
//
// Latency model:
//
//	delay = max(2ms, base + gaussian()*jitter)
//
// Outcome model (rolled in this order, first-match wins):
//
//	1. r < throttleRate  →  HTTP 429
//	2. r < +timeoutRate  →  server hangs past the eRPC timeout
//	3. r < +errorRate    →  HTTP 500 / JSON-RPC error
//	otherwise            →  successful JSON-RPC response
//
// BlockLag is reported via `eth_blockNumber` responses (subtracted from
// the simulator's notion of head height). `Available=false` makes the
// fake server return immediate 503s.
type UpstreamKnobs struct {
	// Identity (immutable; configured at bootstrap).
	ID       string   `json:"id"`
	Vendor   string   `json:"vendor"`
	Tags     []string `json:"tags"`
	Endpoint string   `json:"endpoint"` // The synthetic loopback URL eRPC uses.

	// Behaviour (live-tunable from the UI).
	BaseLatencyMs float64 `json:"base"`         // ms; mean
	JitterMs      float64 `json:"jitter"`       // ms; gaussian sigma
	ErrorRate     float64 `json:"error"`        // [0..1]
	TimeoutRate   float64 `json:"timeoutRate"`  // [0..1]
	ThrottleRate  float64 `json:"throttleRate"` // [0..1]
	BlockLag      int     `json:"blockLag"`     // blocks behind hub head
	BlockTimeMs   int     `json:"blockTimeMs"`  // ms between block bumps (per-upstream synthetic chain)
	Available     bool    `json:"available"`    // false → immediate 503
	// DataAvailability multiplies the per-method baseline "has data?"
	// probability for sparse-data methods (eth_getTransactionReceipt,
	// eth_getBlockByHash, eth_getLogs, eth_getTransactionByHash).
	// 1.0 (default) = use the method's baseline as-is; 0.5 = upstream
	// is twice as likely to return empty/missing; 0 = always misses.
	// Models real-world vendor variance: some indexers are slower
	// and miss more recent receipts than others.
	DataAvailability float64 `json:"dataAvailability"`

	// InjectFailureUntilMs forces error+timeout to ~1.0 until this
	// wall-clock time (unix-millis). 0 = no override.
	InjectFailureUntilMs int64 `json:"injectFailureUntilMs,omitempty"`
}

// AppliesInjectedFailure returns true when the upstream is currently
// inside an "inject 60s failure" window. Called by the fake-upstream
// handler before rolling each request's outcome.
func (k *UpstreamKnobs) AppliesInjectedFailure(now time.Time) bool {
	return k.InjectFailureUntilMs > 0 && now.UnixMilli() < k.InjectFailureUntilMs
}

// MethodMix is one row of the operator-edited method-mix table. The
// traffic generator samples a method per request, weighted by `Weight`.
type MethodMix struct {
	Name      string `json:"name"`
	Weight    int    `json:"weight"`
	Cacheable bool   `json:"cacheable"`
	Finality  string `json:"finality"` // "realtime" | "unfinalized" | "finalized"
}

// TrafficShape is the temporal distribution of synthetic requests.
type TrafficShape string

const (
	ShapePoisson  TrafficShape = "poisson"
	ShapeConstant TrafficShape = "constant"
	ShapeBursty   TrafficShape = "bursty"
)

// TraceEvent is a sampled per-request trace shipped to the browser for
// visualization. One TraceEvent → one dot animation in the flow stage.
//
// Built from `req.ExecState().UpstreamAttemptLog()` after a real
// `Network.Forward()` returns, so the contents are real eRPC behaviour,
// not a model.
type TraceEvent struct {
	ID         uint64    `json:"id"`
	StartedAt  time.Time `json:"ts"`
	Method     string    `json:"method"`
	Outcome    string    `json:"outcome"` // "ok"|"retry-ok"|"hedge-win"|"fail"|"cache-hit"
	Winner     string    `json:"winner,omitempty"`
	DurationMs float64   `json:"duration"`
	UsedHedge  bool      `json:"usedHedge,omitempty"`
	UsedRetry  bool      `json:"usedRetry,omitempty"`
	UsedCache  bool      `json:"cacheHit,omitempty"`
	// Consensus bookkeeping pulled from `ExecState.Snapshot()`:
	//   * ConsensusSlots   — # of consensus participants spawned
	//   * ConsensusDisputes — # of dispute events
	//   * ConsensusLowParts — # of low-participants events
	ConsensusSlots     int `json:"consensusSlots,omitempty"`
	ConsensusDisputes  int `json:"consensusDisputes,omitempty"`
	ConsensusLowParts  int `json:"consensusLowParts,omitempty"`

	// RequestError is the FINAL error string returned by
	// `Network.Forward` to the caller. This is what the client
	// actually sees — distinct from the per-attempt error strings,
	// which only describe one upstream call. For consensus-dispute
	// requests, all per-attempt outcomes can be "ok" while the
	// overall request fails here with an ErrConsensus... wrapper.
	// Empty when the request succeeded.
	RequestError string `json:"requestError,omitempty"`

	// ResponseBody is a truncated preview of the JSON-RPC response
	// the caller received on success. Capped at 8KB so a giant
	// getLogs result doesn't blow up the WS frame budget. Empty on
	// failure (use RequestError instead).
	ResponseBody string `json:"responseBody,omitempty"`

	// Selection trail: the upstreams the policy engine returned for this
	// (network, method), in order. Excluded entries (CB-open, unavailable)
	// are appended at the end with a non-empty Reason.
	Sel []TraceSelectEntry `json:"sel,omitempty"`

	// Attempts: every physical Forward call recorded by the executors.
	Attempts []TraceAttempt `json:"attempts,omitempty"`
}

// TraceSelectEntry is one entry in the selection trail.
type TraceSelectEntry struct {
	ID       string  `json:"id"`
	Idx      int     `json:"idx"`
	Score    float64 `json:"score,omitempty"`
	Excluded bool    `json:"excluded,omitempty"`
	Reason   string  `json:"reason,omitempty"`
}

// TraceAttempt mirrors common.UpstreamAttempt in a UI-friendly shape.
// The extra metadata (selectionReason, isHedge, isRetry, attemptIdx)
// lets the drawer explain WHY each attempt fired — primary call, the
// Nth retry, a hedge fan-out, a consensus participant, etc.
type TraceAttempt struct {
	ID              string  `json:"id"`
	T0Ms            float64 `json:"t0"` // ms relative to request start
	DurationMs      float64 `json:"dur"`
	Outcome         string  `json:"outcome"`
	Reason          string  `json:"reason,omitempty"` // error detail (truncated)
	Winner          bool    `json:"winner,omitempty"`
	SelectionReason string  `json:"selReason,omitempty"` // primary|retry|hedge|consensus_slot|sweep
	IsHedge         bool    `json:"isHedge,omitempty"`
	IsRetry         bool    `json:"isRetry,omitempty"`
	AttemptIdx      int     `json:"attemptIdx,omitempty"`
}

// PolicyDecisionFrame is the compact WS-wire shape of a single policy
// engine tick — what the operator's "policy history" panel renders as
// one row. Sized to fit hundreds of frames in a buffer without
// breaking the per-frame WS budget; per-upstream input metrics are
// intentionally OMITTED here (they're large and rarely needed for
// "what happened?" diagnosis — the tracker view in each stats frame
// already has the live numbers).
//
// IDs:
//   - DecisionID is the engine's `<network>/<method>/<unix-ms>` slug,
//     used by the frontend to dedupe ticks across multiple stats frames.
//   - TickMs is the tick's wall-clock time in unix-ms, sortable.
//
// Output:
//   - Order is the policy's resulting ordered list (primary first).
//   - Excluded enumerates excluded upstreams + the step/reason that
//     dropped them, when the engine recorded one.
//   - Scores carries the per-upstream score the JS produced this tick.
//
// Diff:
//   - PrimaryChanged is the single most useful "did anything happen?"
//     bit; the UI highlights ticks where this is true.
//   - Added / Removed are the deltas vs the prior tick's ordered set.
//
// Error is the eval-error string (timeout / throw / invalid_return /
// fallback_default) when the tick failed; empty otherwise.
type PolicyDecisionFrame struct {
	DecisionID     string                       `json:"id"`
	TickMs         int64                        `json:"tickMs"`
	EvalDurationUs int64                        `json:"evalDurationUs"`
	NetworkID      string                       `json:"networkId"`
	Method         string                       `json:"method"`
	Order          []string                     `json:"order"`
	Excluded       []PolicyDecisionExcludedRow  `json:"excluded,omitempty"`
	Scores         map[string]float64           `json:"scores,omitempty"`
	PrimaryChanged bool                         `json:"primaryChanged,omitempty"`
	OrderChanged   bool                         `json:"orderChanged,omitempty"`
	Added          []string                     `json:"added,omitempty"`
	Removed        []string                     `json:"removed,omitempty"`
	Error          string                       `json:"error,omitempty"`
}

// PolicyDecisionExcludedRow mirrors policy.ExcludedUpstream for the
// wire — kept separate so we can JSON-tag the fields.
type PolicyDecisionExcludedRow struct {
	ID     string `json:"id"`
	Step   string `json:"step,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// StatsFrame is the periodic 100ms aggregate snapshot. The browser uses
// these to update charts, position pills, score chips, and the live RPS
// counter. Cheap to produce because it walks counters held in memory.
type StatsFrame struct {
	TickMs            int64                       `json:"tick"`
	ActualRps         float64                     `json:"actualRps"`
	LastSecondTotal   int                         `json:"lastSecondTotal"`
	LastSecondSucc    int                         `json:"lastSecondSucc"`
	LastSecondErr     int                         `json:"lastSecondErr"`
	LastSecondCache   int                         `json:"lastSecondCache"`
	LastSecondRetryOk int                         `json:"lastSecondRetryOk"` // succeeded via retry
	LastSecondHedge   int                         `json:"lastSecondHedge"`   // succeeded via hedge winner
	LastSecondMiss    int                         `json:"lastSecondMiss"`    // first-attempt empty/missing
	PerUpstreamLastS  map[string]int              `json:"perUpstreamLastS"`
	Upstreams         map[string]UpstreamStatsRow `json:"upstreams"`
	// PolicyLastSwitchMs is the wall-clock time (unix-ms) of the most
	// recent primary-upstream change recorded by the policy engine for
	// the "*" slot. 0 if no switch has happened yet. The frontend uses
	// this to render `stickyPrimary` cooldown remaining ("primary held
	// for Xs / sticky locked for Ys").
	PolicyLastSwitchMs int64          `json:"policyLastSwitchMs,omitempty"`
	Scenario           *ScenarioFrame `json:"scenario,omitempty"`
}

// UpstreamStatsRow is the per-upstream snapshot the UI consumes for the
// position pill, score chip, error sparkline, and tooltip.
//
// EVERY field except Position/SelectionsLast/CBState is read from
// eRPC's health.Tracker — the same object the selection policy's
// `keepHealthy` / `sortByScore` consume on every tick. The UI is a
// pure mirror of policy-visible state; there is no parallel
// simulator-side bookkeeping for health metrics. This means:
//
//   - error rates can differ from "raw failure count / requests": eRPC
//     excludes execution exceptions (JSON-RPC -32000 reverts), client
//     errors, hedge cancellations, etc. from ErrorsTotal because they
//     don't reflect upstream quality. A 20% synthetic error-rate knob
//     produces ~16% observed because 1-in-5 errors are reverts.
//   - throttled rate (429s) is tracked separately from errors.
//   - latency quantiles come from the real exponential-decay quantile
//     estimator, not the simulator's sample buffer.
//
// `PenaltyScore` is computed Go-side using the SAME BALANCED weights
// the JS stdlib's `sortByScore(BALANCED)` uses, so the score chip in
// the UI matches what the policy is ranking on.
type UpstreamStatsRow struct {
	// Health metrics, all from eRPC's health.Tracker:
	ErrorRate       float64 `json:"errorRate"`       // ErrorsTotal / RequestsTotal (policy-visible)
	ThrottleRate    float64 `json:"throttleRate"`    // RemoteRateLimitedTotal / RequestsTotal
	MisbehaviorRate float64 `json:"misbehaviorRate"` // MisbehaviorsTotal / RequestsTotal
	P50Ms           float64 `json:"p50"`             // from ResponseQuantiles (ms)
	P70Ms           float64 `json:"p70"`             // the quantile sortByScore(BALANCED) uses by default
	P90Ms           float64 `json:"p90"`
	P95Ms           float64 `json:"p95"`
	BlockHeadLag           int     `json:"blockHeadLag"`
	FinalizationLag        int     `json:"finalizationLag"`
	BlockHeadLagSeconds    float64 `json:"blockHeadLagSeconds"`    // block-count × tracker's EMA block-time
	FinalizationLagSeconds float64 `json:"finalizationLagSeconds"`
	Cordoned        bool    `json:"cordoned,omitempty"`
	CordonedReason  string  `json:"cordonedReason,omitempty"`
	RequestsTotal   int64   `json:"requestsTotal"` // sample size in the current window — useful for sparse-data signal
	ErrorsTotal     int64   `json:"errorsTotal"`

	// Composite, computed Go-side from tracker fields using BALANCED weights:
	PenaltyScore float64 `json:"score"` // lower = better; matches policy's sortByScore(BALANCED)

	// Routing state (from policy engine + per-upstream CB):
	CBState        string `json:"cbState"`  // "closed"|"half"|"open"
	Position       int    `json:"position"` // 0-based; -1 if excluded by policy
	SelectionsLast int    `json:"selectionsLast"`
}

// ScenarioFrame is the in-flight scenario broadcast on every StatsFrame.
type ScenarioFrame struct {
	Name       string                `json:"name"`
	ElapsedSec float64               `json:"elapsed"`
	DurationS  float64               `json:"duration"`
	Events     []ScenarioEventBubble `json:"events"`
}

// ScenarioEventBubble is one fired scenario step.
type ScenarioEventBubble struct {
	TSec int    `json:"t"`
	Desc string `json:"d"`
}

// PolicyApplyResult is the response to a `policy.apply` or
// `policy.validate` control message. Success means the JS compiled in
// the real sobek runtime; the apply variant additionally swapped it
// into the running engine. On failure, Error is populated and the
// running policy is unchanged.
type PolicyApplyResult struct {
	Ok           bool   `json:"ok"`
	Error        string `json:"error,omitempty"`
	CompiledAtMs int64  `json:"compiledAt,omitempty"`
}

// ConfigApplyResult is the response to `config.apply` / `config.validate`.
type ConfigApplyResult struct {
	Ok          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
	AppliedAtMs int64  `json:"appliedAt,omitempty"`
}

// Counters is the shared aggregate counter set written by the request
// path and read by the stats frame builder. All atomics so the request
// path is lock-free.
type Counters struct {
	Total       atomic.Int64 // requests started (post-cache)
	Success     atomic.Int64
	Failure     atomic.Int64
	CacheHit    atomic.Int64
	HedgeWin    atomic.Int64
	RetryOk     atomic.Int64
	StartedAtNs atomic.Int64
}
