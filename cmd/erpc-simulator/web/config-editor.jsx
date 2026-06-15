// config-editor.jsx — YAML editor with backend-driven validate + apply.
//
// The editor's `draft` lives in the sim store (state.yamlDraft) — that
// way the assistant can read/write the in-progress YAML without
// reaching into the component. `state.yaml` is the last-applied source
// of truth (server-side); `state.yamlDraft` is the editor buffer.
//
// On every change, we debounce a `validate-config` WS frame so the
// editor footer can surface server-side parse/validate errors inline
// without requiring an Apply.

const { useEffect, useRef, useState } = React;

function ConfigEditor() {
  const yaml          = window.useYAML();
  const defaultYaml   = window.useDefaultYaml();
  const draft         = window.useYamlDraft();
  const configValidate = window.useConfigValidate();
  const configResult  = window.useConfigResult();
  const actions       = window.useSimActions();

  const [dragover, setDragover] = useState(false);
  const taRef = useRef(null);
  const preRef = useRef(null);
  const gutRef = useRef(null);

  // Reset flows — mirror the selection-policy editor's two-button UX:
  //   * "↺ default"        — preview the seed YAML in the draft. The user
  //                          still has to hit Apply to commit. Safe; lets
  //                          the user diff against their work first.
  //   * "↺ reset & apply"  — set draft AND apply immediately. For the
  //                          "I broke something, give me defaults NOW"
  //                          case after a bad edit.
  function resetToDefaultDraft() {
    if (!defaultYaml) return;
    actions.setYamlDraft(defaultYaml);
  }
  function resetAndApply() {
    if (!defaultYaml) return;
    actions.setYamlDraft(defaultYaml);
    actions.applyConfig(defaultYaml);
  }

  // ⌘/Ctrl+Enter = apply. ⌘/Ctrl+/ = toggle YAML `#` comment on
  // selected lines (or current line if no selection). Same VS Code-ish
  // semantics as the policy editor: all-already-commented → strip,
  // otherwise add at the minimum shared indent.
  useEffect(() => {
    function onKey(e) {
      if (taRef.current && document.activeElement !== taRef.current) return;
      if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
        e.preventDefault();
        actions.applyConfig(draft);
        return;
      }
      if ((e.metaKey || e.ctrlKey) && e.key === "/") {
        e.preventDefault();
        toggleHashCommentOnSelection();
        return;
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [draft, actions]);

  function toggleHashCommentOnSelection() {
    const ta = taRef.current;
    if (!ta) return;
    const value = ta.value;
    const selStart = ta.selectionStart;
    const selEnd   = ta.selectionEnd;
    const lineStart = value.lastIndexOf("\n", selStart - 1) + 1;
    let lineEnd = value.indexOf("\n", selEnd);
    if (lineEnd < 0) lineEnd = value.length;
    const block = value.slice(lineStart, lineEnd);
    const lines = block.split("\n");
    let minIndent = Infinity;
    for (const ln of lines) {
      if (ln.trim() === "") continue;
      const w = (ln.match(/^[ \t]*/) || [""])[0].length;
      if (w < minIndent) minIndent = w;
    }
    if (!isFinite(minIndent)) minIndent = 0;
    const allCommented = lines.every(ln => {
      if (ln.trim() === "") return true;
      const rest = ln.slice(minIndent);
      return rest.startsWith("# ") || rest.startsWith("#");
    });
    let delta = 0;
    const updated = lines.map(ln => {
      if (ln.trim() === "") return ln;
      if (allCommented) {
        const head = ln.slice(0, minIndent);
        let tail = ln.slice(minIndent);
        if (tail.startsWith("# ")) { tail = tail.slice(2); delta -= 2; }
        else if (tail.startsWith("#")) { tail = tail.slice(1); delta -= 1; }
        return head + tail;
      } else {
        delta += 2;
        return ln.slice(0, minIndent) + "# " + ln.slice(minIndent);
      }
    }).join("\n");
    const newValue = value.slice(0, lineStart) + updated + value.slice(lineEnd);
    actions.setYamlDraft(newValue);
    const perLine = lines.length > 0 ? Math.round(delta / lines.length) : 0;
    requestAnimationFrame(() => {
      const ta2 = taRef.current;
      if (!ta2) return;
      const newStart = selStart + (selStart === lineStart ? 0 : perLine);
      const newEnd = selEnd + delta;
      ta2.setSelectionRange(Math.max(lineStart, newStart), Math.max(newStart, newEnd));
    });
  }

  // Debounced validate as user types.
  useEffect(() => {
    if (!draft) return;
    const id = setTimeout(() => actions.validateConfig(draft), 500);
    return () => clearTimeout(id);
  }, [draft, actions]);

  function onScroll(e) {
    const top = e.target.scrollTop, left = e.target.scrollLeft;
    if (preRef.current) { preRef.current.scrollTop = top; preRef.current.scrollLeft = left; }
    if (gutRef.current) gutRef.current.scrollTop = top;
  }

  function onDrop(e) {
    e.preventDefault();
    setDragover(false);
    const f = e.dataTransfer.files?.[0];
    if (!f) return;
    const reader = new FileReader();
    reader.onload = () => actions.setYamlDraft(String(reader.result));
    reader.readAsText(f);
  }

  const lines = (draft || "").split("\n");
  const dirty = draft !== yaml;

  return (
    <div className="panel-bd">
      <div className={`editor ${dragover ? "dragover" : ""}`}
        onDragOver={e => { e.preventDefault(); setDragover(true); }}
        onDragLeave={() => setDragover(false)}
        onDrop={onDrop}>
        <div className="gutter" ref={gutRef}>
          {lines.map((_, i) => <span key={i} className="ln">{i + 1}</span>)}
        </div>
        <div style={{flex:1, position:"relative", overflow:"hidden"}}>
          <pre ref={preRef} className="code-pre" aria-hidden="true"
            style={{position:"absolute", inset:0, margin:0, overflow:"auto", pointerEvents:"none", zIndex:1}}
            dangerouslySetInnerHTML={{ __html: lines.map(l => window.YAMLU.highlightLine(l)).join("\n") }}
          />
          <textarea ref={taRef} className="code-area" spellCheck="false"
            value={draft || ""}
            onChange={e => actions.setYamlDraft(e.target.value)}
            onScroll={onScroll}
            style={{position:"absolute", inset:0, background:"transparent", color:"transparent", caretColor:"var(--acc-blue)", zIndex:2, overflow:"auto"}}
          />
        </div>
      </div>

      <div className="cfg-foot">
        <ValidationStatus dirty={dirty} configValidate={configValidate} configResult={configResult} />
        <span className="spacer" />
        <button className="chip-btn" onClick={resetToDefaultDraft} disabled={!defaultYaml}
          title="Put the seed config in the draft (Apply still required)">↺ default</button>
        <button className="chip-btn primary" onClick={resetAndApply} disabled={!defaultYaml}
          title="Replace draft with seed config AND apply immediately">↺ reset &amp; apply</button>
        {dirty && <span className="modified"><span className="dot" /> modified</span>}
        <button className="btn primary" disabled={!dirty} onClick={() => actions.applyConfig(draft)}>
          Apply <span className="kbd">⌘↵</span>
        </button>
      </div>
    </div>
  );
}

function ValidationStatus({ dirty, configValidate, configResult }) {
  if (configResult && !configResult.ok) {
    return <span className="diag-count bad" title={configResult.error}>● apply failed: {String(configResult.error).slice(0, 140)}</span>;
  }
  if (configValidate && !configValidate.ok) {
    return <span className="diag-count bad" title={configValidate.error}>● server: {String(configValidate.error).slice(0, 140)}</span>;
  }
  if (configValidate && configValidate.ok) {
    return <span style={{color:"var(--ok)",fontFamily:"var(--font-mono)",fontSize:"10.5px"}}>✓ valid (server-checked)</span>;
  }
  return <span style={{color:"var(--tx-3)",fontFamily:"var(--font-mono)",fontSize:"10.5px"}}>{dirty ? "checking…" : "✓ in sync with server"}</span>;
}

window.ConfigEditor = ConfigEditor;
