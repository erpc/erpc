package simulator

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
)

// Dumper writes a chronological, line-delimited JSON record of every
// observable event in the simulator — boot config, knob changes,
// policy/config applies, scenario starts/stops, and every request +
// response with the full per-attempt log. It's designed to be the
// authoritative artifact an AI agent can grep / jq / parse to answer
// "why did this happen at time X?" without re-running the simulator.
//
// Output: a single `*.jsonl` file at the path passed via `-dump-file`.
// A companion `*.AGENTS.md` is written next to it once, at startup, so
// any agent investigating the dump has the schema + idiomatic queries
// at hand.
//
// Concurrency: every write acquires a mutex and goes through a buffered
// writer. The write path is non-blocking from the caller's point of
// view (the buffer absorbs spikes), and a flush runs every second so a
// crashing process loses at most ~1s of trailing events.
type Dumper struct {
	path string

	mu     sync.Mutex
	f      *os.File
	w      *bufio.Writer
	closed bool

	flushStop chan struct{}
	flushDone chan struct{}
}

// NewDumper opens the dump file (creating parent directories if needed),
// writes the companion AGENTS.md alongside, and starts the periodic
// flush goroutine. Returns nil if `path` is empty (dump disabled).
func NewDumper(path string) (*Dumper, error) {
	if path == "" {
		return nil, nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("dumper: mkdir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("dumper: open %s: %w", path, err)
	}
	d := &Dumper{
		path:      path,
		f:         f,
		w:         bufio.NewWriterSize(f, 64*1024),
		flushStop: make(chan struct{}),
		flushDone: make(chan struct{}),
	}
	if err := writeAgentsMarkdownNearby(path); err != nil {
		// Non-fatal: a missing companion file is a minor inconvenience,
		// not a reason to crash. Log via the dumper itself.
		d.write(map[string]any{
			"ts":   time.Now().UTC().Format(time.RFC3339Nano),
			"kind": "dump.warn",
			"msg":  fmt.Sprintf("could not write agents markdown: %v", err),
		})
	}
	go d.flushLoop()
	return d, nil
}

// flushLoop periodically flushes the buffered writer so a crashing
// process loses at most a second of trailing events. Stops when
// Close() signals via flushStop.
func (d *Dumper) flushLoop() {
	defer close(d.flushDone)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-d.flushStop:
			return
		case <-t.C:
			d.mu.Lock()
			if !d.closed {
				_ = d.w.Flush()
			}
			d.mu.Unlock()
		}
	}
}

// Close flushes and closes the dump file. Idempotent. Safe to call from
// any goroutine.
func (d *Dumper) Close() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	close(d.flushStop)
	d.mu.Unlock()
	<-d.flushDone
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = d.w.Flush()
	return d.f.Close()
}

// write is the only path that touches the writer. JSON-encodes the
// payload, appends a newline, and writes. Errors are logged to stderr
// but don't propagate — a write failure on the dump file shouldn't kill
// the simulator's main loop.
func (d *Dumper) write(payload any) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	b, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "erpc-simulator: dump marshal failed: %v\n", err)
		return
	}
	b = append(b, '\n')
	if _, err := d.w.Write(b); err != nil {
		fmt.Fprintf(os.Stderr, "erpc-simulator: dump write failed: %v\n", err)
	}
}

// ─── Event recorders ───────────────────────────────────────────────────
//
// Every recorder accepts arbitrary metadata and stamps a UTC RFC3339
// timestamp + a `kind` discriminator. The schema is open by design:
// new fields can be added at any time and old consumers will ignore
// them. AGENTS.md describes the canonical kind→fields mapping.

func (d *Dumper) LogBoot(yamlSrc, policySrc string, knobs []UpstreamKnobs, opts map[string]any) {
	d.write(map[string]any{
		"ts":     time.Now().UTC().Format(time.RFC3339Nano),
		"kind":   "boot",
		"yaml":   yamlSrc,
		"policy": policySrc,
		"knobs":  knobs,
		"opts":   opts,
	})
}

func (d *Dumper) LogConfigApply(yamlSrc string, ok bool, errMsg string) {
	d.write(map[string]any{
		"ts":    time.Now().UTC().Format(time.RFC3339Nano),
		"kind":  "config.apply",
		"yaml":  yamlSrc,
		"ok":    ok,
		"error": errMsg,
	})
}

func (d *Dumper) LogPolicyApply(src string, ok bool, errMsg string) {
	d.write(map[string]any{
		"ts":     time.Now().UTC().Format(time.RFC3339Nano),
		"kind":   "policy.apply",
		"source": src,
		"ok":     ok,
		"error":  errMsg,
	})
}

func (d *Dumper) LogKnobChange(id string, before, after UpstreamKnobs, patch UpstreamKnobPatch) {
	d.write(map[string]any{
		"ts":     time.Now().UTC().Format(time.RFC3339Nano),
		"kind":   "knob.change",
		"id":     id,
		"before": before,
		"after":  after,
		"patch":  patch,
	})
}

func (d *Dumper) LogScenarioStart(name string) {
	d.write(map[string]any{
		"ts":   time.Now().UTC().Format(time.RFC3339Nano),
		"kind": "scenario.start",
		"name": name,
	})
}

func (d *Dumper) LogScenarioStop() {
	d.write(map[string]any{
		"ts":   time.Now().UTC().Format(time.RFC3339Nano),
		"kind": "scenario.stop",
	})
}

func (d *Dumper) LogScenarioEvent(name string, elapsedSec float64, desc string) {
	d.write(map[string]any{
		"ts":      time.Now().UTC().Format(time.RFC3339Nano),
		"kind":    "scenario.event",
		"name":    name,
		"elapsed": elapsedSec,
		"desc":    desc,
	})
}

func (d *Dumper) LogPaused(paused bool) {
	d.write(map[string]any{
		"ts":     time.Now().UTC().Format(time.RFC3339Nano),
		"kind":   "paused",
		"paused": paused,
	})
}

func (d *Dumper) LogInjectFailure(id string, durationMs int64) {
	d.write(map[string]any{
		"ts":         time.Now().UTC().Format(time.RFC3339Nano),
		"kind":       "knob.inject",
		"id":         id,
		"durationMs": durationMs,
	})
}

// LogRequest records ONE end-to-end request lifecycle. The TraceEvent
// already includes the full per-attempt log + selection trail + final
// response/error preview, so this single line captures everything an
// agent needs to reconstruct why a request resolved the way it did.
func (d *Dumper) LogRequest(ev *TraceEvent, params, body json.RawMessage) {
	if ev == nil {
		return
	}
	d.write(map[string]any{
		"ts":     time.Now().UTC().Format(time.RFC3339Nano),
		"kind":   "request",
		"event":  ev,
		"params": params,
		"body":   body,
	})
}

// LogStatsFrame records a periodic StatsFrame (~5x/s by default in the
// simulator). High-volume — agents should sample or filter by ts range
// when querying. Useful for reconstructing the per-second selection
// share and observed health metrics at any point in time.
func (d *Dumper) LogStatsFrame(frame StatsFrame) {
	d.write(map[string]any{
		"ts":    time.Now().UTC().Format(time.RFC3339Nano),
		"kind":  "stats",
		"frame": frame,
	})
}

// SnapshotConfig records a one-shot "current state of the world"
// payload — used at boot and whenever a config or policy apply
// succeeds, so an investigator can locate the latest authoritative
// state without replaying every change.
func (d *Dumper) SnapshotConfig(yamlSrc, policySrc string, cfg *common.Config) {
	d.write(map[string]any{
		"ts":     time.Now().UTC().Format(time.RFC3339Nano),
		"kind":   "config.snapshot",
		"yaml":   yamlSrc,
		"policy": policySrc,
		"config": cfg,
	})
}

// writeAgentsMarkdownNearby drops a companion markdown file next to the
// dump path explaining the JSONL schema + how to filter / search the
// dump. Written once at dumper open — overwrites any existing file so
// schema doc stays in sync with the code that produced the dump.
func writeAgentsMarkdownNearby(dumpPath string) error {
	mdPath := derivedAgentsPath(dumpPath)
	return os.WriteFile(mdPath, []byte(agentsMarkdownTemplate(dumpPath)), 0o644)
}

func derivedAgentsPath(dumpPath string) string {
	// `foo.jsonl` → `foo.AGENTS.md`
	// `foo`       → `foo.AGENTS.md`
	base := dumpPath
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	return base + ".AGENTS.md"
}

func agentsMarkdownTemplate(dumpPath string) string {
	return strings.ReplaceAll(agentsMarkdownBody, "{{DUMP_PATH}}", dumpPath)
}

// agentsMarkdownBody is the canonical prompt for an AI agent trying to
// investigate an erpc-simulator dump. Keep it concise and grounded in
// the actual JSONL schema this package emits — every field listed here
// is something the dumper writes.
const agentsMarkdownBody = "# erpc-simulator dump · investigation guide\n" +
	"\n" +
	"This document accompanies `{{DUMP_PATH}}` — a line-delimited JSON\n" +
	"file containing every observable event from one run of the\n" +
	"erpc-simulator: boot config, knob/policy/config changes, scenario\n" +
	"events, and every request + response with its full per-attempt log.\n" +
	"\n" +
	"## File format\n" +
	"\n" +
	"One JSON object per line. Every line has:\n" +
	"- `ts` — UTC RFC3339Nano timestamp (sortable as strings)\n" +
	"- `kind` — discriminator (see table below)\n" +
	"\n" +
	"Other fields depend on `kind`.\n" +
	"\n" +
	"## Event kinds\n" +
	"\n" +
	"| kind              | when emitted                                | key fields |\n" +
	"|-------------------|---------------------------------------------|------------|\n" +
	"| `boot`            | once at startup                             | `yaml` (templated YAML), `policy` (JS source), `knobs[]`, `opts` |\n" +
	"| `config.snapshot` | after every config apply (incl. boot)       | `yaml`, `policy`, `config` (parsed *common.Config) |\n" +
	"| `config.apply`    | user-issued config apply                    | `yaml`, `ok`, `error` |\n" +
	"| `policy.apply`    | user-issued policy apply                    | `source`, `ok`, `error` |\n" +
	"| `knob.change`     | one upstream knob mutation                  | `id`, `before`, `after`, `patch` |\n" +
	"| `knob.inject`     | inject-failure window started               | `id`, `durationMs` |\n" +
	"| `scenario.start`  | scripted scenario started                   | `name` |\n" +
	"| `scenario.stop`   | scripted scenario stopped (or finished)     | — |\n" +
	"| `scenario.event`  | scenario step fired                         | `name`, `elapsed`, `desc` |\n" +
	"| `paused`          | sim paused/resumed                          | `paused` (bool) |\n" +
	"| `request`         | one end-to-end request lifecycle            | `event` (TraceEvent), `params`, `body` |\n" +
	"| `stats`           | ~5x/s aggregate snapshot                    | `frame` (StatsFrame) |\n" +
	"| `dump.warn`       | non-fatal dumper error                      | `msg` |\n" +
	"\n" +
	"### `request.event` shape (TraceEvent)\n" +
	"\n" +
	"```\n" +
	"id            uint64    request id\n" +
	"ts            time      when the request started (wall clock)\n" +
	"method        string    JSON-RPC method (eth_getBalance, eth_call, …)\n" +
	"outcome       string    \"ok\"|\"retry-ok\"|\"hedge-win\"|\"fail\"|\"cache-hit\"\n" +
	"winner        string    upstream id that produced the served response (\"\" on fail)\n" +
	"duration      float64   total wall-clock time, ms\n" +
	"usedHedge     bool      at least one hedge attempt fired\n" +
	"usedRetry     bool      at least one retry fired\n" +
	"consensusSlots int      participants spawned by the consensus block\n" +
	"consensusDisputes int   dispute events recorded\n" +
	"consensusLowParts int   low-participants events\n" +
	"requestError  string    error returned to the caller (non-empty on fail)\n" +
	"responseBody  string    truncated response preview (≤ 8 KB)\n" +
	"sel[]                   selection trail entries: { id, idx, score, excluded, reason }\n" +
	"attempts[]              every physical upstream call: { id, t0, dur, outcome, reason,\n" +
	"                          winner, selReason, isHedge, isRetry, attemptIdx }\n" +
	"```\n" +
	"\n" +
	"### `knob.change.before/after` shape (UpstreamKnobs)\n" +
	"\n" +
	"`id`, `vendor`, `tags[]`, `endpoint`, `base` (ms), `jitter` (ms),\n" +
	"`error` (0..1), `timeoutRate`, `throttleRate`, `blockLag`,\n" +
	"`blockTimeMs`, `available` (bool), `dataAvailability` (0..1),\n" +
	"`injectFailureUntilMs`.\n" +
	"\n" +
	"## Idiomatic queries\n" +
	"\n" +
	"All examples use `jq` against the dump. For very large dumps (GB scale)\n" +
	"prefer `jq --stream` or `rg` (ripgrep) for the initial narrowing pass.\n" +
	"\n" +
	"### Narrow by timestamp window\n" +
	"```\n" +
	"rg -j 8 -e '\"ts\":\"2026-05-18T14:' {{DUMP_PATH}}\n" +
	"```\n" +
	"\n" +
	"### Only failed requests\n" +
	"```\n" +
	"jq -c 'select(.kind==\"request\" and .event.outcome==\"fail\")' {{DUMP_PATH}}\n" +
	"```\n" +
	"\n" +
	"### All knob changes for one upstream\n" +
	"```\n" +
	"jq -c 'select(.kind==\"knob.change\" and .id==\"drpc-eth-1\")' {{DUMP_PATH}}\n" +
	"```\n" +
	"\n" +
	"### Consensus events with disputes\n" +
	"```\n" +
	"jq -c 'select(.kind==\"request\" and (.event.consensusDisputes // 0) > 0)' {{DUMP_PATH}}\n" +
	"```\n" +
	"\n" +
	"### Per-upstream attempt-level error breakdown\n" +
	"```\n" +
	"jq -c 'select(.kind==\"request\") | .event.attempts[]' {{DUMP_PATH}} | \\\n" +
	"  jq -s 'group_by(.id) | map({id: .[0].id, total: length,\n" +
	"          ok: (map(select(.outcome==\"ok\")) | length),\n" +
	"          fail: (map(select(.outcome==\"fail\")) | length)})'\n" +
	"```\n" +
	"\n" +
	"### Last 100 stats frames (sample over time)\n" +
	"```\n" +
	"jq -c 'select(.kind==\"stats\")' {{DUMP_PATH}} | tail -100\n" +
	"```\n" +
	"\n" +
	"### Reconstruct full state at any point: find the most recent `config.snapshot` before T\n" +
	"```\n" +
	"jq -c 'select(.kind==\"config.snapshot\" and .ts < \"2026-05-18T14:32:00Z\")' {{DUMP_PATH}} | tail -1\n" +
	"```\n" +
	"\n" +
	"## Notes for analysts\n" +
	"\n" +
	"- `request.event.attempts[]` contains the *real* eRPC executor's\n" +
	"  per-attempt log (UpstreamAttemptLog) — `outcome` distinguishes\n" +
	"  `ok` / `miss` / `throttled` / `timeout` / `cb-open` / `fail` /\n" +
	"  `hedge-loser`. `selReason` says WHY the attempt fired:\n" +
	"  `primary` / `retry` / `hedge` / `consensus_slot` / `sweep`.\n" +
	"- Selection-policy verdicts live in `request.event.sel[]` — entries\n" +
	"  with `excluded: true` carry a `reason` (e.g. `\"removed:errorRate=0.342\"`).\n" +
	"- The dump is append-only; if the simulator was restarted against the\n" +
	"  same path you'll see multiple `boot` records — boundary them with\n" +
	"  `kind==\"boot\"` in your initial pass.\n" +
	"- Stats frames are emitted ~every 200 ms; expect ~5 per second.\n" +
	"  Filtering on `kind==\"stats\"` will dominate the file size for long runs.\n"
