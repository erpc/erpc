// charts.jsx — selection-share stacked area + per-upstream cards + failsafe strip
// Exposes: window.ChartsPanel

const { useEffect, useRef, useState, useMemo } = React;

function ChartsPanel({ palette }) {
  const upstreams = window.useUpstreams();
  const history = window.usePerSecondHistory();
  const ops = window.useOpsHistory();
  const perSec = window.usePerSecond();

  return (
    <>
      <div className="panel-hd">
        <span className="ttl">Telemetry</span>
        <span className="sub">last 60s</span>
        <span className="spacer" />
      </div>

      <div className="charts-wrap">
        <div className="chart-card">
          <div className="lbl">
            <span>selection share / sec</span>
            <span className="sub">{history.length}s of 60s</span>
          </div>
          <ShareChart history={history} upstreams={upstreams} palette={palette} />
          <div className="share-legend">
            {upstreams.slice(0, 8).map((u, i) => (
              <span className="it" key={u.id}>
                <span className="sw" style={{ background: palette[i % palette.length] }} />
                {u.id}
              </span>
            ))}
          </div>
        </div>

        {/* Ops strip — sparklines for the failsafe knobs people care
            about: how often is the system saving requests with retries
            or hedges, how often is it dropping them, and how often is
            the primary upstream returning an empty miss-class result. */}
        <div className="chart-card ops-strip">
          <div className="lbl">
            <span>ops · last 60s</span>
            <span className="sub">{ops.length}s</span>
          </div>
          <OpsRow label="retry-rescued" color="var(--warn)" series={ops} field="retry" lastVal={perSec.retryOk} />
          <OpsRow label="hedge-wins"    color="var(--hot)"  series={ops} field="hedge" lastVal={perSec.hedge} />
          <OpsRow label="primary missed" color="var(--info)" series={ops} field="miss"  lastVal={perSec.miss} />
          <OpsRow label="failed"        color="var(--bad)"  series={ops} field="err"   lastVal={perSec.err} />
        </div>
      </div>
    </>
  );
}

// OpsRow renders a single sparkline + label + last-second value.
function OpsRow({ label, color, series, field, lastVal }) {
  const W = 220, H = 22;
  const data = series.slice(-60).map(s => s[field] || 0);
  const max = Math.max(1, ...data);
  let path = "";
  if (data.length > 0) {
    const stepX = W / Math.max(1, data.length - 1);
    data.forEach((v, i) => {
      const x = i * stepX;
      const y = H - (v / max) * (H - 2) - 1;
      path += (i === 0 ? "M" : "L") + x.toFixed(1) + "," + y.toFixed(1) + " ";
    });
  }
  return (
    <div className="ops-row">
      <span className="ops-label">{label}</span>
      <svg className="ops-spark" viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none">
        <path d={path} fill="none" stroke={color} strokeWidth="1.4" />
      </svg>
      <span className="ops-val" style={{ color }}>{lastVal || 0}<span className="ops-unit">/s</span></span>
    </div>
  );
}

// ===========================================================================
// ShareChart — SVG stacked area
// ===========================================================================
function ShareChart({ history, upstreams, palette }) {
  const W = 320, H = 110;
  const ids = upstreams.map(u => u.id);
  const data = history.length > 0 ? history.slice(-60) : [];
  // build series: for each bucket, total per upstream
  const maxTotal = Math.max(1, ...data.map(d => Object.values(d.perUpstream || {}).reduce((a, b) => a + b, 0)));
  // x-positions
  const xs = data.map((_, i) => (i / Math.max(1, data.length - 1)) * W);

  // baseline for stack
  const baselines = data.map(() => 0);
  const paths = ids.map((id, idx) => {
    const points = [];
    for (let i = 0; i < data.length; i++) {
      const v = data[i].perUpstream?.[id] || 0;
      const y0 = H - (baselines[i] / maxTotal) * H;
      const y1 = H - ((baselines[i] + v) / maxTotal) * H;
      points.push({ x: xs[i], y0, y1 });
      baselines[i] += v;
    }
    let top = "M0," + H;
    let bot = "";
    points.forEach((p, i) => {
      top += ` L${p.x.toFixed(1)},${p.y1.toFixed(1)}`;
    });
    for (let i = points.length - 1; i >= 0; i--) {
      const p = points[i];
      top += ` L${p.x.toFixed(1)},${p.y0.toFixed(1)}`;
    }
    top += " Z";
    return <path key={id} d={top} fill={palette[idx % palette.length]} opacity="0.85" />;
  });

  // x-axis grid lines
  const grid = [];
  for (let i = 0; i <= 4; i++) {
    const y = (H * i) / 4;
    grid.push(<line key={i} x1="0" x2={W} y1={y} y2={y} stroke="var(--bd-0)" strokeDasharray="2 4" strokeWidth="1" />);
  }

  if (data.length < 2) {
    return (
      <div style={{height: H + "px", display:"flex", alignItems:"center", justifyContent:"center", color:"var(--tx-3)", fontFamily:"var(--font-mono)", fontSize:"10.5px"}}>
        warming up…
      </div>
    );
  }
  return (
    <svg className="share-chart" viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none">
      {grid}
      {paths}
    </svg>
  );
}

window.ChartsPanel = ChartsPanel;
