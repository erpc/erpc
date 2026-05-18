// policy-history.jsx — POLICY HISTORY tab. Shows a tick-by-tick replay
// of the selection-policy engine's recent decisions so operators can
// answer "why did the order change at T=4.2s?"
//
// Each row = one decision (one tick). Rows are newest-first. The list:
//   * highlights ticks where the primary changed
//   * shows the order (compact) + exclusion count
//   * expands on click to show the full ordered list + per-upstream
//     excluded reason + add/remove diff vs prior tick + eval error
//
// Backend: `state.policyHistoryRing` is a deduped ring buffer fed by
// the WS `policy-history` frame in sim-runtime.js. Up to 200 entries.

function PolicyHistory() {
  const ring = window.usePolicyHistoryRing();
  const [expandedId, setExpandedId] = React.useState(null);
  const [filter, setFilter] = React.useState("all");
  // Filter the ring: "all" / "changes" (PrimaryChanged or order delta) /
  // "errors" (Error != "").
  const filtered = React.useMemo(() => {
    if (!ring || ring.length === 0) return [];
    if (filter === "all") return ring;
    if (filter === "changes") {
      return ring.filter(d => d && (d.primaryChanged || d.orderChanged || (d.added && d.added.length) || (d.removed && d.removed.length)));
    }
    if (filter === "errors") {
      return ring.filter(d => d && d.error);
    }
    return ring;
  }, [ring, filter]);
  return (
    <div className="ph-wrap">
      <div className="ph-toolbar">
        <div className="ph-filter">
          {["all", "changes", "errors"].map(k => (
            <button key={k} className={filter === k ? "on" : ""} onClick={() => setFilter(k)}
              title={k === "all" ? "Every recorded tick" : k === "changes" ? "Ticks where primary changed or order shifted" : "Ticks where the eval errored (timeout / throw / invalid_return)"}>{k}</button>
          ))}
        </div>
        <span style={{flex:1}} />
        <span style={{fontFamily:"var(--font-mono)",fontSize:"10px",color:"var(--tx-3)"}}>
          {ring ? ring.length : 0} ticks · {filtered.length} matching · evalInterval drives cadence
        </span>
      </div>
      {filtered.length === 0 ? (
        <div className="ph-empty">no decisions captured yet — wait for the policy engine to tick (default 1 s)</div>
      ) : (
        <div className="ph-list">
          {filtered.map(d => (
            <PolicyHistoryRow key={d.id} d={d}
              expanded={expandedId === d.id}
              onToggle={() => setExpandedId(expandedId === d.id ? null : d.id)} />
          ))}
        </div>
      )}
    </div>
  );
}

function PolicyHistoryRow({ d, expanded, onToggle }) {
  const ts = new Date(d.tickMs);
  const stamp = `${pad2(ts.getUTCHours())}:${pad2(ts.getUTCMinutes())}:${pad2(ts.getUTCSeconds())}.${pad3(ts.getUTCMilliseconds())}`;
  const primary = (d.order && d.order[0]) || "—";
  const excludedCount = (d.excluded && d.excluded.length) || 0;
  const changeBadge = d.primaryChanged
    ? <span className="ph-badge change">primary →</span>
    : d.orderChanged
    ? <span className="ph-badge order">order shift</span>
    : null;
  const errBadge = d.error ? <span className="ph-badge err">! err</span> : null;
  return (
    <div className={`ph-row ${expanded ? "open" : ""} ${d.primaryChanged ? "primary-changed" : ""} ${d.error ? "errored" : ""}`}
      onClick={onToggle}>
      <div className="ph-row-hd">
        <span className="ts">{stamp}</span>
        <span className="dur">{(d.evalDurationUs / 1000).toFixed(1)}ms</span>
        <span className="primary">primary <b>{primary}</b></span>
        <span className="counts">{(d.order || []).length} in · {excludedCount} out</span>
        <span style={{flex:1}} />
        {changeBadge}
        {errBadge}
        <span className="ph-caret">{expanded ? "▾" : "▸"}</span>
      </div>
      {expanded && <PolicyHistoryDetail d={d} />}
    </div>
  );
}

function PolicyHistoryDetail({ d }) {
  return (
    <div className="ph-detail" onClick={e => e.stopPropagation()}>
      {d.error && (
        <div className="ph-section">
          <div className="ph-section-hd">eval error</div>
          <pre className="ph-err">{d.error}</pre>
        </div>
      )}
      <div className="ph-section">
        <div className="ph-section-hd">order (position · upstream)</div>
        <div className="ph-order">
          {(d.order || []).map((id, i) => (
            <div key={id} className={`ph-order-row ${i === 0 ? "primary" : ""}`}>
              <span className="ph-pos">{i}</span>
              <span className="ph-up">{id}</span>
            </div>
          ))}
        </div>
      </div>
      {d.excluded && d.excluded.length > 0 && (
        <div className="ph-section">
          <div className="ph-section-hd">excluded</div>
          <div className="ph-excluded">
            {d.excluded.map((e, i) => (
              <div key={`${e.id}-${i}`} className="ph-excluded-row">
                <span className="ph-up out">{e.id}</span>
                {e.step && <span className="ph-step">{e.step}</span>}
                {e.reason && <span className="ph-reason">{e.reason}</span>}
              </div>
            ))}
          </div>
        </div>
      )}
      {((d.added && d.added.length) || (d.removed && d.removed.length)) && (
        <div className="ph-section">
          <div className="ph-section-hd">diff vs prior tick</div>
          <div className="ph-diff">
            {(d.added || []).map(id => <span key={"a-"+id} className="ph-diff-add">+ {id}</span>)}
            {(d.removed || []).map(id => <span key={"r-"+id} className="ph-diff-rem">− {id}</span>)}
          </div>
        </div>
      )}
    </div>
  );
}

function pad2(n) { return String(n).padStart(2, "0"); }
function pad3(n) { return String(n).padStart(3, "0"); }

window.PolicyHistory = PolicyHistory;
