// top-bar.jsx — slim top bar. Reads state via the sim hooks; mutates via actions.

function TopBar({ theme, setTheme }) {
  const paused = window.usePaused();
  const perSec = window.usePerSecond();
  const policyValidate = window.usePolicyValidate();
  const policyResult = window.usePolicyResult();
  const configValidate = window.useConfigValidate();
  const actions = window.useSimActions();

  // "valid" is true unless the most recent debounced validate (policy
  // or config) said otherwise, OR the most recent apply failed.
  const policyOk = policyResult ? policyResult.ok : (policyValidate ? policyValidate.ok : true);
  const configOk = configValidate ? configValidate.ok : true;
  const ok = policyOk && configOk;

  const status = paused ? "paused" : ok ? "ok" : "bad";
  const pillCls = status === "ok" ? "ok" : status === "bad" ? "bad" : "";
  const pillLabel = status === "ok" ? "running" : status === "bad" ? "config error" : "paused";

  // Sim-time is a derived stat: ms since boot, formatted M:SS.
  const simTime = formatSimTime(Date.now() - (window.useSimState(s => s.bootedAt) || Date.now()));

  // Network label derived from the applied config rather than
  // hard-coded — the svm preset (and operator edits) change it.
  const yamlText = window.useSimState(s => s.yaml) || "";
  const isSvm = /architecture:\s*svm/.test(yamlText);
  const netLabel = isSvm
    ? "solana-mainnet · svm:mainnet-beta"
    : "eth-mainnet · chainId 1";

  return (
    <div className="topbar">
      <div className="tb-logo">
        <span className="glyph" />
        <span className="tb-title"><b>eRPC</b> simulator</span>
      </div>
      <div className="tb-sep" />
      <span className={`tb-pill ${pillCls}`}>
        <span className={`dot ${status === "ok" && !paused ? "pulse" : ""}`} />
        {pillLabel}
      </span>
      <span className={`tb-pill ${policyOk ? "ok" : "bad"}`}>
        <span className="dot" />
        policy {policyOk ? "✓ valid" : "✗ error"}
      </span>

      <div className="tb-sep" />
      <div className="tb-stat">
        <span className="lbl">rps</span>
        <span className="val">{Math.round(perSec.total).toLocaleString()}</span>
      </div>
      <div className="tb-stat">
        <span className="lbl">success</span>
        <span className="val">{perSec.total > 0 ? ((perSec.succ / perSec.total) * 100).toFixed(1) : "—"}%</span>
      </div>
      <div className="tb-stat">
        <span className="lbl">sim time</span>
        <span className="val">{simTime}</span>
      </div>

      <span className="tb-spacer" />

      <select className="tb-select" value={netLabel} onChange={() => {}}>
        <option>{netLabel}</option>
      </select>

      <div className="tb-sep" />

      <button className="tb-btn" onClick={() => actions.setPaused(!paused)} title="pause/play">
        {paused ? "▶ play" : "❚❚ pause"}
        <span className="kbd">space</span>
      </button>
      <button className="tb-btn" onClick={() => actions.clearEvents()} title="clear event log">
        ↺ clear
      </button>
      <button className="tb-btn" title="toggle theme"
        onClick={() => setTheme(t => t === "dark" ? "light" : "dark")}>
        {theme === "dark" ? "☾" : "☀"}
      </button>
    </div>
  );
}

function formatSimTime(ms) {
  const s = Math.max(0, Math.floor(ms / 1000));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}

window.TopBar = TopBar;
