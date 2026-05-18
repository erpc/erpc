// upstream-knobs.jsx — per-upstream synthetic knobs.

const { useEffect, useState } = React;

function UpstreamKnobs() {
  const upstreams = window.useUpstreams();
  const upstreamStats = window.useUpstreamStats();
  const sparkErr = window.useSparkErr();
  const cbState = window.useCBState();
  const focused = window.useFocusedUpstream();
  const actions = window.useSimActions();

  // Derive position pills from each upstream's last-second rank as
  // emitted by the server. `tip` is the tooltip text — empty for the
  // numeric positions, explanatory for the "out" state.
  function posOf(id) {
    const s = upstreamStats[id];
    if (!s) return { pos: "?", cls: "", tip: "" };
    if ((s.lastPosition ?? -1) < 0) {
      return {
        pos: "out",
        cls: "pos-x",
        tip: "Excluded by the selection policy — this upstream is NOT in the request rotation, retry chain, or hedge fan-out for the current network/method. It re-enters only when the policy's filters admit it again (e.g. error rate drops, CB closes, or .probeExcluded() re-admits it).",
      };
    }
    return { pos: String(s.lastPosition), cls: `pos-${Math.min(2, s.lastPosition)}`, tip: "" };
  }

  function setUp(id, patch) { actions.patchUpstream(id, patch); }
  function bulk(predicate, patch) {
    for (const u of upstreams) if (predicate(u)) actions.patchUpstream(u.id, patch);
  }
  function inject(id) { actions.injectFailure(id, 60_000); }

  return (
    <div className="knobs-wrap">
      <div className="knobs-toolbar">
        <span className="label">bulk</span>
        <button className="chip-btn good" onClick={() => bulk(() => true, { error: 0.005, base: 50, jitter: 18, timeoutRate: 0.001, throttleRate: 0.002, available: true })}>all healthy</button>
        <button className="chip-btn warm" onClick={() => bulk(() => true, { error: 0.05, base: 90, jitter: 35 })}>all degraded</button>
        <button className="chip-btn danger" onClick={() => bulk(u => u.vendor === "alchemy", { available: false })}>outage all alchemy</button>
        <button className="chip-btn" onClick={() => bulk(u => u.vendor === "alchemy", { available: true })}>restore alchemy</button>
        <button className="chip-btn warm" onClick={() => bulk(u => (u.tags || []).includes("eth"), { blockLag: 6 })}>+6 blocks lag</button>
      </div>

      <div className="knobs-table">
        <div className="kt-head">
          <div className="col">upstream</div>
          <div className="col">base latency (ms)</div>
          <div className="col">jitter (ms)</div>
          <div className="col">error rate</div>
          <div className="col">timeout</div>
          <div className="col">throttle</div>
          <div className="col">block lag</div>
          <div className="col">block time (ms)</div>
          <div className="col" title="Probability this upstream has data for sparse-data methods (receipts, getBlockByHash, getLogs, getTransactionByHash). 100% = always has data (no synthetic misses). 0% = always misses. Intermediate values multiply the method's natural sparsity baseline.">data avail</div>
          <div className="col">avail</div>
          <div className="col">position</div>
        </div>

        {upstreams.map((u, idx) => {
          const { pos, cls, tip } = posOf(u.id);
          const isFocused = focused === u.id;
          const isDimmed = focused && !isFocused;
          // Color swatch matches the selection-share stack chart's
          // band for this upstream (same index → same palette entry).
          // Lets the operator visually correlate a band in the chart
          // with the corresponding row here without reading the legend.
          const swatchColor = window.upstreamPaletteColor(idx);
          return (
            <div
              key={u.id}
              className={`kt-row ${cls} ${u.available === false ? "unavail" : ""} ${isFocused ? "focused" : ""} ${isDimmed ? "dimmed" : ""}`}
              onClick={(e) => {
                // Only treat clicks on the identity area (the upstream
                // name / vendor / tags column) as focus toggles, so the
                // sliders below don't constantly retrigger focus when
                // the operator drags them.
                if (e.target.closest(".ident")) {
                  actions.toggleFocusedUpstream(u.id);
                }
              }}
            >
              <div className="ident" title={`Click to ${isFocused ? "clear" : "focus on"} ${u.id}`}>
                <div className="id-row">
                  <span className="up-color" style={{ background: swatchColor }} title={`Chart color · ${u.id}`} />
                  <span className="id">{u.id}</span>
                  <span className="vendor">{u.vendor}</span>
                </div>
                <div className="tags">
                  {(u.tags || []).map(t => <span key={t} className="tag">{t}</span>)}
                </div>
              </div>

              <Slider value={u.base} min={5} max={500} unit="ms"
                cls={u.base > 250 ? "warn" : ""} onChange={v => setUp(u.id, { base: v })} />
              <Slider value={u.jitter} min={0} max={250} unit="ms"
                cls={u.jitter > 150 ? "warn" : ""} onChange={v => setUp(u.id, { jitter: v })} />
              <Slider value={u.error} min={0} max={1} step={0.01} pct
                cls={u.error > 0.2 ? "err" : u.error > 0.05 ? "warn" : ""} onChange={v => setUp(u.id, { error: v })} />
              <Slider value={u.timeoutRate} min={0} max={1} step={0.01} pct
                cls={u.timeoutRate > 0.05 ? "err" : ""} onChange={v => setUp(u.id, { timeoutRate: v })} />
              <Slider value={u.throttleRate} min={0} max={1} step={0.01} pct
                cls={u.throttleRate > 0.05 ? "warn" : ""} onChange={v => setUp(u.id, { throttleRate: v })} />
              <Slider value={u.blockLag} min={0} max={30} step={1} unit="blk"
                cls={u.blockLag > 6 ? "warn" : ""} onChange={v => setUp(u.id, { blockLag: v })} />
              {/* Block time in ms (comma-separated). 12,000 ≈ mainnet;
                  2,000 ≈ L2-ish; 60,000+ biases the upstream toward
                  appearing stuck. */}
              <Slider value={u.blockTimeMs ?? 12000} min={500} max={60000} step={100} unit="ms" thousands
                cls={(u.blockTimeMs ?? 12000) > 24000 ? "warn" : ""}
                onChange={v => setUp(u.id, { blockTimeMs: Math.round(v) })} />
              {/* DataAvailability — 0..1. Lower values make this
                  upstream miss more often on sparse-data methods. */}
              <Slider value={u.dataAvailability ?? 1.0} min={0} max={1} step={0.01} pct
                cls={(u.dataAvailability ?? 1.0) < 0.5 ? "warn" : ""}
                onChange={v => setUp(u.id, { dataAvailability: v })} />

              <div style={{display:"flex",justifyContent:"center"}}>
                <div className={`toggle ${u.available !== false ? "on" : ""}`}
                  onClick={() => setUp(u.id, { available: u.available === false })} />
              </div>

              <div style={{display:"flex",alignItems:"center",gap:6}}>
                <span className="pos-pill" title={tip}>{pos}</span>
                <Spark data={sparkErr[u.id] || []} />
                <button
                  className="inject-btn"
                  onClick={() => inject(u.id)}
                  title="Inject a 60s synthetic failure window">
                  inject 60s
                </button>
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}

function Slider({ value, min, max, step = 1, unit, cls = "", pct, thousands, onChange }) {
  const display = pct
    ? (value * 100).toFixed(0)
    : thousands
    ? Math.round(value).toLocaleString()
    : (step < 1 ? value.toFixed(2) : value.toFixed(0));
  const [editing, setEditing] = useState(display);
  useEffect(() => { setEditing(display); }, [display]);
  function commit() {
    const cleaned = String(editing).replace(/[,\s_]/g, "");
    const n = parseFloat(cleaned);
    if (!isNaN(n)) {
      const v = pct ? n / 100 : n;
      onChange(Math.max(min, Math.min(max, v)));
    } else {
      setEditing(display);
    }
  }
  return (
    <div className={`knob ${cls}`}>
      <input type="range" min={min} max={max} step={step} value={value} onChange={e => onChange(+e.target.value)} />
      <input type="text" className="knob-input"
        value={editing}
        onChange={e => setEditing(e.target.value)}
        onBlur={commit}
        onFocus={e => e.target.select()}
        onKeyDown={e => {
          if (e.key === "Enter") { commit(); e.target.blur(); }
          if (e.key === "Escape") { setEditing(display); e.target.blur(); }
        }} />
      {(pct || unit) && <span className="knob-unit">{pct ? "%" : unit}</span>}
    </div>
  );
}

function Spark({ data }) {
  const W = 70, H = 18;
  if (!data.length) return <svg className="sparkline" viewBox={`0 0 ${W} ${H}`} />;
  const max = Math.max(0.001, ...data, 0.05);
  const pts = data.map((v, i) => `${(i / Math.max(1, data.length - 1)) * W},${H - (v / max) * H}`).join(" ");
  const lastV = data[data.length - 1];
  const stroke = lastV > 0.2 ? "var(--bad)" : lastV > 0.05 ? "var(--warn)" : "var(--ok)";
  return (
    <svg className="sparkline" viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none">
      <polyline points={pts} fill="none" stroke={stroke} strokeWidth="1.2" />
    </svg>
  );
}

window.UpstreamKnobs = UpstreamKnobs;
