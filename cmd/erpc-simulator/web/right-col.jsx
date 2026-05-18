// right-col.jsx — Tabs: Traffic gen + scenarios | Event log + Drawer.

const { useEffect, useState, useMemo } = React;

function RightCol({ onOpenDrawer, drawerReqId }) {
  const [tab, setTab] = useState("gen");
  const events = window.useEvents();
  const actions = window.useSimActions();

  return (
    <div className="right-col" style={{height:"100%"}}>
      <div className="panel-hd">
        <div style={{display:"flex",gap:0,marginLeft:-12,height:"100%"}}>
          <TabBtn label="Traffic gen" active={tab==="gen"} onClick={() => setTab("gen")} />
          <TabBtn label="Event log" active={tab==="log"} onClick={() => setTab("log")} count={events.length} />
        </div>
        <span className="spacer" />
        {tab === "log" && events.length > 0 && (
          <button className="btn ghost" onClick={() => actions.clearEvents()} style={{fontSize:"10.5px"}}>clear</button>
        )}
      </div>
      {tab === "gen" ? <TrafficGen /> : <EventLog onOpen={onOpenDrawer} drawerReqId={drawerReqId} />}
    </div>
  );
}

function TabBtn({ label, active, onClick, count }) {
  return (
    <button onClick={onClick} style={{
      padding:"0 12px", height:"100%",
      fontSize:"10.5px",fontWeight:600,letterSpacing:"0.08em",textTransform:"uppercase",
      color: active ? "var(--tx-0)" : "var(--tx-3)",
      borderBottom: active ? "2px solid var(--acc-blue)" : "2px solid transparent",
      display:"flex",alignItems:"center",gap:6,
    }}>
      {label}
      {count != null && <span style={{
        background:"var(--bg-3)",border:"1px solid var(--bd-1)",
        fontFamily:"var(--font-mono)",fontSize:"10px",padding:"0 4px",
        borderRadius:"3px",color:"var(--tx-1)"
      }}>{count}</span>}
    </button>
  );
}

// ============== TrafficGen ==============
function TrafficGen() {
  const targetRps = window.useTargetRps();
  const shape = window.useShape();
  const patternMix = window.usePatternMix();
  const perSec = window.usePerSecond();
  const scenario = window.useScenario();
  const scenarioEvents = window.useScenarioEvents();
  const actions = window.useSimActions();

  const STOPS = [1, 10, 100, 1000, 10000];
  function sliderToRps(v) { return Math.round(Math.pow(10, (v / 1000) * 4)); }
  function rpsToSlider(r) { return Math.round(1000 * (Math.log10(Math.max(1, r)) / 4)); }

  const scnMeta = scenario ? window.eRPCSimRuntime.SCENARIOS[scenario.name] : null;
  const scnElapsed = scenario ? (Date.now() - scenario.t0) / 1000 : 0;
  const scnProgress = scenario && scnMeta ? Math.min(1, scnElapsed / scnMeta.duration) : 0;

  function setPattern(i, patch) {
    const next = patternMix.map((p, j) => i === j ? { ...p, ...patch } : p);
    actions.setPatternMix(next);
  }
  function delPattern(i) {
    actions.setPatternMix(patternMix.filter((_, j) => j !== i));
  }
  function addPattern() {
    const candidates = Object.keys(window.eRPCSimRuntime.PATTERNS);
    actions.setPatternMix([...(patternMix || []), { id: candidates[0], weight: 1 }]);
  }
  function resetPatterns() {
    actions.setPatternMix(window.eRPCSimRuntime.DEFAULT_PATTERN_MIX.map(p => ({ ...p })));
  }

  return (
    <div className="gen-wrap">
      <div className="gen-section">
        <div className="lbl">rps · target <span className="sub">log scale</span></div>
        <div className="gen-rps">
          <span className="val">{targetRps.toLocaleString()}</span>
          <span className="unit">req/s</span>
          <span className="target">actual ~{Math.round(perSec.total)}</span>
        </div>
        <input className="gen-slider" type="range" min={0} max={1000} step={1}
          value={rpsToSlider(targetRps)}
          onChange={e => actions.setTargetRps(sliderToRps(+e.target.value))} />
        <div className="gen-stops">
          {STOPS.map(s => <span key={s}>{s.toLocaleString()}</span>)}
        </div>
      </div>

      <div className="gen-section">
        <div className="lbl">shape</div>
        <div className="gen-row">
          <select value={shape} onChange={e => actions.setShape(e.target.value)}>
            <option value="poisson">poisson · λ = rps</option>
            <option value="constant">constant</option>
            <option value="bursty">bursty · 15% burst windows</option>
          </select>
          <span style={{fontSize:"10.5px",color:"var(--tx-3)",fontFamily:"var(--font-mono)"}}>
            {shape === "poisson" && "natural"}
            {shape === "constant" && "tight"}
            {shape === "bursty" && "spiky"}
          </span>
        </div>
      </div>

      <div className="gen-section scrollable">
        <div className="lbl">
          traffic pattern mix
          <span className="sub">{patternMix.length} patterns</span>
          <span style={{flex:1}} />
          <button className="chip-btn" onClick={resetPatterns}>↺ reset</button>
        </div>
        <div className="method-mix">
          {patternMix.map((p, i) => (
            <div key={i} className="mm-row" style={{gridTemplateColumns:"1fr 50px 18px"}}>
              <select value={p.id} onChange={e => setPattern(i, { id: e.target.value })}
                style={{
                  background:"var(--bg-2)", border:"1px solid var(--bd-1)",
                  borderRadius:"var(--r-sm)", padding:"3px 6px",
                  fontFamily:"var(--font-mono)", fontSize:"10.5px", color:"var(--tx-0)",
                }}>
                {Object.entries(window.eRPCSimRuntime.PATTERNS).map(([id, def]) => (
                  <option key={id} value={id}>{def.label || id}</option>
                ))}
              </select>
              <input type="number" min={0} value={p.weight} onChange={e => setPattern(i, { weight: +e.target.value })} />
              <button className="del" onClick={() => delPattern(i)}>×</button>
            </div>
          ))}
          <button className="chip-btn" onClick={addPattern} style={{alignSelf:"flex-start",marginTop:4}}>+ add pattern</button>
        </div>

        <div className="lbl" style={{marginTop:14}}>
          scenario <span className="sub">{scenario ? `${scenario.name} · ${scnElapsed.toFixed(0)}s/${scnMeta.duration}s` : "idle"}</span>
        </div>
        <div className="gen-row">
          <select value={scenario?.name || "idle"} onChange={e => actions.startScenario(e.target.value)} style={{flex:1}}>
            <option value="idle">— select scenario —</option>
            <option value="gradual-degradation">gradual-degradation</option>
            <option value="vendor-region-outage">vendor-region-outage</option>
            <option value="traffic-spike">traffic-spike</option>
            <option value="hedge-saturator">hedge-saturator</option>
            <option value="circuit-breaker-trip">circuit-breaker-trip</option>
            <option value="slow-network">slow-network</option>
          </select>
          {scenario ? (
            <button className="btn ghost" onClick={() => actions.stopScenario()}>■ stop</button>
          ) : (
            <button className="btn" disabled>▶ idle</button>
          )}
        </div>
        <div className={`scn-bar ${scenario ? "live" : ""}`}>
          <div className="fill" style={{ transform: `scaleX(${scnProgress})` }} />
        </div>
        {scenarioEvents.length > 0 && (
          <div className="scn-events">
            {scenarioEvents.map((e, i) => (
              <div key={i} className="evt"><span className="t">t={e.t}s</span><span className="d">{e.d}</span></div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

// ============== EventLog ==============
function EventLog({ onOpen, drawerReqId }) {
  const events = window.useEvents();
  const focused = window.useFocusedUpstream();
  const actions = window.useSimActions();
  const [filter, setFilter] = useState("all");
  // Two filter axes: an outcome/destiny category (see tabs below) AND
  // a focused-upstream filter (set externally by clicking an upstream
  // node / knobs row). An event matches the focus filter if its winner
  // is the focused upstream OR if any of its attempts touched it.
  //
  // Categories ("destiny" tabs — what HAPPENED to the request):
  //   all       — every event
  //   err       — terminal failure (request returned an error to client)
  //   retry     — succeeded via at least one retry (outcome=retry-ok or usedRetry)
  //   hedge     — hedge fan-out was involved (winner or otherwise)
  //   consensus — consensus block fired (consensusSlots > 0)
  //   miss      — first attempt returned empty/no-data
  // Buffer retention is per-category in the runtime (sim-runtime.js's
  // onTraces), so rare categories don't get evicted by an "ok" flood.
  const filtered = useMemo(() => events.filter(e => {
    if (focused) {
      const touched =
        e.winner === focused ||
        (e.attempts || []).some(a => a.id === focused);
      if (!touched) return false;
    }
    if (filter === "all") return true;
    if (filter === "err") return e.outcome === "fail";
    if (filter === "hedge") return e.outcome === "hedge-win" || e.usedHedge;
    if (filter === "retry") return e.outcome === "retry-ok" || e.usedRetry;
    if (filter === "consensus") return (e.consensusSlots || 0) > 0;
    if (filter === "miss") return e.outcome === "miss"
      || (e.attempts || []).some(a => a.outcome === "miss");
    return true;
  }), [events, filter, focused]);
  // Render-cap so we never DOM-spam at very high RPS. The category
  // buffer is much larger (~1000/category in the runtime), so changing
  // tabs always reveals more events than this cap.
  const visible = filtered.slice(0, 500);

  const focusBanner = focused && (
    <div className="log-focus-banner">
      <span className="dot" />
      focused on <b>{focused}</b>
      <span className="cnt">· {filtered.length} matching</span>
      <span style={{flex:1}} />
      <button className="chip-btn" onClick={() => actions.clearFocusedUpstream()}>clear</button>
    </div>
  );

  if (filtered.length === 0) {
    return (
      <div className="log-wrap">
        <Toolbar filter={filter} setFilter={setFilter} />
        {focusBanner}
        <div className="empty">{focused ? `no events involving ${focused} yet — try widening the filter or wait for more traffic.` : "no events match this filter — try widening it."}</div>
      </div>
    );
  }
  return (
    <div className="log-wrap">
      <Toolbar filter={filter} setFilter={setFilter} />
      {focusBanner}
      <div className="log-table">
        {visible.map(e => {
          const t = new Date(e.ts);
          const stamp = `${pad(t.getUTCHours())}:${pad(t.getUTCMinutes())}:${pad(t.getUTCSeconds())}.${pad(t.getUTCMilliseconds(), 3)}`;
          const icon = e.outcome === "cache-hit" ? { c: "◆", cls: "cache" }
            : e.outcome === "hedge-win" ? { c: "⚡", cls: "hedge" }
            : e.outcome === "retry-ok" ? { c: "↻", cls: "retry" }
            : e.outcome === "fail" ? { c: "✗", cls: "bad" }
            : { c: "✓", cls: "ok" };
          return (
            <div key={e.id}
              className={`log-row ${drawerReqId === e.id ? "selected" : ""} ${e.fresh ? "fresh" : ""}`}
              onClick={() => onOpen(e)}>
              <span className="ts">{stamp}</span>
              <span className="mth">{e.method}</span>
              <span className="up">{e.winner || "—"}</span>
              <span className={`ic ${icon.cls}`}>{icon.c}</span>
              <span className="dur">{e.duration.toFixed(0)}ms</span>
            </div>
          );
        })}
      </div>
    </div>
  );
}
function pad(n, w = 2) { return String(n).padStart(w, "0"); }
function Toolbar({ filter, setFilter }) {
  // Tab order = "destiny" axis: how the request resolved.
  //   all       — every event
  //   err       — request failed terminally
  //   retry     — request succeeded via at least one retry
  //   hedge     — hedge fan-out fired
  //   consensus — request used the consensus block
  //   miss      — first attempt returned empty / no-data
  // Tooltips explain the categories on hover. Click toggles the filter.
  const TABS = [
    { k: "all",       title: "Every request" },
    { k: "err",       title: "Request returned an error to the client" },
    { k: "retry",     title: "Request succeeded via at least one retry" },
    { k: "hedge",     title: "A hedge fan-out fired (winner or otherwise)" },
    { k: "consensus", title: "Request went through the consensus block (multiple participants)" },
    { k: "miss",      title: "First attempt returned empty / no-data (sparse method result)" },
  ];
  return (
    <div className="log-toolbar">
      <div className="filter">
        {TABS.map(({ k, title }) => (
          <button key={k} className={filter === k ? "on" : ""} title={title} onClick={() => setFilter(k)}>{k}</button>
        ))}
      </div>
    </div>
  );
}

// AttemptReason renders the per-attempt explanation in a structured
// way:
//
//   * Winning attempts: just "won".
//   * Non-error outcomes (miss / throttled / cb-open / ...): the
//     outcome itself, plain text.
//   * Errors: a code chip ("ErrUpstreamRequest"), a one-line short
//     message, and a "+" button that toggles a popup showing the
//     full parsed details (any embedded JSON pretty-printed plus the
//     full caused-by chain).
//
// eRPC's error strings follow a stable format:
//   "<ErrCode>: <message> -> (<json-details>)
//   caused by: <ErrCode>: <message> -> (<json-details>) ..."
//
// parseEvmError walks that recursively into an array of nodes the
// popup renders as a tree.
function AttemptReason({ a, csState }) {
  const [popup, setPopup] = React.useState(null); // { top, right } or null
  const btnRef = React.useRef(null);
  if (a.winner) {
    // Consensus override: if the aggregator rejected everyone (dispute /
    // low-participants), the per-attempt "won" flag is misleading —
    // they all responded but none of them is the request's winner.
    // The synthesized "consensus result" row at the bottom shows the
    // real aggregator verdict.
    if (csState === "disputed")          return <>responded · in minority</>;
    if (csState === "low-participants")  return <>responded · low-quorum</>;
    if (csState === "failed")            return <>responded · aggregator failed</>;
    return <>won</>;
  }
  const raw = a.reason || "";
  if (!raw) return <>{a.outcome}</>;
  const parsed = parseErpcError(raw);
  if (!parsed.code) {
    return <span className="lc-reason-plain">{raw}</span>;
  }
  function toggle(e) {
    e.stopPropagation();
    if (popup) { setPopup(null); return; }
    const r = btnRef.current?.getBoundingClientRect();
    if (!r) return;
    // Anchor the popup top-right corner just below the button.
    setPopup({
      top: r.bottom + 6,
      right: Math.max(16, window.innerWidth - r.right - 4),
    });
  }
  return (
    <span className="lc-reason">
      <span className="lc-err-code" title={parsed.code}>{parsed.code}</span>
      <span className="lc-err-msg">{parsed.message}</span>
      <button
        ref={btnRef}
        className={`lc-err-toggle ${popup ? "on" : ""}`}
        onClick={toggle}
        title={popup ? "Hide details" : "Show details"}
      >{popup ? "−" : "+"}</button>
      {popup && (
        <ErrorDetailPopup
          parsed={parsed}
          raw={raw}
          onClose={() => setPopup(null)}
          pos={popup}
        />
      )}
    </span>
  );
}

// parseErpcError walks an eRPC error string into a structured node
// tree. eRPC's `BaseError.Error()` format is:
//
//   <ErrCode>: <message>[ -> (<balanced-parens-json>)]
//     [\ncaused by: <ErrCode>: <message>[ -> (<json>)]
//     [\ncaused by: ...]]
//
// Notes:
//   * "caused by:" boundaries can use either a real \n or escaped \\n
//     depending on whether the string has gone through JSON encoding.
//   * The JSON details are inside parentheses; the JSON itself may
//     contain nested braces. We use a balanced-paren walker to find
//     the matching close — naïve regex .*? misses nested objects.
//   * No assumption of code → message format on the deepest level —
//     if the recursion encounters something that doesn't look like an
//     ErrCode-prefixed line, we render it as-is.
function parseErpcError(s) {
  if (!s) return { code: "", message: "" };
  s = String(s).trim();
  // Split into levels at every "caused by:" boundary (both encoded
  // and literal newline variants are accepted).
  const levels = splitCausedBy(s);
  const nodes = levels.map(parseErpcLevel).filter(Boolean);
  if (nodes.length === 0) return { code: "", message: s.slice(0, 300) };
  // Chain causes back together so the popup can render the tree.
  for (let i = 0; i < nodes.length - 1; i++) nodes[i].cause = nodes[i + 1];
  return nodes[0];
}

function splitCausedBy(s) {
  // Accept all three:  "\ncaused by: ", "  \\ncaused by: ", " caused by: "
  // The two alternatives are deliberately DISJOINT so the `+` quantifier
  // can't backtrack exponentially on long whitespace runs:
  //   \\n — the two-character literal `\n` (e.g. from a JSON-encoded error)
  //   \s  — a single whitespace char (covers real \n / \r / space / tab)
  return s.split(/(?:\\n|\s)+caused by:\s*/);
}

function parseErpcLevel(part) {
  if (!part) return null;
  part = part.trim();
  // <Code>: <rest>
  const m = part.match(/^(Err[A-Za-z0-9]+):\s*([\s\S]*)$/);
  if (!m) return { code: "", message: part };
  const code = m[1];
  let rest = m[2];
  // Pull the " -> (<json>)" detail block off the end, if present.
  let details = null;
  const arrowIdx = rest.lastIndexOf(" -> (");
  if (arrowIdx > -1) {
    const inner = rest.slice(arrowIdx + 5); // skip ` -> (`
    let depth = 1, end = -1;
    for (let i = 0; i < inner.length; i++) {
      const c = inner[i];
      if (c === "(") depth++;
      else if (c === ")") { depth--; if (depth === 0) { end = i; break; } }
    }
    if (end > -1) {
      const jsonText = inner.slice(0, end);
      try { details = JSON.parse(jsonText); }
      catch { details = jsonText; }
      rest = rest.slice(0, arrowIdx).trim();
    }
  }
  let message = rest.trim();
  if (!message) message = "—";
  return { code, message, details };
}

function ErrorDetailPopup({ parsed, raw, onClose, pos }) {
  // Portaled to document.body so the popup ALWAYS sits on top of the
  // drawer / panels / overlays — being inside the drawer DOM put it
  // under the drawer's own stacking context, which masked the
  // z-index: 9999.
  React.useEffect(() => {
    function onKey(e) { if (e.key === "Escape") onClose(); }
    function onClick(e) {
      const el = document.querySelector(".lc-err-popup");
      if (el && !el.contains(e.target)) onClose();
    }
    window.addEventListener("keydown", onKey);
    setTimeout(() => window.addEventListener("click", onClick), 0);
    return () => {
      window.removeEventListener("keydown", onKey);
      window.removeEventListener("click", onClick);
    };
  }, [onClose]);
  const style = { top: pos.top, right: pos.right };
  const popup = (
    <div className="lc-err-popup" style={style} onClick={(e) => e.stopPropagation()}>
      <div className="lc-err-popup-hd">
        <span className="ttl">error chain</span>
        <span className="lc-err-popup-depth">{countDepth(parsed)} {countDepth(parsed) === 1 ? "level" : "levels"}</span>
        <span style={{flex:1}} />
        <button className="x" onClick={onClose}>✕</button>
      </div>
      <div className="lc-err-popup-bd">
        <ErrorNodeView node={parsed} depth={0} />
      </div>
      <details className="lc-err-popup-raw">
        <summary>raw</summary>
        <pre>{raw}</pre>
      </details>
    </div>
  );
  return ReactDOM.createPortal(popup, document.body);
}

function countDepth(node) {
  let n = 0;
  for (let c = node; c; c = c.cause) n++;
  return n;
}

function ErrorNodeView({ node, depth }) {
  if (!node) return null;
  return (
    <div className={`lc-err-node depth-${depth}`}>
      <div className="row">
        <span className="lc-err-code lg">{node.code || "Error"}</span>
        <span className="lc-err-msg">{node.message}</span>
      </div>
      {node.details && (
        <pre className="lc-err-details">
          {typeof node.details === "string" ? node.details : JSON.stringify(node.details, null, 2)}
        </pre>
      )}
      {node.cause && <ErrorNodeView node={node.cause} depth={depth + 1} />}
    </div>
  );
}

// FinalResponseSection renders the response (or error) the CLIENT
// received from `net.Forward`. For successful requests we pretty-print
// the JSON body; for failures we run the error string through the same
// parseErpcError + structured-popup machinery we already use for
// per-attempt errors. This is the section that answers "the request
// shows outcome:fail but every attempt was won — what actually
// happened?" — typically a consensus dispute where individual
// upstreams succeeded but disagreed on the data.
function FinalResponseSection({ req }) {
  const [open, setOpen] = React.useState(false);
  if (req.requestError) {
    return (
      <div className="drw-section">
        <div className="lbl">final error · what the client saw</div>
        <FinalErrorView raw={req.requestError} />
      </div>
    );
  }
  // Success — render a pretty-printed JSON-RPC response.
  let parsed = null;
  try { parsed = JSON.parse(req.responseBody); } catch { /* leave null */ }
  const pretty = parsed ? JSON.stringify(parsed, null, 2) : req.responseBody;
  const isLong = pretty.length > 320;
  return (
    <div className="drw-section">
      <div className="lbl" style={{display:"flex",alignItems:"center",gap:8}}>
        <span>final response · what the client received</span>
        {isLong && (
          <button className="chip-btn" onClick={() => setOpen(o => !o)} style={{fontSize:"9.5px",padding:"1px 6px"}}>
            {open ? "collapse" : "expand"}
          </button>
        )}
      </div>
      <pre className="drw-response">
        {isLong && !open ? pretty.slice(0, 320) + "…" : pretty}
      </pre>
    </div>
  );
}

// FinalErrorView renders the error chain the same way per-attempt
// errors do, so the visual language is consistent: top-level code
// chip, message, expandable popup with the full caused-by tree.
function FinalErrorView({ raw }) {
  const [popup, setPopup] = React.useState(null);
  const btnRef = React.useRef(null);
  const parsed = parseErpcError(raw);
  function toggle(e) {
    e.stopPropagation();
    if (popup) { setPopup(null); return; }
    const r = btnRef.current?.getBoundingClientRect();
    if (!r) return;
    setPopup({
      top: r.bottom + 6,
      right: Math.max(16, window.innerWidth - r.right - 4),
    });
  }
  return (
    <div className="drw-final-error">
      <div className="row">
        <span className="lc-err-code lg">{parsed.code || "Error"}</span>
        <span className="lc-err-msg">{parsed.message}</span>
        <button
          ref={btnRef}
          className={`lc-err-toggle ${popup ? "on" : ""}`}
          onClick={toggle}
          title={popup ? "Hide details" : "Show full error chain"}
        >{popup ? "−" : "+"}</button>
      </div>
      {parsed.details && (
        <pre className="lc-err-details">
          {typeof parsed.details === "string" ? parsed.details : JSON.stringify(parsed.details, null, 2)}
        </pre>
      )}
      {popup && (
        <ErrorDetailPopup parsed={parsed} raw={raw} onClose={() => setPopup(null)} pos={popup} />
      )}
    </div>
  );
}

// buildLifecycleRows interleaves "gap" rows between attempts so the
// drawer surfaces WHY each attempt started when it did. Two distinct
// cases:
//
//   * Sequential gap (next.t0 ≥ previous.end):
//     the executor was sleeping between attempts — failsafe's
//     retry-delay + jitter + backoffFactor. Rendered as a
//     dashed "⏱ retry backoff · Xms" row.
//
//   * Parallel overlap (next.t0 < previous.end AND > previous.t0):
//     a hedge was fan-out from the network executor at +hedgeDelay
//     while the previous attempt was still in flight. Rendered as a
//     "⚡ hedge fan-out · Xms after primary" row, immediately before
//     the hedge attempt. The hedge attempt ALSO gets an inline
//     "⚡ hedge" chip so it's clear that row was concurrent with the
//     primary, not a sequential retry.
//
// Attempts are sorted by `t0` ascending so the timeline reads
// chronologically top-to-bottom (primary first, hedge after, the
// possible second-sweep retries last).
function buildLifecycleRows(attempts) {
  if (!attempts || attempts.length === 0) return [];
  const sorted = [...attempts].sort((a, b) => a.t0 - b.t0);
  const rows = [];
  let prevEnd = -1;
  let prevT0 = -1;
  sorted.forEach((a, i) => {
    if (i > 0) {
      if (a.t0 >= prevEnd) {
        // Sequential gap = retry backoff time.
        const gap = a.t0 - prevEnd;
        if (gap > 3) {
          rows.push({ kind: "gap", t0: prevEnd, dur: gap, label: "retry backoff", icon: "⏱" });
        }
      } else if (a.t0 > prevT0) {
        // Parallel overlap = hedge fired this many ms after primary.
        const hedgeAt = a.t0;
        rows.push({
          kind: "gap",
          t0: prevT0,
          dur: hedgeAt - prevT0,
          label: "hedge fan-out",
          icon: "⚡",
          isHedge: true,
        });
      }
    }
    const isHedgeParallel = i > 0 && a.t0 < prevEnd;
    rows.push({ kind: "attempt", a, isHedgeParallel });
    prevT0 = a.t0;
    if (a.t0 + a.dur > prevEnd) prevEnd = a.t0 + a.dur;
  });
  return rows;
}

// ============== Drawer ==============
function EventDrawer({ req, onClose }) {
  // Persisted resizable width.
  const [drawerW, setDrawerW] = React.useState(() => {
    try {
      const v = parseInt(localStorage.getItem("erpc-sim-drawer-w") || "");
      return isFinite(v) && v > 320 ? v : 460;
    } catch { return 460; }
  });
  React.useEffect(() => {
    try { localStorage.setItem("erpc-sim-drawer-w", String(drawerW)); } catch (_) {}
  }, [drawerW]);
  function onResizeStart(e) {
    e.preventDefault();
    document.body.classList.add("dragging-split", "col-resize");
    const startX = e.clientX;
    const startW = drawerW;
    function move(ev) {
      // Drawer is anchored to the right edge of the viewport. Dragging
      // its left handle to the LEFT increases the drawer width.
      const newW = Math.max(320, Math.min(window.innerWidth - 80, startW + (startX - ev.clientX)));
      setDrawerW(newW);
    }
    function up() {
      document.body.classList.remove("dragging-split", "col-resize");
      window.removeEventListener("mousemove", move);
      window.removeEventListener("mouseup", up);
    }
    window.addEventListener("mousemove", move);
    window.addEventListener("mouseup", up);
  }

  if (!req) return <div className="drawer-back" />;
  const maxDur = Math.max(...(req.attempts || []).map(a => a.t0 + a.dur), req.duration);
  // Consensus end-state — derived once for the whole drawer. Used by
  // per-attempt rows (to relabel "won" → "responded" / etc. when the
  // aggregator rejected everyone) and by the synthesized "consensus
  // result" row appended after the last attempt.
  const csState = (() => {
    if ((req.consensusSlots || 0) === 0) return null;
    if (req.outcome !== "fail") return "agreed";
    const err = req.requestError || "";
    if (err.includes("ErrConsensusDispute")) return "disputed";
    if (err.includes("ErrConsensusLowParticipants")) return "low-participants";
    return "failed";
  })();
  return (
    <>
      <div className={`drawer-back ${req ? "on" : ""}`} onClick={onClose} />
      <div className={`drawer ${req ? "on" : ""}`} style={{ width: drawerW }}>
        {/* Left-edge resizer (dragging makes the drawer wider/narrower). */}
        <div className="drawer-resize" onMouseDown={onResizeStart} title="Drag to resize" />
        <div className="drawer-hd">
          <span style={{fontFamily:"var(--font-mono)",color:"var(--tx-3)",fontSize:"11px"}}>req #{req.id}</span>
          <span className="mth">{req.method}</span>
          <span className="spacer" />
          <button className="x" onClick={onClose}>✕</button>
        </div>
        <div className="drawer-bd">
          <div className="drw-section">
            <div className="pills">
              <span className="meta-pill"><span className="k">outcome</span>{req.outcome}</span>
              <span className="meta-pill"><span className="k">winner</span>{req.winner || "—"}</span>
              <span className="meta-pill"><span className="k">duration</span>{req.duration.toFixed(0)}ms</span>
              <span className="meta-pill"><span className="k">attempts</span>{(req.attempts || []).length}</span>
              {req.usedHedge && <span className="meta-pill"><span className="k">hedge</span>fired</span>}
              {req.usedRetry && <span className="meta-pill"><span className="k">retry</span>fired</span>}
              {req.consensusSlots > 0 && (
                <span className="meta-pill consensus-pill">
                  <span className="k">consensus</span>{req.consensusSlots} slots
                </span>
              )}
              {req.consensusDisputes > 0 && (
                <span className="meta-pill consensus-dispute">
                  <span className="k">dispute</span>{req.consensusDisputes}
                </span>
              )}
              {req.consensusLowParts > 0 && (
                <span className="meta-pill consensus-low">
                  <span className="k">low parts</span>{req.consensusLowParts}
                </span>
              )}
            </div>
          </div>

          {(req.sel || []).length > 0 && (
            <div className="drw-section">
              <div className="lbl">selection trail · evalFunc returned</div>
              <div className="sel-trail">
                {req.sel.map((s, i) => {
                  const winner = s.id === req.winner;
                  const cls = winner ? "winner" : s.excluded ? "excluded" : "";
                  return (
                    <div key={i} className={`it ${cls}`}>
                      <span className="idx">{s.excluded ? "—" : s.idx}</span>
                      <span className="nm">{s.id}</span>
                      <span className="why">{s.excluded ? `excluded · ${s.reason}` : `score ${s.score?.toFixed(3)}`}</span>
                    </div>
                  );
                })}
              </div>
            </div>
          )}

          {/* Final response / error — what the CLIENT actually saw.
              For failed requests this surfaces things invisible from
              per-attempt rows (e.g. consensus dispute where all
              participants ok'd but disagreed on data). For successful
              requests it shows the actual JSON-RPC result body. */}
          {(req.requestError || req.responseBody) && (
            <FinalResponseSection req={req} />
          )}

          <div className="drw-section">
            <div className="lbl">lifecycle</div>
            <div className="lifecycle">
              {buildLifecycleRows(req.attempts || []).map((row, i) => {
                if (row.kind === "gap") {
                  // Sequential retry-backoff (dashed line) vs parallel
                  // fan-out. When consensus is active, the parallel
                  // fan-out IS the consensus spawning its N participants
                  // — relabel the row so it's not confused with hedge.
                  const offsetPct = (row.t0 / maxDur) * 100;
                  const widthPct = (row.dur / maxDur) * 100;
                  const inConsensus = (req.consensusSlots || 0) > 0;
                  const cls = row.isHedge
                    ? (inConsensus ? "lc-gap consensus" : "lc-gap hedge")
                    : "lc-gap";
                  const icon = row.isHedge
                    ? (inConsensus ? "⊕" : "⚡")
                    : "⏱";
                  const labelMain = row.isHedge
                    ? (inConsensus ? "consensus slot spawned" : "hedge fired")
                    : "wait";
                  const labelDesc = row.isHedge
                    ? (inConsensus ? "consensus fan-out" : row.label)
                    : row.label;
                  return (
                    <div key={"gap-" + i} className={cls}>
                      <div className="hd">
                        <span className="lbl">{icon} {labelMain}</span>
                        <span className="why">{labelDesc}</span>
                        <span className="dur">+{row.dur.toFixed(0)}ms after primary</span>
                      </div>
                      <div className="latbar">
                        <div className="gapfill" style={{ left: offsetPct + "%", width: widthPct + "%" }} />
                      </div>
                    </div>
                  );
                }
                const a = row.a;
                // Color semantics (per user spec):
                //   green   = winner of any flavor
                //   amber   = miss / no data / throttled (bad event,
                //             not an error)
                //   red     = upstream error (timeout, fail, cb-open)
                //   blue    = harmless / parallel attempt that lost
                //             its race (hedge-loser, cancelled)
                //
                // Consensus override: if this request went through
                // consensus AND failed at the aggregator (disputed /
                // low-participants), the per-participant `won` flag
                // is misleading — none of them actually contributed
                // to the final response. We render them as amber
                // ("responded") instead of green ("won"), and add a
                // synthesized "consensus result" row at the bottom
                // (see below) showing the aggregator's verdict.
                let cls;
                const consensusFailed = csState === "disputed" || csState === "low-participants" || csState === "failed";
                if (a.winner && consensusFailed) {
                  // amber — they DID respond with data; the aggregator just
                  // rejected the response. The `consensus-loser` modifier
                  // suppresses the "no data · " CSS prefix that .miss would
                  // otherwise prepend (those participants returned data).
                  cls = "miss consensus-loser";
                } else if (a.winner) {
                  cls = "ok";
                } else if (a.outcome === "miss" || a.outcome === "throttled") {
                  cls = "miss";
                } else if (a.outcome === "hedge-loser") {
                  cls = "neutral hedge-loser";
                } else if (a.outcome === "cb-open" ||
                           a.outcome === "timeout" ||
                           a.outcome === "fail") {
                  cls = "bad";
                } else {
                  cls = "neutral";
                }
                const widthPct = (a.dur / maxDur) * 100;
                const offsetPct = (a.t0 / maxDur) * 100;
                const isHedgeParallel = row.isHedgeParallel;
                // Cumulative wall-clock at end of this attempt — the
                // "time so far" the user reads on the RIGHT.
                const cumulativeMs = a.t0 + a.dur;
                // `+` = this step's duration contributed sequentially.
                // `~` = parallel work that didn't add to the request's
                //       total time (typically a hedge participant that
                //       lost; or any attempt running concurrently with
                //       another and not the winner).
                const sequentialAdd = !isHedgeParallel && !(a.isHedge && !a.winner);
                const durPrefix = sequentialAdd ? "+" : "~";
                // Per-attempt selection-reason chip — clarifies WHY this
                // attempt fired. The decision tree:
                //   1. If the WHOLE request went through consensus
                //      (req.consensusSlots > 0), any parallel attempt
                //      is a consensus participant — label it as such
                //      even though eRPC's internal SelectionReason may
                //      say "hedge" (because consensus is implemented
                //      on top of the hedge fan-out machinery).
                //   2. Otherwise fall back to the SelectionReason
                //      field as recorded by the executors.
                const inConsensus = (req.consensusSlots || 0) > 0;
                let reasonChip = null;
                if (a.selReason === "retry" || a.isRetry) {
                  reasonChip = <span className="lc-tag retry">↻ retry #{a.attemptIdx || i}</span>;
                } else if (inConsensus && (isHedgeParallel || i === 0)) {
                  reasonChip = <span className="lc-tag consensus">⊕ consensus #{i + 1}</span>;
                } else if (a.selReason === "hedge" || isHedgeParallel) {
                  reasonChip = <span className="lc-tag hedge">⚡ hedge</span>;
                } else if (a.selReason === "consensus_slot") {
                  reasonChip = <span className="lc-tag consensus">⊕ consensus</span>;
                } else if (a.selReason === "sweep") {
                  reasonChip = <span className="lc-tag sweep">↳ sweep #{a.attemptIdx || 1}</span>;
                } else if (i === 0) {
                  // First attempt in sequence — primary call (no other reason matched).
                  reasonChip = <span className="lc-tag primary">primary</span>;
                } else {
                  // Position-based fallback: a 2nd+ attempt with no specific
                  // reason from the backend is a sweep-fallback after the
                  // prior upstream failed (e.g. empty result on a sparse
                  // method, transient error). eRPC's executor sometimes
                  // doesn't tag these as `retry` because they happen
                  // within ONE sweep iteration (no NetworkRetries bump),
                  // so this position-heuristic surfaces them as retries
                  // from the operator's point of view.
                  reasonChip = <span className="lc-tag retry">↻ retry #{i}</span>;
                }
                return (
                  <div key={i} className={`lc-node ${cls}`}>
                    <div className="hd">
                      <span className="up">{a.id}</span>
                      <span className="why">
                        {reasonChip}
                        <AttemptReason a={a} csState={csState} />
                      </span>
                      <span className="dur">
                        <span className="dur-step">{durPrefix}{a.dur.toFixed(0)}ms</span>
                        <span className="dur-sep">·</span>
                        <span className="dur-cumulative">{cumulativeMs.toFixed(0)}ms</span>
                      </span>
                    </div>
                    <div className="latbar">
                      <div className="fill" style={{ left: offsetPct + "%", width: widthPct + "%" }} />
                    </div>
                  </div>
                );
              })}
              {csState && <ConsensusResultRow req={req} csState={csState} />}
            </div>
          </div>
        </div>
      </div>
    </>
  );
}

// ConsensusResultRow is the SYNTHETIC final row showing what the
// consensus aggregator decided. Without this the lifecycle ends with
// the last participant marked "won" — confusing because the request
// actually failed AT the aggregator AFTER all participants responded.
// Colors per the project spec:
//   green = agreed   (consensus reached, request succeeded)
//   red   = disputed (no agreement, returnError fired)
//   amber = low-participants (didn't meet threshold)
//   red   = failed   (other aggregator failure)
function ConsensusResultRow({ req, csState }) {
  const map = {
    agreed:            { cls: "ok",   icon: "✓", label: "consensus agreed",     why: `${req.consensusSlots} participants reached agreement` },
    disputed:          { cls: "bad",  icon: "✗", label: "consensus DISPUTED",   why: `${req.consensusSlots} participants disagreed on the response` },
    "low-participants":{ cls: "miss", icon: "△", label: "consensus low quorum", why: `fewer than threshold participants responded` },
    failed:            { cls: "bad",  icon: "✗", label: "consensus FAILED",     why: "aggregator could not resolve" },
  };
  const m = map[csState];
  if (!m) return null;
  return (
    <div className={`lc-node lc-consensus-result ${m.cls}`}>
      <div className="hd">
        <span className="up">{m.icon} {m.label}</span>
        <span className="why">{m.why}</span>
        <span className="dur">
          <span className="dur-cumulative">{req.duration.toFixed(0)}ms total</span>
        </span>
      </div>
      <div className="latbar">
        <div className="fill" style={{ left: "0%", width: "100%" }} />
      </div>
    </div>
  );
}

window.RightCol = RightCol;
window.EventDrawer = EventDrawer;
