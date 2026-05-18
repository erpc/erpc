// FlowStage.jsx — top-down particle flow visualization.
//   Source (top) → eRPC core (middle) → Upstream nodes (bottom)
//   Real-request dots travel along the lines.
// Exposes: window.FlowStage

const { useEffect, useRef, useState, useMemo } = React;

function FlowStage() {
  // Read live state via hooks. We assemble a `state` shim with the
  // same shape the previous component consumed so the canvas/render
  // logic below stays untouched.
  const upstreams = window.useUpstreams();
  const cbState = window.useCBState();
  const perSec = window.usePerSecond();
  const upstreamStats = window.useUpstreamStats();
  const scenario = window.useScenario();
  const score = window.useScore();
  const perSecondHistory = window.usePerSecondHistory();
  const shape = window.useShape();
  const patternMix = window.usePatternMix();
  const focused = window.useFocusedUpstream();
  const actions = window.useSimActions();
  const state = {
    upstreams, cbState, score, perSecondHistory, scenario,
    perSecond: perSec,
    stats: { upstream: upstreamStats },
    shape,
    patternCount: patternMix.length,
    // The eRPC core badge used to read these from the in-browser sim.
    // Now the YAML editor is the source of truth; show static-ish
    // values that match the seed YAML defaults.
    failsafe: {
      retry: { maxAttempts: 3 },
      hedge: { delay: 350 },
      circuitBreaker: { failureThresholdCount: 7, failureThresholdCapacity: 10 },
    },
  };
  const wrapRef = useRef(null);
  const canvasRef = useRef(null);
  const sourceRef = useRef(null);
  const coreRef = useRef(null);
  const upRefs = useRef({});
  const particles = useRef([]);
  const dims = useRef({ w: 0, h: 0, dpr: 1 });
  const colors = useThemeColors();

  // ---------- canvas sizing ----------
  useEffect(() => {
    function resize() {
      const c = canvasRef.current;
      if (!c) return;
      const r = c.getBoundingClientRect();
      const dpr = window.devicePixelRatio || 1;
      c.width = Math.floor(r.width * dpr);
      c.height = Math.floor(r.height * dpr);
      dims.current = { w: r.width, h: r.height, dpr };
    }
    resize();
    const ro = new ResizeObserver(resize);
    if (wrapRef.current) ro.observe(wrapRef.current);
    return () => ro.disconnect();
  }, []);

  // ---------- measure node positions ----------
  function measure() {
    const wrap = wrapRef.current;
    if (!wrap) return null;
    const wr = wrap.getBoundingClientRect();
    function bottomCenter(el) {
      if (!el) return null;
      const r = el.getBoundingClientRect();
      return { x: r.left + r.width / 2 - wr.left, y: r.bottom - wr.top };
    }
    function topCenter(el) {
      if (!el) return null;
      const r = el.getBoundingClientRect();
      return { x: r.left + r.width / 2 - wr.left, y: r.top - wr.top };
    }
    const sourceBottom = bottomCenter(sourceRef.current);
    const coreTop = topCenter(coreRef.current);
    const coreBottom = bottomCenter(coreRef.current);
    const ups = {};
    for (const id in upRefs.current) {
      if (upRefs.current[id]) ups[id] = topCenter(upRefs.current[id]);
    }
    return { sourceBottom, coreTop, coreBottom, ups };
  }

  // ---------- particle spawn from a completed Request ----------
  function spawnFromEvent(req) {
    const g = measure();
    if (!g || !g.sourceBottom || !g.coreTop) return;
    const baseStart = performance.now();

    // Every ingress dot represents ONE arriving request — it's the
    // same kind of event regardless of how the request ultimately
    // resolved. Color stays neutral blue across the board; the
    // outcome story is told by the DOWNSTREAM dots (the per-attempt
    // particles), not by the ingress. This makes the source-to-core
    // line read as "client traffic flowing in" rather than "color-
    // coded preview of every request's outcome".
    particles.current.push({
      kind: "ingress", req,
      touchedUpstreams: (req.attempts || []).map(a => a.id),
      from: g.sourceBottom, to: g.coreTop,
      t0: baseStart, dur: 480,
      color: colors.info,
    });

    if (req.outcome === "cache-hit") {
      // cache hit: don't bounce a particle back up (confused users on direction).
      // Just flash the core (handled via __flowFlash from the parent).
      return;
    }

    // per-attempt: core → upstream (downward).
    //
    // Color scheme — reflects the SEMANTIC outcome of the attempt, not
    // just its raw status. The same "fail" status is visualized
    // differently depending on whether the request as a whole
    // recovered:
    //
    //   * winner of an "ok" request           → green
    //   * winner of a "retry-ok" request      → yellow (the lucky retry)
    //   * winner of a "hedge-win" request     → hot orange
    //   * hedge-loser (didn't lose; got beat) → hot dim
    //   * failed BUT the request still
    //     eventually succeeded (got retried)  → gray dim
    //     ↑ the user asked for this: a transient attempt failure that
    //     wasn't the request's outcome shouldn't dominate the lane.
    //   * failed AND the request itself failed
    //     terminally (no more retries left)   → red, full
    //
    const attemptsList = req.attempts || [];
    const lastAttemptIdx = attemptsList.length - 1;
    attemptsList.forEach((a, idx) => {
      const up = g.ups[a.id];
      if (!up) return;
      const t0 = baseStart + 500 + a.t0 * 0.16;
      const dur = Math.max(260, Math.min(800, a.dur * 0.18));
      const isWinner = !!a.winner;
      const isHardFail = a.outcome === "fail" || a.outcome === "timeout";
      // Terminal failure = the LAST attempt of a request that gave up
      // (req.outcome === "fail"). That earns the loud red.
      const isTerminalFail = isHardFail && req.outcome === "fail" && idx === lastAttemptIdx;
      let color = colors.dim, dim = false;
      if (a.outcome === "ok" && isWinner) {
        // Color the winner by HOW the request succeeded.
        if (req.outcome === "ok") color = colors.ok;
        else if (req.outcome === "hedge-win") color = colors.hot;
        else if (req.outcome === "retry-ok") color = colors.warn;
        else color = colors.ok;
      } else if (a.outcome === "hedge-loser") {
        // Hedge that lost the race. Real call, just cancelled.
        color = colors.hot; dim = true;
      } else if (isTerminalFail) {
        color = colors.bad;
      } else if (a.outcome === "miss") {
        // Upstream responded but didn't have the data (e.g. random tx
        // hash → null receipt). NOT a failure of the upstream — it
        // just doesn't have what was asked. Blue dim distinguishes
        // "miss" from "broken".
        color = colors.info; dim = true;
      } else if (a.outcome === "throttled") {
        // 429 at upstream. Real signal but not an error — operator
        // sees yellow tinge instead of red.
        color = colors.warn; dim = true;
      } else if (a.outcome === "cb-open") {
        // eRPC's circuit-breaker gate refused the call. Show subtle
        // red so it's still distinguishable from a transient miss.
        color = colors.bad; dim = true;
      } else if (isHardFail) {
        // Genuine fail that got rescued by retry — keep visible but
        // non-dominant so the winner is the obvious story.
        color = colors.dim; dim = true;
      } else {
        color = colors.dim; dim = true;
      }
      particles.current.push({
        kind: "attempt", req,
        upstreamId: a.id, // for focus-mode filtering
        from: g.coreBottom, to: up,
        t0, dur,
        color, dim,
        isWinner,
      });
    });

    // Note: NO synthetic dots are added beyond the real attempts. Each
    // particle above corresponds 1:1 to a row in the trace's
    // `req.ExecState().UpstreamAttemptLog()` recorded server-side.
  }

  // Spawn particles directly from every trace batch the WS pushed. This
  // is decoupled from `state.events` (200-deep log buffer) so the
  // visualization fidelity holds at high RPS where the log buffer
  // overflows.
  //
  // Invariant: 1 ingress dot per request + 1 dot per ExecState attempt.
  // No synthetic particles. Cache-hits emit only the ingress and flash
  // the core (no upstream call happened, no dots fan out).
  const traceBatch = window.useTraceBatch();
  const traceBatchSeq = window.useTraceBatchSeq();
  React.useEffect(() => {
    if (!traceBatch || traceBatch.length === 0) return;
    for (const t of traceBatch) spawnFromEvent(t);
  }, [traceBatchSeq]);

  // Read focused into a ref so the rAF loop sees fresh values without
  // re-subscribing the effect on every focus toggle (which would
  // restart the animation).
  const focusedRef = useRef(focused);
  useEffect(() => { focusedRef.current = focused; }, [focused]);

  // ---------- draw loop ----------
  useEffect(() => {
    let raf;
    function loop() {
      const c = canvasRef.current;
      if (!c) { raf = requestAnimationFrame(loop); return; }
      const ctx = c.getContext("2d");
      const { w, h, dpr } = dims.current;
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      ctx.clearRect(0, 0, w, h);
      const focusedId = focusedRef.current;

      const g = measure();
      if (g && g.sourceBottom && g.coreTop && g.coreBottom) {
        // ---- static channels ----
        // source → core is always at normal intensity; the focus story
        // happens BELOW the core, between core → upstream.
        drawChannel(ctx, g.sourceBottom, g.coreTop, colors.line, 0.6, 1.0);
        // core → each upstream: both opacity AND stroke-width scale with
        // the upstream's share of last-second attempts. A 50%-share lane
        // is visibly thicker + brighter than a 2%-share lane — this is
        // the at-a-glance "where is traffic going?" cue.
        //
        // We use `share = sel / total` rather than `sel / max(sel)` so
        // a single upstream getting all the traffic doesn't dim every
        // other lane to invisibility; we want absolute share, not
        // relative-to-leader. Both alpha and width use the same share
        // value put through a mild exponent to amplify contrast:
        //   * a 1% share lane stays near-invisible
        //   * a 10% share lane is clearly visible
        //   * a 30%+ share lane saturates as a thick bright "river"
        const last = state.perSecondHistory[state.perSecondHistory.length - 1] || { perUpstream: {} };
        const totalSel = Object.values(last.perUpstream || {}).reduce((a, b) => a + b, 0);
        for (const id in g.ups) {
          const sel = last.perUpstream?.[id] || 0;
          const share = totalSel > 0 ? sel / totalSel : 0;
          // saturate around 25–30% share; below 1% nearly invisible
          const intensity = Math.min(1, Math.pow(share * 4, 0.85));
          let alpha = 0.04 + intensity * 0.85;
          let width = 0.4 + intensity * 2.4;
          // Focus mode: keep the same NEUTRAL line color for every
          // lane (no green-tinted focus accent — the share-based
          // brightness signal is preserved). Just dial non-focused
          // channels down a notch so the focused lane reads as the
          // dominant one without being recolored. The dots themselves
          // — which we WANT to draw attention to — are dim-filtered
          // per-particle in the next loop.
          if (focusedId && id !== focusedId) {
            alpha *= 0.45;
            width *= 0.65;
          }
          drawChannel(ctx, g.coreBottom, g.ups[id], colors.line, alpha, width);
        }
      }

      // ---- particles ----
      const now = performance.now();
      const active = [];
      for (const p of particles.current) {
        const t = (now - p.t0) / p.dur;
        if (t < 0) { active.push(p); continue; }
        if (t > 1.05) continue;
        const tt = Math.max(0, Math.min(1, t));
        const ease = tt < 0.5 ? 2 * tt * tt : 1 - Math.pow(-2 * tt + 2, 2) / 2;
        const x = p.from.x + (p.to.x - p.from.x) * ease;
        const y = p.from.y + (p.to.y - p.from.y) * ease;
        // trail
        const trailT = Math.max(0, tt - 0.12);
        const te = trailT < 0.5 ? 2 * trailT * trailT : 1 - Math.pow(-2 * trailT + 2, 2) / 2;
        const tx = p.from.x + (p.to.x - p.from.x) * te;
        const ty = p.from.y + (p.to.y - p.from.y) * te;

        // Focus-mode dimming: a particle is "interesting" if it
        // involves the focused upstream — either it's a per-attempt
        // dot to that upstream, or it's the ingress for a request
        // that touched it. Non-relevant dots fade BUT stay clearly
        // visible — the user wants the focused ones to "stand out
        // more", not for the others to disappear.
        let focusDim = 1;
        if (focusedId) {
          const isRelevant =
            (p.kind === "attempt" && p.upstreamId === focusedId) ||
            (p.kind === "ingress" && (p.touchedUpstreams || []).includes(focusedId));
          focusDim = isRelevant ? 1.0 : 0.40;
        }

        ctx.strokeStyle = p.color;
        ctx.globalAlpha = (p.dim ? 0.22 : 0.46) * focusDim;
        ctx.lineWidth = p.dim ? 1.5 : 2.4;
        ctx.lineCap = "round";
        ctx.beginPath();
        ctx.moveTo(tx, ty);
        ctx.lineTo(x, y);
        ctx.stroke();
        // dot
        const r = p.dim ? 2 : 2.9;
        ctx.globalAlpha = (p.dim ? 0.55 : 1) * focusDim;
        ctx.fillStyle = p.color;
        ctx.beginPath();
        ctx.arc(x, y, r, 0, Math.PI * 2);
        ctx.fill();
        // winner glow
        if (p.isWinner && tt > 0.55 && tt < 0.95) {
          ctx.globalAlpha = 0.22;
          ctx.beginPath();
          ctx.arc(x, y, r * 3.2, 0, Math.PI * 2);
          ctx.fill();
        }
        // excluded X near upstream
        if (p.kind === "excluded" && tt > 0.7) {
          ctx.globalAlpha = 0.6;
          ctx.strokeStyle = p.color;
          ctx.lineWidth = 1.2;
          ctx.beginPath();
          ctx.moveTo(x - 3, y - 3); ctx.lineTo(x + 3, y + 3);
          ctx.moveTo(x + 3, y - 3); ctx.lineTo(x - 3, y + 3);
          ctx.stroke();
        }
        active.push(p);
      }
      particles.current = active;
      ctx.globalAlpha = 1;
      raf = requestAnimationFrame(loop);
    }
    raf = requestAnimationFrame(loop);
    return () => cancelAnimationFrame(raf);
  }, [colors]);

  // ---------- ranking ----------
  // Position pills come straight from the server's stats frame. The
  // backend's policy engine evaluates the live JS policy + metrics on
  // every request; the per-second selection share is the closest the
  // browser can get to "which upstream is the policy currently
  // preferring?" without re-implementing the engine here.
  const posOf = (id) => {
    const s = state.stats.upstream[id];
    if (!s) return { pos: "?", cls: "", tip: "" };
    const p = s.lastPosition;
    if (p == null) return { pos: "?", cls: "", tip: "" };
    if (p < 0) return {
      pos: "out",
      cls: "pos-x",
      tip: "Excluded by selection policy — not in rotation, retry chain, or hedge fan-out.",
    };
    return { pos: String(p), cls: `pos-${Math.min(2, p)}`, tip: "" };
  };

  // ---------- tooltip ----------
  //
  // We deliberately store ONLY the position + the upstream id here.
  // The tooltip content is rebuilt on every render from live state, so
  // values (errorRate, p50/p90/p95, score, …) update in real time as
  // stats frames flow in — no need to re-enter the node to see fresh
  // numbers. setTip is called only on enter/leave/move-to-different-node.
  const [tip, setTip] = useState(null);
  function onNodeEnter(e, u) {
    const r = e.currentTarget.getBoundingClientRect();
    // Tooltip is ~500px tall now (header + tags + 3 sections). Prefer
    // to anchor it ABOVE the node, but if that would clip past the top
    // of the viewport, flip below the node. Clamp horizontally too.
    const tipH = 520;
    const above = r.top - tipH - 8;
    const flipBelow = above < 8;
    setTip({
      x: Math.max(8, Math.min(window.innerWidth - 320, r.left + r.width / 2 - 150)),
      y: flipBelow ? (r.bottom + 8) : above,
      id: u.id,
    });
  }
  function onNodeLeave() { setTip(null); }

  // ---------- per-second activity ----------
  //
  // The under-node meter shows each upstream's SHARE of last-second
  // attempts (not relative-to-leader): a 50%-share node fills half the
  // bar, a 5%-share node fills 1/20th. We sum the per-upstream
  // selection counts to get the total attempts in the last bucket; if
  // there's no activity yet we default to 0% bars rather than maxing
  // out on the first stray attempt.
  const last = state.perSecondHistory[state.perSecondHistory.length - 1] || { perUpstream: {} };
  const totalSel = Object.values(last.perUpstream || {}).reduce((a, b) => a + b, 0);

  return (
    <div className="flow-wrap" ref={wrapRef}>
      <canvas ref={canvasRef} className="flow-canvas" />

      {state.scenario && (
        <div className="scn-banner">
          <span className="dot" />
          scenario · {state.scenario.name} · {Math.round((Date.now() - state.scenario.t0) / 1000)}s
        </div>
      )}

      {focused && (
        <div className="focus-banner" onClick={() => actions.clearFocusedUpstream()}>
          <span className="dot" />
          focused on <b>{focused}</b>
          <span className="kbd">ESC</span>
          <span className="clear">clear</span>
        </div>
      )}

      <div className="flow-stats" title="Each dot is a real eRPC request lifecycle: 1 ingress + 1 per upstream attempt (retries / hedges included).">
        <div className="st"><span>requests/s</span><span className="v">{Math.round(state.perSecond.total)}</span></div>
        <div className="st"><span>attempts/s</span><span className="v">{Object.values(state.perSecond.sel || {}).reduce((a, b) => a + b, 0)}</span></div>
        <div className="st"><span>success</span><span className="v">{state.perSecond.total > 0 ? ((state.perSecond.succ / state.perSecond.total) * 100).toFixed(1) : "—"}%</span></div>
        <div className="st"><span>cache hit</span><span className="v">{state.perSecond.total > 0 ? ((state.perSecond.cacheHit / state.perSecond.total) * 100).toFixed(0) : "0"}%</span></div>
        <div className="st"><span>particles</span><span className="v">{particles.current.length}</span></div>
      </div>

      <div className="flow-layout">
        <div className="fl-source-row">
          <div className="fl-source" ref={sourceRef}>
            <div className="icon" />
            <div className="label">
              <div className="nm">Client</div>
              <div className="sub">{state.shape} · {state.patternCount} patterns</div>
            </div>
            <div className="rate">
              <span className="v">{Math.round(state.perSecond.total).toLocaleString()}</span>
              <span className="u">req/s</span>
            </div>
          </div>
        </div>

        <div className="fl-core-row">
          <div className="fl-core" ref={coreRef}>
            <div className="hd">
              <span className="glyph" />
              <span className="nm">eRPC</span>
              <span className="badge">routing</span>
            </div>
            <div className="sub">
              <span><span className="k">policy</span> <span className="v">scored</span></span>
              <span><span className="k">retry</span> <span className="v">×{state.failsafe.retry.maxAttempts}</span></span>
              <span><span className="k">hedge</span> <span className="v">{state.failsafe.hedge.delay}ms</span></span>
              <span><span className="k">cb</span> <span className="v">{state.failsafe.circuitBreaker.failureThresholdCount}/{state.failsafe.circuitBreaker.failureThresholdCapacity}</span></span>
            </div>
          </div>
        </div>

        <div className="fl-ups-row">
          {state.upstreams.map(u => {
            const { pos, cls, tip } = posOf(u.id);
            const cb = state.cbState[u.id];
            const cbCls = cb?.state === "open" ? "cb-open" : cb?.state === "half" ? "cb-half" : "";
            const sel = last.perUpstream?.[u.id] || 0;
            // Share of TOTAL last-second attempts (not share-of-leader).
            // A node with 0 activity → 0% bar; a node handling all
            // traffic → 100%. This matches the operator's mental model
            // of "what fraction of requests is this upstream handling?"
            const share = totalSel > 0 ? sel / totalSel : 0;
            // Bar color tier comes from the REAL tracker error rate —
            // not the deprecated simulator tally — so it matches what
            // the policy is filtering on. Warn at 5%, red at 20%.
            const row = (state.stats.upstream && state.stats.upstream[u.id]) || {};
            const er = row.errorRate || 0;
            const meterCls = er > 0.2 ? "bad" : er > 0.05 ? "warn" : "";
            const sharePct = (share * 100);
            const shareLabel = sharePct >= 10 ? sharePct.toFixed(0) + "%"
                            : sharePct >= 1  ? sharePct.toFixed(1) + "%"
                            : sharePct > 0   ? "<1%"
                                              : "0";
            const isFocused = focused === u.id;
            const isDimmed = focused && !isFocused;
            return (
              <div
                key={u.id}
                className={`fl-node ${cls} ${cbCls} ${isFocused ? "focused" : ""} ${isDimmed ? "dimmed" : ""}`}
                ref={el => { upRefs.current[u.id] = el; }}
                onMouseEnter={e => onNodeEnter(e, u)}
                onMouseLeave={onNodeLeave}
                onClick={() => actions.toggleFocusedUpstream(u.id)}
                title={isFocused ? `Click to clear focus (or press ESC)` : `Click to focus on ${u.id}`}
              >
                <span className="pos-pill" title={tip}>{pos}</span>
                {cb?.state === "open" && <span className="cb-pill">CB</span>}
                {cb?.state === "half" && <span className="cb-pill half">HALF</span>}
                <div className="nm">{u.id}</div>
                <div className="vendor">{u.vendor}</div>
                <div
                  className={`meter ${meterCls}`}
                  title={`share of last-second attempts · ${sel} / ${totalSel}`}
                >
                  <div className="fill" style={{ transform: `scaleX(${share})` }} />
                  <span className="meter-label">{shareLabel}</span>
                </div>
              </div>
            );
          })}
        </div>
      </div>

      {/* Legend removed: outcome colors are explained in the lifecycle
          drawer (per-attempt row tints + the ConsensusResultRow). The
          standalone strip below the flow stage was visual noise. */}

      {tip && (() => {
        // LIVE read: every render pulls the latest row from state, so
        // values update as fast as stats frames arrive (~5×/sec) while
        // the tooltip is hovered.
        // NB: the per-upstream stats map is mounted at `state.stats.upstream`
        // (see how `state` is composed at the top of this component) — NOT
        // `state.upstreamStats`. Using the wrong path throws
        // "Cannot read properties of undefined (reading '<id>')".
        const u = (state.upstreams || []).find(x => x.id === tip.id);
        if (!u) return null;
        const row = (state.stats.upstream && state.stats.upstream[tip.id]) || {};
        const cb = (state.cbState && state.cbState[tip.id]?.state) || row.cbState || "closed";
        const last = state.perSecondHistory[state.perSecondHistory.length - 1] || { perUpstream: {} };
        const sel = last.perUpstream?.[tip.id] || 0;
        const pos = (row.position ?? -1);
        const posLabel = pos < 0 ? "OUT" : String(pos);
        const fmtPct = v => (v == null ? "—" : (v * 100).toFixed(2) + "%");
        const fmtMs  = v => (v == null ? "—" : Math.round(v) + "ms");
        const fmtNum = v => (v == null ? "—" : String(v));
        const fmtScore = v => (v == null ? "—" : Number(v).toFixed(3));
        return (
          <div className="fs-tip" style={{ left: tip.x, top: tip.y }}>
            <div className="hd">
              <span>{u.id} · {u.vendor}</span>
              <span className={`fs-tip-pos pos-${pos < 0 ? "x" : Math.min(2, pos)}`}>{posLabel}</span>
            </div>
            {(u.tags && u.tags.length > 0) && (
              <div className="fs-tip-tags">
                {u.tags.map(t => <span key={t} className="tag">{t}</span>)}
              </div>
            )}
            <div className="fs-tip-section">
              <div className="fs-tip-section-hd">policy-visible metrics</div>
              <div className="grd">
                <span className="k" title="Penalty score from sortByScore(BALANCED). LOWER = healthier — the policy sorts ascending and the upstream with the smallest score becomes primary. Formula: errorRate×8 + p70×4 + throttleRate×3 + blockHeadLag×2 + finalizationLag×1 + misbehaviorRate×6.">score · BALANCED ↓</span><span className="v">{fmtScore(row.score)}</span>
                <span className="k">error rate</span><span className="v">{fmtPct(row.errorRate)}</span>
                <span className="k">throttle rate</span><span className="v">{fmtPct(row.throttleRate)}</span>
                <span className="k">misbehavior rate</span><span className="v">{fmtPct(row.misbehaviorRate)}</span>
                <span className="k">p50 latency</span><span className="v">{fmtMs(row.p50)}</span>
                <span className="k">p90 latency</span><span className="v">{fmtMs(row.p90)}</span>
                <span className="k">p95 latency</span><span className="v">{fmtMs(row.p95)}</span>
                <span className="k">block head lag</span><span className="v">{fmtNum(row.blockHeadLag)} blk{row.blockHeadLagSeconds > 0 ? ` (~${row.blockHeadLagSeconds.toFixed(1)}s)` : ""}</span>
                <span className="k">finalization lag</span><span className="v">{fmtNum(row.finalizationLag)} blk{row.finalizationLagSeconds > 0 ? ` (~${row.finalizationLagSeconds.toFixed(1)}s)` : ""}</span>
                <span className="k">cordoned</span><span className="v">{row.cordoned ? `yes · ${row.cordonedReason || ""}` : "no"}</span>
              </div>
            </div>
            <div className="fs-tip-section">
              <div className="fs-tip-section-hd">score breakdown (BALANCED weights)</div>
              <div className="grd">
                {/* Same formula as stdlib.js `computePenalty(BALANCED)`.
                    Surfaced here so the operator can see which factor
                    is dominating an upstream's penalty without
                    re-deriving it from the metrics above. The TOTAL
                    label matches what's shown above as `score · BALANCED ↓`. */}
                {(() => {
                  const W = { error: 8, lat: 4, throttle: 3, lag: 2, fin: 1, misb: 6 };
                  const c = {
                    error: (row.errorRate || 0) * W.error,
                    lat:   ((row.p70 || 0) / 1000) * W.lat,
                    throttle: (row.throttleRate || 0) * W.throttle,
                    lag:   (row.blockHeadLag || 0) * W.lag,
                    fin:   (row.finalizationLag || 0) * W.fin,
                    misb:  (row.misbehaviorRate || 0) * W.misb,
                  };
                  const total = c.error + c.lat + c.throttle + c.lag + c.fin + c.misb;
                  const fmtC = v => v.toFixed(3);
                  return <>
                    <span className="k">errorRate × 8</span>      <span className="v">{fmtC(c.error)}</span>
                    <span className="k">p70 × 4 (s)</span>         <span className="v">{fmtC(c.lat)}</span>
                    <span className="k">throttleRate × 3</span>    <span className="v">{fmtC(c.throttle)}</span>
                    <span className="k">blockHeadLag × 2</span>    <span className="v">{fmtC(c.lag)}</span>
                    <span className="k">finalizationLag × 1</span> <span className="v">{fmtC(c.fin)}</span>
                    <span className="k">misbehaviorRate × 6</span> <span className="v">{fmtC(c.misb)}</span>
                    <span className="k fs-tip-total">TOTAL (lower wins)</span><span className="v fs-tip-total">{fmtC(total)}</span>
                  </>;
                })()}
              </div>
            </div>
            <div className="fs-tip-section">
              <div className="fs-tip-section-hd">window counters</div>
              <div className="grd">
                <span className="k">requests total</span><span className="v">{fmtNum(row.requestsTotal)}</span>
                <span className="k">errors total</span><span className="v">{fmtNum(row.errorsTotal)}</span>
                <span className="k">selections / 1s</span><span className="v">{sel}</span>
                <span className="k">circuit breaker</span><span className="v">{cb}</span>
              </div>
            </div>
          </div>
        );
      })()}
    </div>
  );
}

// straight-line channel
function drawChannel(ctx, a, b, color, alpha, width = 1) {
  ctx.strokeStyle = color;
  ctx.globalAlpha = alpha;
  ctx.lineWidth = width;
  ctx.lineCap = "round";
  ctx.beginPath();
  ctx.moveTo(a.x, a.y);
  ctx.lineTo(b.x, b.y);
  ctx.stroke();
  ctx.globalAlpha = 1;
  ctx.lineWidth = 1;
}

// ---------- theme colors hook ----------
function useThemeColors() {
  const [c, setC] = useState(readColors());
  useEffect(() => {
    function update() { setC(readColors()); }
    const obs = new MutationObserver(update);
    obs.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
    return () => obs.disconnect();
  }, []);
  return c;
}
function readColors() {
  const s = getComputedStyle(document.documentElement);
  const v = (k, fb) => s.getPropertyValue(k).trim() || fb;
  return {
    ok: v("--ok", "#4ade80"),
    warn: v("--warn", "#facc15"),
    hot: v("--hot", "#fb923c"),
    bad: v("--bad", "#f87171"),
    info: v("--info", "#60a5fa"),
    dim: v("--dim", "#4a4a54"),
    line: v("--bd-2", "#34343e"),
  };
}

window.FlowStage = FlowStage;
