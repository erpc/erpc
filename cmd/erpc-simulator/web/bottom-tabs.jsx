// bottom-tabs.jsx — tabbed bottom panel: Selection policy | Upstream synth | Config

function BottomTabs() {
  const [tab, setTab] = React.useState("policy");
  const upstreams = window.useUpstreams();
  const policyResult = window.usePolicyResult();
  const policyValidate = window.usePolicyValidate();
  const yamlDraft = window.useYamlDraft();
  const yaml = window.useYAML();
  const configValidate = window.useConfigValidate();
  const configResult = window.useConfigResult();

  // Approximate "pending changes" badge: number of differing lines.
  const pendingCount = React.useMemo(() => {
    if (!yamlDraft || yamlDraft === yaml) return 0;
    const a = (yaml || "").split("\n"); const b = yamlDraft.split("\n");
    let n = 0; const L = Math.max(a.length, b.length);
    for (let i = 0; i < L; i++) if (a[i] !== b[i]) n++;
    return n;
  }, [yaml, yamlDraft]);

  const errCount =
    (configResult && !configResult.ok ? 1 : 0) +
    (configValidate && !configValidate.ok ? 1 : 0);

  const policyErr =
    (policyResult && !policyResult.ok) ||
    (policyValidate && !policyValidate.ok);

  return (
    <div style={{display:"flex",flexDirection:"column",height:"100%",minHeight:0}}>
      <div className="panel-hd">
        <div style={{display:"flex",gap:0,marginLeft:-12,height:"100%"}}>
          <BtTabBtn label="Selection policy" sub="evalFunc" active={tab === "policy"} onClick={() => setTab("policy")} badge={policyErr ? "!" : null} />
          <BtTabBtn label="Upstream synth" sub={`${upstreams.length}`} active={tab === "upstreams"} onClick={() => setTab("upstreams")} />
          <BtTabBtn label="Config" sub="erpc.yaml" active={tab === "config"} onClick={() => setTab("config")}
            badge={pendingCount > 0 ? pendingCount : null} errCount={errCount} />
        </div>
        <span className="spacer" />
        {tab === "config" && <span style={{fontSize:"10px",color:"var(--tx-3)",fontFamily:"var(--font-mono)"}}>⌘↵ apply</span>}
        {tab === "policy" && <span style={{fontSize:"10px",color:"var(--tx-3)",fontFamily:"var(--font-mono)"}}>runs per request · ⌘↵ apply</span>}
      </div>
      {tab === "upstreams" ? <window.UpstreamKnobs />
        : tab === "policy" ? <window.SelectionPolicy />
        : <window.ConfigEditor />}
    </div>
  );
}

function BtTabBtn({ label, sub, active, onClick, badge, warnCount, errCount }) {
  return (
    <button onClick={onClick} style={{
      padding: "0 14px", height: "100%", display: "flex", alignItems: "center", gap: 7,
      fontSize: "10.5px", fontWeight: 600, letterSpacing: "0.08em", textTransform: "uppercase",
      color: active ? "var(--tx-0)" : "var(--tx-3)",
      borderBottom: active ? "2px solid var(--acc-blue)" : "2px solid transparent",
      background: active ? "var(--bg-2)" : "transparent", borderRight: "1px solid var(--bd-0)",
    }}>
      <span>{label}</span>
      {sub && <span style={{
        textTransform: "none", letterSpacing: 0, fontWeight: 400, fontSize: "10.5px",
        color: active ? "var(--tx-3)" : "var(--tx-4)", fontFamily: "var(--font-mono)",
      }}>{sub}</span>}
      {badge != null && <span style={{
        background: "var(--warn-d)", color: "var(--warn)", fontFamily: "var(--font-mono)",
        fontSize: "9.5px", padding: "1px 5px", borderRadius: "999px", fontWeight: 600,
      }}>{badge}</span>}
      {errCount > 0 && <span style={{
        background: "var(--bad-d)", color: "var(--bad)", fontFamily: "var(--font-mono)",
        fontSize: "9.5px", padding: "1px 5px", borderRadius: "999px", fontWeight: 600,
      }}>● {errCount}</span>}
    </button>
  );
}

window.BottomTabs = BottomTabs;
