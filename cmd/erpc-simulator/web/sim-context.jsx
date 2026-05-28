// ============================================================
// SimContext — the React surface over the runtime store.
//
//   <SimProvider>            → owns a single runtime instance, drives
//                              the traffic-gen tick, exposes context.
//   useSim()                 → raw runtime (rarely needed).
//   useSimState(selector?)   → subscribes to state via
//                              useSyncExternalStore. With no selector
//                              returns the full state. With a selector
//                              returns just that slice (re-renders only
//                              when the selector result changes by
//                              Object.is).
//   useSimActions()          → stable bag of mutation functions
//                              (applyConfig, patchUpstream, …).
//
// Components NEVER reach into window.* — every read/write goes through
// the context. The runtime keeps a small `window.eRPCSimRuntime` export
// only so the inline <script> graph can find the factory; it's not the
// runtime instance and components should not touch it.
// ============================================================
const SimContext = React.createContext(null);

function SimProvider({ children }) {
  const runtimeRef = React.useRef(null);
  if (runtimeRef.current === null) {
    runtimeRef.current = window.eRPCSimRuntime.createSimRuntime();
  }
  const runtime = runtimeRef.current;

  // Drive the traffic-gen tick at 10Hz. The runtime is a vanilla
  // module; React owns the timer + cleanup.
  React.useEffect(() => {
    let last = performance.now();
    const id = setInterval(() => {
      const now = performance.now();
      const dt = Math.min(200, now - last);
      last = now;
      runtime.tick(dt);
    }, 100);
    return () => {
      clearInterval(id);
      runtime.destroy();
    };
  }, [runtime]);

  // Spacebar = pause/resume (when not focused on an input).
  React.useEffect(() => {
    function onKey(e) {
      if (e.target.tagName === "INPUT" || e.target.tagName === "TEXTAREA") return;
      if (e.code === "Space") {
        e.preventDefault();
        runtime.actions.setPaused(!runtime.get().paused);
      }
      if (e.code === "Escape" && runtime.get().focusedUpstream) {
        runtime.actions.clearFocusedUpstream();
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [runtime]);

  return <SimContext.Provider value={runtime}>{children}</SimContext.Provider>;
}

function useSim() {
  const v = React.useContext(SimContext);
  if (!v) throw new Error("useSim() outside <SimProvider>");
  return v;
}

// Subscribe to the runtime state. With no selector, returns the full
// state object — re-renders on EVERY mutation (use sparingly). With a
// selector, returns the selector's result — re-renders only when the
// result changes (Object.is). Pass a stable selector ref (defined
// outside the component or memoized) to avoid re-subscriptions.
function useSimState(selector) {
  const runtime = useSim();
  const getSnapshot = React.useCallback(() => {
    return selector ? selector(runtime.get()) : runtime.get();
  }, [runtime, selector]);
  return React.useSyncExternalStore(runtime.subscribe, getSnapshot, getSnapshot);
}

function useSimActions() {
  const runtime = useSim();
  return runtime.actions;
}

// Common selector hooks. Each one is a tiny wrapper around
// `useSimState(selector)`. Adding a new one is cheap; encouraging
// component code to use these instead of inline selectors keeps the
// re-render footprint visible at a glance.
function useUpstreams()        { return useSimState(s => s.upstreams); }
function useUpstreamStats()    { return useSimState(s => s.upstreamStats); }
function useScore()            { return useSimState(s => s.score); }
function usePerSecond()        { return useSimState(s => s.perSecond); }
function usePerSecondHistory() { return useSimState(s => s.perSecondHistory); }
function useOpsHistory()       { return useSimState(s => s.opsHistory); }
function useEvents()           { return useSimState(s => s.events); }
function useTraceBatch()       { return useSimState(s => s.traceBatch); }
function useTraceBatchSeq()    { return useSimState(s => s.traceBatchSeq); }
function useScenario()         { return useSimState(s => s.scenario); }
function useScenarioEvents()   { return useSimState(s => s.scenarioEvents); }
function usePaused()           { return useSimState(s => s.paused); }
function useTargetRps()        { return useSimState(s => s.targetRps); }
function useShape()            { return useSimState(s => s.shape); }
function usePatternMix()       { return useSimState(s => s.patternMix); }
function useYAML()             { return useSimState(s => s.yaml); }
function useDefaultYaml()      { return useSimState(s => s.defaultYaml); }
function useYamlDraft()        { return useSimState(s => s.yamlDraft); }
function usePolicyDraft()      { return useSimState(s => s.policyDraft); }
function useServerPolicy()     { return useSimState(s => s.serverPolicy); }
function useDefaultPolicy()    { return useSimState(s => s.defaultPolicy); }
function usePolicyHistory()    { return useSimState(s => s.policyHistory); }
function usePolicyHistoryRing() { return useSimState(s => s.policyHistoryRing); }
function usePolicyHistoryIdx() { return useSimState(s => s.policyHistoryIdx); }
function useConfigValidate()   { return useSimState(s => s.configValidate); }
function useConfigResult()     { return useSimState(s => s.configResult); }
function usePolicyValidate()   { return useSimState(s => s.policyValidate); }
function usePolicyResult()     { return useSimState(s => s.policyResult); }
function useSparkErr()         { return useSimState(s => s.sparkErr); }
function useActualRps()        { return useSimState(s => s.actualRps); }
function useSnapshotReady()    { return useSimState(s => s.snapshotReady); }
function useFocusedUpstream()  { return useSimState(s => s.focusedUpstream); }

// Expose for the (Babel-in-browser) script graph.
window.SimContext = SimContext;
window.SimProvider = SimProvider;
window.useSim = useSim;
window.useSimState = useSimState;
window.useSimActions = useSimActions;
window.useUpstreams = useUpstreams;
window.useUpstreamStats = useUpstreamStats;
window.useScore = useScore;
window.usePerSecond = usePerSecond;
window.usePerSecondHistory = usePerSecondHistory;
window.useOpsHistory = useOpsHistory;
window.useEvents = useEvents;
window.useTraceBatch = useTraceBatch;
window.useTraceBatchSeq = useTraceBatchSeq;
window.useScenario = useScenario;
window.useScenarioEvents = useScenarioEvents;
window.usePaused = usePaused;
window.useTargetRps = useTargetRps;
window.useShape = useShape;
window.usePatternMix = usePatternMix;
window.useYAML = useYAML;
window.useDefaultYaml = useDefaultYaml;
window.useYamlDraft = useYamlDraft;
window.usePolicyDraft = usePolicyDraft;
window.useServerPolicy = useServerPolicy;
window.useDefaultPolicy = useDefaultPolicy;
window.usePolicyHistory = usePolicyHistory;
window.usePolicyHistoryRing = usePolicyHistoryRing;
window.usePolicyHistoryIdx = usePolicyHistoryIdx;
window.useConfigValidate = useConfigValidate;
window.useConfigResult = useConfigResult;
window.usePolicyValidate = usePolicyValidate;
window.usePolicyResult = usePolicyResult;
window.useSparkErr = useSparkErr;
window.useActualRps = useActualRps;
window.useSnapshotReady = useSnapshotReady;
window.useFocusedUpstream = useFocusedUpstream;
