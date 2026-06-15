// @ts-nocheck
/**
 * eRPC hero diagram — SVG markup + animation script.
 *
 * Hand-maintained. The original design prototype HTML has been split into:
 *   - styles/hero-diagram.css (CSS, scoped to .cv-hero-root)
 *   - components/hero-diagram.internals.ts (this file)
 *   - components/HeroDiagram.tsx (the React wrapper)
 *
 * The animation script is verified-working vanilla JS; ts-nocheck keeps TS
 * out of relitigating its types.
 */

export const HERO_SVG_HTML = "<svg viewBox=\"0 0 1200 620\" preserveAspectRatio=\"xMidYMid meet\">\n\n    <defs>\n      <linearGradient id=\"grad-blue\" x1=\"0\" x2=\"1\" y1=\"0\" y2=\"0\">\n        <stop offset=\"0%\"  stop-color=\"#60a5fa\"/>\n        <stop offset=\"100%\" stop-color=\"#3b82f6\"/>\n      </linearGradient>\n    </defs>\n\n    <!-- ════════ FLOW LINES ════════ -->\n\n    <!-- Cache-ON wires-in: client → Cache entry at (264, 170) -->\n    <g id=\"wires-in\" class=\"cache-on-rails\">\n      <path id=\"rail-in-c0\" class=\"flow\" d=\"M170 160 C 200 160, 234 126, 264 126\"/>\n      <path id=\"rail-in-c1\" class=\"flow\" d=\"M170 280 C 220 280, 230 126, 264 126\"/>\n      <path id=\"rail-in-c2\" class=\"flow\" d=\"M170 400 C 220 400, 230 126, 264 126\"/>\n    </g>\n    <!-- Cache-OFF wires-in: client → Multiplex entry at (264, 257), bypassing Cache -->\n    <g id=\"wires-in-off\" class=\"cache-off-rails\">\n      <path id=\"rail-off-c0\" class=\"flow\" d=\"M170 160 C 210 160, 234 229, 264 229\"/>\n      <path id=\"rail-off-c1\" class=\"flow\" d=\"M170 280 C 210 280, 234 229, 264 229\"/>\n      <path id=\"rail-off-c2\" class=\"flow\" d=\"M170 400 C 210 400, 234 229, 264 229\"/>\n    </g>\n\n    <g id=\"rails-internal\">\n      <path id=\"rail-c-m\" class=\"flow\" d=\"M520 168 L 520 192\"/>\n      <path id=\"rail-m-retry\"     class=\"flow\" d=\"M520 266 C 520 295, 326 295, 326 324\"/>\n      <path id=\"rail-m-hedge\"     class=\"flow\" d=\"M520 266 C 520 295, 456 295, 456 324\"/>\n      <path id=\"rail-m-consensus\" class=\"flow\" d=\"M520 266 C 520 295, 586 295, 586 324\"/>\n      <path id=\"rail-m-circuit\"   class=\"flow\" d=\"M520 266 C 520 295, 716 295, 716 324\"/>\n      <path id=\"rail-d-retry\"     class=\"flow\" d=\"M326 404 L 326 420\"/>\n      <path id=\"rail-d-hedge\"     class=\"flow\" d=\"M456 404 L 456 420\"/>\n      <path id=\"rail-d-consensus\" class=\"flow\" d=\"M586 404 L 586 420\"/>\n      <path id=\"rail-d-circuit\"   class=\"flow\" d=\"M716 404 L 716 420\"/>\n      <path id=\"rail-exit\"        class=\"flow exit-rail\" d=\"M326 420 L 800 420\"/>\n    </g>\n\n    <!-- Wires-out: now shorter — upstreams shifted left to x=870 -->\n    <g id=\"wires-out\">\n      <path id=\"rail-out-u0\" class=\"flow\" data-wire=\"0\" d=\"M800 420 C 830 420, 850 120, 870 120\"/>\n      <path id=\"rail-out-u1\" class=\"flow\" data-wire=\"1\" d=\"M800 420 C 830 420, 850 232, 870 232\"/>\n      <path id=\"rail-out-u2\" class=\"flow\" data-wire=\"2\" d=\"M800 420 C 830 420, 850 344, 870 344\"/>\n      <path id=\"rail-out-u3\" class=\"flow\" data-wire=\"3\" d=\"M800 420 C 830 420, 850 456, 870 456\"/>\n    </g>\n\n    <!-- ════════ CLIENTS ════════ -->\n    <g id=\"clients\">\n      <g transform=\"translate(20,120)\">\n        <rect class=\"card-bg\" width=\"150\" height=\"80\" rx=\"10\"/>\n        <g class=\"icon icon-indexer\" transform=\"translate(16,28)\">\n          <ellipse cx=\"12\" cy=\"3\" rx=\"11\" ry=\"3.5\"/>\n          <path d=\"M1 3 v8 c0 1.9 4.9 3.5 11 3.5 s11-1.6 11-3.5 v-8\"/>\n          <path d=\"M1 11 v8 c0 1.9 4.9 3.5 11 3.5 s11-1.6 11-3.5 v-8\"/>\n        </g>\n        <text class=\"t-label\" x=\"50\" y=\"36\">Indexer</text>\n        <text class=\"t-sub\"   x=\"50\" y=\"55\">backfill · realtime</text>\n      </g>\n      <g transform=\"translate(20,240)\">\n        <rect class=\"card-bg\" width=\"150\" height=\"80\" rx=\"10\"/>\n        <g class=\"icon icon-frontend\" transform=\"translate(16,28)\">\n          <rect x=\"0\" y=\"0\" width=\"24\" height=\"20\" rx=\"2.5\"/>\n          <path d=\"M0 6 H24\"/>\n        </g>\n        <text class=\"t-label\" x=\"50\" y=\"36\">Frontend</text>\n        <text class=\"t-sub\"   x=\"50\" y=\"55\">wallet · dapp UI</text>\n      </g>\n      <g transform=\"translate(20,360)\">\n        <rect class=\"card-bg\" width=\"150\" height=\"80\" rx=\"10\"/>\n        <g class=\"icon icon-backend\" transform=\"translate(16,28)\">\n          <rect x=\"0\" y=\"1\" width=\"24\" height=\"6\" rx=\"1.5\"/>\n          <rect x=\"0\" y=\"9\" width=\"24\" height=\"6\" rx=\"1.5\"/>\n          <rect x=\"0\" y=\"17\" width=\"24\" height=\"6\" rx=\"1.5\"/>\n        </g>\n        <text class=\"t-label\" x=\"50\" y=\"36\">Backend</text>\n        <text class=\"t-sub\"   x=\"50\" y=\"55\">API · workers</text>\n      </g>\n    </g>\n\n    <!-- ════════ eRPC CORE ════════ -->\n    <g id=\"core\">\n      <rect class=\"core-bg\" x=\"240\" y=\"60\" width=\"560\" height=\"540\" rx=\"16\"/>\n\n      <!-- CACHE lane -->\n      <g id=\"lane-cache\" class=\"lane-clickable\" data-focus=\"cache\">\n        <rect class=\"lane\" x=\"264\" y=\"84\" width=\"512\" height=\"84\" rx=\"10\"/>\n        <text class=\"t-lane\" x=\"520\" y=\"112\" text-anchor=\"middle\">Cache</text>\n        <text class=\"t-lane-sub\" x=\"520\" y=\"132\" text-anchor=\"middle\">Finality-aware · 50–90% compressed</text>\n      </g>\n\n      <!-- MULTIPLEX lane (title centered) -->\n      <g id=\"lane-multiplex\" class=\"lane-clickable\" data-focus=\"multiplex\">\n        <rect class=\"lane\" x=\"264\" y=\"192\" width=\"512\" height=\"74\" rx=\"10\"/>\n        <text class=\"t-lane\" x=\"520\" y=\"218\" text-anchor=\"middle\">Multiplexer</text>\n        <text class=\"t-lane-sub\" x=\"520\" y=\"238\" text-anchor=\"middle\">Deduplicates in-flight requests</text>\n        <g transform=\"translate(742,222)\" stroke=\"var(--blue)\" stroke-width=\"1.2\" fill=\"none\" opacity=\"0.55\">\n          <path d=\"M0 0 L8 6 L16 0\"/>\n          <path d=\"M8 6 L8 12\"/>\n        </g>\n        <circle id=\"mux-burst\" class=\"burst blue\" cx=\"520\" cy=\"229\" r=\"3\" opacity=\"0\"/>\n      </g>\n\n      <!-- FAILSAFE header (centered, t-lane size) -->\n      <text class=\"t-lane\" x=\"520\" y=\"306\" text-anchor=\"middle\">Failsafe</text>\n\n      <g id=\"fs-row\">\n        <!-- Boxes h=80, pushed down for more space below the FAILSAFE header -->\n        <g class=\"fs-box\" data-focus=\"retry\" data-fs=\"retry\">\n          <rect class=\"fs-bg\" x=\"266\" y=\"324\" width=\"120\" height=\"80\" rx=\"8\"/>\n          <text class=\"fs-name\" x=\"326\" y=\"358\" text-anchor=\"middle\">retry</text>\n          <text class=\"fs-tag\"  x=\"326\" y=\"378\" text-anchor=\"middle\">auto-retry</text>\n        </g>\n        <g class=\"fs-box\" data-focus=\"hedge\" data-fs=\"hedge\">\n          <rect class=\"fs-bg\" x=\"396\" y=\"324\" width=\"120\" height=\"80\" rx=\"8\"/>\n          <text class=\"fs-name\" x=\"456\" y=\"358\" text-anchor=\"middle\">hedge</text>\n          <text class=\"fs-tag\"  x=\"456\" y=\"378\" text-anchor=\"middle\">race upstreams</text>\n        </g>\n        <g class=\"fs-box\" data-focus=\"consensus\" data-fs=\"consensus\">\n          <rect class=\"fs-bg\" x=\"526\" y=\"324\" width=\"120\" height=\"80\" rx=\"8\"/>\n          <text class=\"fs-name\" x=\"586\" y=\"358\" text-anchor=\"middle\">consensus</text>\n          <text class=\"fs-tag\"  x=\"586\" y=\"378\" text-anchor=\"middle\">quorum reads</text>\n        </g>\n        <g class=\"fs-box\" data-focus=\"circuit\" data-fs=\"circuit\">\n          <rect class=\"fs-bg\" x=\"656\" y=\"324\" width=\"120\" height=\"80\" rx=\"8\"/>\n          <text class=\"fs-name\" x=\"716\" y=\"358\" text-anchor=\"middle\">circuit</text>\n          <text class=\"fs-tag\"  x=\"716\" y=\"378\" text-anchor=\"middle\">breaker on failures</text>\n        </g>\n        <g id=\"quorum-badge\" transform=\"translate(596,334)\" opacity=\"0\">\n          <rect x=\"0\" y=\"0\" width=\"40\" height=\"18\" rx=\"9\" fill=\"rgba(251,191,36,0.18)\" stroke=\"rgba(251,191,36,0.75)\"/>\n          <text x=\"20\" y=\"13\" text-anchor=\"middle\" font-family=\"JetBrains Mono, ui-monospace, monospace\" font-size=\"10\" fill=\"var(--amber)\">3/3 ✓</text>\n        </g>\n      </g>\n\n      <!-- ROUTING & SCORING — single horizontal row of 4 score cells -->\n      <g id=\"lane-route\">\n        <text class=\"t-lane\" x=\"282\" y=\"478\">Routing &amp; Scoring</text>\n        <text class=\"t-lane-sub\" x=\"282\" y=\"505\">picks the best upstream · refreshed every 30s</text>\n        <rect class=\"score-box\" x=\"264\" y=\"530\" width=\"512\" height=\"46\" rx=\"8\"/>\n        <g id=\"route-bars\">\n          <g class=\"bar-cell\" data-cell=\"0\" transform=\"translate(278, 542)\">\n            <rect class=\"bar-bg\" width=\"100\" height=\"5\" rx=\"2.5\"/>\n            <rect class=\"bar\" data-bar=\"0\" width=\"0\" height=\"5\" rx=\"2.5\"/>\n            <text class=\"bar-name\" x=\"0\" y=\"20\">alchemy:1</text>\n            <text class=\"bar-score\" data-score=\"0\" x=\"100\" y=\"20\" text-anchor=\"end\">—</text>\n          </g>\n          <g class=\"bar-cell\" data-cell=\"1\" transform=\"translate(406, 542)\">\n            <rect class=\"bar-bg\" width=\"100\" height=\"5\" rx=\"2.5\"/>\n            <rect class=\"bar\" data-bar=\"1\" width=\"0\" height=\"5\" rx=\"2.5\"/>\n            <text class=\"bar-name\" x=\"0\" y=\"20\">drpc:1</text>\n            <text class=\"bar-score\" data-score=\"1\" x=\"100\" y=\"20\" text-anchor=\"end\">—</text>\n          </g>\n          <g class=\"bar-cell\" data-cell=\"2\" transform=\"translate(534, 542)\">\n            <rect class=\"bar-bg\" width=\"100\" height=\"5\" rx=\"2.5\"/>\n            <rect class=\"bar\" data-bar=\"2\" width=\"0\" height=\"5\" rx=\"2.5\"/>\n            <text class=\"bar-name\" x=\"0\" y=\"20\">archive</text>\n            <text class=\"bar-score\" data-score=\"2\" x=\"100\" y=\"20\" text-anchor=\"end\">—</text>\n          </g>\n          <g class=\"bar-cell\" data-cell=\"3\" transform=\"translate(662, 542)\">\n            <rect class=\"bar-bg\" width=\"100\" height=\"5\" rx=\"2.5\"/>\n            <rect class=\"bar\" data-bar=\"3\" width=\"0\" height=\"5\" rx=\"2.5\"/>\n            <text class=\"bar-name\" x=\"0\" y=\"20\">infura:137</text>\n            <text class=\"bar-score\" data-score=\"3\" x=\"100\" y=\"20\" text-anchor=\"end\">—</text>\n          </g>\n        </g>\n      </g>\n    </g>\n\n    <!-- Reset link (no hint copy; the diagram speaks for itself) -->\n    <g id=\"reset-group\" transform=\"translate(1130,48)\">\n      <text class=\"reset-link\" id=\"reset-link\" text-anchor=\"end\">↻  reset</text>\n    </g>\n\n    <!-- ════════ UPSTREAMS (shifted left to x=870, closer to eRPC) ════════ -->\n    <g id=\"upstreams\">\n      <g class=\"up-node\" data-up=\"0\" transform=\"translate(870,80)\">\n        <rect class=\"card-bg\" width=\"260\" height=\"80\" rx=\"10\"/>\n        <circle class=\"dot healthy\" cx=\"18\" cy=\"22\" r=\"4\"/>\n        <text class=\"t-mono\"     x=\"34\"  y=\"26\" data-role=\"name\">alchemy:1</text>\n        <text class=\"t-mono-s\"   x=\"246\" y=\"26\" text-anchor=\"end\" data-role=\"status\">healthy</text>\n        <text class=\"t-mono-dim\" x=\"18\"  y=\"46\">eth-mainnet · us-east</text>\n        <text class=\"t-mono-dim\" x=\"18\"  y=\"64\" data-role=\"latency-text\">p50 32ms · 99.98%</text>\n        <g class=\"slider\" data-up-slider=\"0\" transform=\"translate(180,68)\">\n          <line class=\"slider-track\" x1=\"0\" y1=\"0\" x2=\"60\" y2=\"0\"/>\n          <line class=\"slider-fill\"  x1=\"0\" y1=\"0\" x2=\"6\" y2=\"0\"/>\n          <circle class=\"slider-thumb\" cx=\"6\" cy=\"0\" r=\"5\"/>\n        </g>\n      </g>\n      <g class=\"cordon-toggle\" data-up=\"0\" transform=\"translate(1140,98)\">\n        <rect class=\"ct-bg\" width=\"22\" height=\"44\" rx=\"11\"/>\n        <circle class=\"ct-knob\" cx=\"11\" cy=\"11\" r=\"7\"/>\n      </g>\n      <g class=\"up-node\" data-up=\"1\" transform=\"translate(870,192)\">\n        <rect class=\"card-bg\" width=\"260\" height=\"80\" rx=\"10\"/>\n        <circle class=\"dot healthy\" cx=\"18\" cy=\"22\" r=\"4\"/>\n        <text class=\"t-mono\"     x=\"34\"  y=\"26\" data-role=\"name\">drpc:1</text>\n        <text class=\"t-mono-s\"   x=\"246\" y=\"26\" text-anchor=\"end\" data-role=\"status\">healthy</text>\n        <text class=\"t-mono-dim\" x=\"18\"  y=\"46\">eth-mainnet · eu-west</text>\n        <text class=\"t-mono-dim\" x=\"18\"  y=\"64\" data-role=\"latency-text\">p50 41ms · 99.92%</text>\n        <g class=\"slider\" data-up-slider=\"1\" transform=\"translate(180,68)\">\n          <line class=\"slider-track\" x1=\"0\" y1=\"0\" x2=\"60\" y2=\"0\"/>\n          <line class=\"slider-fill\"  x1=\"0\" y1=\"0\" x2=\"9\" y2=\"0\"/>\n          <circle class=\"slider-thumb\" cx=\"9\" cy=\"0\" r=\"5\"/>\n        </g>\n      </g>\n      <g class=\"cordon-toggle\" data-up=\"1\" transform=\"translate(1140,210)\">\n        <rect class=\"ct-bg\" width=\"22\" height=\"44\" rx=\"11\"/>\n        <circle class=\"ct-knob\" cx=\"11\" cy=\"11\" r=\"7\"/>\n      </g>\n      <g class=\"up-node\" data-up=\"2\" transform=\"translate(870,304)\">\n        <rect class=\"card-bg\" width=\"260\" height=\"80\" rx=\"10\"/>\n        <circle class=\"dot slow\" cx=\"18\" cy=\"22\" r=\"4\"/>\n        <text class=\"t-mono\"     x=\"34\"  y=\"26\" data-role=\"name\">self-hosted-archive</text>\n        <text class=\"t-mono-s\"   x=\"246\" y=\"26\" text-anchor=\"end\" data-role=\"status\" fill=\"var(--amber)\">slow</text>\n        <text class=\"t-mono-dim\" x=\"18\"  y=\"46\">eth-mainnet · in-cluster · archive</text>\n        <text class=\"t-mono-dim\" x=\"18\"  y=\"64\" data-role=\"latency-text\">p50 280ms · 99.6%</text>\n        <g class=\"slider\" data-up-slider=\"2\" transform=\"translate(180,68)\">\n          <line class=\"slider-track\" x1=\"0\" y1=\"0\" x2=\"60\" y2=\"0\"/>\n          <line class=\"slider-fill\"  x1=\"0\" y1=\"0\" x2=\"35\" y2=\"0\"/>\n          <circle class=\"slider-thumb\" cx=\"35\" cy=\"0\" r=\"5\"/>\n        </g>\n      </g>\n      <g class=\"cordon-toggle\" data-up=\"2\" transform=\"translate(1140,322)\">\n        <rect class=\"ct-bg\" width=\"22\" height=\"44\" rx=\"11\"/>\n        <circle class=\"ct-knob\" cx=\"11\" cy=\"11\" r=\"7\"/>\n      </g>\n      <g class=\"up-node\" data-up=\"3\" transform=\"translate(870,416)\">\n        <rect class=\"card-bg\" width=\"260\" height=\"80\" rx=\"10\"/>\n        <circle class=\"dot healthy\" cx=\"18\" cy=\"22\" r=\"4\"/>\n        <text class=\"t-mono\"     x=\"34\"  y=\"26\" data-role=\"name\">infura:137</text>\n        <text class=\"t-mono-s\"   x=\"246\" y=\"26\" text-anchor=\"end\" data-role=\"status\">healthy</text>\n        <text class=\"t-mono-dim\" x=\"18\"  y=\"46\">polygon · us-west</text>\n        <text class=\"t-mono-dim\" x=\"18\"  y=\"64\" data-role=\"latency-text\">p50 64ms · 99.95%</text>\n        <g class=\"slider\" data-up-slider=\"3\" transform=\"translate(180,68)\">\n          <line class=\"slider-track\" x1=\"0\" y1=\"0\" x2=\"60\" y2=\"0\"/>\n          <line class=\"slider-fill\"  x1=\"0\" y1=\"0\" x2=\"13\" y2=\"0\"/>\n          <circle class=\"slider-thumb\" cx=\"13\" cy=\"0\" r=\"5\"/>\n        </g>\n      </g>\n      <g class=\"cordon-toggle\" data-up=\"3\" transform=\"translate(1140,434)\">\n        <rect class=\"ct-bg\" width=\"22\" height=\"44\" rx=\"11\"/>\n        <circle class=\"ct-knob\" cx=\"11\" cy=\"11\" r=\"7\"/>\n      </g>\n    </g>\n\n    <!-- Annotation removed (was at y=582) — dynamic info now lives in Routing & Scoring subtitle -->\n\n    <g id=\"tooltips\"></g>\n    <g id=\"pulses\"></g>\n  </svg>";

/**
 * Mount the animation script against a hero root element. Returns a cleanup
 * that stops the requestAnimationFrame loop on unmount.
 */
export function initHero(root: HTMLElement): () => void {
	let rafId = 0;
	let stopped = false;
	// --- begin prototype script (vanilla JS body) ---
	/* ============================================================================
   eRPC HERO DIAGRAM — orchestrator
   Architecture:
     - Visible <path> rails (curved + dashed flow). Pulses sample them via
       getPointAtLength so dots trace the visible lines exactly.
     - When cache is OFF, a second set of wires-in (going straight to Multi)
       is shown instead. Pulses route accordingly.
   State: window.heroState = { cordoned, cacheOff, focus, upstreams }
   ============================================================================ */

const SVG = root.querySelector('svg');
	const gid = (id) => root.querySelector('#' + id);
const NS  = 'http://www.w3.org/2000/svg';
const RM  = matchMedia('(prefers-reduced-motion: reduce)').matches;

const FAILSAFE_BOXES = {
  retry:     { cx: 326, top: 324, bottom: 404 },
  hedge:     { cx: 456, top: 324, bottom: 404 },
  consensus: { cx: 586, top: 324, bottom: 404 },
  circuit:   { cx: 716, top: 324, bottom: 404 },
};

const CACHE = { entry: {x:264, y:126}, exit: {x:520, y:168}, mid: {x:450, y:126} };
const MULTI = { entry: {x:264, y:229}, top: {x:520, y:192}, exit: {x:520, y:266} };
const EXIT  = { x: 800, y: 420 };

const STATIONS = {
  c0: {x: 170, y: 160}, c1: {x: 170, y: 280}, c2: {x: 170, y: 400},
  u0: {x: 870, y: 120}, u1: {x: 870, y: 232}, u2: {x: 870, y: 344}, u3: {x: 870, y: 456},
};

const CLIENT_COLOR = { c0: 'indexer', c1: 'frontend', c2: 'backend' };

const DEFAULT_LATENCIES = { u0: 32, u1: 41, u2: 280, u3: 64 };
window.heroState = {
  cordoned: new Set(['u3']),
  cacheOff: false,
  focus: null,
  upstreams: {
    u0: { latency: DEFAULT_LATENCIES.u0 },
    u1: { latency: DEFAULT_LATENCIES.u1 },
    u2: { latency: DEFAULT_LATENCIES.u2 },
    u3: { latency: DEFAULT_LATENCIES.u3 },
  },
};
const ALL_UPSTREAMS = ['u0','u1','u2','u3'];

function isCordoned(id) { return window.heroState.cordoned.has(id); }
function latencyOf(id)  { return window.heroState.upstreams[id].latency; }
function scoreOf(id) {
  if (isCordoned(id)) return 0;
  return Math.max(0, Math.round(100 - (latencyOf(id) - 20) * 0.20));
}
function statusOf(id) {
  if (isCordoned(id)) return 'cordoned';
  if (latencyOf(id) > 160) return 'slow';
  return 'healthy';
}
function pickHealthy(except) {
  const excl = new Set([].concat(except || []));
  const cand = ALL_UPSTREAMS.filter(u => !isCordoned(u) && !excl.has(u));
  if (!cand.length) return ALL_UPSTREAMS.find(u => !isCordoned(u)) || 'u0';
  const scored = cand.map(u => ({ u, w: Math.max(2, scoreOf(u)) }));
  const total = scored.reduce((a,b) => a + b.w, 0);
  let r = Math.random() * total;
  for (const s of scored) { r -= s.w; if (r <= 0) return s.u; }
  return scored[0].u;
}
function pickTwoBest(except) {
  const excl = new Set([].concat(except || []));
  return ALL_UPSTREAMS
    .filter(u => !isCordoned(u) && !excl.has(u))
    .sort((a,b) => scoreOf(b) - scoreOf(a))
    .slice(0, 2);
}

const DEFAULT_EVENTS = [
  { t: 0.0,  kind: 'cacheHit',  client: 'c0' },
  { t: 0.7,  kind: 'cacheHit',  client: 'c1' },
  { t: 1.4,  kind: 'upstream',  client: 'c2' },
  { t: 2.3,  kind: 'cacheHit',  client: 'c0' },
  { t: 3.0,  kind: 'multiplex', client: 'c1' },
  { t: 4.5,  kind: 'cacheHit',  client: 'c2' },
  { t: 5.2,  kind: 'hedge',     client: 'c0' },
  { t: 7.4,  kind: 'cacheHit',  client: 'c1' },
  { t: 8.1,  kind: 'consensus', client: 'c2' },
  { t: 10.2, kind: 'retry',     client: 'c0' },
  { t: 11.0, kind: 'cacheHit',  client: 'c1' },
];
const DEFAULT_LOOP = 12.0;

const FOCUS_LOOPS = {
  cache: { len: 8.0, events: [
    { t: 0.0, kind: 'cacheHit', client: 'c0' },
    { t: 1.5, kind: 'cacheHit', client: 'c1' },
    { t: 3.0, kind: 'cacheHit', client: 'c2' },
    { t: 4.7, kind: 'cacheHit', client: 'c0' },
    { t: 6.0, kind: 'cacheHit', client: 'c1' },
  ]},
  multiplex: { len: 8.0, events: [
    { t: 0.5, kind: 'multiplex', client: 'c0' },
    { t: 4.5, kind: 'multiplex', client: 'c1' },
  ]},
  hedge: { len: 9.0, events: [
    { t: 0.5, kind: 'hedge', client: 'c0' },
    { t: 4.7, kind: 'hedge', client: 'c1' },
  ]},
  retry: { len: 9.0, events: [
    { t: 0.5, kind: 'retry', client: 'c0' },
    { t: 5.0, kind: 'retry', client: 'c2' },
  ]},
  consensus: { len: 9.0, events: [
    { t: 0.5, kind: 'consensus', client: 'c2' },
    { t: 5.0, kind: 'consensus', client: 'c1' },
  ]},
  circuit: { len: 10.0, events: [
    { t: 0.3, kind: 'retry',    client: 'c0', _policy: 'circuit' },
    { t: 2.5, kind: 'retry',    client: 'c1', _policy: 'circuit' },
    { t: 4.7, kind: 'retry',    client: 'c2', _policy: 'circuit' },
    { t: 7.0, kind: 'upstream', client: 'c0' },
    { t: 8.3, kind: 'upstream', client: 'c1' },
  ]},
};

const TOOLTIPS = {
  cache: [
    { x: 24,   y: 22, w: 230, text: ["Apps call eRPC like any","RPC — no SDK change."] },
    { x: 420,  y: 22, w: 270, text: ["Cache stores responses by","chain state. Hits in ~4ms."] },
    { x: 870,  y: 22, w: 250, text: ["Upstreams only reached on","cache miss (~27% of traffic)."] },
  ],
  multiplex: [
    { x: 24,   y: 22, w: 240, text: ["Two callers ask for the","same block at almost the","same time."] },
    { x: 420,  y: 22, w: 270, text: ["Multiplex merges them into","one single upstream call."] },
    { x: 870,  y: 22, w: 250, text: ["Upstream sees one request,","both clients get the answer."] },
  ],
  hedge: [
    { x: 24,   y: 22, w: 290, text: ["If the first upstream is","slow, eRPC sends a second","request to a different one."] },
    { x: 870,  y: 22, w: 250, text: ["Whichever responds first","wins. The slower fork is","cancelled."] },
  ],
  retry: [
    { x: 24,   y: 22, w: 290, text: ["When an upstream returns","an error, eRPC re-tries on","a different healthy one."] },
    { x: 870,  y: 22, w: 250, text: ["The client never sees","the transient failure."] },
  ],
  consensus: [
    { x: 24,   y: 22, w: 290, text: ["For high-value reads, eRPC","queries N upstreams and","returns the majority answer."] },
    { x: 870,  y: 22, w: 250, text: ["3/3 ✓ — all agreed.","Forks/reorgs are rejected."] },
  ],
  circuit: [
    { x: 24,   y: 22, w: 290, text: ["After repeated failures, an","upstream is auto-cordoned","for a cooldown period."] },
    { x: 870,  y: 22, w: 250, text: ["Subsequent requests skip","it entirely — no error","bursts to the client."] },
  ],
};

function rewriteEvent(ev) {
  const s = window.heroState;
  let r = { ...ev };
  if (ev.kind === 'cacheHit') return r;
  if (ev.kind === 'upstream' || ev.kind === 'multiplex') r.upstream = ev.upstream || pickHealthy();
  if (ev.kind === 'hedge' && !ev.winners) {
    const two = pickTwoBest();
    r.winners = [two[0] || pickHealthy()];
    r.losers  = [two[1] || pickHealthy(two[0])];
  }
  if (ev.kind === 'consensus' && !ev.upstreams) {
    r.upstreams = ALL_UPSTREAMS.filter(u => !isCordoned(u)).slice(0, 3);
  }
  if (ev.kind === 'retry') {
    if (!ev.failed) r.failed = [...s.cordoned][0] || 'u3';
    if (!ev.retry)  r.retry  = pickHealthy(r.failed);
  }
  if (s.cacheOff && r.kind === 'cacheHit') return { ...r, kind: 'upstream', upstream: pickHealthy() };
  if (r.kind === 'upstream' && isCordoned(r.upstream)) {
    return { ...r, kind: 'retry', failed: r.upstream, retry: pickHealthy(r.upstream) };
  }
  if (r.kind === 'multiplex' && isCordoned(r.upstream)) r.upstream = pickHealthy(r.upstream);
  if (r.kind === 'hedge') {
    r.winners = r.winners.filter(u => !isCordoned(u));
    r.losers  = r.losers.filter(u => !isCordoned(u));
    if (!r.winners.length) return { ...r, kind: 'retry', failed: ev.losers ? ev.losers[0] : 'u3', retry: pickHealthy() };
    if (!r.losers.length)  r.losers = [pickHealthy(r.winners[0])];
  }
  if (r.kind === 'consensus') {
    r.upstreams = r.upstreams.filter(u => !isCordoned(u));
    if (r.upstreams.length < 2) return { ...r, kind: 'upstream', upstream: pickHealthy() };
  }
  if (r.kind === 'retry' && isCordoned(r.retry)) r.retry = pickHealthy(r.failed);
  return r;
}

/* Pre-sample rails into waypoints */
const RAIL_PTS = {};
function sampleRail(id) {
  const el = gid(id);
  if (!el) return [];
  const len = el.getTotalLength();
  const n = Math.max(2, Math.ceil(len / 18));
  const pts = [];
  for (let i = 0; i <= n; i++) {
    const p = el.getPointAtLength((i / n) * len);
    pts.push({ x: p.x, y: p.y });
  }
  return pts;
}
const RAIL_IDS = [
  'rail-in-c0','rail-in-c1','rail-in-c2',
  'rail-off-c0','rail-off-c1','rail-off-c2',
  'rail-c-m',
  'rail-m-retry','rail-m-hedge','rail-m-consensus','rail-m-circuit',
  'rail-d-retry','rail-d-hedge','rail-d-consensus','rail-d-circuit',
  'rail-exit',
  'rail-out-u0','rail-out-u1','rail-out-u2','rail-out-u3',
];
function preSampleRails() { RAIL_IDS.forEach(id => RAIL_PTS[id] = sampleRail(id)); }

function railFwd(id, fromT, dur) {
  const pts = RAIL_PTS[id]; const n = pts.length - 1;
  return pts.map((p, i) => ({ x: p.x, y: p.y, t: fromT + (i / n) * dur }));
}
function railRev(id, fromT, dur) {
  const pts = RAIL_PTS[id].slice().reverse(); const n = pts.length - 1;
  return pts.map((p, i) => ({ x: p.x, y: p.y, t: fromT + (i / n) * dur }));
}

/* ─── Path builders ─── */

function pathCacheHit(client) {
  // Only called when cache is ON
  const out = railFwd('rail-in-c' + client.slice(1), 0, 0.25);
  const tEnterCache = out[out.length-1].t;
  out.push({ x: CACHE.entry.x, y: CACHE.entry.y, t: tEnterCache });
  out.push({ x: CACHE.mid.x,   y: CACHE.mid.y,   t: tEnterCache + 0.18 });
  const tBounce = tEnterCache + 0.18;
  const ret = [
    { x: CACHE.mid.x,   y: CACHE.mid.y,   t: tBounce },
    { x: CACHE.entry.x, y: CACHE.entry.y, t: tBounce + 0.18 },
  ];
  ret.push(...railRev('rail-in-c' + client.slice(1), tBounce + 0.18, 0.25).slice(1));
  return { out, ret, burstT: tBounce, laneHits: [{ id: 'lane-cache', t: tBounce - 0.05 }] };
}

/* Client → ... → e_out through the given failsafe policy box.
   When cacheOff, the wire-in goes straight to Multi entry (skips Cache). */
function pathToEout(client, policy) {
  const cacheOff = window.heroState.cacheOff;
  const wp = cacheOff
    ? railFwd('rail-off-c' + client.slice(1), 0, 0.30)
    : railFwd('rail-in-c'  + client.slice(1), 0, 0.28);
  let t = wp[wp.length-1].t;

  if (cacheOff) {
    // Skip Cache; enter Multi from left edge
    wp.push({ x: MULTI.entry.x, y: MULTI.entry.y, t });
    wp.push({ x: MULTI.exit.x,  y: MULTI.exit.y,  t: t + 0.18 });
    t += 0.18;
  } else {
    // Cache traversal
    wp.push({ x: CACHE.entry.x, y: CACHE.entry.y, t });
    wp.push({ x: CACHE.exit.x,  y: CACHE.exit.y,  t: t + 0.18 });
    t += 0.18;
    wp.push(...railFwd('rail-c-m', t, 0.04).slice(0));
    t += 0.04;
    wp.push({ x: MULTI.top.x,  y: MULTI.top.y,  t });
    wp.push({ x: MULTI.exit.x, y: MULTI.exit.y, t: t + 0.14 });
    t += 0.14;
  }
  // multi → failsafe-box rail
  wp.push(...railFwd('rail-m-' + policy, t, 0.10).slice(1));
  t = wp[wp.length-1].t;
  const box = FAILSAFE_BOXES[policy];
  wp.push({ x: box.cx, y: box.top,    t });
  wp.push({ x: box.cx, y: box.bottom, t: t + 0.16 });
  t += 0.16;
  wp.push(...railFwd('rail-d-' + policy, t, 0.04).slice(1));
  t = wp[wp.length-1].t;
  wp.push({ x: EXIT.x, y: EXIT.y, t: t + 0.20 });
  t += 0.20;
  return { wp, tArrive: t };
}

function pathEoutToClient(client, fromT) {
  const cacheOff = window.heroState.cacheOff;
  const c = STATIONS[client];
  const wp = [];
  wp.push({ x: EXIT.x, y: EXIT.y, t: fromT });
  wp.push({ x: MULTI.exit.x, y: MULTI.exit.y, t: fromT + 0.22 });
  if (cacheOff) {
    wp.push({ x: MULTI.entry.x, y: MULTI.entry.y, t: fromT + 0.36 });
    wp.push(...railRev('rail-off-c' + client.slice(1), fromT + 0.36, 0.30).slice(1));
  } else {
    wp.push({ x: MULTI.top.x,  y: MULTI.top.y,  t: fromT + 0.36 });
    wp.push({ x: CACHE.exit.x, y: CACHE.exit.y, t: fromT + 0.42 });
    wp.push({ x: CACHE.entry.x, y: CACHE.entry.y, t: fromT + 0.56 });
    wp.push(...railRev('rail-in-c' + client.slice(1), fromT + 0.56, 0.25).slice(1));
  }
  return wp;
}

function pathUpstream(client, upstream) {
  const { wp, tArrive } = pathToEout(client, 'retry');
  const out = wp.concat(railFwd('rail-out-' + upstream, tArrive, 0.22).slice(1));
  const tAtU = out[out.length-1].t;
  const ret = railRev('rail-out-' + upstream, tAtU, 0.22)
                .concat(pathEoutToClient(client, tAtU + 0.22).slice(1));
  return { out, ret, laneHits: [
    { id: 'lane-cache', t: 0.30 }, { id: 'lane-multiplex', t: 0.50 },
  ]};
}

function pathHedge(client, winner, loser) {
  const { wp, tArrive } = pathToEout(client, 'hedge');
  const winnerOut = wp.concat(railFwd('rail-out-' + winner, tArrive, 0.22).slice(1));
  const loserOut  = wp.concat(railFwd('rail-out-' + loser,  tArrive + 0.06, 0.24).slice(1));
  const tAtWin = winnerOut[winnerOut.length-1].t;
  const winRet = railRev('rail-out-' + winner, tAtWin, 0.22)
                  .concat(pathEoutToClient(client, tAtWin + 0.22).slice(1));
  return { winnerOut, loserOut, winRet, forkT: tArrive, laneHits: [
    { id: 'lane-cache', t: 0.30 }, { id: 'lane-multiplex', t: 0.50 },
    { id: 'fs-hedge',   t: 0.65 },
  ]};
}

function pathConsensus(client, upstreams) {
  const { wp, tArrive } = pathToEout(client, 'consensus');
  const outs = upstreams.map(u => wp.concat(railFwd('rail-out-' + u, tArrive, 0.24).slice(1)));
  const tAtUps = outs[0][outs[0].length-1].t;
  const rets = upstreams.map(u => railRev('rail-out-' + u, tAtUps, 0.22));
  const merged = pathEoutToClient(client, tAtUps + 0.22);
  return { outs, rets, merged, forkT: tArrive, laneHits: [
    { id: 'lane-cache', t: 0.30 }, { id: 'lane-multiplex', t: 0.50 },
    { id: 'fs-consensus', t: 0.65 },
  ]};
}

function pathMultiplex(client, upstream) {
  // Multi-client multiplex: multiple clients send the same request in parallel.
  // They all converge inside the Multiplex lane, become a SINGLE upstream call,
  // and the response is then SPLIT back to each originating client.
  const cacheOff = window.heroState.cacheOff;
  const clients = ['c0', 'c1', 'c2'];          // all three clients participate
  const policy  = 'retry';
  const mergeT  = cacheOff ? 0.72 : 0.86;      // when all client pulses arrive at the merge

  // One outbound pulse per client → ends at Multi exit (merge point)
  const outs = clients.map((cl, i) => {
    const c = STATIONS[cl];
    const railIn = cacheOff ? 'rail-off-c' + cl.slice(1) : 'rail-in-c' + cl.slice(1);
    const start = i * 0.10;                     // small stagger
    const wp = railFwd(railIn, start, 0.28);
    let t = wp[wp.length-1].t;
    if (cacheOff) {
      wp.push({ x: MULTI.entry.x, y: MULTI.entry.y, t });
      wp.push({ x: MULTI.exit.x,  y: MULTI.exit.y,  t: mergeT });
    } else {
      wp.push({ x: CACHE.entry.x, y: CACHE.entry.y, t });
      wp.push({ x: CACHE.exit.x,  y: CACHE.exit.y,  t: t + 0.16 });
      wp.push({ x: MULTI.top.x,   y: MULTI.top.y,   t: t + 0.22 });
      wp.push({ x: MULTI.exit.x,  y: MULTI.exit.y,  t: mergeT });
    }
    return wp;
  });

  // Continuation: ONE merged pulse goes from Multi exit → retry box → exit → upstream
  const cont = [{ x: MULTI.exit.x, y: MULTI.exit.y, t: mergeT }];
  cont.push(...railFwd('rail-m-' + policy, mergeT, 0.10).slice(1));
  let t = cont[cont.length-1].t;
  const box = FAILSAFE_BOXES[policy];
  cont.push({ x: box.cx, y: box.top,    t });
  cont.push({ x: box.cx, y: box.bottom, t: t + 0.16 });
  t += 0.16;
  cont.push(...railFwd('rail-d-' + policy, t, 0.04).slice(1));
  t = cont[cont.length-1].t;
  cont.push({ x: EXIT.x, y: EXIT.y, t: t + 0.20 });
  t += 0.20;
  cont.push(...railFwd('rail-out-' + upstream, t, 0.22).slice(1));
  const tArr = cont[cont.length-1].t;

  // Single return pulse from upstream → back to Multi exit (split point)
  const splitT = tArr + 0.22 + 0.36;  // arrive at the split point
  const mergedRet = railRev('rail-out-' + upstream, tArr, 0.22);
  mergedRet.push({ x: EXIT.x, y: EXIT.y, t: mergedRet[mergedRet.length-1].t + 0.04 });
  mergedRet.push({ x: MULTI.exit.x, y: MULTI.exit.y, t: splitT });

  // SPLIT: from Multi exit, N return pulses each go back to their client
  const rets = clients.map(cl => {
    const c = STATIONS[cl];
    const wp = [{ x: MULTI.exit.x, y: MULTI.exit.y, t: splitT }];
    if (cacheOff) {
      wp.push({ x: MULTI.entry.x, y: MULTI.entry.y, t: splitT + 0.18 });
      wp.push(...railRev('rail-off-c' + cl.slice(1), splitT + 0.18, 0.30).slice(1));
    } else {
      wp.push({ x: MULTI.top.x,   y: MULTI.top.y,   t: splitT + 0.14 });
      wp.push({ x: CACHE.exit.x,  y: CACHE.exit.y,  t: splitT + 0.22 });
      wp.push({ x: CACHE.entry.x, y: CACHE.entry.y, t: splitT + 0.34 });
      wp.push(...railRev('rail-in-c' + cl.slice(1), splitT + 0.34, 0.26).slice(1));
    }
    return wp;
  });

  return { outs, cont, mergedRet, rets, mergeT, laneHits: [
    cacheOff ? { id: 'lane-multiplex', t: 0.55 } : { id: 'lane-cache', t: 0.40 },
    { id: 'lane-multiplex', t: 0.65 },
  ]};
}

function pathRetry(client, failed, retry, policyId) {
  const policy = policyId || 'retry';
  const { wp, tArrive } = pathToEout(client, policy);
  const pre = wp.concat(railFwd('rail-out-' + failed, tArrive, 0.22).slice(1));
  const failArr = pre[pre.length-1].t;
  const failRet = railRev('rail-out-' + failed, failArr, 0.20);
  const retryT  = failRet[failRet.length-1].t;
  const retryOut = railFwd('rail-out-' + retry, retryT, 0.22);
  const retryArr = retryOut[retryOut.length-1].t;
  const retryRet = railRev('rail-out-' + retry, retryArr, 0.22)
                     .concat(pathEoutToClient(client, retryArr + 0.22).slice(1));
  return { pre, failRet, retryOut, retryRet, exitT: tArrive, laneHits: [
    { id: 'lane-cache', t: 0.30 }, { id: 'lane-multiplex', t: 0.50 },
    { id: 'fs-' + policy, t: 0.65 },
  ]};
}

/* ─── Pulse engine ─── */
const pulseLayer = gid('pulses');
const pulses = [];
function spawn(waypoints, color, startOffset, opts = {}) {
  const el = document.createElementNS(NS, 'circle');
  el.setAttribute('r', opts.r || 5);
  el.setAttribute('class', 'pulse ' + color);
  el.style.opacity = '0';
  pulseLayer.appendChild(el);
  pulses.push({ el, waypoints, color, startOffset, fadeOutEnd: !!opts.fadeOutEnd, throb: !!opts.throb, baseR: opts.r || 5 });
}
function lerp(a, b, k) { return a + (b - a) * k; }
function positionAt(wp, t) {
  if (t <= wp[0].t) return { x: wp[0].x, y: wp[0].y };
  const last = wp[wp.length - 1];
  if (t >= last.t) return { x: last.x, y: last.y };
  for (let i = 0; i < wp.length - 1; i++) {
    const a = wp[i], b = wp[i+1];
    if (t >= a.t && t <= b.t) {
      const dur = (b.t - a.t) || 1;
      const k = (t - a.t) / dur;
      return { x: lerp(a.x, b.x, k), y: lerp(a.y, b.y, k) };
    }
  }
  return { x: last.x, y: last.y };
}

const activations = {};
function activate(id, color, untilLoopT) {
  if (!activations[id] || activations[id].until < untilLoopT) {
    activations[id] = { until: untilLoopT, color };
  }
}
const LANE_IDS = ['lane-cache', 'lane-multiplex'];
const FS_IDS   = ['retry','hedge','consensus','circuit'];
function renderActivations(loopT) {
  LANE_IDS.forEach(id => {
    const rect = root.querySelector(`#${id} .lane`);
    if (!rect) return;
    const a = activations[id];
    const cls = ['lane'];
    if (a && loopT < a.until) cls.push('active-' + a.color);
    rect.setAttribute('class', cls.join(' '));
  });
  FS_IDS.forEach(p => {
    const g = root.querySelector(`.fs-box[data-fs="${p}"]`);
    if (!g) return;
    const a = activations['fs-' + p];
    g.classList.toggle('active', !!(a && loopT < a.until));
  });
}

const cacheBursts = [];
const muxBursts   = [];

function materializeEvent(ev) {
  const offs = ev.t;
  const cc = CLIENT_COLOR[ev.client];
  const hit = (lst, color) => lst.forEach(h => activate(h.id, color, offs + h.t + 0.55));

  if (ev.kind === 'cacheHit') {
    const p = pathCacheHit(ev.client);
    spawn(p.out, cc, offs, { throb: true });
    spawn(p.ret, cc, offs, { throb: true });
    activate('lane-cache', 'green', offs + p.laneHits[0].t + 0.55);
    cacheBursts.push(offs + p.burstT);
    return;
  }
  if (ev.kind === 'upstream') {
    const p = pathUpstream(ev.client, ev.upstream);
    spawn(p.out, cc, offs); spawn(p.ret, cc, offs);
    hit(p.laneHits, 'blue');
    return;
  }
  if (ev.kind === 'hedge') {
    const p = pathHedge(ev.client, ev.winners[0], ev.losers[0]);
    spawn(p.winnerOut, cc, offs);
    spawn(p.loserOut,  cc, offs, { fadeOutEnd: true });
    spawn(p.winRet, cc, offs);
    hit(p.laneHits, 'amber');
    return;
  }
  if (ev.kind === 'consensus') {
    const p = pathConsensus(ev.client, ev.upstreams);
    p.outs.forEach(o => spawn(o, cc, offs));
    p.rets.forEach(o => spawn(o, cc, offs, { fadeOutEnd: true }));
    spawn(p.merged, cc, offs);
    hit(p.laneHits, 'amber');
    showQuorum(offs + p.forkT + 0.10, offs + p.forkT + 0.95);
    return;
  }
  if (ev.kind === 'multiplex') {
    const p = pathMultiplex(ev.client, ev.upstream);
    // Each client sends in its own colour
    ['c0','c1','c2'].forEach((cl, i) => spawn(p.outs[i], CLIENT_COLOR[cl], offs));
    // Merged single call to/from upstream is shown as blue (system-level)
    spawn(p.cont,      'blue', offs);
    spawn(p.mergedRet, 'blue', offs);
    // Split returns: each client gets back its own colored response
    ['c0','c1','c2'].forEach((cl, i) => spawn(p.rets[i], CLIENT_COLOR[cl], offs));
    hit(p.laneHits, 'blue');
    muxBursts.push(offs + p.mergeT);
    return;
  }
  if (ev.kind === 'retry') {
    const p = pathRetry(ev.client, ev.failed, ev.retry, ev._policy);
    spawn(p.pre, cc, offs);
    spawn(p.failRet, 'crimson', offs, { fadeOutEnd: true });
    spawn(p.retryOut, cc, offs);
    spawn(p.retryRet, cc, offs);
    hit(p.laneHits, 'blue');
    return;
  }
}

const quorumEl = gid('quorum-badge');
let quorumShow = { start: -1, end: -1 };
function showQuorum(start, end) { quorumShow = { start, end }; }
function renderQuorum(loopT) {
  quorumEl.setAttribute('opacity', (loopT >= quorumShow.start && loopT <= quorumShow.end) ? '1' : '0');
}

/* Bursts + cache hit indicator removed; the moving green pulse itself throbs */
const muxBurstEl   = gid('mux-burst');
function renderBurst(el, times, loopT, baseR, maxR, dur) {
  let active = -1;
  for (const t of times) { const dt = loopT - t; if (dt >= 0 && dt < dur) { active = dt; break; } }
  if (active >= 0) {
    const p = active / dur;
    el.setAttribute('r', (baseR + (maxR - baseR) * p).toFixed(2));
    el.setAttribute('opacity', (0.85 * (1 - p)).toFixed(2));
  } else el.setAttribute('opacity', '0');
}

const bars = [...root.querySelectorAll('#route-bars .bar')];
const barCells = [...root.querySelectorAll('#route-bars .bar-cell')];
const barScores = [...root.querySelectorAll('#route-bars .bar-score')];
const BAR_MAX_WIDTH = 100;
function renderBars() {
  for (let i = 0; i < bars.length; i++) {
    const id = ALL_UPSTREAMS[i];
    const cordoned = isCordoned(id);
    const slow = statusOf(id) === 'slow';
    const s  = scoreOf(id);
    bars[i].setAttribute('class', cordoned ? 'bar cordoned' : (slow ? 'bar slow' : 'bar'));
    const w = Math.max(0, Math.min(BAR_MAX_WIDTH, s));
    bars[i].setAttribute('width', w.toFixed(1));
    if (barCells[i]) barCells[i].classList.toggle('cordoned', cordoned);
    if (barScores[i]) barScores[i].textContent = cordoned ? 'down' : String(Math.round(s));
  }
}

/* Route subtitle (replaces the old bottom annotation — shows dynamic status) */
const DEFAULT_SUBTITLE = 'picks the best upstream · refreshed every 30s';
const routeSubtitle = root.querySelector('#lane-route .t-lane-sub');
function renderRouteSubtitle() {
  const s = window.heroState;
  let msg = DEFAULT_SUBTITLE;
  if (s.cacheOff) msg = 'cache disabled · all traffic to upstreams';
  else if (s.cordoned.size > 0) {
    const word = s.cordoned.size === 1 ? 'upstream' : 'upstreams';
    msg = `${s.cordoned.size} ${word} cordoned · auto-failover active`;
  }
  if (routeSubtitle.textContent !== msg) routeSubtitle.textContent = msg;
}

function activeSchedule() {
  if (window.heroState.focus && FOCUS_LOOPS[window.heroState.focus]) return FOCUS_LOOPS[window.heroState.focus];
  return { len: DEFAULT_LOOP, events: DEFAULT_EVENTS };
}
function clearPulses() {
  pulses.forEach(p => p.el.remove());
  pulses.length = 0;
  cacheBursts.length = 0;
  muxBursts.length = 0;
  Object.keys(activations).forEach(k => delete activations[k]);
  quorumShow = { start: -1, end: -1 };
}
function buildLoop() {
  clearPulses();
  activeSchedule().events.map(rewriteEvent).forEach(materializeEvent);
}

/* Pulse speed: faster in normal mode, slower in focus (per user request) */
const SPEED_NORMAL = 1.15;
// 40% slower in focus mode so viewers can follow each step.
const SPEED_FOCUS  = SPEED_NORMAL * 0.6;
function currentSpeed() { return window.heroState.focus ? SPEED_FOCUS : SPEED_NORMAL; }

let t0 = performance.now() / 1000;
let lastLoop = 0;
function tick() {
  const now = performance.now() / 1000;
  const elapsed = now - t0;
  const sched = activeSchedule();
  // Stretch the real-time loop length by 1/speed so events near the end of
  // the schedule still get enough wall-clock time to complete at the slower
  // rate. Without this, slow mode would cut pulses off mid-flight and read
  // as patchy / stuttery. `loopT` is then scaled back to "schedule time"
  // (0..sched.len) so pulse waypoints don't need rescaling.
  const speed = currentSpeed();
  const effectiveLen = sched.len / speed;
  const loopT = (elapsed % effectiveLen) * speed;
  const loopIdx = Math.floor(elapsed / effectiveLen);
  if (loopIdx !== lastLoop) { buildLoop(); lastLoop = loopIdx; }
  for (let i = 0; i < pulses.length; i++) {
    const p = pulses[i];
    const t = loopT - p.startOffset;
    const wp = p.waypoints;
    if (t < wp[0].t - 0.02) { p.el.style.opacity = '0'; continue; }
    const last = wp[wp.length - 1];
    if (t > last.t + 0.4) { p.el.style.opacity = '0'; continue; }
    const pos = positionAt(wp, t);
    p.el.setAttribute('cx', pos.x.toFixed(2));
    p.el.setAttribute('cy', pos.y.toFixed(2));
    if (p.throb) {
      const r = p.baseR + 2.0 * Math.abs(Math.sin(t * 11));
      p.el.setAttribute('r', r.toFixed(2));
    }
    let op = 1;
    if (t < wp[0].t)     op = Math.max(0, 1 - (wp[0].t - t) / 0.10);
    else if (t > last.t) op = p.fadeOutEnd ? Math.max(0, 1 - (t - last.t) / 0.4) : Math.max(0, 1 - (t - last.t) / 0.10);
    if (p.fadeOutEnd && t > last.t - 0.25) op *= Math.max(0.15, 1 - (t - (last.t - 0.25)) / 0.4);
    p.el.style.opacity = op.toFixed(2);
  }
  renderActivations(loopT);
  renderQuorum(loopT);
  renderBurst(muxBurstEl,   muxBursts,   loopT, 4, 30, 0.55);
  renderBars();
  renderRouteSubtitle();
  rafId = requestAnimationFrame(tick);
}

/* ─── Visual sync ─── */
const upNodes = [...root.querySelectorAll('.up-node')];
const wireOutEls = [...root.querySelectorAll('#wires-out .flow')];
const resetGroup  = gid('reset-group');

function syncUpstreamVisuals() {
  upNodes.forEach((el, i) => {
    const id = 'u' + i;
    const cord = isCordoned(id);
    el.classList.toggle('cordoned', cord);
    const statusText = el.querySelector('[data-role="status"]');
    if (cord) { statusText.textContent = 'cordoned'; statusText.setAttribute('fill', 'var(--crimson)'); }
    else if (latencyOf(id) > 160) { statusText.textContent = 'slow'; statusText.setAttribute('fill', 'var(--amber)'); }
    else { statusText.textContent = 'healthy'; statusText.removeAttribute('fill'); }
    const latText = el.querySelector('[data-role="latency-text"]');
    if (latText) {
      const errPct = Math.max(0.01, Math.min(2.5, (latencyOf(id) - 20) / 200)).toFixed(2);
      latText.textContent = `p50 ${latencyOf(id)|0}ms · ${(100 - errPct).toFixed(2)}%`;
    }
    const slider = el.querySelector('.slider');
    if (slider) {
      const x = latencyToTrackX(latencyOf(id));
      slider.querySelector('.slider-thumb').setAttribute('cx', x);
      slider.querySelector('.slider-fill').setAttribute('x2', x);
    }
    const wire = wireOutEls[i];
    wire.classList.toggle('cordoned', cord);
    wire.classList.toggle('slow',     !cord && latencyOf(id) > 160);
    // Sync cordon-toggle widget state
    const ctEl = root.querySelector(`.cordon-toggle[data-up="${i}"]`);
    if (ctEl) ctEl.classList.toggle('cordoned', cord);
  });
  const isDefault = (
    window.heroState.cordoned.size === 1 && window.heroState.cordoned.has('u3') &&
    !window.heroState.cacheOff && !window.heroState.focus &&
    ALL_UPSTREAMS.every(u => latencyOf(u) === DEFAULT_LATENCIES[u])
  );
  resetGroup.classList.toggle('show', !isDefault);
}
function syncCacheVisuals() {
  // The cache toggle UI was removed from the topbar; cache is always on.
  // Kept as a function (other code calls it) but now only ensures the SVG
  // is not stuck in the cache-off state.
  SVG.classList.toggle('cache-off', !!window.heroState.cacheOff);
  const cacheLane = root.querySelector('#lane-cache .lane');
  if (cacheLane) cacheLane.style.opacity = window.heroState.cacheOff ? '0.35' : '';
  syncUpstreamVisuals();
}

const tooltipLayer = gid('tooltips');
function setFocus(name) {
  if (window.heroState.focus === name) name = null;
  window.heroState.focus = name;
  t0 = performance.now() / 1000;
  lastLoop = 0;
  syncFocusVisuals();
  buildLoop();
}
function syncFocusVisuals() {
  const f = window.heroState.focus;
  SVG.classList.toggle('focused', !!f);
  root.querySelectorAll('.focus-target').forEach(el => el.classList.remove('focus-target'));
  if (f) {
    if (f === 'cache')     gid('lane-cache').classList.add('focus-target');
    if (f === 'multiplex') gid('lane-multiplex').classList.add('focus-target');
    if (['retry','hedge','consensus','circuit'].includes(f)) {
      const b = root.querySelector(`.fs-box[data-focus="${f}"]`);
      if (b) b.classList.add('focus-target');
    }
  }
  renderTooltips();
  syncUpstreamVisuals();
}
function renderTooltips() {
  tooltipLayer.innerHTML = '';
  const f = window.heroState.focus;
  if (!f || !TOOLTIPS[f]) return;
  const lineH = 18;
  TOOLTIPS[f].forEach((tt, i) => {
    const g = document.createElementNS(NS, 'g');
    g.setAttribute('class', 'tooltip show');
    g.setAttribute('transform', `translate(${tt.x},${tt.y})`);
    const h = 18 + tt.text.length * lineH;
    const rect = document.createElementNS(NS, 'rect');
    rect.setAttribute('class', 'tt-bg');
    rect.setAttribute('x', '0'); rect.setAttribute('y', '0');
    rect.setAttribute('width', tt.w); rect.setAttribute('height', h);
    rect.setAttribute('rx', '12');
    g.appendChild(rect);
    const stepG = document.createElementNS(NS, 'g');
    stepG.setAttribute('class', 'tt-step');
    const c = document.createElementNS(NS, 'circle');
    c.setAttribute('r', '17'); c.setAttribute('cx', '0'); c.setAttribute('cy', '0');
    stepG.appendChild(c);
    const num = document.createElementNS(NS, 'text');
    num.setAttribute('x', '0'); num.setAttribute('y', '6');
    num.textContent = String(i + 1);
    stepG.appendChild(num);
    g.appendChild(stepG);
    tt.text.forEach((line, li) => {
      const txt = document.createElementNS(NS, 'text');
      txt.setAttribute('class', 'tt-text');
      txt.setAttribute('x', li === 0 ? '24' : '16');
      txt.setAttribute('y', String(24 + li * lineH));
      txt.textContent = line;
      g.appendChild(txt);
    });
    tooltipLayer.appendChild(g);
  });
}

/* Slider drag */
const TRACK_W = 60;
const LAT_MIN = 20, LAT_MAX = 500;
const SLIDER_ABS_X = 870 + 180;   // upstream card at x=870; slider local x=180
function latencyToTrackX(lat) { return ((Math.min(LAT_MAX, Math.max(LAT_MIN, lat)) - LAT_MIN) / (LAT_MAX - LAT_MIN)) * TRACK_W; }
function trackXToLatency(x) { const c = Math.max(0, Math.min(TRACK_W, x)); return Math.round(LAT_MIN + (c / TRACK_W) * (LAT_MAX - LAT_MIN)); }
function clientToSvgX(clientX) {
  const pt = SVG.createSVGPoint(); pt.x = clientX; pt.y = 0;
  return pt.matrixTransform(SVG.getScreenCTM().inverse()).x;
}
function attachSlider(slider, idx) {
  const id = 'u' + idx;
  const thumb = slider.querySelector('.slider-thumb');
  const track = slider.querySelector('.slider-track');
  const fill  = slider.querySelector('.slider-fill');
  function setFromClientX(cx) {
    const localX = clientToSvgX(cx) - SLIDER_ABS_X;
    const lat = trackXToLatency(localX);
    window.heroState.upstreams[id].latency = lat;
    const x = latencyToTrackX(lat);
    thumb.setAttribute('cx', x);
    fill.setAttribute('x2', x);
    syncUpstreamVisuals();
  }
  function down(e) {
    if (isCordoned(id)) return;
    e.stopPropagation(); e.preventDefault();
    setFromClientX(e.clientX);
    function move(ev) { ev.stopPropagation(); setFromClientX(ev.clientX); }
    function up() {
      window.removeEventListener('pointermove', move);
      window.removeEventListener('pointerup', up);
      window.removeEventListener('pointercancel', up);
      buildLoop();
    }
    window.addEventListener('pointermove', move);
    window.addEventListener('pointerup', up);
    window.addEventListener('pointercancel', up);
  }
  thumb.addEventListener('pointerdown', down);
  track.addEventListener('pointerdown', down);
  fill .addEventListener('pointerdown', down);
}

function toggleUpstream(idx) {
  const id = 'u' + idx;
  if (window.heroState.cordoned.has(id)) window.heroState.cordoned.delete(id);
  else                                    window.heroState.cordoned.add(id);
  syncUpstreamVisuals();
  buildLoop();
}
function toggleCache() {
  window.heroState.cacheOff = !window.heroState.cacheOff;
  syncCacheVisuals();
  buildLoop();
}
function resetAll() {
  window.heroState.cordoned = new Set(['u3']);
  window.heroState.cacheOff = false;
  window.heroState.focus = null;
  ALL_UPSTREAMS.forEach(u => window.heroState.upstreams[u].latency = DEFAULT_LATENCIES[u]);
  t0 = performance.now() / 1000; lastLoop = 0;
  syncCacheVisuals(); syncFocusVisuals(); syncUpstreamVisuals();
  buildLoop();
}

upNodes.forEach((el, i) => {
  // Cordoning now happens via the dedicated cordon-toggle widget, not card clicks.
  // We still keep slider interactions inside the card.
});
root.querySelectorAll('.cordon-toggle').forEach(el => {
  el.addEventListener('click', (e) => {
    e.stopPropagation();
    toggleUpstream(parseInt(el.getAttribute('data-up'), 10));
  });
});
root.querySelectorAll('.slider').forEach((s) => attachSlider(s, parseInt(s.getAttribute('data-up-slider'), 10)));
gid('reset-link').addEventListener('click', resetAll);
root.querySelectorAll('.lane-clickable').forEach(g => g.addEventListener('click', () => setFocus(g.getAttribute('data-focus'))));
root.querySelectorAll('.fs-box').forEach(g => g.addEventListener('click', (e) => { e.stopPropagation(); setFocus(g.getAttribute('data-focus')); }));
window.addEventListener('keydown', (e) => { if (e.key === 'Escape' && window.heroState.focus) setFocus(null); });

/* Boot */
preSampleRails();
syncUpstreamVisuals();
syncCacheVisuals();
syncFocusVisuals();
buildLoop();
rafId = rafId = requestAnimationFrame(tick);
	// --- end prototype script ---
	return () => {
		stopped = true;
		if (rafId) cancelAnimationFrame(rafId);
	};
}
