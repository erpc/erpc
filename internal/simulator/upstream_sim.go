package simulator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// UpstreamHub runs ONE HTTP server multiplexed across N synthetic
// upstreams keyed by URL path (`/sim/<id>`). The eRPC instance the
// simulator boots points its real upstreams at these URLs, so every
// JSON-RPC call goes through the real network → upstream → http
// transport path and ends up here.
//
// Each handler reads its UpstreamKnobs under an RLock per request, so
// the operator's slider changes take effect on the next request without
// restarting anything.
//
// Block model: every upstream advances its own synthetic chain at its
// own `BlockTimeMs` rate, with `BlockLag` subtracted from the head at
// response time. So tuning either knob changes the upstream's view of
// the chain without affecting its peers — exactly the dynamic that
// motivates `keepHealthy({ maxBlockHeadLag })` in the real default
// policy.
//
// Block timestamps: every `eth_getBlockByNumber` / `eth_getBlockByHash`
// response uses `simGenesisUnix + block * simBlockTimeSec` so that
// consecutive blocks are spaced by a stable interval. The eRPC health
// tracker's block-time EMA samples these deltas to estimate "seconds
// per block" — without a synthetic-but-consistent timestamp the EMA
// converges to nonsense and `blockSecondsLagAbove(...)` policy
// predicates never fire.
const (
	// 12 second blocks. Matches Ethereum mainnet's nominal rate.
	// Per-upstream `BlockTimeMs` knobs control HEAD ADVANCEMENT
	// (how fast each upstream's view of tip moves) but the chain's
	// canonical block-time-on-the-wire is one number across upstreams
	// — a real chain has ONE block N, with ONE timestamp.
	simBlockTimeSec int64 = 12
	// Genesis offset. Picked so a block in the low millions yields a
	// timestamp in the last decade — feels realistic when an operator
	// inspects raw responses in the dumper. Absolute value doesn't
	// matter for the EMA (it only reads diffs).
	simGenesisUnix int64 = 1_438_269_973 // Aug 2015, near Eth mainnet genesis
)

// blockTimestamp returns the synthetic Unix seconds for `block`. Same
// value for the same block across every upstream — required for the
// network's block-time EMA to converge.
func blockTimestamp(block int64) int64 {
	return simGenesisUnix + block*simBlockTimeSec
}

// SVM slot model: 400ms slots from a Solana-mainnet-ish genesis. Same
// rationale as blockTimestamp — deltas must be stable so lag math
// converges. The per-upstream `BlockTimeMs` knob (defaulted to 400 for
// `arch:svm`-tagged upstreams in defaultKnobsFor) drives head
// ADVANCEMENT; this is the canonical on-the-wire timestamp per slot.
const svmGenesisUnix int64 = 1_584_332_940 // Mar 2020, near Solana mainnet genesis

func svmSlotTimestamp(slot int64) int64 {
	return svmGenesisUnix + (slot*2)/5 // 400ms per slot
}

// svmCommitment extracts the `commitment` field from a Solana-style
// params array (the options object may sit at any position). Returns
// "finalized" when absent — Solana's documented default.
func svmCommitment(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "finalized"
	}
	var ps []any
	if err := json.Unmarshal(raw, &ps); err != nil {
		return "finalized"
	}
	for _, p := range ps {
		if m, ok := p.(map[string]any); ok {
			if c, ok := m["commitment"].(string); ok && c != "" {
				return c
			}
		}
	}
	return "finalized"
}

// firstNumberParam returns the first numeric param — Solana methods
// address blocks by bare slot number where EVM uses hex strings.
func firstNumberParam(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var ps []any
	if err := json.Unmarshal(raw, &ps); err != nil || len(ps) == 0 {
		return 0, false
	}
	f, ok := ps[0].(float64)
	if !ok {
		return 0, false
	}
	return int64(f), true
}

// b58ish renders a deterministic base58-alphabet string of length n
// from a seed — shaped like Solana blockhashes (44) and signatures (87).
func b58ish(seed uint64, n int) string {
	const alpha = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	b := make([]byte, n)
	x := seed | 1
	for i := range b {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		b[i] = alpha[x%58]
	}
	return string(b)
}

type UpstreamHub struct {
	mu    sync.RWMutex
	knobs map[string]*UpstreamKnobs

	heads   sync.Map // map[string]*upstreamHead — per-upstream chain view
	hubHead atomic.Int64 // ceiling shared across upstreams (set from max of all heads)

	server   *http.Server
	listener net.Listener
}

// upstreamHead tracks one upstream's view of the chain head. Advanced
// by a per-upstream ticker on the cadence the operator set in
// `BlockTimeMs`. The handler subtracts `BlockLag` at read time.
type upstreamHead struct {
	headBlock atomic.Int64
}

// NewUpstreamHub binds a TCP listener on `bindAddr` (typically
// "127.0.0.1:0" to pick a free port) and returns a hub ready to serve.
// Knobs are added via AddUpstream after construction.
func NewUpstreamHub(bindAddr string) (*UpstreamHub, error) {
	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return nil, fmt.Errorf("simulator: bind fake-upstream listener: %w", err)
	}
	h := &UpstreamHub{
		knobs:    make(map[string]*UpstreamKnobs),
		listener: ln,
	}
	h.hubHead.Store(22_000_000) // arbitrary plausible mainnet head
	mux := http.NewServeMux()
	mux.HandleFunc("/sim/", h.handle)
	h.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	// One advancement ticker for the entire hub. It walks the knob map
	// each tick and advances each upstream's head iff enough time has
	// elapsed since its last bump. This is much cheaper than running a
	// goroutine per upstream and keeps a single source of timing truth.
	return h, nil
}

// Addr returns the loopback address the hub bound to, e.g.
// "127.0.0.1:53412". Used to assemble each upstream's endpoint URL.
func (h *UpstreamHub) Addr() string {
	return h.listener.Addr().String()
}

// Serve blocks until ctx is cancelled or the listener closes. Run in a
// goroutine.
func (h *UpstreamHub) Serve(ctx context.Context) error {
	go h.advanceBlocks(ctx)
	go func() { // #nosec G118 -- intentional: ctx is already done; background is correct for shutdown
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = h.server.Shutdown(shutdownCtx)
	}()
	if err := h.server.Serve(h.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// advanceBlocks ticks every 250ms and bumps each upstream's head
// according to its `BlockTimeMs`. Bookkeeping (last-tick wall time) is
// stored on the per-upstream head struct so re-tuning `BlockTimeMs`
// live just changes the next-tick cadence — no restart required.
func (h *UpstreamHub) advanceBlocks(ctx context.Context) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	type bookkeeping struct{ lastBumpMs int64 }
	book := map[string]*bookkeeping{}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			nowMs := now.UnixMilli()
			h.mu.RLock()
			snap := make(map[string]int, len(h.knobs))
			for id, k := range h.knobs {
				bt := k.BlockTimeMs
				if bt <= 0 {
					bt = 12_000
				}
				snap[id] = bt
			}
			h.mu.RUnlock()
			maxHead := h.hubHead.Load()
			for id, bt := range snap {
				bk, ok := book[id]
				if !ok {
					bk = &bookkeeping{lastBumpMs: nowMs}
					book[id] = bk
					h.headFor(id) // ensure head struct exists
				}
				steps := int((nowMs - bk.lastBumpMs) / int64(bt))
				if steps <= 0 {
					continue
				}
				bk.lastBumpMs += int64(steps) * int64(bt)
				newHead := h.headFor(id).headBlock.Add(int64(steps))
				if newHead > maxHead {
					maxHead = newHead
				}
			}
			h.hubHead.Store(maxHead)
		}
	}
}

// headFor returns the per-upstream head struct, creating one (seeded
// at the current hub ceiling) on first read.
func (h *UpstreamHub) headFor(id string) *upstreamHead {
	if v, ok := h.heads.Load(id); ok {
		return v.(*upstreamHead)
	}
	uh := &upstreamHead{}
	uh.headBlock.Store(h.hubHead.Load())
	actual, _ := h.heads.LoadOrStore(id, uh)
	return actual.(*upstreamHead)
}

// svmSlotFor maps this upstream's synthetic head to a Solana slot at
// the given commitment: head-BlockLag is the PROCESSED slot; confirmed
// trails it by 2 and finalized by 32 (typical mainnet offsets). So the
// BlockLag knob means "slots behind" for SVM upstreams, and the
// selection policy's blockNumberLagAbove(...) predicate reads slot lag.
func (h *UpstreamHub) svmSlotFor(k *UpstreamKnobs, commitment string) int64 {
	s := h.headFor(k.ID).headBlock.Load() - int64(k.BlockLag)
	switch commitment {
	case "processed":
	case "confirmed":
		s -= 2
	default: // finalized — Solana's default when commitment is omitted
		s -= 32
	}
	if s < 0 {
		return 0
	}
	return s
}

// AddUpstream registers (or replaces) one upstream's knobs.
func (h *UpstreamHub) AddUpstream(k UpstreamKnobs) {
	h.mu.Lock()
	defer h.mu.Unlock()
	copy := k
	h.knobs[k.ID] = &copy
}

// RefreshIdentity updates the vendor / tags of an existing knob entry
// without touching its behavioural fields. Called on every config
// reload so the YAML's tag changes propagate even when the operator
// keeps the same upstream ID.
func (h *UpstreamHub) RefreshIdentity(id, vendor string, tags []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if k, ok := h.knobs[id]; ok {
		k.Vendor = vendor
		k.Tags = append(k.Tags[:0:0], tags...)
	}
}

// RemoveUpstream drops an entry. Called when the YAML config no longer
// declares an upstream that was previously present.
func (h *UpstreamHub) RemoveUpstream(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.knobs, id)
}

// UpdateKnob applies a partial patch to one upstream's knobs. Only
// non-nil pointer fields in `patch` overwrite the existing value. Returns
// the updated snapshot for the WS echo back to the client.
func (h *UpstreamHub) UpdateKnob(id string, patch UpstreamKnobPatch) (UpstreamKnobs, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k, ok := h.knobs[id]
	if !ok {
		return UpstreamKnobs{}, false
	}
	if patch.BaseLatencyMs != nil {
		k.BaseLatencyMs = *patch.BaseLatencyMs
	}
	if patch.JitterMs != nil {
		k.JitterMs = *patch.JitterMs
	}
	if patch.ErrorRate != nil {
		k.ErrorRate = *patch.ErrorRate
	}
	if patch.TimeoutRate != nil {
		k.TimeoutRate = *patch.TimeoutRate
	}
	if patch.ThrottleRate != nil {
		k.ThrottleRate = *patch.ThrottleRate
	}
	if patch.BlockLag != nil {
		k.BlockLag = *patch.BlockLag
	}
	if patch.Available != nil {
		k.Available = *patch.Available
	}
	if patch.BlockTimeMs != nil {
		k.BlockTimeMs = *patch.BlockTimeMs
	}
	if patch.DataAvailability != nil {
		k.DataAvailability = *patch.DataAvailability
	}
	if patch.InjectFailureUntilMs != nil {
		k.InjectFailureUntilMs = *patch.InjectFailureUntilMs
	}
	return *k, true
}

// Snapshot returns a stable copy of all knobs for outbound state frames.
func (h *UpstreamHub) Snapshot() []UpstreamKnobs {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]UpstreamKnobs, 0, len(h.knobs))
	for _, k := range h.knobs {
		out = append(out, *k)
	}
	return out
}

// UpstreamKnobPatch is a partial update from the UI. Nil fields are
// untouched; non-nil fields are applied.
type UpstreamKnobPatch struct {
	BaseLatencyMs        *float64 `json:"base,omitempty"`
	JitterMs             *float64 `json:"jitter,omitempty"`
	ErrorRate            *float64 `json:"error,omitempty"`
	TimeoutRate          *float64 `json:"timeoutRate,omitempty"`
	ThrottleRate         *float64 `json:"throttleRate,omitempty"`
	BlockLag             *int     `json:"blockLag,omitempty"`
	BlockTimeMs          *int     `json:"blockTimeMs,omitempty"`
	Available            *bool    `json:"available,omitempty"`
	DataAvailability     *float64 `json:"dataAvailability,omitempty"`
	InjectFailureUntilMs *int64   `json:"injectFailureUntilMs,omitempty"`
}

// ---------- HTTP handler -----------------------------------------------

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

func (h *UpstreamHub) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// path is /sim/<id>
	id := strings.TrimPrefix(r.URL.Path, "/sim/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	h.mu.RLock()
	k, ok := h.knobs[id]
	var snap UpstreamKnobs
	if ok {
		snap = *k
	}
	h.mu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}

	if !snap.Available {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","error":{"code":-32099,"message":"upstream unavailable"}}`)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Handle a batch by routing each entry through respondOne and
	// stitching the results back together. eRPC issues both forms.
	trimmed := bytes.TrimSpace(body)
	now := time.Now()
	injected := snap.AppliesInjectedFailure(now)

	if len(trimmed) > 0 && trimmed[0] == '[' {
		var batch []jsonRPCRequest
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			http.Error(w, "bad batch: "+err.Error(), http.StatusBadRequest)
			return
		}
		out := make([]json.RawMessage, 0, len(batch))
		for _, req := range batch {
			resp, status := h.respondOne(r.Context(), &snap, injected, req)
			if status >= 500 || status == http.StatusTooManyRequests {
				// the entire batch fails on the first transport-layer error
				w.WriteHeader(status)
				return
			}
			b, _ := json.Marshal(resp)
			out = append(out, b)
		}
		full := []byte("[")
		for i, b := range out {
			if i > 0 {
				full = append(full, ',')
			}
			full = append(full, b...)
		}
		full = append(full, ']')
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(full)
		return
	}

	var req jsonRPCRequest
	if err := json.Unmarshal(trimmed, &req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	resp, status := h.respondOne(r.Context(), &snap, injected, req)
	if status >= 500 || status == http.StatusTooManyRequests {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","error":{"code":-32603,"message":"upstream error"}}`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(resp)
	_, _ = w.Write(b)
}

// respondOne sleeps for the simulated latency, rolls the outcome, and
// returns either a status code ≥ 500 (transport-layer failure) or a
// well-formed JSON-RPC response. The simulator's eRPC instance will see
// a real HTTP-layer outcome that matches the operator's knobs.
func (h *UpstreamHub) respondOne(ctx context.Context, k *UpstreamKnobs, injected bool, req jsonRPCRequest) (*jsonRPCResponse, int) {
	delay := sampleLatency(k)

	errRate := k.ErrorRate
	timeoutRate := k.TimeoutRate
	throttleRate := k.ThrottleRate
	if injected {
		// "Inject failure" overrides the dials toward total degradation
		// without permanently overwriting the operator's slider values.
		errRate = math.Max(errRate, 0.9)
		timeoutRate = math.Max(timeoutRate, 0.3)
	}
	// math/rand is the correct tool for SIMULATION randomness
	// (throttle/timeout/error dispatch on synthetic upstreams). The
	// simulator is a developer harness with no auth/crypto context;
	// crypto/rand is for security tokens, not statistical sampling.
	roll := rand.Float64() // #nosec G404

	// Throttle BEFORE sleeping — represents the rate-limiter rejecting
	// the request at the door (fast 429).
	if roll < throttleRate {
		return nil, http.StatusTooManyRequests
	}

	// Sleep for the simulated work. Context cancellation (eRPC timeout)
	// short-circuits the sleep so a cancelled request returns promptly.
	select {
	case <-ctx.Done():
		// Outcome the eRPC executor will see is "timeout" because we
		// never wrote a response; let the connection drop.
		return nil, 0
	case <-time.After(delay):
	}

	roll -= throttleRate
	if roll < timeoutRate {
		// Hang past the eRPC per-attempt timeout. We do this by sleeping
		// for the cancelled-context's remaining deadline (capped at 30s
		// so a misconfigured simulator doesn't leak goroutines).
		<-time.After(30 * time.Second)
		return nil, 0
	}
	roll -= timeoutRate
	if roll < errRate {
		// Spread the synthetic error across a realistic mix so the
		// visualization shows varied failure modes instead of every
		// error being identical. The dispatch is rolled per-call so
		// over time the operator sees a representative blend.
		errMode := rand.IntN(5) // #nosec G404 — simulator-only error-mode dispatch
		switch errMode {
		case 0: // server error 500
			return &jsonRPCResponse{JSONRPC: "2.0", ID: req.ID,
				Error: &jsonRPCError{Code: -32603, Message: "synthetic internal error"}}, http.StatusInternalServerError
		case 1: // bad gateway 502
			return nil, http.StatusBadGateway
		case 2: // service unavailable 503
			return nil, http.StatusServiceUnavailable
		case 3: // JSON-RPC server-side execution revert
			return &jsonRPCResponse{JSONRPC: "2.0", ID: req.ID,
				Error: &jsonRPCError{Code: -32000, Message: "execution reverted"}}, http.StatusOK
		default: // generic JSON-RPC internal error
			return &jsonRPCResponse{JSONRPC: "2.0", ID: req.ID,
				Error: &jsonRPCError{Code: -32603, Message: "upstream backend error"}}, http.StatusOK
		}
	}

	return h.synthResult(k, req), http.StatusOK
}

// synthResult returns a plausible-looking response for the methods eRPC
// expects to see frequently. We don't need to be schema-perfect — eRPC
// only validates structure for a handful of methods, and the simulator's
// goal is not data correctness but routing realism.
//
// Data-availability model: for "lookup" methods (receipt, block-by-hash,
// random-historical block), we deterministically vary whether THIS
// upstream has the requested data using a hash of (param + upstream id).
// Same query against the same upstream always returns the same answer
// (so caching behaviour can be tested), but different upstreams disagree
// — which is exactly what happens in production with patchy indexing.
func (h *UpstreamHub) synthResult(k *UpstreamKnobs, req jsonRPCRequest) *jsonRPCResponse {
	resp := &jsonRPCResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "eth_blockNumber":
		head := h.headFor(k.ID).headBlock.Load() - int64(k.BlockLag)
		resp.Result = json.RawMessage(fmt.Sprintf(`"0x%x"`, head))
	case "eth_chainId":
		resp.Result = json.RawMessage(`"0x1"`)
	case "net_version":
		resp.Result = json.RawMessage(`"1"`)
	case "eth_syncing":
		resp.Result = json.RawMessage(`false`)
	case "eth_getBalance":
		// Production reality: every address has a balance answer. Even
		// "0x0" is a legitimate result for an unfunded account — it's
		// data, not a miss. We always return a non-zero synthetic
		// balance so eRPC's integrity check doesn't fire empty-result
		// retries for what should be normal traffic. The "miss" model
		// is reserved for methods that genuinely have sparse data in
		// prod (receipts, tx-by-hash, getLogs with weird ranges).
		bal := absHashFromParams(req.Params) % uint64(1_000_000_000_000_000_000)
		if bal == 0 {
			bal = 1
		}
		resp.Result = json.RawMessage(fmt.Sprintf(`"0x%x"`, bal))
	case "eth_getCode":
		// Always return non-empty synthetic bytecode. In prod most
		// addresses an indexer queries are contracts; the few EOA
		// cases that legitimately return "0x" are infrequent and
		// don't typically cascade retries.
		resp.Result = json.RawMessage(fmt.Sprintf(`"0x60806040526004361061%016x"`, absHashFromParams(req.Params)))
	case "eth_call":
		// Always return a synthetic non-empty result so view-function
		// calls don't get classified as empty/missing.
		resp.Result = json.RawMessage(fmt.Sprintf(`"0x%064x"`, absHashFromParams(req.Params)))
	case "eth_estimateGas":
		resp.Result = json.RawMessage(`"0x5208"`)
	case "eth_gasPrice":
		resp.Result = json.RawMessage(`"0x3b9aca00"`)
	case "eth_getTransactionCount":
		// Random per-address nonce so it doesn't look like every
		// address has zero outgoing txs.
		nonce := absHashFromParams(req.Params) % 1000
		resp.Result = json.RawMessage(fmt.Sprintf(`"0x%x"`, nonce))
	case "eth_getBlockByNumber":
		head := h.headFor(k.ID).headBlock.Load() - int64(k.BlockLag)
		block := head
		// "latest" stays at this upstream's head. Specific block
		// numbers are honoured. The upstream "has" any block at or
		// below its current head — historical-block lookups always
		// succeed; future-block lookups (above head + lag) miss.
		if v, ok := firstStringParam(req.Params); ok && strings.HasPrefix(v, "0x") {
			if n, err := parseHex(v); err == nil {
				block = n
				if n > head {
					// Beyond this upstream's view → no data.
					resp.Result = json.RawMessage(`null`)
					return resp
				}
			}
		}
		resp.Result = json.RawMessage(fmt.Sprintf(
			`{"number":"0x%x","hash":"0x%064x","parentHash":"0x%064x","timestamp":"0x%x","transactions":[]}`,
			block, block, block-1, blockTimestamp(block),
		))
	case "eth_getBlockByHash":
		// 90% baseline — most upstreams have indexed any given block
		// hash. The rare 10% miss models a vendor whose index is
		// behind tip. Scaled by the per-upstream DataAvailability knob.
		if !upstreamHasData(k.ID, req.Method, req.Params, scaleAvailability(90, k)) {
			resp.Result = json.RawMessage(`null`)
			return resp
		}
		head := h.headFor(k.ID).headBlock.Load() - int64(k.BlockLag)
		resp.Result = json.RawMessage(fmt.Sprintf(
			`{"number":"0x%x","hash":"0x%064x","parentHash":"0x%064x","timestamp":"0x%x","transactions":[]}`,
			head, head, head-1, blockTimestamp(head),
		))
	case "eth_getTransactionReceipt":
		// 80% baseline — most upstreams have any given recent tx
		// receipt. The 20% miss covers the "slower-indexing vendor /
		// behind tip" case which DOES happen in prod but is the
		// exception, not the rule. Stable per (tx, upstream) so the
		// same hash always gets the same answer. Scaled by the
		// per-upstream DataAvailability knob.
		if !upstreamHasData(k.ID, req.Method, req.Params, scaleAvailability(80, k)) {
			resp.Result = json.RawMessage(`null`)
			return resp
		}
		head := h.headFor(k.ID).headBlock.Load() - int64(k.BlockLag)
		resp.Result = json.RawMessage(fmt.Sprintf(
			`{"transactionHash":"0x%064x","blockNumber":"0x%x","status":"0x1","gasUsed":"0x5208","logs":[]}`,
			absHashFromParams(req.Params), head-1,
		))
	case "eth_getTransactionByHash":
		// 80% baseline — tx-by-hash and receipt-by-hash live in the
		// same index, so similar sparsity. Scaled by knob.
		if !upstreamHasData(k.ID, req.Method, req.Params, scaleAvailability(80, k)) {
			resp.Result = json.RawMessage(`null`)
			return resp
		}
		head := h.headFor(k.ID).headBlock.Load() - int64(k.BlockLag)
		resp.Result = json.RawMessage(fmt.Sprintf(
			`{"hash":"0x%064x","blockNumber":"0x%x","from":"0x%040x","to":"0x%040x","value":"0x0"}`,
			absHashFromParams(req.Params), head-1, uint64(0xdead), uint64(0xbeef),
		))
	case "eth_getLogs":
		// 85% baseline; scaled by the per-upstream DataAvailability
		// knob. An empty array IS a valid response, not a miss — but
		// eRPC's integrity check on getLogs can classify all-zero
		// results suspiciously for short ranges, so we err on the
		// side of returning a synthetic log to keep noise low.
		if !upstreamHasData(k.ID, req.Method, req.Params, scaleAvailability(85, k)) {
			resp.Result = json.RawMessage(`[]`)
			return resp
		}
		head := h.headFor(k.ID).headBlock.Load() - int64(k.BlockLag)
		resp.Result = json.RawMessage(fmt.Sprintf(
			`[{"address":"0x%040x","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"],"data":"0x","blockNumber":"0x%x","transactionHash":"0x%064x","logIndex":"0x0"}]`,
			uint64(0xfeed), head-1, absHashFromParams(req.Params),
		))
	case "eth_feeHistory":
		resp.Result = json.RawMessage(`{"oldestBlock":"0x0","reward":[],"baseFeePerGas":["0x0"],"gasUsedRatio":[]}`)

	// ── SVM (Solana) methods ─────────────────────────────────────────
	// Same head/knob model as EVM: the per-upstream head (minus the
	// BlockLag knob) is the PROCESSED slot; svmSlotFor derives confirmed
	// and finalized views from it.
	case "getSlot":
		resp.Result = json.RawMessage(fmt.Sprintf(`%d`, h.svmSlotFor(k, svmCommitment(req.Params))))
	case "getHealth":
		resp.Result = json.RawMessage(`"ok"`)
	case "getVersion":
		resp.Result = json.RawMessage(`{"solana-core":"2.3.0-sim","feature-set":1}`)
	case "getGenesisHash":
		// Real mainnet-beta genesis so the SVM bootstrap's fail-closed
		// genesis gate passes for `cluster: mainnet-beta` upstreams.
		resp.Result = json.RawMessage(`"5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"`)
	case "getMaxShredInsertSlot":
		resp.Result = json.RawMessage(fmt.Sprintf(`%d`, h.svmSlotFor(k, "processed")+1))
	case "getBlockHeight":
		resp.Result = json.RawMessage(fmt.Sprintf(`%d`, h.svmSlotFor(k, svmCommitment(req.Params))))
	case "getEpochInfo":
		s := h.svmSlotFor(k, svmCommitment(req.Params))
		resp.Result = json.RawMessage(fmt.Sprintf(
			`{"absoluteSlot":%d,"blockHeight":%d,"epoch":%d,"slotIndex":%d,"slotsInEpoch":432000,"transactionCount":%d}`,
			s, s, s/432000, s%432000, s*1500))
	case "getLatestBlockhash":
		s := h.svmSlotFor(k, svmCommitment(req.Params))
		resp.Result = json.RawMessage(fmt.Sprintf(
			`{"context":{"slot":%d},"value":{"blockhash":"%s","lastValidBlockHeight":%d}}`,
			s, b58ish(uint64(s)^0xb10c, 44), s+150))
	case "getBalance":
		lam := absHashFromParams(req.Params) % 1_000_000_000_000
		resp.Result = json.RawMessage(fmt.Sprintf(
			`{"context":{"slot":%d},"value":%d}`, h.svmSlotFor(k, svmCommitment(req.Params)), lam))
	case "getAccountInfo":
		resp.Result = json.RawMessage(fmt.Sprintf(
			`{"context":{"slot":%d},"value":{"lamports":%d,"owner":"11111111111111111111111111111111","data":["","base64"],"executable":false,"rentEpoch":361}}`,
			h.svmSlotFor(k, svmCommitment(req.Params)), absHashFromParams(req.Params)%1_000_000_000))
	case "getMultipleAccounts":
		resp.Result = json.RawMessage(fmt.Sprintf(
			`{"context":{"slot":%d},"value":[]}`, h.svmSlotFor(k, svmCommitment(req.Params))))
	case "getBlock":
		// Solana addresses blocks by bare slot number. Beyond this
		// upstream's view → the canonical -32004 (block not available)
		// so eRPC's SVM missing-data classification gets exercised
		// rather than a bare null.
		s := h.svmSlotFor(k, "processed")
		n, ok := firstNumberParam(req.Params)
		if !ok {
			n = s
		}
		if n > s {
			resp.Error = &jsonRPCError{Code: -32004, Message: fmt.Sprintf("Block not available for slot %d", n)}
			return resp
		}
		if n < 1 {
			n = 1
		}
		resp.Result = json.RawMessage(fmt.Sprintf(
			`{"blockhash":"%s","previousBlockhash":"%s","parentSlot":%d,"blockHeight":%d,"blockTime":%d,"transactions":[]}`,
			b58ish(uint64(n), 44), b58ish(uint64(n-1), 44), n-1, n, svmSlotTimestamp(n)))
	case "getTransaction":
		// Sparse-data model like eth receipts: 80% baseline scaled by
		// the DataAvailability knob; a miss is a legitimate null.
		if !upstreamHasData(k.ID, req.Method, req.Params, scaleAvailability(80, k)) {
			resp.Result = json.RawMessage(`null`)
			return resp
		}
		s := h.svmSlotFor(k, "finalized")
		resp.Result = json.RawMessage(fmt.Sprintf(
			`{"slot":%d,"blockTime":%d,"meta":{"err":null,"fee":5000},"transaction":{"signatures":["%s"]}}`,
			s, svmSlotTimestamp(s), b58ish(absHashFromParams(req.Params), 87)))
	case "getSignatureStatuses":
		s := h.svmSlotFor(k, "processed")
		resp.Result = json.RawMessage(fmt.Sprintf(
			`{"context":{"slot":%d},"value":[{"slot":%d,"confirmations":null,"err":null,"confirmationStatus":"finalized"}]}`, s, s-40))
	case "getSignaturesForAddress":
		s := h.svmSlotFor(k, "finalized")
		resp.Result = json.RawMessage(fmt.Sprintf(
			`[{"signature":"%s","slot":%d,"err":null,"blockTime":%d}]`,
			b58ish(absHashFromParams(req.Params), 87), s, svmSlotTimestamp(s)))
	case "sendTransaction":
		resp.Result = json.RawMessage(fmt.Sprintf(`"%s"`, b58ish(absHashFromParams(req.Params), 87)))
	case "simulateTransaction":
		resp.Result = json.RawMessage(fmt.Sprintf(
			`{"context":{"slot":%d},"value":{"err":null,"logs":[],"unitsConsumed":2100}}`, h.svmSlotFor(k, "processed")))
	case "getRecentPrioritizationFees":
		s := h.svmSlotFor(k, "processed")
		resp.Result = json.RawMessage(fmt.Sprintf(
			`[{"slot":%d,"prioritizationFee":0},{"slot":%d,"prioritizationFee":1200}]`, s-1, s))
	case "getFirstAvailableBlock":
		s := h.svmSlotFor(k, "finalized") - 100_000
		if s < 0 {
			s = 0
		}
		resp.Result = json.RawMessage(fmt.Sprintf(`%d`, s))
	default:
		resp.Result = json.RawMessage(`"0x0"`)
	}
	return resp
}

// scaleAvailability resolves the per-upstream `DataAvailability` knob
// into a probability percentage that the upstream has data for a
// sparse-data method (eth_getTransactionReceipt, eth_getBlockByHash,
// eth_getLogs, eth_getTransactionByHash).
//
// Knob semantics (UI slider 0..100%):
//
//	1.0  (100%) → ALWAYS has data — no synthetic misses, regardless
//	              of the method's natural sparsity baseline.
//	0    (  0%) → NEVER has data — always returns empty/null.
//	0..1        → Multiplier on the per-method baseline (e.g. 0.5 on
//	              eth_getTransactionReceipt's 80% baseline → 40%
//	              probability of having data).
//
// This matches the user's intuition that the slider is a direct
// "how often does this upstream actually have what I asked for?"
// control. The per-method baseline is still applied when the knob is
// dialed BELOW 100% so partial sparsity simulations remain realistic;
// at exactly 100%, the operator is opting out of synthetic misses
// entirely on that upstream.
func scaleAvailability(baselinePct int, k *UpstreamKnobs) int {
	scale := k.DataAvailability
	if scale <= 0 {
		return 0
	}
	if scale >= 1 {
		// 100% on the slider = always has data. The per-method baseline
		// is intentionally bypassed here — operators reaching for 100%
		// expect zero synthetic misses, not "baseline×1.0".
		return 100
	}
	out := int(float64(baselinePct) * scale)
	if out < 0 {
		return 0
	}
	if out > 100 {
		return 100
	}
	return out
}

// upstreamHasData returns true with probability `pct` percent,
// deterministically derived from the (upstream-id, method, params)
// triple. Same query against the same upstream always yields the same
// answer; different upstreams disagree in a stable, reproducible way.
func upstreamHasData(upstreamID, method string, params json.RawMessage, pct int) bool {
	var h uint64 = 1469598103934665603
	mix := func(b []byte) {
		for _, c := range b {
			h ^= uint64(c)
			h *= 1099511628211
		}
	}
	mix([]byte(upstreamID))
	mix([]byte("|"))
	mix([]byte(method))
	mix([]byte("|"))
	mix(params)
	return int(h%100) < pct
}

// firstStringParam returns the first param of a JSON-RPC params array
// if it's a string. Used to extract the block number / tag.
func firstStringParam(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var ps []any
	if err := json.Unmarshal(raw, &ps); err != nil || len(ps) == 0 {
		return "", false
	}
	s, ok := ps[0].(string)
	return s, ok
}

// absHashFromParams returns a stable uint64 hash derived from the params
// blob, suitable for synthesizing a "hash" field in fake responses.
func absHashFromParams(raw json.RawMessage) uint64 {
	var h uint64 = 1469598103934665603
	for _, c := range raw {
		h ^= uint64(c)
		h *= 1099511628211
	}
	if h == 0 {
		h = 1
	}
	return h
}

// parseHex decodes a 0x-prefixed hex string into an int64. Returns 0
// for any malformed input — synthetic responses don't fail validation
// on bad params, they just collapse to head=0.
func parseHex(s string) (int64, error) {
	if !strings.HasPrefix(s, "0x") || len(s) < 3 {
		return 0, fmt.Errorf("not hex")
	}
	var v int64
	for _, c := range s[2:] {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= int64(c - '0')
		case c >= 'a' && c <= 'f':
			v |= int64(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v |= int64(c-'A') + 10
		default:
			return 0, fmt.Errorf("bad hex char")
		}
	}
	return v, nil
}

func sampleLatency(k *UpstreamKnobs) time.Duration {
	g := math.Abs(gaussian())
	ms := k.BaseLatencyMs + g*k.JitterMs*0.6
	if ms < 1 {
		ms = 1
	}
	return time.Duration(ms * float64(time.Millisecond))
}

// gaussian returns a Box-Muller standard normal sample.
//
// #nosec G404 — math/rand is the correct primitive for statistical
// sampling (Box-Muller transform). The simulator uses this to vary
// per-request synthetic latency around a mean; no security context.
func gaussian() float64 {
	u := rand.Float64() // #nosec G404
	v := rand.Float64() // #nosec G404
	if u < 1e-12 {
		u = 1e-12
	}
	return math.Sqrt(-2*math.Log(u)) * math.Cos(2*math.Pi*v)
}
