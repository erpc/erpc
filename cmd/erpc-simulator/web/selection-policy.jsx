// selection-policy.jsx — JS/TS function editor for the routing policy.
//
// Draft + lastApplied live in the sim store (state.policyDraft /
// state.policyLastApplied) so the AI assistant can read & write them
// via the same hook surface as the editor.
//
// Compilation is ENTIRELY server-side (sobek): typing debounces a
// `validate-policy` frame; Apply sends `apply-policy`. Both round-trip
// real compile errors back through the WS shim.
//
// History: every successful apply is pushed to a 50-deep LRU stored
// in localStorage. ⌘/Ctrl-Z / Shift-⌘/Ctrl-Z navigate it; the history
// menu lists past versions with their timestamp + source.

const { useEffect, useRef, useState } = React;

function SelectionPolicy() {
  const draft         = window.usePolicyDraft();
  const serverPolicy  = window.useServerPolicy();
  const defaultPolicy = window.useDefaultPolicy();
  const policyResult  = window.usePolicyResult();
  const policyValidate = window.usePolicyValidate();
  const policyHistory = window.usePolicyHistory();
  const policyHistoryIdx = window.usePolicyHistoryIdx();
  const actions       = window.useSimActions();
  const perSec        = window.usePerSecond();
  const upstreams     = window.useUpstreams();
  const upstreamStats = window.useUpstreamStats();

  const [hint, setHint] = useState(null);
  const [historyOpen, setHistoryOpen] = useState(false);
  const [errOpen, setErrOpen] = useState(false);
  const taRef = useRef(null);
  const preRef = useRef(null);
  const gutRef = useRef(null);

  // The displayed source of truth — either the live draft or the
  // history snapshot the user is browsing.
  const lastApplied = policyHistory.length > 0
    ? policyHistory[policyHistory.length - 1].code
    : (serverPolicy || defaultPolicy || "");
  const dirty = draft !== lastApplied;

  // Surface error from validate or apply.
  const error = (policyResult && !policyResult.ok && policyResult.error)
    || (policyValidate && !policyValidate.ok && policyValidate.error)
    || null;

  // Surface "applied" toast when policyResult flips to ok and matches
  // current draft.
  useEffect(() => {
    if (policyResult && policyResult.ok && policyResult.compiledAt) {
      setHint("Applied · " + new Date(policyResult.compiledAt).toLocaleTimeString());
      const id = setTimeout(() => setHint(null), 2200);
      return () => clearTimeout(id);
    }
  }, [policyResult]);

  // Debounced server-side validate as user types.
  useEffect(() => {
    if (!draft || draft === lastApplied) return;
    const id = setTimeout(() => actions.validatePolicy(draft), 450);
    return () => clearTimeout(id);
  }, [draft, lastApplied, actions]);

  // ⌘↵ apply / ⌘Z undo / ⌘⇧Z redo / ⌘/ comment-toggle (when editor focused).
  useEffect(() => {
    function onKey(e) {
      if (taRef.current && document.activeElement !== taRef.current) return;
      if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
        e.preventDefault(); actions.applyPolicy(draft); return;
      }
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "z") {
        e.preventDefault();
        if (e.shiftKey) actions.redoPolicy();
        else actions.undoPolicy();
        return;
      }
      if ((e.metaKey || e.ctrlKey) && e.key === "/") {
        e.preventDefault();
        toggleCommentOnSelection();
        return;
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [draft, actions]);

  // toggleCommentOnSelection — Cmd+/ behavior modeled on VS Code:
  //   1. Find the line range covered by the textarea's current
  //      selection (or just the caret line if no selection).
  //   2. If EVERY non-blank line in the range is already commented
  //      (starts with `// ` after its indent), strip the comment.
  //   3. Otherwise add `// ` after the minimum indent shared by all
  //      non-blank lines in the range — keeps the block visually
  //      aligned when toggling indented code.
  // The selection is preserved across the toggle so chained ⌘/ presses
  // expand/contract the same block consistently.
  function toggleCommentOnSelection() {
    const ta = taRef.current;
    if (!ta) return;
    const value = ta.value;
    const selStart = ta.selectionStart;
    const selEnd   = ta.selectionEnd;

    // Expand the selection to whole lines.
    const lineStart = value.lastIndexOf("\n", selStart - 1) + 1;
    let lineEnd = value.indexOf("\n", selEnd);
    if (lineEnd < 0) lineEnd = value.length;

    const block = value.slice(lineStart, lineEnd);
    const lines = block.split("\n");

    // Min indent across non-blank lines — the comment prefix is
    // inserted at this column so the toggle stays visually aligned.
    let minIndent = Infinity;
    for (const ln of lines) {
      if (ln.trim() === "") continue;
      const m = ln.match(/^[ \t]*/);
      const w = m ? m[0].length : 0;
      if (w < minIndent) minIndent = w;
    }
    if (!isFinite(minIndent)) minIndent = 0;

    // Are ALL non-blank lines already commented at minIndent?
    const allCommented = lines.every(ln => {
      if (ln.trim() === "") return true;
      const rest = ln.slice(minIndent);
      return rest.startsWith("// ") || rest.startsWith("//");
    });

    let delta = 0; // net char-count change for selection adjustment
    const updated = lines.map(ln => {
      if (ln.trim() === "") return ln;
      if (allCommented) {
        // Strip `// ` or `//` after the shared indent.
        const head = ln.slice(0, minIndent);
        let tail = ln.slice(minIndent);
        if (tail.startsWith("// ")) { tail = tail.slice(3); delta -= 3; }
        else if (tail.startsWith("//")) { tail = tail.slice(2); delta -= 2; }
        return head + tail;
      } else {
        delta += 3;
        return ln.slice(0, minIndent) + "// " + ln.slice(minIndent);
      }
    }).join("\n");

    const newValue = value.slice(0, lineStart) + updated + value.slice(lineEnd);
    actions.setPolicyDraft(newValue);
    // Restore selection — adjust by per-line delta. We approximate by
    // shifting both ends by the average delta-per-line × lines-before.
    // For most cases (single line, or block of similar lines) this
    // lands the caret/selection where the user expects.
    const perLine = lines.length > 0 ? Math.round(delta / lines.length) : 0;
    requestAnimationFrame(() => {
      const ta2 = taRef.current;
      if (!ta2) return;
      const newStart = selStart + (selStart === lineStart ? 0 : perLine);
      const newEnd = selEnd + delta;
      ta2.setSelectionRange(Math.max(lineStart, newStart), Math.max(newStart, newEnd));
    });
  }

  // Reset-to-default flow:
  //   * "↺ default"        — preview the default in the draft. Non-destructive;
  //                          user still must hit Apply (or ⌘↵) to commit.
  //   * "↺ reset & apply"  — set draft AND immediately apply. Skips the
  //                          preview step for the common "I broke it, give
  //                          me the defaults back NOW" case.
  function resetToDefaultDraft() {
    actions.setPolicyDraft(defaultPolicy);
  }
  function resetAndApply() {
    actions.setPolicyDraft(defaultPolicy);
    actions.applyPolicy(defaultPolicy);
  }

  // Syntax highlight (tokenized — see comment above the loop).
  //
  // History note: an earlier chained-regex implementation wrapped strings
  // with `<span class="tk-str">…</span>` and then ran a keyword regex
  // that included `class` — which matched the literal `class` attribute
  // INSIDE the span tag we'd just inserted, producing busted markup like
  //   `<span <span class="tk-kw">class</span>="tk-str">…</span>`
  // which the browser renders as the raw attribute text. That's the
  // `class="tk-str">'!tier:fallback'` glitch users reported.
  //
  // The robust fix: split the line into [text, str, text, str, …]
  // segments FIRST, run keyword/number/prop regexes ONLY on the text
  // segments, then wrap each str segment wholesale at the end. The
  // keyword regex literally cannot see the string-span markup, so the
  // bug class is eliminated by construction.
  function highlight(line) {
    if (line.match(/^\s*\/\//)) return `<span class="tk-com">${esc(line) || "&nbsp;"}</span>`;
    const escLine = esc(line);
    const strRe = /(&quot;[^&]*?&quot;|'[^']*?'|`[^`]*?`)/g;
    const segs = [];
    let lastEnd = 0;
    let m;
    while ((m = strRe.exec(escLine)) !== null) {
      if (m.index > lastEnd) segs.push({ k: "t", v: escLine.slice(lastEnd, m.index) });
      segs.push({ k: "s", v: m[0] });
      lastEnd = m.index + m[0].length;
    }
    if (lastEnd < escLine.length) segs.push({ k: "t", v: escLine.slice(lastEnd) });
    const html = segs.map(seg => {
      if (seg.k === "s") return `<span class="tk-str">${seg.v}</span>`;
      let s = seg.v;
      s = s.replace(/\b(const|let|var|function|return|if|else|for|while|do|break|continue|new|in|of|typeof|instanceof|true|false|null|undefined|this|throw|try|catch|finally|class|extends|interface|type|as|export|import|from|async|await)\b/g, '<span class="tk-kw">$1</span>');
      s = s.replace(/(?<![\w])(\d+(?:\.\d+)?)\b/g, '<span class="tk-num">$1</span>');
      s = s.replace(/\.([a-zA-Z_]\w*)\b/g, '.<span class="tk-prop">$1</span>');
      return s;
    }).join("");
    return html || "&nbsp;";
  }
  function esc(t) { return t.replace(/[&<>"]/g, c => ({ "&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;" }[c])); }

  function onScroll(e) {
    if (preRef.current) {
      preRef.current.scrollTop = e.target.scrollTop;
      preRef.current.scrollLeft = e.target.scrollLeft;
    }
    if (gutRef.current) gutRef.current.scrollTop = e.target.scrollTop;
  }

  const lines = (draft || "").split("\n");

  return (
    <div className="sp-wrap">
      <div className="sp-toolbar">
        <span className="sp-meta">
          <span className="sp-meta-k">signature</span>
          <code className="sp-sig">(upstreams, ctx) =&gt; Upstream[]</code>
        </span>
        <span style={{flex:1}} />
        <button className="chip-btn" onClick={() => actions.undoPolicy()} disabled={policyHistory.length < 2} title="Undo (⌘Z)">↶ undo</button>
        <button className="chip-btn" onClick={() => actions.redoPolicy()} disabled={policyHistoryIdx < 0} title="Redo (⌘⇧Z)">↷ redo</button>
        <button className="chip-btn" onClick={() => setHistoryOpen(o => !o)} title={`${policyHistory.length} versions`}>
          history · {policyHistory.length}
        </button>
        <button className="chip-btn" onClick={resetToDefaultDraft} title="Put the eRPC default in the draft (Apply still required)">↺ default</button>
        <button className="chip-btn primary" onClick={resetAndApply} title="Replace draft with default AND apply immediately">↺ reset &amp; apply</button>
      </div>

      {historyOpen && (
        <div className="sp-history-panel">
          {policyHistory.length === 0 ? (
            <div className="sp-history-empty">no edits yet</div>
          ) : (
            <div className="sp-history-list">
              {policyHistory.slice().reverse().map((h, i) => {
                const idx = policyHistory.length - 1 - i;
                const active = policyHistoryIdx === idx || (policyHistoryIdx < 0 && idx === policyHistory.length - 1);
                return (
                  <div key={h.ts}
                    className={`sp-history-row ${active ? "active" : ""}`}
                    onClick={() => { actions.gotoPolicyHistory(idx); setHistoryOpen(false); }}>
                    <span className="ts">{new Date(h.ts).toLocaleTimeString()}</span>
                    <span className="src">{h.source}</span>
                    <span className="preview">{(h.code || "").replace(/\s+/g, " ").slice(0, 80)}</span>
                  </div>
                );
              })}
            </div>
          )}
        </div>
      )}

      <div className="sp-body">
        <div className="sp-editor">
          <div className="gutter" ref={gutRef}>
            {lines.map((_, i) => <span key={i} className="ln">{i + 1}</span>)}
          </div>
          <div style={{flex:1, position:"relative", overflow:"hidden"}}>
            <pre ref={preRef} className="code-pre" aria-hidden="true"
              style={{position:"absolute", inset:0, margin:0, overflow:"auto", pointerEvents:"none", zIndex:1}}
              dangerouslySetInnerHTML={{ __html: lines.map(highlight).join("\n") }} />
            <textarea ref={taRef} className="code-area" spellCheck="false"
              value={draft || ""}
              onChange={e => actions.setPolicyDraft(e.target.value)}
              onScroll={onScroll}
              style={{position:"absolute", inset:0, background:"transparent", color:"transparent", caretColor:"var(--acc-blue)", zIndex:2, overflow:"auto"}} />
          </div>
        </div>

        <aside className="sp-side">
          <div className="sp-side-section">
            <div className="sp-side-lbl">Upstream shape</div>
            <pre className="sp-type">{`type Upstream = {
  id: string;
  vendor: string;
  tags: string[];
  type: string;          // 'http' | 'evm' | …
  // Tag check — equivalent to tags.includes(t);
  // 'is' is an alias.
  hasTag(t: string): boolean;
  is(t: string):     boolean;
  metrics: {
    errorRate:       number;   // 0..1 (excludes reverts)
    throttledRate:   number;   // 0..1 (429s)
    misbehaviorRate: number;   // 0..1
    requestsTotal:   number;
    errorsTotal:     number;
    blockHeadLag:    number;   // blocks behind tip
    finalizationLag: number;   // blocks behind finalized
    blockHeadLagSeconds:    number; // seconds behind tip (block-count × EMA block-time)
    finalizationLagSeconds: number; // seconds behind finalized
    p50ResponseSeconds: number;
    p70ResponseSeconds: number;
    p90ResponseSeconds: number;
    p95ResponseSeconds: number;
    p99ResponseSeconds: number;
    cordonedReason: string;
    // p70 by default; accepts 50/70/90/95/99 or 0.5/0.7/0.9/0.95/0.99.
    // Returns milliseconds.
    latencyP(q: number): number;
  };
};`}</pre>
          </div>
          <div className="sp-side-section">
            <div className="sp-side-lbl">Admission control</div>
            <ul className="sp-notes">
              <li><code>.excludeIf(pred, {"{ outFor?, reason? }"})</code> — drop upstreams matching <code>pred</code>; optional <code>outFor</code> keeps them dropped that long (anti-flap).</li>
              <li><code>.readmitExcluded({"{ reAdmitAfter, maxConcurrent, position }"})</code> — cautiously bring back excluded upstreams. Pairs with <code>excludeIf</code>. (<code>probeExcluded</code> remains as an alias.)</li>
              <li><code>.removeCordoned()</code> — drop explicitly cordoned upstreams.</li>
              <li><code>.whenEmpty(() =&gt; upstreams)</code> — safety net: serve the raw set rather than fail closed.</li>
            </ul>
          </div>
          <div className="sp-side-section">
            <div className="sp-side-lbl">Predicate factories</div>
            <ul className="sp-notes">
              <li><code>errorRateAbove(rate)</code> / <code>errorRateBelow(rate)</code></li>
              <li><code>throttleRateAbove(rate)</code> / <code>throttleRateBelow(rate)</code></li>
              <li><code>misbehaviorRateAbove(rate)</code></li>
              <li><code>latencyP50Above(ms)</code> / <code>latencyP70Above(ms)</code> / <code>latencyP90Above(ms)</code> / <code>latencyP95Above(ms)</code> / <code>latencyP99Above(ms)</code></li>
              <li><code>latencyAbove(quantile, ms)</code> — arbitrary quantile</li>
              <li><code>blockNumberLagAbove(blocks)</code> · <code>finalizationLagAbove(blocks)</code></li>
              <li><code>blockSecondsLagAbove(s)</code> · <code>finalizationSecondsLagAbove(s)</code></li>
              <li><code>samplesBelow(n)</code> / <code>samplesAbove(n)</code></li>
              <li><b>Combinators</b>: <code>all(...preds)</code> · <code>any(...preds)</code> · <code>not(pred)</code></li>
            </ul>
          </div>
          <div className="sp-side-section">
            <div className="sp-side-lbl">Ranking + stability</div>
            <ul className="sp-notes">
              <li><code>.preferTag(pat, {"{ minHealthy, fallback }"})</code> — tier split.</li>
              <li><code>.sortByScore(BALANCED)</code> — composite penalty (lower = better). Presets: <code>BALANCED</code>, <code>PREFER_FASTER</code>, <code>PREFER_FEWER_ERRORS</code>, <code>PREFER_FRESHER_HEAD</code>, <code>PREFER_LESS_THROTTLED</code>.</li>
              <li><code>.stickyPrimary({"{ hysteresis, minSwitchInterval }"})</code> — avoid flapping the primary.</li>
            </ul>
          </div>
          <div className="sp-side-section">
            <div className="sp-side-lbl">Notes</div>
            <ul className="sp-notes">
              <li>Compiled server-side via sobek. Return order is the routing order — index 0 is primary, failures retry through 1, 2, …</li>
              <li>Throwing or returning a non-array falls back to the canonical default.</li>
              <li><code>⌘↵</code> apply · <code>⌘Z</code>/<code>⌘⇧Z</code> undo/redo · <code>⌘/</code> toggle comment.</li>
            </ul>
          </div>
          <div className="sp-side-section">
            <div className="sp-side-lbl">Live impact</div>
            <div className="sp-live-stat"><span className="sp-meta-k">routed last 1s</span><span className="sp-meta-v">{Math.round(perSec.total)} req</span></div>
            <div className="sp-live-stat"><span className="sp-meta-k">success rate</span><span className="sp-meta-v">{perSec.total > 0 ? ((perSec.succ / perSec.total) * 100).toFixed(1) : "—"}%</span></div>
            <div className="sp-live-stat"><span className="sp-meta-k">eligible upstreams</span><span className="sp-meta-v">{upstreams.filter(u => {
              // Eligibility comes straight from the policy's verdict —
              // position >= 0 means the upstream is in the current
              // ordered list; -1 means an excludeIf rule (or a tier
              // filter) dropped it this tick.
              const row = upstreamStats[u.id] || {};
              return (row.position == null || row.position >= 0) && u.available !== false;
            }).length} / {upstreams.length}</span></div>
          </div>
        </aside>
      </div>

      <div className="sp-foot">
        {error ? (
          <ErrorChip error={error} open={errOpen} setOpen={setErrOpen} />
        ) : (
          <span className="sp-ok"><span className="dot" /> {dirty ? "validated · awaiting apply" : "in use"}</span>
        )}
        <span style={{flex:1}} />
        {hint && <span className="sp-hint">{hint}</span>}
        {dirty && !hint && <span className="sp-dirty"><span className="dot" /> modified</span>}
        <button className="btn primary" disabled={!dirty || !!error} onClick={() => actions.applyPolicy(draft)}>
          Apply <span className="kbd">⌘↵</span>
        </button>
      </div>
    </div>
  );
}

// ErrorChip is the footer compile-error indicator. Click to toggle a
// floating popup with the full error text — the inline chip is short
// (~80 chars) so it fits next to Apply, the popup shows everything
// (multi-line, scrollable). Portals the popup to <body> so it isn't
// clipped by the editor pane's `overflow:hidden` parents.
function ErrorChip({ error, open, setOpen }) {
  const anchorRef = useRef(null);
  const [pos, setPos] = useState(null);
  useEffect(() => {
    if (!open) { setPos(null); return; }
    const r = anchorRef.current?.getBoundingClientRect();
    if (!r) return;
    setPos({ left: r.left, bottom: window.innerHeight - r.top + 6 });
  }, [open, error]);
  // Close on outside-click or Esc.
  useEffect(() => {
    if (!open) return;
    function onDoc(e) {
      if (anchorRef.current && anchorRef.current.contains(e.target)) return;
      if (e.target.closest && e.target.closest(".sp-err-popup")) return;
      setOpen(false);
    }
    function onKey(e) { if (e.key === "Escape") setOpen(false); }
    document.addEventListener("mousedown", onDoc);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDoc);
      document.removeEventListener("keydown", onKey);
    };
  }, [open, setOpen]);
  const full = String(error || "");
  // The chip shows the FIRST error line — that's almost always the
  // useful summary (SyntaxError: …:Line N:M Unexpected …). Anything
  // past the first newline goes into the popup.
  const firstLine = full.split("\n", 1)[0];
  return (
    <>
      <button
        ref={anchorRef}
        className={`sp-error sp-err-toggle ${open ? "on" : ""}`}
        onClick={() => setOpen(o => !o)}
        title="Click for full error"
      >
        <span className="dot" /> compile error · <code>{firstLine}</code>
        <span className="sp-err-caret">{open ? "▲" : "▼"}</span>
      </button>
      {open && pos && ReactDOM.createPortal(
        <div className="sp-err-popup" style={{ left: pos.left, bottom: pos.bottom }}>
          <div className="sp-err-popup-hd">
            <span className="sp-err-popup-title">compile error · full detail</span>
            <button className="sp-err-popup-x" onClick={() => setOpen(false)}>✕</button>
          </div>
          <pre className="sp-err-popup-body">{full}</pre>
        </div>,
        document.body,
      )}
    </>
  );
}

window.SelectionPolicy = SelectionPolicy;
