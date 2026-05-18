package simulator

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/erpc/erpc/internal/policy"
)

// WSHandler returns an http.Handler that upgrades to WebSocket and
// runs one Session per connection.
//
// Protocol overview (JSON text frames):
//
// Client → Server:
//
//	{"kind":"hello"}                          — connection start
//	{"kind":"send-batch","items":[{ ... }]}   — execute a batch of requests
//	{"kind":"send-one","req":{ ... }}         — execute a single request
//	{"kind":"set-knob","id":"...","patch":{}} — mutate one upstream
//	{"kind":"validate-policy","src":"..."}    — sobek compile only (no swap)
//	{"kind":"apply-policy","src":"..."}       — compile + swap policy
//	{"kind":"validate-config","yaml":"..."}   — parse + validate config (no swap)
//	{"kind":"apply-config","yaml":"..."}      — parse + validate + reboot eRPC
//	{"kind":"set-paused","paused":true}       — pause/resume execution
//	{"kind":"start-scenario","name":"..."}    — start a scripted scenario
//	{"kind":"stop-scenario"}                  — stop the current scenario
//
// Server → Client:
//
//	{"kind":"snapshot","state":{ ... }}       — initial state push on hello
//	{"kind":"stats","frame":{ ... }}          — periodic 200ms snapshot
//	{"kind":"traces","items":[{ ... }]}       — batched per-request lifecycles
//	{"kind":"policy-result","result":{ ... }} — response to apply-policy
//	{"kind":"knob-changed","knob":{ ... }}    — echo of set-knob (and scenario-driven changes)
//	{"kind":"error","msg":"..."}              — generic error report
func WSHandler(o *Orchestrator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true, // local-only tool; origin not enforced
			CompressionMode:    websocket.CompressionDisabled,
		})
		if err != nil {
			return
		}
		// Default read limit is 32KB; bump it because batched send /
		// trace frames at peak RPS can easily exceed that.
		conn.SetReadLimit(8 * 1024 * 1024)
		defer conn.Close(websocket.StatusInternalError, "session ended")

		sess := &Session{
			orc:    o,
			conn:   conn,
			outCh:  make(chan outFrame, 256),
			doneCh: make(chan struct{}),
		}
		sess.run(r.Context())
	})
}

// Session is one connected WebSocket. Three goroutines:
//
//   - reader: parses inbound frames and dispatches them.
//   - writer: serializes outbound frames from outCh.
//   - tickers: pushes periodic stats + flushed trace batches into outCh.
//
// Trace events are buffered and flushed every 50ms (or 256 items,
// whichever comes first) so the wire never sees 10k frames/sec even
// at peak.
type Session struct {
	orc  *Orchestrator
	conn *websocket.Conn

	outCh  chan outFrame
	doneCh chan struct{}

	mu     sync.Mutex
	traceQ []TraceEvent
}

type outFrame struct {
	kind string
	body any
}

// envelope is the on-wire JSON shape every outbound frame uses.
type envelope struct {
	Kind string `json:"kind"`
	Body any    `json:"body,omitempty"`
}

func (s *Session) run(ctx context.Context) {
	defer close(s.doneCh)

	rctx, cancel := context.WithCancel(ctx)
	defer cancel()

	traceSub, unsub := s.subscribeTraces()
	defer unsub()

	go s.writer(rctx)
	go s.statsTicker(rctx)
	go s.traceFlusher(rctx, traceSub)

	s.reader(rctx)
}

// subscribeTraces wires this session into the orchestrator's trace
// stream. Since the orchestrator doesn't itself expose a Subscribe()
// anymore (executions are driven by sessions calling Execute), we make
// the session itself observe its own Execute calls. The buffer is the
// session's traceQ — Execute appends, the flusher dequeues.
func (s *Session) subscribeTraces() (<-chan TraceEvent, func()) {
	// no-op channel: we only push to traceQ from our own Execute path.
	ch := make(chan TraceEvent)
	return ch, func() { close(ch) }
}

// reader parses inbound frames and dispatches them to the orchestrator.
func (s *Session) reader(ctx context.Context) {
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			return
		}
		var msg struct {
			Kind string          `json:"kind"`
			Raw  json.RawMessage `json:"-"`
		}
		// Decode just the kind so we know what shape to expect next.
		if err := json.Unmarshal(data, &msg); err != nil {
			s.send("error", map[string]string{"msg": "bad frame: " + err.Error()})
			continue
		}
		s.dispatch(ctx, msg.Kind, data)
	}
}

func (s *Session) dispatch(ctx context.Context, kind string, data []byte) {
	switch kind {
	case "hello":
		s.send("snapshot", s.stateSnapshot())
	case "send-one":
		var f struct {
			Req struct {
				ID     uint64          `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			} `json:"req"`
		}
		if err := json.Unmarshal(data, &f); err != nil {
			s.send("error", map[string]string{"msg": "bad send-one: " + err.Error()})
			return
		}
		go s.execute(ctx, f.Req.ID, f.Req.Method, f.Req.Params)
	case "send-batch":
		var f struct {
			Items []struct {
				ID     uint64          `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			} `json:"items"`
		}
		if err := json.Unmarshal(data, &f); err != nil {
			s.send("error", map[string]string{"msg": "bad send-batch: " + err.Error()})
			return
		}
		for _, it := range f.Items {
			go s.execute(ctx, it.ID, it.Method, it.Params)
		}
	case "set-knob":
		var f struct {
			ID    string            `json:"id"`
			Patch UpstreamKnobPatch `json:"patch"`
		}
		if err := json.Unmarshal(data, &f); err != nil {
			s.send("error", map[string]string{"msg": "bad set-knob: " + err.Error()})
			return
		}
		if knob, ok := s.orc.UpdateUpstream(f.ID, f.Patch); ok {
			s.send("knob-changed", knob)
		}
	case "apply-policy":
		var f struct {
			Src string `json:"src"`
		}
		if err := json.Unmarshal(data, &f); err != nil {
			s.send("error", map[string]string{"msg": "bad apply-policy: " + err.Error()})
			return
		}
		go func() {
			res := s.orc.ApplyPolicy(ctx, f.Src)
			s.send("policy-result", res)
		}()
	case "validate-policy":
		var f struct {
			Src string `json:"src"`
		}
		if err := json.Unmarshal(data, &f); err != nil {
			s.send("error", map[string]string{"msg": "bad validate-policy: " + err.Error()})
			return
		}
		go func() {
			res := s.orc.ValidatePolicy(f.Src)
			s.send("policy-validate-result", res)
		}()
	case "apply-config":
		var f struct {
			YAML string `json:"yaml"`
		}
		if err := json.Unmarshal(data, &f); err != nil {
			s.send("error", map[string]string{"msg": "bad apply-config: " + err.Error()})
			return
		}
		go func() {
			res := s.orc.ApplyConfig(ctx, f.YAML)
			s.send("config-result", res)
			// Also push a fresh snapshot so the editor + knob panel
			// re-sync against the newly-loaded upstreams + policy.
			if res.Ok {
				s.send("snapshot", s.stateSnapshot())
			}
		}()
	case "validate-config":
		var f struct {
			YAML string `json:"yaml"`
		}
		if err := json.Unmarshal(data, &f); err != nil {
			s.send("error", map[string]string{"msg": "bad validate-config: " + err.Error()})
			return
		}
		go func() {
			res := s.orc.ValidateConfig(f.YAML)
			s.send("config-validate-result", res)
		}()
	case "set-paused":
		var f struct {
			Paused bool `json:"paused"`
		}
		if err := json.Unmarshal(data, &f); err != nil {
			s.send("error", map[string]string{"msg": "bad set-paused: " + err.Error()})
			return
		}
		s.orc.SetPaused(f.Paused)
	case "start-scenario":
		var f struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(data, &f); err != nil {
			s.send("error", map[string]string{"msg": "bad start-scenario: " + err.Error()})
			return
		}
		s.orc.StartScenario(f.Name)
	case "stop-scenario":
		s.orc.StopScenario()
	default:
		s.send("error", map[string]string{"msg": "unknown kind: " + kind})
	}
}

// execute runs one real eRPC request and queues the resulting trace
// for the next flush window.
func (s *Session) execute(ctx context.Context, id uint64, method string, params json.RawMessage) {
	ev := s.orc.Execute(ctx, method, params)
	if ev == nil {
		return
	}
	if id != 0 {
		// preserve the client-side request id for correlation
		ev.ID = id
	}
	s.mu.Lock()
	s.traceQ = append(s.traceQ, *ev)
	s.mu.Unlock()
}

// writer drains outCh and writes each frame to the wire.
func (s *Session) writer(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.doneCh:
			return
		case f := <-s.outCh:
			env := envelope{Kind: f.kind, Body: f.body}
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := wsjson.Write(wctx, s.conn, env)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// statsTicker pushes a periodic StatsFrame to the client. Also
// piggy-backs a `policy-history` frame so the frontend's "policy
// history" panel stays current without a separate ticker. We send the
// last 16 decisions every 200 ms — the frontend dedupes by tick id,
// so this absorbs gaps and brief disconnects without missing ticks.
func (s *Session) statsTicker(ctx context.Context) {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.doneCh:
			return
		case <-t.C:
			s.send("stats", s.orc.Stats())
			if hist := s.orc.RecentPolicyDecisions("*", 16); len(hist) > 0 {
				s.send("policy-history", map[string]any{"items": hist})
			}
		}
	}
}

// traceFlusher batches buffered trace events and flushes them every
// 50ms or whenever the queue hits 256 entries.
func (s *Session) traceFlusher(ctx context.Context, sub <-chan TraceEvent) {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.doneCh:
			return
		case <-t.C:
			s.mu.Lock()
			if len(s.traceQ) == 0 {
				s.mu.Unlock()
				continue
			}
			batch := s.traceQ
			s.traceQ = nil
			s.mu.Unlock()
			s.send("traces", map[string]any{"items": batch})
		}
	}
}

// send queues an outbound frame. Drops silently if the writer is
// backlogged so a slow client can't OOM the server.
func (s *Session) send(kind string, body any) {
	select {
	case s.outCh <- outFrame{kind: kind, body: body}:
	default:
		// writer backpressured; drop. Stats frames will rebuild
		// state from the rolling counters anyway.
	}
}

// stateSnapshot returns the initial state the client needs on connect:
// the current YAML config + the live policy source + every upstream's
// knobs + the canonical default policy source + the canonical seed
// YAML (so the editor's "↺ default" button has something to reset
// to) + the paused flag.
func (s *Session) stateSnapshot() map[string]any {
	return map[string]any{
		"yaml":          s.orc.CurrentYAML(),
		"defaultYaml":   SeedYAML,
		"upstreams":     s.orc.Hub().Snapshot(),
		"policy":        s.orc.CurrentPolicy(),
		"defaultPolicy": policy.DefaultPolicySource(),
		"paused":        s.orc.IsPaused(),
		"serverTimeMs":  time.Now().UnixMilli(),
	}
}
