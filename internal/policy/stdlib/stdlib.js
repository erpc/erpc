// Selection-policy std-lib.
//
// All chainable methods are installed on Array.prototype within this sobek
// runtime. They are pure JS where possible; cross-tick state is read from
// the per-tick global `__policyCtx` that the engine sets before running
// the eval function.
//
// Bound by `internal/policy/stdlib/install.go`. See spec §4 for the
// reference manual.

(function () {
  const proto = Array.prototype;
  const _set = Object.defineProperty;
  function define(name, fn) {
    if (proto[name]) return; // idempotent: a re-installed primer must not throw
    _set(proto, name, { value: fn, enumerable: false, writable: false, configurable: false });
  }

  // ─── small helpers ──────────────────────────────────────────────────────

  // toIDs maps an upstream array to its IDs.
  function toIDs(arr) {
    const out = new Array(arr.length);
    for (let i = 0; i < arr.length; i++) out[i] = arr[i].id;
    return out;
  }

  // globMatch supports '*', '?', and '!' prefix for negation.
  function globMatch(pattern, value) {
    if (pattern == null) return true;
    if (pattern === '*' || pattern === value) return true;
    let neg = false;
    if (pattern.startsWith('!')) { neg = true; pattern = pattern.slice(1); }
    const re = new RegExp(
      '^' + pattern.replace(/[.+^${}()|[\]\\]/g, '\\$&').replace(/\*/g, '.*').replace(/\?/g, '.') + '$'
    );
    const m = re.test(value);
    return neg ? !m : m;
  }

  // matchAny tests value against a pattern, an array of patterns (OR), or null.
  function matchAny(pat, value) {
    if (pat == null) return true;
    if (Array.isArray(pat)) {
      for (const p of pat) if (globMatch(p, value)) return true;
      return false;
    }
    return globMatch(pat, value);
  }

  function annotate(u, note) {
    // The Go bridge sometimes returns array-likes that don't support
    // .push (e.g. value types vs pointer types). Best-effort only —
    // annotations are diagnostic.
    try {
      if (!u.annotations) u.annotations = [];
      if (typeof u.annotations.push === 'function') {
        u.annotations.push(note);
      }
    } catch (_) {}
  }

  // ─── 4.2 Identity & label selection ─────────────────────────────────────

  // Tags model. Every upstream carries a `tags: string[]` array of
  // user-applied labels. Convention is `<dimension>:<value>` so a single
  // upstream can carry orthogonal labels:
  //
  //   tags: [ 'tier:main', 'region:us-east', 'sequencer:op-base' ]
  //
  // `byTag` / `excludeTag` accept a glob pattern OR an array of patterns.
  //
  // Positive pattern (`tier:main`, `region:us-*`): matches if ANY tag
  // on the upstream matches the pattern. Array form is OR — match
  // succeeds if any pattern hits any tag.
  //
  // Negated pattern (`!tier:fallback`): matches if NO tag on the
  // upstream matches the un-negated pattern. Reads as "upstream does
  // NOT have tier:fallback". Array form is AND on the negation —
  // `['!tier:fallback', '!tier:dev']` matches if NEITHER tier:fallback
  // NOR tier:dev appears.
  //
  // Glob characters supported: `*`, `?`. The `!` prefix is consumed
  // here; it never makes it down into globMatch.
  function hasMatchingTag(u, pat) {
    const tags = u.tags || [];
    // Array: positives are OR'd, negations are AND'd, upstream matches iff
    //   (no positives OR at least one positive matches) AND (all negations hold)
    if (Array.isArray(pat)) {
      let hasPositive = false;
      let anyPositiveMatches = false;
      let allNegationsHold = true;
      for (const p of pat) {
        if (typeof p === 'string' && p.charAt(0) === '!') {
          if (!hasMatchingTag(u, p)) allNegationsHold = false;
        } else {
          hasPositive = true;
          if (hasMatchingTag(u, p)) anyPositiveMatches = true;
        }
      }
      return allNegationsHold && (!hasPositive || anyPositiveMatches);
    }
    // Negated string pattern: matches if NO tag matches the un-negated form.
    if (typeof pat === 'string' && pat.charAt(0) === '!') {
      const positive = pat.slice(1);
      for (const t of tags) if (globMatch(positive, t)) return false;
      return true;
    }
    // Positive string pattern: matches if ANY tag matches.
    for (const t of tags) if (globMatch(pat, t)) return true;
    return false;
  }

  define('byId', function (id) { return this.filter(u => matchAny(id, u.id)); });
  define('excludeId', function (id) { return this.filter(u => !matchAny(id, u.id)); });
  define('byTag', function (pat) { return this.filter(u => hasMatchingTag(u, pat)); });
  define('excludeTag', function (pat) { return this.filter(u => !hasMatchingTag(u, pat)); });
  define('byVendor', function (v) { return this.filter(u => matchAny(v, u.vendor)); });
  define('excludeVendor', function (v) { return this.filter(u => !matchAny(v, u.vendor)); });
  define('byType', function (t) { return this.filter(u => matchAny(t, u.type)); });

  define('where', function (f) {
    return this.filter(u => {
      if (f.id != null && !matchAny(f.id, u.id)) return false;
      if (f.tag != null && !hasMatchingTag(u, f.tag)) return false;
      if (f.vendor != null && !matchAny(f.vendor, u.vendor)) return false;
      if (f.type != null && !matchAny(f.type, u.type)) return false;
      return true;
    });
  });
  define('whereNot', function (f) {
    return this.filter(u => {
      if (f.id != null && matchAny(f.id, u.id)) return false;
      if (f.tag != null && hasMatchingTag(u, f.tag)) return false;
      if (f.vendor != null && matchAny(f.vendor, u.vendor)) return false;
      if (f.type != null && matchAny(f.type, u.type)) return false;
      return true;
    });
  });

  // ─── 4.3 Health filters ──────────────────────────────────────────────────

  define('removeByErrorRate', function (max) {
    return this.filter(u => {
      if (u.metrics.errorRate <= max) return true;
      annotate(u, 'removed:errorRate=' + u.metrics.errorRate.toFixed(3));
      return false;
    });
  });
  define('removeByThrottling', function (max) {
    return this.filter(u => u.metrics.throttledRate <= max);
  });
  define('removeByMisbehavior', function (max) {
    return this.filter(u => u.metrics.misbehaviorRate <= max);
  });
  define('removeByLag', function (opts) {
    const bh = (opts && opts.blockHead != null) ? opts.blockHead : Infinity;
    const fz = (opts && opts.finalization != null) ? opts.finalization : Infinity;
    return this.filter(u => u.metrics.blockHeadLag < bh && u.metrics.finalizationLag < fz);
  });
  define('removeByMinRequests', function (min) {
    return this.filter(u => u.metrics.requestsTotal >= min);
  });
  define('removeCordoned', function () {
    return this.filter(u => !u.metrics.cordonedReason);
  });
  define('removeByLatency', function (opts) {
    return this.filter(u => {
      const m = u.metrics;
      if (opts.p50Ms != null && m.p50ResponseSeconds * 1000 > opts.p50Ms) return false;
      if (opts.p70Ms != null && m.p70ResponseSeconds * 1000 > opts.p70Ms) return false;
      if (opts.p90Ms != null && m.p90ResponseSeconds * 1000 > opts.p90Ms) return false;
      if (opts.p95Ms != null && m.p95ResponseSeconds * 1000 > opts.p95Ms) return false;
      if (opts.p99Ms != null && m.p99ResponseSeconds * 1000 > opts.p99Ms) return false;
      return true;
    });
  });
  define('keepHealthy', function (opts) {
    opts = opts || {};
    const maxErr = opts.maxErrorRate != null ? opts.maxErrorRate : 0.5;
    const maxLag = opts.maxBlockHeadLag != null ? opts.maxBlockHeadLag : 10;
    const maxP95 = opts.maxP95Ms != null ? opts.maxP95Ms : 5000;
    const maxThr = opts.maxThrottledRate != null ? opts.maxThrottledRate : 0.3;
    return this.filter(u => {
      const m = u.metrics;
      return m.errorRate <= maxErr
        && m.blockHeadLag <= maxLag
        && (m.p95ResponseSeconds * 1000) <= maxP95
        && m.throttledRate <= maxThr;
    });
  });

  // ─── 4.3a excludeIf — predicate-driven exclusion with sticky cooldown ──
  //
  // The composable replacement for `keepHealthy`. Each `excludeIf` call
  // takes a predicate function and (optionally) an `outFor` cooldown:
  //
  //   .excludeIf(errorRateAbove(0.5), { outFor: '30s', reason: 'errors>50%' })
  //
  // Semantics:
  //   * If `predicate(u)` is true this tick → drop `u` (and the engine's
  //     `excludedSince[id]` gets set/maintained as part of the slot's
  //     normal cross-tick bookkeeping).
  //   * If `outFor` is set AND `now - excludedSince[id] < outFor`, keep `u`
  //     dropped even when the predicate no longer matches. This is the
  //     anti-flap layer — once a rule trips an upstream out, the upstream
  //     stays out for at least `outFor` before becoming a candidate again,
  //     regardless of how fast the underlying metric oscillates.
  //   * Annotates the upstream with `opts.reason` (or a default) for
  //     diagnostics. Multiple rules each add their own annotation.
  //
  // Pair with `readmitExcluded(...)` at the end of the chain to control
  // how cooled-down upstreams come back into rotation.
  define('excludeIf', function (predicate, opts) {
    if (typeof predicate !== 'function') {
      // Graceful no-op for invalid first arg — keeps the chain alive
      // rather than throwing mid-eval and falling back to the default
      // policy at the engine level.
      return this.slice();
    }
    opts = opts || {};
    const reason = opts.reason || 'excludeIf';
    const outFor = opts.outFor != null ? globalThis.durationMs(opts.outFor) : 0;
    const ctx = globalThis.__policyCtx || {};
    const now = ctx.now || Date.now();
    const excludedSince = ctx.excludedSince || {};
    return this.filter(u => {
      if (predicate(u)) {
        annotate(u, reason);
        return false;
      }
      if (outFor > 0) {
        const since = excludedSince[u.id];
        if (since != null && (now - since) < outFor) {
          annotate(u, reason + ':held');
          return false;
        }
      }
      return true;
    });
  });

  // ─── 4.4 Generic functional (extends what JS already gives us) ──────────

  define('reject', function (fn) { return this.filter((u, i, a) => !fn(u, i, a)); });
  define('partition', function (fn) {
    const yes = [], no = [];
    for (const u of this) (fn(u) ? yes : no).push(u);
    return [yes, no];
  });
  define('unique', function (keyFn) {
    const seen = new Set();
    const out = [];
    for (const u of this) {
      const k = keyFn ? keyFn(u) : u.id;
      if (!seen.has(k)) { seen.add(k); out.push(u); }
    }
    return out;
  });
  define('union', function (other) {
    const seen = new Set(this.map(u => u.id));
    const out = this.slice();
    for (const u of other) if (!seen.has(u.id)) { seen.add(u.id); out.push(u); }
    return out;
  });
  define('intersect', function (other) {
    const ids = new Set(other.map(u => u.id));
    return this.filter(u => ids.has(u.id));
  });
  define('difference', function (other) {
    const ids = new Set(other.map(u => u.id));
    return this.filter(u => !ids.has(u.id));
  });
  Object.defineProperty(proto, 'isEmpty', { get() { return this.length === 0; } });

  // ─── 4.5 Sorting ─────────────────────────────────────────────────────────

  // Score presets — kept in sync with stdlib/install.go. We re-expose them
  // here in pure JS so the runtime doesn't need a round-trip to Go.
  const PRESETS = {
    BALANCED:               { errorRate: 8, respLatency: 4, throttledRate: 3, blockHeadLag: 2, finalizationLag: 1, misbehaviors: 6 },
    PREFER_FASTER:          { errorRate: 4, respLatency: 12, throttledRate: 4, blockHeadLag: 1, finalizationLag: 0, misbehaviors: 2 },
    PREFER_FEWER_ERRORS:    { errorRate: 15, respLatency: 2, throttledRate: 6, blockHeadLag: 2, finalizationLag: 1, misbehaviors: 12 },
    PREFER_FRESHER_HEAD:    { errorRate: 4, respLatency: 2, throttledRate: 2, blockHeadLag: 15, finalizationLag: 8, misbehaviors: 3 },
    PREFER_LESS_THROTTLED:  { errorRate: 8, respLatency: 4, throttledRate: 15, blockHeadLag: 2, finalizationLag: 1, misbehaviors: 4 },
  };
  // PREFER_CHEAP is an alias of BALANCED per spec §4.1.
  PRESETS.PREFER_CHEAP = PRESETS.BALANCED;

  // Install presets as globals.
  for (const k of Object.keys(PRESETS)) {
    globalThis[k] = PRESETS[k];
  }
  // Also install the named-preset symbols (the JS object identity is used
  // as the "preset" sentinel; weights map is what counts).

  function quantileKey(name) {
    const ok = { p50: 'p50ResponseSeconds', p70: 'p70ResponseSeconds', p90: 'p90ResponseSeconds', p95: 'p95ResponseSeconds', p99: 'p99ResponseSeconds' };
    return ok[name] || 'p70ResponseSeconds';
  }

  function computePenalty(u, weights, latencyKey, overall) {
    const m = u.metrics;
    let p = 0;
    if (weights.errorRate)       p += m.errorRate       * weights.errorRate;
    if (weights.respLatency)     p += m[latencyKey]     * weights.respLatency;
    if (weights.throttledRate)   p += m.throttledRate   * weights.throttledRate;
    if (weights.blockHeadLag)    p += m.blockHeadLag    * weights.blockHeadLag;
    if (weights.finalizationLag) p += m.finalizationLag * weights.finalizationLag;
    if (weights.misbehaviors)    p += m.misbehaviorRate * weights.misbehaviors;
    if (overall) p *= overall(u);
    return p;
  }

  define('sortByScore', function (weightsOrFnOrPreset, opts) {
    opts = opts || {};
    const latencyKey = quantileKey(opts.latencyQuantile);
    const overall = (typeof opts.overall === 'function') ? opts.overall : null;
    const isFn = (typeof weightsOrFnOrPreset === 'function');
    const out = this.slice();
    for (const u of out) {
      const w = isFn ? weightsOrFnOrPreset(u) : weightsOrFnOrPreset;
      u.score = computePenalty(u, w, latencyKey, overall);
    }
    out.sort((a, b) => a.score - b.score || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
    return out;
  });
  define('sortBy', function (fn, opts) {
    const desc = !!(opts && opts.desc);
    const out = this.slice().sort((a, b) => {
      const va = fn(a), vb = fn(b);
      return desc ? (vb - va) : (va - vb);
    });
    return out;
  });
  define('sortByDesc', function (fn) { return this.sortBy(fn, { desc: true }); });
  define('sortByLatency', function (q) {
    const key = quantileKey(q);
    return this.sortBy(u => u.metrics[key]);
  });
  define('sortByErrorRate', function () { return this.sortBy(u => u.metrics.errorRate); });
  define('sortByThrottling', function () { return this.sortBy(u => u.metrics.throttledRate); });
  define('sortByMisbehavior', function () { return this.sortBy(u => u.metrics.misbehaviorRate); });
  define('sortByHeadLag', function () { return this.sortBy(u => u.metrics.blockHeadLag); });
  define('sortByFinalizationLag', function () { return this.sortBy(u => u.metrics.finalizationLag); });

  // ─── 4.6 Randomization & rotation ───────────────────────────────────────

  function rngFromSeed(seed) {
    let s = (seed | 0) || 1;
    return () => { s = (s * 9301 + 49297) % 233280; return s / 233280; };
  }
  define('shuffle', function (seed) {
    const out = this.slice();
    const rand = seed != null ? rngFromSeed(seed) : Math.random;
    for (let i = out.length - 1; i > 0; i--) {
      const j = Math.floor(rand() * (i + 1));
      [out[i], out[j]] = [out[j], out[i]];
    }
    return out;
  });
  define('rotateBy', function (n) {
    if (this.length === 0) return this.slice();
    const k = ((n % this.length) + this.length) % this.length;
    return this.slice(k).concat(this.slice(0, k));
  });

  // ─── 4.7 Stability (cross-tick) ─────────────────────────────────────────

  // Spec §4.7: hold the previous primary across ticks unless BOTH
  //   (a) challenger.score < prev.score × (1 - hysteresis), AND
  //   (b) at least `minSwitchInterval` has elapsed since the last switch.
  // Both are needed: during an incident the score gap between primary
  // and challenger can be huge, so without (b) sticky becomes a no-op
  // and a degrading primary still flaps every tick. With (b), a recent
  // switch is locked in for the cooldown window regardless of score gap.
  define('stickyPrimary', function (opts) {
    opts = opts || {};
    const hysteresis    = (opts.hysteresis != null) ? opts.hysteresis : 0.10;
    const minSwitchMs   = (opts.minSwitchInterval != null)
      ? durationMs(opts.minSwitchInterval)
      : 30_000;
    const ctx = globalThis.__policyCtx || {};
    const prevPrimary = (ctx.previousOrder && ctx.previousOrder.length) ? ctx.previousOrder[0] : null;
    if (!prevPrimary || this.length === 0) return this.slice();
    const cur = this[0];
    if (cur.id === prevPrimary) return this.slice();          // already sticky
    const prevIdx = this.findIndex(u => u.id === prevPrimary);
    if (prevIdx < 0) return this.slice();                     // prev is gone (excluded etc.)
    const prevU = this[prevIdx];

    // Move prev to head; we'll only NOT do this when both conditions
    // for a switch are satisfied.
    function keepPrev() {
      const out = this.slice();
      const removed = out.splice(prevIdx, 1)[0];
      out.unshift(removed);
      return out;
    }

    // Cooldown not elapsed → keep prev regardless of score gap.
    if (ctx.lastSwitchAt != null && (ctx.now - ctx.lastSwitchAt) < minSwitchMs) {
      return keepPrev.call(this);
    }

    const curScore = cur.score, prevScore = prevU.score;
    if (curScore == null || prevScore == null) {
      // No sortByScore ran; positional sticky: keep prev as primary.
      return keepPrev.call(this);
    }

    // Cooldown elapsed; check hysteresis.
    if (curScore < prevScore * (1 - hysteresis)) {
      return this.slice();                                    // switch
    }
    return keepPrev.call(this);
  });
  // stickyOrder + keepRecentPrimary: deferred; uncommon use cases.

  // ─── 4.8 Grouping & multi-tier (subset) ─────────────────────────────────

  // preferTag is the canonical tier-selection primitive. It picks the
  // subset of upstreams whose tags match `pat` (glob; `!negation`
  // accepted) — provided at least `minHealthy` upstreams match. If
  // fewer than `minHealthy` upstreams match the primary pattern, falls
  // through to the `fallback` pattern. If neither matches enough,
  // returns the input unchanged.
  //
  // Common use:
  //   .preferTag('!tier:fallback', { fallback: 'tier:fallback' })
  //   // primary tier = everything not tagged tier:fallback.
  //   // If primary tier is empty, fall back to upstreams tagged tier:fallback.
  define('preferTag', function (pat, opts) {
    opts = opts || {};
    const minHealthy = opts.minHealthy != null ? opts.minHealthy : 1;
    const fallback = opts.fallback;
    const inTag = this.filter(u => hasMatchingTag(u, pat));
    if (inTag.length >= minHealthy) return inTag;
    if (fallback) {
      const fb = this.filter(u => hasMatchingTag(u, fallback));
      if (fb.length > 0) return fb;
    }
    return this.slice();
  });
  define('preferVendor', function (name, opts) {
    opts = opts || {};
    const minHealthy = opts.minHealthy != null ? opts.minHealthy : 1;
    const fallback = opts.fallback;
    const inVendor = this.filter(u => matchAny(name, u.vendor));
    if (inVendor.length >= minHealthy) return inVendor;
    if (fallback) {
      const fb = this.filter(u => matchAny(fallback, u.vendor));
      if (fb.length > 0) return fb;
    }
    return this.slice();
  });

  // spreadAcrossTags re-interleaves an already-sorted list so that
  // adjacent positions don't share the SAME tag matching the given
  // prefix. Use AFTER sortByScore — input order is preserved within
  // each partition; output is a stable round-robin across partitions.
  //
  // Purpose: blast-radius diversity. When N upstreams share a backend
  // (same sequencer, same region), they fail together; the best-by-score
  // top-3 might all be in one partition, giving 0 actual fault
  // tolerance. This primitive ensures position[0] and position[1]
  // come from different partitions when possible.
  //
  // The `prefix` arg selects which tag-dimension to partition by. With
  // upstreams tagged `region:us-east`, `region:us-west`, etc.,
  // `spreadAcrossTags('region:')` partitions by the region tag.
  // Upstreams with no matching tag are bucketed together under the
  // empty key.
  define('spreadAcrossTags', function (prefix) {
    if (this.length <= 1) return this.slice();
    if (typeof prefix !== 'string' || prefix === '') {
      // Defensive: no prefix → degenerate to one bucket, no-op.
      return this.slice();
    }
    return _interleaveByKey.call(this, (u) => {
      const tags = u.tags || [];
      for (const t of tags) if (typeof t === 'string' && t.indexOf(prefix) === 0) return t;
      return '';
    });
  });

  // Stable round-robin interleave used by spreadAcrossTags. Preserves
  // input order WITHIN each bucket (so the first occurrence of each
  // key remains the best representative of that partition) and
  // round-robins across buckets in insertion order.
  function _interleaveByKey(keyFn) {
    const buckets = new Map(); // key -> Upstream[]
    const order   = [];        // insertion order of distinct keys
    for (const u of this) {
      const k = keyFn(u) || '';
      let b = buckets.get(k);
      if (!b) { b = []; buckets.set(k, b); order.push(k); }
      b.push(u);
    }
    const out = [];
    let pulled = true;
    while (pulled) {
      pulled = false;
      for (const k of order) {
        const b = buckets.get(k);
        if (b && b.length > 0) {
          out.push(b.shift());
          pulled = true;
        }
      }
    }
    return out;
  }

  // ─── 4.9 Slicing & limits ───────────────────────────────────────────────

  define('pickTop',    function (n) { return this.slice(0, n); });
  define('pickBottom', function (n) { return this.slice(Math.max(0, this.length - n)); });
  define('dropTop',    function (n) { return this.slice(n); });
  define('dropBottom', function (n) { return this.slice(0, Math.max(0, this.length - n)); });
  define('take',       function (n) { return this.slice(0, n); });
  define('skip',       function (n) { return this.slice(n); });
  define('at_',        function (i) { return this[i] || null; });
  // `.at(i)` is already on Array.prototype in modern JS; we don't redefine it.

  // ─── 4.10 Probing & forced inclusion ────────────────────────────────────

  // readmitExcluded — controlled re-introduction of upstreams that have
  // been out of rotation for at least `reAdmitAfter`. The verb pairs with
  // `excludeIf`: that primitive marks unhealthy upstreams as excluded;
  // this one decides when (and how aggressively) to give them another
  // chance. Same semantics as the legacy `probeExcluded` name — both are
  // bound below for backward compatibility.
  //
  // Options:
  //   reAdmitAfter   — minimum time an upstream must have been excluded
  //                     (default 5m). Pair with `excludeIf({outFor})`:
  //                     the upstream's own rule keeps it out for `outFor`,
  //                     and `readmitExcluded` decides when to re-admit it
  //                     from the engine's previously-excluded pool.
  //   maxConcurrent  — at most this many re-admits per tick (default 1).
  //                     Limits blast radius when many upstreams cool down
  //                     simultaneously.
  //   position       — where in the returned array re-admitted upstreams
  //                     land: 'tail' (default, cautious — only gets traffic
  //                     via retry/hedge fallback), 'head' (aggressive —
  //                     immediately becomes a candidate primary), 'random'
  //                     (canary-style interleave).
  //   longestFirst   — re-admit the upstream that's been excluded longest
  //                     first (default true).
  function readmitExcludedFn(opts) {
    opts = opts || {};
    const ctx = globalThis.__policyCtx || {};
    const reAdmitAfterMs = globalThis.durationMs(opts.reAdmitAfter != null ? opts.reAdmitAfter : '5m');
    const maxConcurrent  = opts.maxConcurrent != null ? opts.maxConcurrent : 1;
    const longestFirst   = opts.longestFirst !== false;
    const position       = opts.position || 'tail';
    const now = ctx.now || Date.now();
    const excludedSince = ctx.excludedSince || {};
    const currentIDs = new Set(toIDs(this));
    // Find candidates from the input universe (not the chain so far) — we
    // need the engine to expose them; for now, fall back to ctx.previousExcluded.
    const candidatePool = (ctx.previousExcluded || []).filter(id => !currentIDs.has(id));
    const eligible = [];
    for (const id of candidatePool) {
      const since = excludedSince[id];
      if (since == null) continue;
      if (now - since < reAdmitAfterMs) continue;
      eligible.push({ id, since });
    }
    eligible.sort((a, b) => longestFirst ? (a.since - b.since) : (b.since - a.since));
    const pick = eligible.slice(0, maxConcurrent).map(e => e.id);

    if (pick.length === 0) return this.slice();

    // Materialize the actual upstream objects for these ids; the engine
    // exposes `__policyAllUpstreams` per-tick.
    const all = globalThis.__policyAllUpstreams || [];
    const byId = {};
    for (const u of all) byId[u.id] = u;
    const probed = pick.map(id => byId[id]).filter(Boolean);
    for (const u of probed) annotate(u, 'readmitted');

    if (position === 'head') return probed.concat(this);
    if (position === 'random') {
      const out = this.slice();
      for (const p of probed) out.splice(Math.floor(Math.random() * (out.length + 1)), 0, p);
      return out;
    }
    return this.concat(probed); // tail (default)
  }
  define('readmitExcluded', readmitExcludedFn);
  // Backward-compat alias: existing policies using `probeExcluded` keep
  // working unchanged. Documentation should reference `readmitExcluded`.
  define('probeExcluded', readmitExcludedFn);

  define('forceInclude', function (idOrFn, position) {
    const all = globalThis.__policyAllUpstreams || [];
    const matchFn = (typeof idOrFn === 'function')
      ? idOrFn
      : (u) => matchAny(idOrFn, u.id);
    const haves = new Set(this.map(u => u.id));
    const adds = all.filter(u => !haves.has(u.id) && matchFn(u));
    if (adds.length === 0) return this.slice();
    return (position === 'head') ? adds.concat(this) : this.concat(adds);
  });

  // ─── 4.11 Cooldown & warmup ─────────────────────────────────────────────

  define('cooldown', function (duration) {
    const ctx = globalThis.__policyCtx || {};
    const ms = globalThis.durationMs(duration);
    const since = ctx.excludedSince || {};
    const now = ctx.now || Date.now();
    return this.filter(u => {
      const t = since[u.id];
      if (t == null) return true;
      return (now - t) >= ms;
    });
  });

  // ─── 4.12 Combinators ───────────────────────────────────────────────────

  define('if', function (cond, thenFn, elseFn) {
    const c = (typeof cond === 'function') ? cond(this) : !!cond;
    if (c) return thenFn(this);
    if (elseFn) return elseFn(this);
    return this.slice();
  });
  define('unless', function (cond, fn) {
    const c = (typeof cond === 'function') ? cond(this) : !!cond;
    if (!c) return fn(this);
    return this.slice();
  });
  define('whenEmpty', function (fn) { return this.length === 0 ? fn() : this.slice(); });
  define('whenNotEmpty', function (fn) { return this.length > 0 ? fn(this) : this.slice(); });
  define('fallbackTo', function (arrOrFn) {
    if (this.length > 0) return this.slice();
    return (typeof arrOrFn === 'function') ? arrOrFn(globalThis.__policyCtx) : arrOrFn;
  });
  define('ensureMin', function (n, fn) { return this.length < n ? fn(this) : this.slice(); });

  // byFinality routes to one of four handlers based on ctx.finality —
  // syntactic sugar over `.if(ctx.finality === ..., ...)` for the
  // recurring "strict consensus on latest, trust-any on finalized,
  // custom for pending" pattern. A missing handler for the current
  // finality bucket is a passthrough (returns the input unchanged),
  // which means `byFinality({ finalized: f })` only branches on
  // FINALIZED and is a no-op for everything else.
  define('byFinality', function (handlers) {
    handlers = handlers || {};
    const ctx = globalThis.__policyCtx || {};
    const fin = ctx.finality || 'unknown';
    const h = handlers[fin];
    return (typeof h === 'function') ? h(this) : this.slice();
  });

  // ─── 4.13 Annotations & debug ───────────────────────────────────────────

  define('tap',      function (fn) { fn(this); return this; });
  define('label',    function (_name) { return this; }); // no-op for now; decision-record wiring in phase 6
  define('annotate', function (fn) { for (const u of this) annotate(u, fn(u)); return this; });
  define('mark',     function (pred, note) { for (const u of this) if (pred(u)) annotate(u, note); return this; });
  define('dump',     function (level) {
    const fn = (console[level || 'debug']) || console.log;
    fn('[policy.dump]', toIDs(this));
    return this;
  });

  // ─── 4.14 Module-level helpers ─────────────────────────────────────────

  // upstreamsFromIds is bound by the Go side because it needs the input set.
  globalThis.methodMatches = function (pat) {
    const ctx = globalThis.__policyCtx || {};
    return matchAny(pat, ctx.method);
  };
  globalThis.isFinalityRequest = function () {
    const ctx = globalThis.__policyCtx || {};
    return ctx.finality === 'finalized';
  };

  // ─── 4.15 Predicate factories + combinators ────────────────────────────
  //
  // These return predicate functions you can pass to `.excludeIf(...)`.
  // The naming convention is `<signal><Above|Below>(threshold)` so an
  // operator reading the policy can decode each one without a docs trip:
  //
  //   excludeIf(errorRateAbove(0.30), { outFor: '30s' })
  //
  // Combinators (`all` / `any` / `not`) compose them into compound rules:
  //
  //   excludeIf(all(errorRateAbove(0.2), latencyP99Above(2000)),
  //             { outFor: '60s' })
  //
  // Authors can write their own factories in their policy file — they're
  // just functions that return `u => boolean`. Stdlib exports cover the
  // common signals; ad-hoc compounds stay inline as `u => ...`.

  // Rate-based factories (0..1 fractions).
  globalThis.errorRateAbove       = function (rate) { return u => u.metrics.errorRate       > rate; };
  globalThis.errorRateBelow       = function (rate) { return u => u.metrics.errorRate       < rate; };
  globalThis.throttleRateAbove    = function (rate) { return u => u.metrics.throttledRate   > rate; };
  globalThis.throttleRateBelow    = function (rate) { return u => u.metrics.throttledRate   < rate; };
  globalThis.misbehaviorRateAbove = function (rate) { return u => u.metrics.misbehaviorRate > rate; };

  // Latency factories — millisecond thresholds; quantile accepts either
  // 0..1 fractions or 0..100 percentile numbers (auto-normalized inside
  // `u.metrics.latencyP`).
  globalThis.latencyAbove    = function (quantile, ms) { return u => u.metrics.latencyP(quantile) > ms; };
  globalThis.latencyP50Above = function (ms) { return u => u.metrics.latencyP(50) > ms; };
  globalThis.latencyP70Above = function (ms) { return u => u.metrics.latencyP(70) > ms; };
  globalThis.latencyP90Above = function (ms) { return u => u.metrics.latencyP(90) > ms; };
  globalThis.latencyP95Above = function (ms) { return u => u.metrics.latencyP(95) > ms; };
  globalThis.latencyP99Above = function (ms) { return u => u.metrics.latencyP(99) > ms; };

  // Lag-based factories — block-count thresholds.
  globalThis.blockNumberLagAbove  = function (blocks) { return u => u.metrics.blockHeadLag    > blocks; };
  globalThis.finalizationLagAbove = function (blocks) { return u => u.metrics.finalizationLag > blocks; };

  // Time-based lag — block-count × network's EMA-estimated block time.
  // Threshold is in SECONDS. Returns 0 (predicate always false) until the
  // tracker has enough block-time samples; on Eth mainnet that's a few
  // seconds after first traffic. Useful when you want a wall-clock SLO
  // ("trip if more than 60s behind tip") instead of a chain-relative
  // block count that means different things on different chains.
  globalThis.blockSecondsLagAbove         = function (seconds) { return u => u.metrics.blockHeadLagSeconds    > seconds; };
  globalThis.finalizationSecondsLagAbove  = function (seconds) { return u => u.metrics.finalizationLagSeconds > seconds; };

  // Sample-size guard — useful as an AND-term to avoid tripping rules on
  // low-sample-count noise (when an upstream has only had a handful of
  // requests, a single error blows up errorRate to 100%).
  globalThis.samplesBelow = function (n) { return u => u.metrics.requestsTotal < n; };
  globalThis.samplesAbove = function (n) { return u => u.metrics.requestsTotal > n; };

  // Logical combinators — flat, variadic. Predicates compose freely:
  //   excludeIf(all(errorRateAbove(0.3), not(samplesBelow(10))),
  //             { outFor: '30s' })
  // means "trip if errorRate>0.3 AND we actually have enough samples".
  globalThis.all = function (...preds) { return u => preds.every(p => p(u)); };
  globalThis.any = function (...preds) { return u => preds.some(p => p(u));  };
  globalThis.not = function (pred)     { return u => !pred(u);                };
})();
