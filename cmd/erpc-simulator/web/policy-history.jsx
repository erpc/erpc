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
  // `selectedId` is the tick whose detail is showing in the modal.
  // Defaulting the filter to "changes" matches what the operator
  // actually cares about — "all" floods the list with no-op ticks at
  // steady state.
  const [selectedId, setSelectedId] = React.useState(null);
  const [filter, setFilter] = React.useState("changes");
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
  // Look up the selected decision from the live ring so its detail
  // stays in sync if the same decision id gets re-sent. The ring uses
  // dedupe-by-id so this is effectively a stable pointer.
  const selected = React.useMemo(() => {
    if (!selectedId || !ring) return null;
    return ring.find(d => d && d.id === selectedId) || null;
  }, [selectedId, ring]);
  // ESC closes the modal so the operator can dismiss with the keyboard.
  React.useEffect(() => {
    if (!selectedId) return;
    function onKey(e) { if (e.key === "Escape") setSelectedId(null); }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [selectedId]);
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
        <div className="ph-empty">no decisions match this filter — wait for the policy engine to tick (default 1 s) or switch to "all"</div>
      ) : (
        <div className="ph-list">
          {filtered.map(d => (
            <PolicyHistoryRow key={d.id} d={d}
              selected={selectedId === d.id}
              onSelect={() => setSelectedId(d.id)} />
          ))}
        </div>
      )}
      {selected && (
        <PolicyHistoryModal d={selected} onClose={() => setSelectedId(null)} />
      )}
    </div>
  );
}

function PolicyHistoryRow({ d, selected, onSelect }) {
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
    <div className={`ph-row ${selected ? "selected" : ""} ${d.primaryChanged ? "primary-changed" : ""} ${d.error ? "errored" : ""}`}
      onClick={onSelect}>
      <div className="ph-row-hd">
        <span className="ts">{stamp}</span>
        <span className="dur">{(d.evalDurationUs / 1000).toFixed(1)}ms</span>
        <span className="primary">primary <b>{primary}</b></span>
        <span className="counts">{(d.order || []).length} in · {excludedCount} out</span>
        <span style={{flex:1}} />
        {changeBadge}
        {errBadge}
        <span className="ph-caret">▸</span>
      </div>
    </div>
  );
}

// PolicyHistoryModal is the focused-detail overlay opened when the
// operator clicks a tick row. Backdrop click and Escape both close it
// (Escape is wired in the parent's keydown effect). The dialog body
// reuses `PolicyHistoryDetail` so the rendered sections stay 1:1 with
// the inline-detail shape the runtime backend wires up.
function PolicyHistoryModal({ d, onClose }) {
  const ts = new Date(d.tickMs);
  const stamp = `${pad2(ts.getUTCHours())}:${pad2(ts.getUTCMinutes())}:${pad2(ts.getUTCSeconds())}.${pad3(ts.getUTCMilliseconds())}`;
  return (
    <div className="ph-modal-backdrop" onClick={onClose}>
      <div className="ph-modal" onClick={e => e.stopPropagation()} role="dialog" aria-modal="true">
        <div className="ph-modal-hd">
          <div className="ph-modal-hd-l">
            <span className="ts">{stamp}</span>
            <span className="dur">{(d.evalDurationUs / 1000).toFixed(1)}ms</span>
            <span className="netmethod">{d.networkId} · {d.method}</span>
            {d.primaryChanged && <span className="ph-badge change">primary →</span>}
            {!d.primaryChanged && d.orderChanged && <span className="ph-badge order">order shift</span>}
            {d.error && <span className="ph-badge err">! err</span>}
          </div>
          <button className="ph-modal-close" onClick={onClose} title="Close (Esc)">×</button>
        </div>
        <div className="ph-modal-body">
          <PolicyHistoryDetail d={d} />
        </div>
      </div>
    </div>
  );
}

// PolicyTipContext exposes a setter the UpstreamPill components below
// use to publish their hovered (id, anchor-rect) to the surrounding
// PolicyHistoryDetail. The detail renders a single floating tooltip
// at viewport scope (position: fixed) so it escapes the modal body's
// `overflow: auto` clipping — without this, tooltips on pills near the
// bottom of the scroll area would be cut off.
const PolicyTipContext = React.createContext(null);

function PolicyHistoryDetail({ d }) {
  const metrics = d.metrics || {};
  const scores = d.perUpstreamScore || {};
  const steps = d.steps || [];
  const [tip, setTip] = React.useState(null); // { id, rect } | null
  return (
    <PolicyTipContext.Provider value={setTip}>
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
              <UpstreamPill id={id} metrics={metrics[id]} score={scores[id]} className="ph-up" />
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
                <UpstreamPill id={e.id} metrics={metrics[e.id]} score={scores[e.id]} className="ph-up out" />
                {e.step && <span className="ph-step">{e.step}</span>}
                {e.reason && e.reason !== e.step && (
                  <span className="ph-reason">{e.reason}</span>
                )}
              </div>
            ))}
          </div>
        </div>
      )}
      {steps.length > 0 && (
        <div className="ph-section">
          <div className="ph-section-hd">step trail
            <span className="ph-section-sub">{steps.length} steps · stdlib chain order</span>
          </div>
          <div className="ph-steps">
            {steps.map((s, i) => (
              <PolicyStepRow key={i} idx={i} s={s} metrics={metrics} scores={scores} />
            ))}
          </div>
        </div>
      )}
      {((d.added && d.added.length) || (d.removed && d.removed.length)) && (
        <div className="ph-section">
          <div className="ph-section-hd">diff vs prior tick</div>
          <div className="ph-diff">
            {(d.added || []).map(id => (
              <UpstreamPill key={"a-"+id} id={id} metrics={metrics[id]} score={scores[id]}
                className="ph-diff-add" prefix="+ " />
            ))}
            {(d.removed || []).map(id => (
              <UpstreamPill key={"r-"+id} id={id} metrics={metrics[id]} score={scores[id]}
                className="ph-diff-rem" prefix="− " />
            ))}
          </div>
        </div>
      )}
    </div>
    {tip && <UpstreamTooltip {...tip} metrics={metrics[tip.id]} score={scores[tip.id]} />}
    </PolicyTipContext.Provider>
  );
}

// UpstreamPill is a self-contained pill that publishes hover state to
// PolicyTipContext. The pill itself just renders its label; the
// floating tooltip is drawn once at the detail-component level (see
// `UpstreamTooltip` below) with `position: fixed` so it escapes the
// modal body's overflow clipping.
function UpstreamPill({ id, metrics, score, className, prefix }) {
  const setTip = React.useContext(PolicyTipContext);
  const ref = React.useRef(null);
  const hasData = !!metrics || score != null;
  const onEnter = React.useCallback(() => {
    if (!hasData || !setTip || !ref.current) return;
    setTip({ id, rect: ref.current.getBoundingClientRect() });
  }, [hasData, setTip, id]);
  const onLeave = React.useCallback(() => {
    if (!setTip) return;
    setTip(null);
  }, [setTip]);
  return (
    <span ref={ref}
      className={`ph-up-pill ${className || ""} ${hasData ? "has-tip" : ""}`}
      onMouseEnter={onEnter} onMouseLeave={onLeave}>
      {prefix}{id}
    </span>
  );
}

// UpstreamTooltip is the floating popover rendered at viewport scope.
// Anchors to the pill's bounding rect and flips above/below depending
// on which side has more room. Updates its own measurements via a ref
// so the initial render (which doesn't know the tooltip's actual
// height yet) can correct itself on the next layout tick.
function UpstreamTooltip({ id, rect, metrics, score }) {
  const ref = React.useRef(null);
  const [placement, setPlacement] = React.useState({ top: rect.bottom + 4, left: rect.left, flipped: false });
  React.useLayoutEffect(() => {
    if (!ref.current) return;
    const tipRect = ref.current.getBoundingClientRect();
    const vw = window.innerWidth;
    const vh = window.innerHeight;
    let top = rect.bottom + 4;
    let left = rect.left;
    let flipped = false;
    // Flip above the anchor if it would overflow the viewport bottom AND
    // there's room above (i.e. the anchor isn't pinned to the top).
    if (top + tipRect.height > vh - 8 && rect.top - tipRect.height - 4 > 8) {
      top = rect.top - tipRect.height - 4;
      flipped = true;
    }
    // Nudge left so the tooltip never escapes the viewport on the right.
    if (left + tipRect.width > vw - 8) {
      left = Math.max(8, vw - tipRect.width - 8);
    }
    if (left < 8) left = 8;
    setPlacement({ top, left, flipped });
  }, [rect, id]);
  return (
    <div ref={ref} className={`ph-tip-floating ${placement.flipped ? "flipped" : ""}`}
      style={{ top: placement.top, left: placement.left }}
      role="tooltip">
      <span className="ph-tip-hd">{id}</span>
      {score != null && (
        <span className="ph-tip-row score">
          <span className="k">score</span>
          <span className="v"><b>{fmtScore(score)}</b> <span className="muted">lower = better</span></span>
        </span>
      )}
      {metrics && <MetricsRows m={metrics} />}
      {!metrics && score == null && (
        <span className="ph-tip-row"><span className="muted">no tick data — outside this tick's input set</span></span>
      )}
    </div>
  );
}

// MetricsRows renders one tick's per-upstream metrics in the same
// dense key/value layout the tooltip and the sortByScore breakdown
// share. Units are explicit (ms / s / fraction / blocks) so the
// operator never has to guess what they're looking at.
function MetricsRows({ m }) {
  return (
    <>
      <span className="ph-tip-row">
        <span className="k">errorRate</span>
        <span className={`v ${m.errorRate > 0.5 ? "bad" : m.errorRate > 0.05 ? "warn" : ""}`}>
          {fmtPct(m.errorRate)}<span className="muted"> · {m.errorsTotal}/{m.requestsTotal}</span>
        </span>
      </span>
      <span className="ph-tip-row">
        <span className="k">throttledRate</span>
        <span className={`v ${m.throttledRate > 0.3 ? "bad" : m.throttledRate > 0.05 ? "warn" : ""}`}>{fmtPct(m.throttledRate)}</span>
      </span>
      <span className="ph-tip-row">
        <span className="k">misbehaviorRate</span>
        <span className={`v ${m.misbehaviorRate > 0.05 ? "warn" : ""}`}>{fmtPct(m.misbehaviorRate)}</span>
      </span>
      <span className="ph-tip-row">
        <span className="k">p50 / p70 / p95</span>
        <span className={`v ${m.p95Ms > 10_000 ? "bad" : m.p95Ms > 2_000 ? "warn" : ""}`}>
          {fmtMs(m.p50Ms)} · {fmtMs(m.p70Ms)} · {fmtMs(m.p95Ms)}
        </span>
      </span>
      <span className="ph-tip-row">
        <span className="k">blockHeadLag</span>
        <span className={`v ${m.blockHeadLag > 16 ? "bad" : m.blockHeadLag > 4 ? "warn" : ""}`}>
          {m.blockHeadLag} blocks
          {m.blockHeadLagSeconds > 0 && <span className="muted"> · {m.blockHeadLagSeconds.toFixed(1)}s</span>}
        </span>
      </span>
      <span className="ph-tip-row">
        <span className="k">finalizationLag</span>
        <span className="v">
          {m.finalizationLag} blocks
          {m.finalizationLagSeconds > 0 && <span className="muted"> · {m.finalizationLagSeconds.toFixed(1)}s</span>}
        </span>
      </span>
    </>
  );
}

// PolicyStepRow renders one entry of the per-step trail. The compact
// row shows step name · arg summary · in→out counts · dropped/added
// pills. Clicking expands to a full in/out list — useful when the
// step reordered a large set or when you want to see exactly which
// upstreams survived a `preferTag` filter. For score-sorting steps the
// expanded body also includes a sortable breakdown table with the
// per-upstream metric values that produced the order.
function PolicyStepRow({ idx, s, metrics, scores }) {
  const [open, setOpen] = React.useState(false);
  const argSummary = React.useMemo(() => summarizeStepArgs(s.args), [s.args]);
  const inN = (s.in || []).length;
  const outN = (s.out || []).length;
  const dropped = s.dropped || [];
  const added = s.added || [];
  const isNoop = dropped.length === 0 && added.length === 0 && !s.reordered;
  const isScoreStep = isScoringStep(s.step);
  // Toggle lives on the HEADER only — putting it on the outer row
  // would collapse the accordion whenever the user tries to select
  // text or click a pill inside the expanded body. Body content stays
  // freely interactable; click the header strip (or the caret) to
  // collapse back.
  return (
    <div className={`ph-step-row ${open ? "open" : ""} ${isNoop ? "noop" : ""}`}>
      <div className="ph-step-hd" onClick={() => setOpen(o => !o)}>
        <span className="ph-step-idx">{idx + 1}</span>
        <span className="ph-step-name">.{s.step}</span>
        {argSummary && <span className="ph-step-args">({argSummary})</span>}
        <span style={{flex:1}} />
        <span className="ph-step-counts">
          {inN}→{outN}
        </span>
        {dropped.length > 0 && <span className="ph-step-pill drop">−{dropped.length}</span>}
        {added.length > 0 && <span className="ph-step-pill add">+{added.length}</span>}
        {s.reordered && <span className="ph-step-pill reorder">⇅</span>}
        {isNoop && <span className="ph-step-pill noop">noop</span>}
        <span className="ph-caret">{open ? "▾" : "▸"}</span>
      </div>
      {open && (
        <div className="ph-step-body">
          {isScoreStep && (
            <ScoreBreakdown ids={s.out || s.in || []} metrics={metrics} scores={scores} />
          )}
          {dropped.length > 0 && (
            <div className="ph-step-line">
              <span className="ph-step-lbl">dropped</span>
              <span className="ph-step-ids">
                {dropped.map(id => (
                  <UpstreamPill key={id} id={id} metrics={metrics[id]} score={scores[id]}
                    className="ph-diff-rem" prefix="− " />
                ))}
              </span>
            </div>
          )}
          {added.length > 0 && (
            <div className="ph-step-line">
              <span className="ph-step-lbl">added</span>
              <span className="ph-step-ids">
                {added.map(id => (
                  <UpstreamPill key={id} id={id} metrics={metrics[id]} score={scores[id]}
                    className="ph-diff-add" prefix="+ " />
                ))}
              </span>
            </div>
          )}
          <div className="ph-step-line">
            <span className="ph-step-lbl">in</span>
            <span className="ph-step-ids">
              {(s.in || []).map((id, i) => (
                <UpstreamPill key={`${id}-${i}`} id={id} metrics={metrics[id]} score={scores[id]}
                  className="ph-step-id" />
              ))}
            </span>
          </div>
          <div className="ph-step-line">
            <span className="ph-step-lbl">out</span>
            <span className="ph-step-ids">
              {(s.out || []).map((id, i) => (
                <UpstreamPill key={`${id}-${i}`} id={id} metrics={metrics[id]} score={scores[id]}
                  className={`ph-step-id ${i === 0 ? "primary" : ""}`} />
              ))}
            </span>
          </div>
          {s.args && (
            <div className="ph-step-line">
              <span className="ph-step-lbl">args</span>
              <pre className="ph-step-args-raw">{JSON.stringify(s.args, null, 2)}</pre>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

// isScoringStep returns true for stdlib methods that rank by a numeric
// score — these get the dedicated breakdown table because seeing the
// per-upstream metric values that produced the order is the whole
// point of "why did this upstream win?".
function isScoringStep(name) {
  return name === "sortByScore" || name === "sortBy" || name === "sortByDesc"
    || name === "sortByLatency" || name === "sortByErrorRate"
    || name === "sortByThrottling" || name === "sortByMisbehavior"
    || name === "sortByHeadLag" || name === "sortByFinalizationLag";
}

// ScoreBreakdown is the per-upstream metric+score table shown inside
// a scoring step's expanded body. Rows are kept in the OUT order
// (best-ranked first) so the visual top-to-bottom matches the policy's
// verdict. Cells get a tier color (bad/warn) when a value crosses the
// thresholds the default policy filters on, so "what made the policy
// rank rpc1 last?" reads at a glance.
function ScoreBreakdown({ ids, metrics, scores }) {
  if (!ids || ids.length === 0) return null;
  return (
    <div className="ph-step-line ph-score-table-wrap">
      <span className="ph-step-lbl">scoring</span>
      <div className="ph-score-table">
        <div className="ph-score-row hd">
          <span className="c-up">upstream</span>
          <span className="c-score">score</span>
          <span className="c-err">errorRate</span>
          <span className="c-thr">throttled</span>
          <span className="c-mis">misbehavior</span>
          <span className="c-p70">p70</span>
          <span className="c-p95">p95</span>
          <span className="c-lag">headLag</span>
          <span className="c-finlag">finLag</span>
        </div>
        {ids.map((id, i) => {
          const m = metrics[id] || {};
          const s = scores[id];
          return (
            <div key={id} className={`ph-score-row ${i === 0 ? "primary" : ""}`}>
              <span className="c-up"><UpstreamPill id={id} metrics={m} score={s} /></span>
              <span className="c-score"><b>{fmtScore(s)}</b></span>
              <span className={`c-err ${m.errorRate > 0.5 ? "bad" : m.errorRate > 0.05 ? "warn" : ""}`}>{fmtPct(m.errorRate)}</span>
              <span className={`c-thr ${m.throttledRate > 0.3 ? "bad" : m.throttledRate > 0.05 ? "warn" : ""}`}>{fmtPct(m.throttledRate)}</span>
              <span className={`c-mis ${m.misbehaviorRate > 0.05 ? "warn" : ""}`}>{fmtPct(m.misbehaviorRate)}</span>
              <span className="c-p70">{fmtMs(m.p70Ms)}</span>
              <span className={`c-p95 ${m.p95Ms > 10_000 ? "bad" : m.p95Ms > 2_000 ? "warn" : ""}`}>{fmtMs(m.p95Ms)}</span>
              <span className={`c-lag ${m.blockHeadLag > 16 ? "bad" : m.blockHeadLag > 4 ? "warn" : ""}`}>{m.blockHeadLag ?? "—"}</span>
              <span className="c-finlag">{m.finalizationLag ?? "—"}</span>
            </div>
          );
        })}
      </div>
    </div>
  );
}

function fmtScore(s) { return (s == null || Number.isNaN(s)) ? "—" : Number(s).toFixed(3); }
function fmtPct(p)   { return (p == null) ? "—" : (p * 100).toFixed(1) + "%"; }
function fmtMs(ms)   { return (ms == null || ms === 0) ? "—" : Math.round(ms) + "ms"; }

// summarizeStepArgs flattens the JS-captured args object into a short
// "key=value" string for the row header. Keeps presentation compact;
// the expanded body shows the raw JSON for inspection.
function summarizeStepArgs(args) {
  if (!args) return null;
  const parts = [];
  for (const k of Object.keys(args)) {
    const v = args[k];
    if (v == null) continue;
    if (typeof v === "string" || typeof v === "number" || typeof v === "boolean") {
      parts.push(`${k}=${v}`);
    } else if (Array.isArray(v)) {
      parts.push(`${k}=[${v.length}]`);
    } else if (typeof v === "object") {
      // Inline up to 3 keys.
      const keys = Object.keys(v).slice(0, 3);
      const inner = keys.map(kk => {
        const vv = v[kk];
        return (typeof vv === "string" || typeof vv === "number" || typeof vv === "boolean")
          ? `${kk}=${vv}` : kk;
      }).join(", ");
      if (inner) parts.push(inner);
    }
  }
  return parts.join(" · ");
}

function pad2(n) { return String(n).padStart(2, "0"); }
function pad3(n) { return String(n).padStart(3, "0"); }

window.PolicyHistory = PolicyHistory;
