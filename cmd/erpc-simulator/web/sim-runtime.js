// ============================================================
// eRPC Traffic Simulator — runtime store + WebSocket shim.
//
// One-paragraph rationale: the components used to reach into
// `window.eRPCSim` and a passed-down `state` ref directly. That worked
// against React's grain — there were no boundaries, mutations were
// implicit, and tests would have been ugly. This module turns the
// simulator into a small external store (Zustand-style):
//
//   * state lives in ONE object, replaced (not mutated) on every change
//   * components subscribe via `useSyncExternalStore` through the
//     `SimProvider` context (see sim-context.jsx)
//   * mutations happen through named actions on the runtime object
//
// The runtime owns:
//
//   * the WebSocket to /ws (auto-reconnect)
//   * traffic-gen tick (browser owns RPS, shape, pattern mix)
//   * the per-upstream synthetic-chain block-number drift (12s default)
//
// It does NOT own:
//
//   * routing / failsafe / policy compile — those live server-side
//   * any "fake" simulation of upstream behaviour — that's the backend's
//     UpstreamHub
//
// Public surface (used by sim-context.jsx):
//
//   createSimRuntime()  → { get, subscribe, actions, destroy, defaultPolicyCode, PATTERNS, ... }
// ============================================================

(function () {
  "use strict";

  // ---- Pattern catalog (k6-inspired) --------------------------------------
  const TRANSFER_EVENT_TOPIC =
    "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef";

  function randInt(lo, hi) { return Math.floor(Math.random() * (hi - lo + 1)) + lo; }
  function randAddr() {
    let s = "0x"; for (let i = 0; i < 40; i++) s += "0123456789abcdef"[randInt(0, 15)]; return s;
  }
  function randHash() {
    let s = "0x"; for (let i = 0; i < 64; i++) s += "0123456789abcdef"[randInt(0, 15)]; return s;
  }
  function toHex(n) { return "0x" + Math.max(0, n).toString(16); }
  const B58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";
  function randB58(n) { let s = ""; for (let i = 0; i < n; i++) s += B58[randInt(0, 57)]; return s; }
  function randPubkey() { return randB58(44); }  // Solana account pubkey shape
  function randSig() { return randB58(87); }     // Solana tx signature shape

  const PATTERNS = {
    "latest-block-number":  { label: "eth_blockNumber",                  build: () => ({ method: "eth_blockNumber", params: [] }) },
    "chain-id":             { label: "eth_chainId",                      build: () => ({ method: "eth_chainId", params: [] }) },
    "latest-block-full":    { label: "eth_getBlockByNumber('latest')",  build: () => ({ method: "eth_getBlockByNumber", params: ["latest", true] }) },
    "recent-block-fewback": { label: "eth_getBlockByNumber (latest-N)", build: (ctx) => ({ method: "eth_getBlockByNumber", params: [toHex((ctx.latestBlock || 22e6) - randInt(0, 500)), true] }) },
    "random-historical-block": { label: "eth_getBlockByNumber (random history)", build: (ctx) => ({ method: "eth_getBlockByNumber", params: [toHex(randInt(1, Math.max(1, (ctx.latestBlock || 22e6) - 1000))), false] }) },
    "logs-recent":          { label: "eth_getLogs (transfer @ tip)",     build: (ctx) => {
      const tip = ctx.latestBlock || 22e6; const shift = randInt(0, 5000); const span = randInt(0, shift || 1);
      return { method: "eth_getLogs", params: [{ fromBlock: toHex(Math.max(0, tip - shift)), toBlock: toHex(Math.max(0, tip - shift + span)), topics: [TRANSFER_EVENT_TOPIC] }] };
    }},
    "logs-random-range":    { label: "eth_getLogs (random history range)", build: (ctx) => {
      const tip = ctx.latestBlock || 22e6; const from = randInt(1, Math.max(1, tip - 1000)); const span = randInt(1, 100);
      return { method: "eth_getLogs", params: [{ fromBlock: toHex(from), toBlock: toHex(from + span), topics: [TRANSFER_EVENT_TOPIC] }] };
    }},
    "random-balance":       { label: "eth_getBalance (random addr @ latest)", build: () => ({ method: "eth_getBalance", params: [randAddr(), "latest"] }) },
    "random-receipt":       { label: "eth_getTransactionReceipt (random hash)", build: () => ({ method: "eth_getTransactionReceipt", params: [randHash()] }) },
    "eth-call-random":      { label: "eth_call (random to/data)",        build: () => ({ method: "eth_call", params: [{ to: randAddr(), data: "0x70a08231000000000000000000000000" + randAddr().slice(2) }, "latest"] }) },

    // ---- Solana (SVM) patterns — for `-preset svm` configs. Same
    // catalog mechanics; slot numbers ride ctx.latestBlock.
    "sol-get-slot":          { label: "getSlot (confirmed)",             build: () => ({ method: "getSlot", params: [{ commitment: "confirmed" }] }) },
    "sol-latest-blockhash":  { label: "getLatestBlockhash",              build: () => ({ method: "getLatestBlockhash", params: [{ commitment: "confirmed" }] }) },
    "sol-balance":           { label: "getBalance (random pubkey)",      build: () => ({ method: "getBalance", params: [randPubkey(), { commitment: "confirmed" }] }) },
    "sol-account-info":      { label: "getAccountInfo (random pubkey)",  build: () => ({ method: "getAccountInfo", params: [randPubkey(), { encoding: "base64", commitment: "confirmed" }] }) },
    "sol-block-recent":      { label: "getBlock (finalized, tip-N)",     build: (ctx) => ({ method: "getBlock", params: [Math.max(1, (ctx.latestBlock || 1000) - randInt(35, 400)), { maxSupportedTransactionVersion: 0, transactionDetails: "none", rewards: false, commitment: "finalized" }] }) },
    "sol-block-future":      { label: "getBlock (beyond tip → -32004)",  build: (ctx) => ({ method: "getBlock", params: [(ctx.latestBlock || 1000) + randInt(50, 500), { maxSupportedTransactionVersion: 0, transactionDetails: "none", commitment: "finalized" }] }) },
    "sol-transaction":       { label: "getTransaction (random sig)",     build: () => ({ method: "getTransaction", params: [randSig(), { maxSupportedTransactionVersion: 0, commitment: "finalized" }] }) },
    "sol-epoch-info":        { label: "getEpochInfo",                    build: () => ({ method: "getEpochInfo", params: [] }) },
    "sol-prio-fees":         { label: "getRecentPrioritizationFees",     build: () => ({ method: "getRecentPrioritizationFees", params: [[]] }) },
    "sol-send-tx":           { label: "sendTransaction (fake blob)",     build: () => ({ method: "sendTransaction", params: [randSig() + randSig(), { encoding: "base64", skipPreflight: true }] }) },
  };

  const DEFAULT_PATTERN_MIX = [
    { id: "latest-block-number",     weight: 12 },
    { id: "latest-block-full",       weight: 8 },
    { id: "recent-block-fewback",    weight: 18 },
    { id: "random-historical-block", weight: 14 },
    { id: "logs-recent",             weight: 10 },
    { id: "logs-random-range",       weight: 10 },
    { id: "random-balance",          weight: 10 },
    { id: "random-receipt",          weight: 8 },
    { id: "chain-id",                weight: 4 },
    { id: "eth-call-random",         weight: 6 },
  ];

  // Default mix for SVM configs: getAccountInfo-heavy, mirroring real
  // Solana RPC traffic (wallets/bots poll accounts; indexers pull
  // blocks/txs; a trickle of sends exercises the write path).
  const DEFAULT_SVM_PATTERN_MIX = [
    { id: "sol-get-slot",         weight: 14 },
    { id: "sol-account-info",     weight: 22 },
    { id: "sol-balance",          weight: 12 },
    { id: "sol-latest-blockhash", weight: 10 },
    { id: "sol-block-recent",     weight: 14 },
    { id: "sol-block-future",     weight: 4 },
    { id: "sol-transaction",      weight: 12 },
    { id: "sol-epoch-info",       weight: 4 },
    { id: "sol-prio-fees",        weight: 4 },
    { id: "sol-send-tx",          weight: 4 },
  ];

  const SCENARIOS = {
    "idle":                 { name: "idle", duration: 0 },
    "gradual-degradation":  { name: "gradual-degradation", duration: 90 },
    "vendor-region-outage": { name: "vendor-region-outage", duration: 60 },
    "traffic-spike":        { name: "traffic-spike", duration: 60 },
    "hedge-saturator":      { name: "hedge-saturator", duration: 60 },
    "policy-exclude-cascade": { name: "policy-exclude-cascade", duration: 50 },
    "slow-network":         { name: "slow-network", duration: 60 },
  };

  // ---- Initial state shape -------------------------------------------------
  function initialState() {
    return {
      // Server-pushed state (overwritten on snapshot / stats frames).
      upstreams: [],
      yaml: "",
      defaultYaml: "",
      serverPolicy: "",
      defaultPolicy: "",
      paused: false,
      score: {},
      perSecond: { sel: {}, succ: 0, err: 0, total: 0, cacheHit: 0, retryOk: 0, hedge: 0, miss: 0 },
      // 60s ring of {ts, total, retry, hedge, miss, err} for the
      // telemetry "ops strip" sparklines below the main share chart.
      opsHistory: [],
      actualRps: 0,
      perSecondHistory: [],
      upstreamStats: {},      // {id: {p50, p90, p95, errorRate, lastPosition, selectionsLast}}
      sparkErr: {},
      events: [],
      // Per-batch fan-out for the flow stage. The flow stage subscribes
      // via `useSimState(s => s.traceBatchSeq)` and reads `traceBatch`
      // on every increment — see the comment in `onTraces`.
      traceBatch: [],
      traceBatchSeq: 0,
      scenario: null,
      scenarioEvents: [],

      // Frontend-owned traffic gen.
      targetRps: 250,
      shape: "poisson",
      patternMix: DEFAULT_PATTERN_MIX.map(p => ({ ...p })),

      // Selection-policy decision history (ring buffer, max 200).
      // Each item is a PolicyDecisionFrame (see Go types.go). Stored
      // newest-first so the POLICY HISTORY panel scans top-to-bottom
      // in time-reversed order. Deduped by `.id`.
      policyHistoryRing:       [],
      policyHistoryRingMax:    200,
      // Set of decision ids seen this session — cheaper than scanning
      // the ring on every dedupe check.
      policyHistorySeen:       new Set(),

      // Editor state (mirrors what the editor has uncommitted).
      yamlDraft: "",
      policyDraft: "",
      policyLastApplied: "",
      configValidate: null,   // { ok, error }
      configResult: null,
      policyValidate: null,
      policyResult: null,
      // `pendingConfigApplyAck` / `pendingPolicyApplyAck` are set when
      // the user clicks Apply on the YAML or policy editor. They
      // instruct onSnapshot to FORCE-reset the corresponding draft to
      // the server's normalized form on the next snapshot — without
      // this, the editor stays "modified" forever because the eRPC
      // YAML round-trip (yaml.v3) normalizes formatting (inline arrays
      // become block lists, etc.) so the user's text never byte-matches
      // what the server publishes back. Cleared after the next snapshot.
      pendingConfigApplyAck: false,
      pendingPolicyApplyAck: false,

      // Selection-policy edit history (LRU-ish, max 50). Each entry:
      // { ts, code, source: "manual"|"snapshot"|"assistant"|"undo" }.
      // Persisted to localStorage on every change.
      policyHistory: [],
      policyHistoryIdx: -1,    // -1 means "at latest"; otherwise index into history we're viewing

      // Pattern context shared across pattern builds.
      patternCtx: { latestBlock: 22_000_000 },

      // Focus mode — when set to an upstream id, the UI dims/filters
      // everything OTHER than that upstream. Click an upstream node
      // (or a row in the upstream-knobs table) to focus; click it
      // again or hit ESC to clear.
      focusedUpstream: null,

      // Bookkeeping
      bootedAt: Date.now(),
      snapshotReady: false,
      reqSeq: 0,
    };
  }

  // ============================================================
  // createSimRuntime — the public entrypoint.
  // ============================================================
  function createSimRuntime() {
    let state = initialState();
    const subs = new Set();
    let stepCarry = 0;
    let ws = null;
    let wsReady = false;
    let outQueue = [];
    let blockTicker = null;
    let destroyed = false;

    // ---- store primitives ------------------------------------------------
    function get() { return state; }
    function subscribe(fn) { subs.add(fn); return () => subs.delete(fn); }
    function emit() { for (const fn of subs) fn(); }
    function replace(patch) {
      state = (typeof patch === "function") ? patch(state) : { ...state, ...patch };
      emit();
    }
    function mutate(patch) {
      // Sugar: shallow-merge into a fresh object so React's bail-out by
      // identity works for unchanged top-level keys.
      state = { ...state, ...patch };
      emit();
    }

    // ---- WS plumbing -----------------------------------------------------
    function wsURL() {
      const proto = location.protocol === "https:" ? "wss:" : "ws:";
      return `${proto}//${location.host}/ws`;
    }
    function connect() {
      if (destroyed) return;
      ws = new WebSocket(wsURL());
      ws.onopen = () => {
        wsReady = true;
        send({ kind: "hello" });
        for (const f of outQueue) ws.send(JSON.stringify(f));
        outQueue = [];
      };
      ws.onclose = () => {
        wsReady = false;
        ws = null;
        if (!destroyed) setTimeout(connect, 1000);
      };
      ws.onerror = () => {};
      ws.onmessage = (e) => {
        let env; try { env = JSON.parse(e.data); } catch { return; }
        handleFrame(env);
      };
    }
    function send(f) {
      if (wsReady && ws) ws.send(JSON.stringify(f));
      else {
        outQueue.push(f);
        if (outQueue.length > 4096) outQueue.splice(0, outQueue.length - 4096);
      }
    }

    function handleFrame(env) {
      const k = env.kind;
      const b = env.body || {};
      switch (k) {
        case "snapshot":               return onSnapshot(b);
        case "stats":                  return onStats(b);
        case "traces":                 return onTraces(b.items || []);
        case "policy-history":         return onPolicyHistory(b.items || []);
        case "knob-changed":           return onKnobChanged(b);
        case "policy-result": {
          // Arm the snapshot-side draft reset on successful apply so
          // the "modified" pill clears when the next snapshot lands.
          // The reset is the only safe way to drop "modified" without
          // false-clearing when an external edit just changed the
          // server baseline (see `pendingPolicyApplyAck` in initialState).
          const patch = { policyResult: b };
          if (b && b.ok) patch.pendingPolicyApplyAck = true;
          return mutate(patch);
        }
        case "policy-validate-result": return mutate({ policyValidate: b });
        case "config-result": {
          const patch = { configResult: b };
          if (b && b.ok) patch.pendingConfigApplyAck = true;
          return mutate(patch);
        }
        case "config-validate-result": return mutate({ configValidate: b });
        case "error":                  return console.warn("[sim] server error:", b.msg);
      }
    }

    function onSnapshot(b) {
      const upstreams = (b.upstreams || []).map(u => ({ ...u, chainId: 1 }));
      const score = {};
      const upstreamStats = {};
      const sparkErr = {};
      for (const u of upstreams) {
        score[u.id] = 0.5;
        upstreamStats[u.id] = { sel: 0, succ: 0, err: 0, latencies: [] };
        sparkErr[u.id] = new Array(60).fill(0);
      }
      // Seed the policy draft from the server if the user hasn't typed yet.
      const policySrc = b.policy || b.defaultPolicy || "";
      // The backend stores YAML with the policy inlined; the editor
      // always shows the templated form. Templatize on entry so the
      // user never sees the policy body duplicated in two places.
      const templatedYaml = b.yaml ? templatizeYaml(b.yaml) : state.yaml;
      const templatedDefault = b.defaultYaml ? templatizeYaml(b.defaultYaml) : state.defaultYaml;
      const newPatch = {
        upstreams,
        score,
        upstreamStats,
        sparkErr,
        yaml:         templatedYaml,
        defaultYaml:  templatedDefault,
        serverPolicy: policySrc,
        defaultPolicy: b.defaultPolicy || state.defaultPolicy,
        paused:       b.paused != null ? !!b.paused : state.paused,
        snapshotReady: true,
        bootedAt: state.snapshotReady ? state.bootedAt : Date.now(),
      };
      // SVM configs get the Solana traffic mix — but only while the
      // operator hasn't customized the mix (it still equals the EVM
      // default). Sniffing the YAML keeps the backend protocol
      // unchanged; `-preset svm` is the only current source of svm
      // configs and its seed always declares `architecture: svm`.
      if (templatedYaml && /architecture:\s*svm/.test(templatedYaml) &&
          JSON.stringify(state.patternMix) === JSON.stringify(DEFAULT_PATTERN_MIX)) {
        newPatch.patternMix = DEFAULT_SVM_PATTERN_MIX.map(p => ({ ...p }));
      }
      // Reset yamlDraft if either:
      //   * the user hasn't typed anything (draft is empty or matches
      //     the prior server yaml), OR
      //   * a config-apply just succeeded — in which case the server
      //     is the authority on the canonical form and we accept its
      //     normalized output as the new draft (clears the "modified"
      //     flag that would otherwise persist due to yaml.v3 formatting
      //     differences).
      if (!state.yamlDraft || state.yamlDraft === state.yaml || state.pendingConfigApplyAck) {
        newPatch.yamlDraft = templatedYaml || state.yamlDraft;
        if (state.pendingConfigApplyAck) {
          newPatch.pendingConfigApplyAck = false;
        }
      }
      // Same idea for the policy editor — on a successful policy apply
      // the engine round-trips through `injectPolicyIntoYAML` which can
      // shift whitespace; accept the server's form as the new baseline.
      if (!state.policyDraft || state.policyDraft === state.policyLastApplied || state.pendingPolicyApplyAck) {
        newPatch.policyDraft = policySrc;
        newPatch.policyLastApplied = policySrc;
        if (state.pendingPolicyApplyAck) {
          newPatch.pendingPolicyApplyAck = false;
        }
      } else if (state.policyLastApplied !== policySrc) {
        // Server's "in use" baseline changed (e.g. from external admin
        // reapply) — keep policyLastApplied in sync so the "dirty"
        // marker stays accurate even when we don't touch the draft.
        newPatch.policyLastApplied = policySrc;
      }
      mutate(newPatch);
    }

    function onStats(frame) {
      const perSec = {
        sel: { ...(frame.perUpstreamLastS || {}) },
        succ: frame.lastSecondSucc || 0,
        err: frame.lastSecondErr || 0,
        total: frame.lastSecondTotal || 0,
        cacheHit: frame.lastSecondCache || 0,
        retryOk: frame.lastSecondRetryOk || 0,
        hedge: frame.lastSecondHedge || 0,
        miss: frame.lastSecondMiss || 0,
      };
      const sc = { ...state.score };
      const us = { ...state.upstreamStats };
      const sp = { ...state.sparkErr };
      if (frame.upstreams) {
        for (const id in frame.upstreams) {
          const row = frame.upstreams[id];
          sc[id] = row.score || 0;
          // Merge the FULL UpstreamStatsRow (all tracker fields) into
          // upstreamStats so the tooltip + UI can read every field the
          // selection policy itself sees:
          //   errorRate, throttleRate, misbehaviorRate, p50/p90/p95,
          //   blockHeadLag, finalizationLag, cordoned, cordonedReason,
          //   requestsTotal, errorsTotal, score (PREFER_FASTEST), position,
          //   selectionsLast.
          // `last*`-prefixed aliases mirror the canonical fields for
          // any tooltip/chart components that read those keys.
          us[id] = {
            ...(us[id] || {}),
            ...row,
            lastP50: row.p50,
            lastP90: row.p90,
            lastP95: row.p95,
            lastErrorRate: row.errorRate,
            lastPosition: row.position,
            selectionsLast: row.selectionsLast,
          };
          const series = (sp[id] || new Array(60).fill(0)).slice();
          series.push(row.errorRate || 0);
          while (series.length > 60) series.shift();
          sp[id] = series;
        }
      }
      const nextBucket = Math.floor((Date.now() - (state.bootedAt || Date.now())) / 1000);
      const history = state.perSecondHistory.slice();
      const ops = state.opsHistory.slice();
      if (history.length === 0 || history[history.length - 1].ts !== nextBucket) {
        history.push({ ts: nextBucket, perUpstream: { ...(frame.perUpstreamLastS || {}) } });
        while (history.length > 60) history.shift();
        ops.push({
          ts: nextBucket,
          total: perSec.total,
          retry: perSec.retryOk,
          hedge: perSec.hedge,
          miss: perSec.miss,
          err: perSec.err,
        });
        while (ops.length > 60) ops.shift();
      }
      let scenario = state.scenario;
      let scnEvents = state.scenarioEvents;
      if (frame.scenario) {
        scenario = { name: frame.scenario.name, t0: Date.now() - frame.scenario.elapsed * 1000, idx: 0 };
        scnEvents = (frame.scenario.events || []).map(e => ({ t: e.t, d: e.d }));
      } else if (state.scenario) {
        scenario = null; scnEvents = [];
      }
      mutate({
        perSecond: perSec, actualRps: frame.actualRps || 0,
        score: sc, upstreamStats: us, sparkErr: sp,
        perSecondHistory: history, opsHistory: ops,
        scenario, scenarioEvents: scnEvents,
      });
    }

    // onPolicyHistory ingests the WS `policy-history` frame the
    // backend sends on every stats tick. Items are PolicyDecisionFrames
    // (see Go types.go). We dedupe by `id` because the backend re-sends
    // the last N (currently 16) ticks every 200 ms — the steady-state
    // overlap is large, but this absorbs gaps cleanly when a slow
    // browser misses a frame. New decisions are prepended (newest-first
    // ordering for the POLICY HISTORY panel).
    function onPolicyHistory(items) {
      if (!items.length) return;
      const seen = state.policyHistorySeen;
      // Sort items oldest-first so the prepend loop ends with the
      // newest item at index 0 of the resulting array.
      const sorted = items.slice().sort((a, b) => (a.tickMs || 0) - (b.tickMs || 0));
      let ring = state.policyHistoryRing;
      let added = 0;
      for (const it of sorted) {
        if (!it || !it.id) continue;
        if (seen.has(it.id)) continue;
        seen.add(it.id);
        ring = [it, ...ring];
        added++;
      }
      if (added === 0) return;
      const cap = state.policyHistoryRingMax;
      if (ring.length > cap) {
        // Evict tail (oldest). Also prune from the seen set so memory
        // doesn't grow unbounded over a long session.
        const evicted = ring.slice(cap);
        ring = ring.slice(0, cap);
        for (const e of evicted) {
          if (e && e.id) seen.delete(e.id);
        }
      }
      mutate({ policyHistoryRing: ring });
    }

    function onTraces(items) {
      if (!items.length) return;
      // Normalize once; reuse for both the log buffer AND the visualizer.
      const normalized = items.map(t => ({
        id: t.id || (Date.now() * 1000 + Math.floor(Math.random() * 1000)),
        ts: t.ts ? new Date(t.ts).getTime() : Date.now(),
        method: t.method, outcome: t.outcome, winner: t.winner || "",
        duration: t.duration || 0,
        usedHedge: !!t.usedHedge, usedRetry: !!t.usedRetry,
        consensusSlots:    t.consensusSlots || 0,
        consensusDisputes: t.consensusDisputes || 0,
        consensusLowParts: t.consensusLowParts || 0,
        requestError: t.requestError || "",
        responseBody: t.responseBody || "",
        sel: t.sel || [],
        attempts: (t.attempts || []).map(a => ({
          id: a.id, t0: a.t0, dur: a.dur,
          outcome: serverAttemptOutcome(a.outcome),
          reason: a.reason || "", winner: !!a.winner,
          selReason: a.selReason || "",  // primary|retry|hedge|consensus_slot|sweep
          isHedge: !!a.isHedge,
          isRetry: !!a.isRetry,
          attemptIdx: a.attemptIdx || 0,
        })),
      }));
      // Two consumers, two channels.
      //
      // 1) state.events: a category-aware ring buffer for the Event-log
      //    table. We don't use a flat global cap because at high RPS
      //    "ok" events would otherwise evict every rare event (consensus
      //    disputes, hedge wins, failures, retries) before the operator
      //    can find them. Instead we apply a per-category cap so each
      //    filter tab always has a meaningful population.
      // 2) state.traceBatch / state.traceBatchSeq: a per-batch fan-out
      //    used by the FlowStage to spawn particles. Replaced wholesale
      //    on every batch. Decoupled from the log buffer so high-RPS
      //    visualization fidelity isn't gated by the log's size.
      const events = state.events.slice();
      for (const t of normalized) {
        events.unshift({ ...t, fresh: true });
      }
      // Per-category retention. Categories are derived from the outcome
      // + flags, mirroring the filter tabs in EventLog. Each category
      // keeps up to 1000 entries chronologically (newest first), so
      // even at sustained high-RPS the "consensus", "err", "retry",
      // and "hedge" tabs always have a decent backfill.
      const PER_CAT_CAP = 1000;
      const cats = { ok: [], err: [], hedge: [], retry: [], consensus: [], miss: [] };
      for (const e of events) {
        if (e.outcome === "fail") cats.err.push(e);
        else if (e.usedHedge || e.outcome === "hedge-win") cats.hedge.push(e);
        else if (e.usedRetry || e.outcome === "retry-ok") cats.retry.push(e);
        else cats.ok.push(e);
        if ((e.consensusSlots || 0) > 0) cats.consensus.push(e);
        // "miss" = first-attempt empty / no-data result. We don't carry
        // this as an outcome on the wire today, but reserve the bucket
        // for the new tab so the categorization stays stable.
        if (e.outcome === "miss") cats.miss.push(e);
      }
      // Trim each bucket to PER_CAT_CAP. Then merge back to a unique
      // event list (preserving order: newer first). An event can live
      // in multiple categories — we de-dup by id.
      const seen = new Set();
      const merged = [];
      for (const list of [cats.err, cats.hedge, cats.retry, cats.consensus, cats.miss, cats.ok]) {
        const capped = list.slice(0, PER_CAT_CAP);
        for (const e of capped) {
          if (seen.has(e.id)) continue;
          seen.add(e.id);
          merged.push(e);
        }
      }
      // Re-sort by ts desc so the table renders newest-first.
      merged.sort((a, b) => (b.ts || 0) - (a.ts || 0));
      events.length = 0;
      events.push(...merged);
      mutate({
        events,
        traceBatch: normalized,
        traceBatchSeq: (state.traceBatchSeq || 0) + 1,
      });
      setTimeout(() => {
        // De-fresh log rows after 800ms so the highlight stops.
        const stripped = state.events.map(e => e.fresh ? { ...e, fresh: false } : e);
        mutate({ events: stripped });
      }, 800);
    }

    function onKnobChanged(b) {
      const idx = state.upstreams.findIndex(u => u.id === b.id);
      if (idx < 0) return;
      const updated = state.upstreams.slice();
      updated[idx] = { ...updated[idx], ...b };
      mutate({ upstreams: updated });
    }

    // Map the on-wire `common.UpstreamAttemptOutcome` enum
    // (upstream/upstream.go classifyUpstreamOutcome) to a UI-friendly
    // bucket. A coarse fold here is OK — visualization wants ~6
    // visually-distinct categories rather than the full 13.
    //
    //   success       → "ok"        (winning response)
    //   hedge-winner  → "ok"        (winning response — flagged by Won)
    //   empty         → "miss"      (upstream answered, no data — NOT a failure)
    //   missing_data  → "miss"
    //   block_unavailable → "miss"
    //   timeout       → "timeout"
    //   rate_limited  → "throttled" (429 at upstream)
    //   skipped       → "hedge-loser"
    //   cancelled     → "hedge-loser" (hedge cancelled when another won)
    //   transport_error / server_error / client_error / exec_revert
    //                 → "fail"      (genuine upstream failures)
    function serverAttemptOutcome(o) {
      switch (o) {
        case "success":
        case "hedge-winner":
          return "ok";
        case "empty":
        case "missing_data":
        case "block_unavailable":
          return "miss";
        case "timeout":
          return "timeout";
        case "rate_limited":
          return "throttled";
        case "skipped":
        case "cancelled":
          return "hedge-loser";
        default:
          return "fail";
      }
    }

    // ---- Traffic generator -----------------------------------------------
    function pickPattern(mix) {
      let total = 0;
      for (const m of mix) total += Math.max(0, m.weight || 0);
      if (total <= 0) return PATTERNS["latest-block-number"];
      let r = Math.random() * total;
      for (const m of mix) {
        r -= Math.max(0, m.weight || 0);
        if (r <= 0) return PATTERNS[m.id] || PATTERNS["latest-block-number"];
      }
      return PATTERNS[mix[0].id] || PATTERNS["latest-block-number"];
    }
    function poisson(lambda) {
      if (lambda <= 0) return 0;
      const L = Math.exp(-lambda); let k = 0, p = 1;
      do { k++; p *= Math.random(); } while (p > L && k < lambda * 10 + 50);
      return k - 1;
    }

    function tick(dtMs) {
      if (!state.snapshotReady) return;
      if (state.paused) return;
      const base = state.targetRps * (dtMs / 1000);
      let count;
      if (state.shape === "poisson") count = poisson(base);
      else if (state.shape === "bursty") count = Math.round(base * (Math.random() < 0.15 ? 4 + Math.random() * 6 : 0.2 + Math.random() * 0.6));
      else { stepCarry += base; count = Math.floor(stepCarry); stepCarry -= count; }
      count = Math.max(0, count);
      if (count === 0) return;
      const items = new Array(count);
      let seq = state.reqSeq;
      for (let i = 0; i < count; i++) {
        const pat = pickPattern(state.patternMix);
        const built = pat.build(state.patternCtx);
        items[i] = { id: ++seq, method: built.method, params: built.params };
      }
      mutate({ reqSeq: seq });
      send({ kind: "send-batch", items });
    }

    // ---- Actions ---------------------------------------------------------

    // Selection-policy substitution / templatization (FRONTEND-ONLY).
    //
    // The YAML editor in the browser always shows the templated form:
    //   selectionPolicy:
    //     evalFunc: "{SELECTION_POLICY_FUNC}"
    // The backend, on the other hand, only ever sees / stores the
    // fully-inlined form. We bridge the two with two transforms that
    // run only on the wire:
    //
    //   * substitutePolicyInYaml — applied BEFORE sending apply-config /
    //     validate-config. Replaces the placeholder with the current
    //     policy draft, emitted as a YAML literal-block scalar.
    //
    //   * templatizeYaml — applied AFTER receiving yaml from the server
    //     (snapshot / config-result). Finds the inlined `evalFunc: |`
    //     block and rewrites it back to `evalFunc: "{SELECTION_POLICY_FUNC}"`
    //     so the editor stays clean and the policy editor remains the
    //     authoritative view for the function body.
    //
    // The backend treats YAML as opaque text — placeholder logic lives
    // entirely here, in the frontend.
    const POLICY_PLACEHOLDER = "{SELECTION_POLICY_FUNC}";

    function substitutePolicyInYaml(yaml) {
      if (!yaml || yaml.indexOf(POLICY_PLACEHOLDER) < 0) return yaml;
      const policy = state.policyDraft || state.policyLastApplied || "";
      if (!policy) return yaml;
      const lines = yaml.split("\n");
      for (let i = 0; i < lines.length; i++) {
        const ln = lines[i];
        const idx = ln.indexOf("evalFunc:");
        if (idx < 0 || ln.indexOf(POLICY_PLACEHOLDER) < 0) continue;
        const indent = ln.slice(0, idx);
        const bodyIndent = indent + "  ";
        const body = policy.replace(/\n$/, "").split("\n").map(b => bodyIndent + b);
        lines[i] = [indent + "evalFunc: |", ...body].join("\n");
      }
      return lines.join("\n");
    }

    function templatizeYaml(yaml) {
      if (!yaml || yaml.indexOf("evalFunc:") < 0) return yaml;
      // If the placeholder is already present, nothing to do.
      if (yaml.indexOf(POLICY_PLACEHOLDER) >= 0) return yaml;
      const lines = yaml.split("\n");
      const out = [];
      for (let i = 0; i < lines.length; i++) {
        const ln = lines[i];
        const idx = ln.indexOf("evalFunc:");
        if (idx < 0) { out.push(ln); continue; }
        const indent = ln.slice(0, idx);
        const after = ln.slice(idx + "evalFunc:".length).trim();
        // Case 1: block scalar `evalFunc: |` (or |- / |+ etc.) — consume
        // every following line indented strictly deeper than this one.
        if (after.startsWith("|") || after.startsWith(">")) {
          out.push(indent + 'evalFunc: "' + POLICY_PLACEHOLDER + '"');
          let j = i + 1;
          while (j < lines.length) {
            const nx = lines[j];
            if (nx.length === 0) { j++; continue; }
            // Stop when a line is indented LESS or EQUAL to evalFunc.
            const nxIndent = nx.match(/^[ ]*/)[0].length;
            if (nxIndent <= indent.length) break;
            j++;
          }
          i = j - 1;
          continue;
        }
        // Case 2: inline scalar — `evalFunc: "(u, c) => …"` or
        // `evalFunc: '...'` or a bare plain scalar on one line. Replace
        // the value with the quoted placeholder.
        out.push(indent + 'evalFunc: "' + POLICY_PLACEHOLDER + '"');
      }
      return out.join("\n");
    }

    const actions = {
      applyConfig(yaml) { send({ kind: "apply-config", yaml: substitutePolicyInYaml(yaml) }); },
      validateConfig(yaml) { send({ kind: "validate-config", yaml: substitutePolicyInYaml(yaml) }); },
      applyPolicy(src) { send({ kind: "apply-policy", src }); pushPolicyHistory(src, "manual"); },
      validatePolicy(src) { send({ kind: "validate-policy", src }); },
      patchUpstream(id, patch) {
        // Optimistic local merge so the slider snaps immediately; the
        // server's `knob-changed` echo confirms.
        const idx = state.upstreams.findIndex(u => u.id === id);
        if (idx >= 0) {
          const updated = state.upstreams.slice();
          updated[idx] = { ...updated[idx], ...patch };
          mutate({ upstreams: updated });
        }
        send({ kind: "set-knob", id, patch });
      },
      setPaused(paused) { mutate({ paused }); send({ kind: "set-paused", paused }); },
      startScenario(name) {
        if (name === "idle") { send({ kind: "stop-scenario" }); mutate({ scenario: null, scenarioEvents: [] }); return; }
        send({ kind: "start-scenario", name });
      },
      stopScenario() { send({ kind: "stop-scenario" }); mutate({ scenario: null, scenarioEvents: [] }); },
      setTargetRps(rps) { mutate({ targetRps: rps }); },
      setShape(shape) { mutate({ shape }); },
      setPatternMix(patternMix) { mutate({ patternMix }); },
      setYamlDraft(yamlDraft) { mutate({ yamlDraft }); },
      setPolicyDraft(policyDraft) { mutate({ policyDraft }); },
      clearEvents() { mutate({ events: [] }); },
      // Focus mode helpers — toggle is the natural UX (click again
      // to clear). Passing the same id clears; passing null clears.
      toggleFocusedUpstream(id) {
        if (!id || state.focusedUpstream === id) mutate({ focusedUpstream: null });
        else mutate({ focusedUpstream: id });
      },
      setFocusedUpstream(id) { mutate({ focusedUpstream: id || null }); },
      clearFocusedUpstream() { mutate({ focusedUpstream: null }); },
      injectFailure(id, durationMs = 60_000) {
        const expiresAt = Date.now() + durationMs;
        send({ kind: "set-knob", id, patch: { injectFailureUntilMs: expiresAt } });
      },

      // ---- Policy history (localStorage-backed) ----
      // pushPolicyHistory is called by applyPolicy and by external
      // (assistant) tool calls. Exposed for the upcoming AI integration.
      pushPolicyHistory(code, source) { pushPolicyHistory(code, source); },
      gotoPolicyHistory(idx) {
        const h = state.policyHistory;
        if (idx < 0 || idx >= h.length) return;
        const entry = h[idx];
        mutate({ policyHistoryIdx: idx, policyDraft: entry.code });
      },
      undoPolicy() {
        const h = state.policyHistory;
        if (h.length < 2) return;
        const target = (state.policyHistoryIdx < 0 ? h.length - 1 : state.policyHistoryIdx) - 1;
        if (target < 0) return;
        actions.gotoPolicyHistory(target);
      },
      redoPolicy() {
        const h = state.policyHistory;
        if (state.policyHistoryIdx < 0) return;
        const target = state.policyHistoryIdx + 1;
        if (target >= h.length) { mutate({ policyHistoryIdx: -1, policyDraft: h[h.length - 1].code }); return; }
        actions.gotoPolicyHistory(target);
      },
    };

    function pushPolicyHistory(code, source) {
      if (!code) return;
      const last = state.policyHistory[state.policyHistory.length - 1];
      if (last && last.code === code) return; // de-dup consecutive duplicates
      const entry = { ts: Date.now(), code, source: source || "manual" };
      const next = state.policyHistory.concat(entry);
      while (next.length > 50) next.shift();
      mutate({ policyHistory: next, policyHistoryIdx: -1, policyLastApplied: code });
      persistHistory(next);
    }

    function persistHistory(history) {
      try { localStorage.setItem("erpc-sim-policy-history", JSON.stringify(history)); } catch (_) {}
    }
    function loadPersistedHistory() {
      try {
        const raw = localStorage.getItem("erpc-sim-policy-history");
        if (!raw) return [];
        const parsed = JSON.parse(raw);
        return Array.isArray(parsed) ? parsed : [];
      } catch { return []; }
    }

    // ---- Boot ------------------------------------------------------------
    // Seed history from localStorage before connecting.
    state = { ...state, policyHistory: loadPersistedHistory() };
    connect();
    blockTicker = setInterval(() => {
      mutate({ patternCtx: { ...state.patternCtx, latestBlock: (state.patternCtx.latestBlock || 22e6) + 1 } });
    }, 12_000);

    function destroy() {
      destroyed = true;
      if (blockTicker) clearInterval(blockTicker);
      if (ws) try { ws.close(); } catch (_) {}
      subs.clear();
    }

    return {
      get, subscribe, actions, tick, destroy,
      PATTERNS, DEFAULT_PATTERN_MIX, DEFAULT_SVM_PATTERN_MIX, SCENARIOS,
    };
  }

  // ---- Expose as a vanilla module on window for the inline <script> graph.
  // (Babel-in-browser doesn't have ES modules; this is the smallest
  // possible shim — components NEVER reach for this directly. They go
  // through `useSim()` / `useSimState()` in sim-context.jsx.)
  window.eRPCSimRuntime = { createSimRuntime, PATTERNS, DEFAULT_PATTERN_MIX, DEFAULT_SVM_PATTERN_MIX, SCENARIOS };
})();
