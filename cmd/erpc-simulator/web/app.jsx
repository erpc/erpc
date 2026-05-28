// app.jsx — main shell.
//
// The runtime + state live in <SimProvider>. This component owns only:
//   - theme (dark/light)
//   - the resizable layout pane sizes (persisted to localStorage)
//   - the currently-open event drawer
//
// Everything else goes through `useSim*()` hooks. No more `simRef`,
// no more `setTick`, no more `window.eRPCSim.state` reads from inside
// React components.

const { useState, useEffect, useRef, useMemo, useCallback } = React;

const SHARE_PALETTE = [
  "oklch(0.72 0.13 245)",
  "oklch(0.78 0.16 145)",
  "oklch(0.78 0.15 50)",
  "oklch(0.74 0.15 320)",
  "oklch(0.84 0.15 90)",
  "oklch(0.74 0.13 195)",
  "oklch(0.72 0.16 25)",
  "oklch(0.66 0.04 270)",
];

// Expose so other components (upstream-knobs row swatches) can use the
// SAME palette as the selection-share stack chart — that way the user
// can correlate a colored band in the chart with the corresponding
// row in the knobs table just by matching the dot color.
window.SHARE_PALETTE = SHARE_PALETTE;
window.upstreamPaletteColor = function (idOrIndex, upstreams) {
  // Stable mapping: `upstreams` array's index → palette index.
  // (The server's snapshot keeps a stable order, so the same upstream
  // gets the same color across renders.)
  let idx = -1;
  if (typeof idOrIndex === "number") {
    idx = idOrIndex;
  } else if (Array.isArray(upstreams)) {
    idx = upstreams.findIndex(u => u.id === idOrIndex);
  }
  if (idx < 0) return "var(--tx-3)";
  return SHARE_PALETTE[idx % SHARE_PALETTE.length];
};

function Shell() {
  const events = window.useEvents();
  const [drawerReq, setDrawerReq] = useState(null);
  const [theme, setTheme] = useState("dark");

  // Resizable layout (persisted).
  const [paneSizes, setPaneSizes] = useState(() => {
    try {
      const saved = JSON.parse(localStorage.getItem("erpc-sim-panes") || "{}");
      return { flowH: saved.flowH ?? 460, rightW: saved.rightW ?? 360, chartsH: saved.chartsH ?? 240 };
    } catch { return { flowH: 460, rightW: 360, chartsH: 240 }; }
  });
  useEffect(() => {
    localStorage.setItem("erpc-sim-panes", JSON.stringify(paneSizes));
  }, [paneSizes]);
  const resizeFlow   = useCallback(dy => setPaneSizes(s => ({ ...s, flowH:   Math.max(160, Math.min(window.innerHeight - 200, s.flowH + dy)) })), []);
  const resizeRight  = useCallback(dx => setPaneSizes(s => ({ ...s, rightW:  Math.max(240, Math.min(window.innerWidth - 480, s.rightW - dx)) })), []);
  const resizeCharts = useCallback(dy => setPaneSizes(s => ({ ...s, chartsH: Math.max(140, Math.min(window.innerHeight - 200, s.chartsH + dy)) })), []);

  useEffect(() => { document.documentElement.setAttribute("data-theme", theme); }, [theme]);

  return (
    <div className="app">
      <window.TopBar theme={theme} setTheme={setTheme} />
      <div className="main">
        <div className="left-stack">
          <div className="panel flow" style={{ height: paneSizes.flowH, flex: "0 0 auto" }}>
            <FlowHeader />
            <window.FlowStage />
          </div>
          <window.Splitter orientation="h" onResize={resizeFlow} />
          <div className="panel bottom" style={{ flex: "1 1 0", minHeight: 0 }}>
            <window.BottomTabs />
          </div>
        </div>
        <window.Splitter orientation="v" onResize={resizeRight} />
        <div className="right-stack" style={{ width: paneSizes.rightW }}>
          <div className="panel chart-block" style={{ height: paneSizes.chartsH, flex: "0 0 auto" }}>
            <window.ChartsPanel palette={SHARE_PALETTE} />
          </div>
          <window.Splitter orientation="h" onResize={resizeCharts} />
          <div className="panel gen-log-block" style={{ flex: "1 1 0", minHeight: 0 }}>
            <window.RightCol onOpenDrawer={setDrawerReq} drawerReqId={drawerReq?.id} />
          </div>
        </div>
      </div>
      <window.EventDrawer req={drawerReq} onClose={() => setDrawerReq(null)} />
    </div>
  );
}

// The "Traffic flow" panel header lives here because it pulls the
// actual-rps stat from context — keeping it next to the panel layout
// avoids prop-drilling.
function FlowHeader() {
  const perSec = window.usePerSecond();
  return (
    <div className="panel-hd">
      <span className="ttl">Traffic flow</span>
      <span className="sub">client → eRPC → upstream pool</span>
      <span className="spacer" />
      <span style={{fontSize:"10px",color:"var(--tx-3)",fontFamily:"var(--font-mono)"}}>
        ~50 particles/s sampled · {Math.round(perSec.total)} actual rps
      </span>
    </div>
  );
}

function App() {
  return (
    <window.SimProvider>
      <Shell />
    </window.SimProvider>
  );
}

ReactDOM.createRoot(document.getElementById("root")).render(<App />);
