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

  // ─── Step-trail instrumentation ─────────────────────────────────────────
  //
  // Every chainable stdlib method is wrapped so its (input → output)
  // transition can be recorded into `globalThis.__policyStepLog` —
  // an array of `{step, args, inIds, outIds, dropped, added, reordered}`
  // entries the Go side reads back after `runEval` completes.
  //
  // Wrap point: the `define(name, fn, argsExtractor)` helper. `fn` runs
  // unchanged; if both `this` and the result are arrays, the wrapper
  // computes a delta and pushes a log entry. `argsExtractor(args)` is
  // optional and returns a small JSON-able object summarizing the
  // chain-step's interesting opts (e.g. `excludeIf`'s `reason`,
  // `preferTag`'s `pat`) — kept tight so we don't blow up the WS frame
  // budget at high tick rates.
  //
  // Two modes:
  //   * __policyStepLogEnabled !== true  → no-op; zero overhead beyond
  //     the wrapper function call. The default in production, kept fast.
  //   * __policyStepLogEnabled === true  → record full trail. The
  //     simulator and DEBUG-level eRPC callers set this once before
  //     each eval (eval.go).
  //
  // The log buffer is reset to a fresh array per tick (Go sets it back
  // to `[]` before each eval); steps push entries in chain order, so the
  // resulting array IS the timeline.
  // _captureArgs builds a compact, JSON-safe summary of an invocation's
  // arguments for the step trail. Captures primitives + shallow objects
  // of primitives + short string/number arrays. Functions (predicates,
  // thunks, callbacks) are skipped — they don't render meaningfully and
  // would balloon the wire frame. Same idea as a structured-log
  // sanitizer: keep what's diagnostic, drop what isn't.
  function _captureArgs(args) {
    if (!args || args.length === 0) return null;
    const out = {};
    for (let i = 0; i < args.length; i++) {
      const a = args[i];
      if (a == null || typeof a === 'function') continue;
      const ta = typeof a;
      if (ta === 'string' || ta === 'number' || ta === 'boolean') {
        out[i] = a;
        continue;
      }
      if (Array.isArray(a)) {
        if (a.length <= 16 && a.every(x => typeof x === 'string' || typeof x === 'number')) {
          out[i] = a.slice();
        }
        continue;
      }
      if (ta === 'object') {
        const obj = {};
        for (const k in a) {
          if (!Object.prototype.hasOwnProperty.call(a, k)) continue;
          const v = a[k];
          if (v == null || typeof v === 'function') continue;
          const tv = typeof v;
          if (tv === 'string' || tv === 'number' || tv === 'boolean') {
            obj[k] = v;
          } else if (Array.isArray(v) && v.length <= 16 &&
                     v.every(x => typeof x === 'string' || typeof x === 'number')) {
            obj[k] = v.slice();
          }
        }
        if (Object.keys(obj).length > 0) out[i] = obj;
      }
    }
    return Object.keys(out).length > 0 ? out : null;
  }

  function _recordStep(name, beforeArr, afterArr, args) {
    if (!globalThis.__policyStepLogEnabled) return;
    const log = globalThis.__policyStepLog;
    if (!log) return;
    const inIds = toIDs(beforeArr);
    const outIds = toIDs(afterArr);
    const inSet = new Set(inIds);
    const outSet = new Set(outIds);
    const dropped = inIds.filter(id => !outSet.has(id));
    const added   = outIds.filter(id => !inSet.has(id));
    let reordered = false;
    if (dropped.length === 0 && added.length === 0 && inIds.length === outIds.length) {
      for (let i = 0; i < inIds.length; i++) {
        if (inIds[i] !== outIds[i]) { reordered = true; break; }
      }
    }
    log.push({
      step: name,
      args: args || null,
      inIds,
      outIds,
      dropped,
      added,
      reordered,
    });
  }

  // _stepDropAttribute records which stdlib primitive first dropped a
  // given upstream this tick — drives `erpc_selection_rejection_total{step}`
  // with a low-cardinality primitive label (`removeCordoned`, `excludeIf`,
  // `take`, `byTag`, ...).
  //
  // The step name lands as a SENTINEL-prefixed entry on the existing
  // `__policyLeafReasons[id]` array (`"@step:removeCordoned"`). The Go
  // side splits it back out before the leaf-reason metric emitter sees
  // it. Folding step + leaves into one map lets us delete the
  // `__policyStepReasons` global, its reader, and the parallel field
  // on EvalResult — at the cost of a 6-char prefix scan.
  //
  // ALWAYS-ON — independent of `__policyStepLogEnabled`. Cost per step
  // transition:
  //   * Skip when `before.length === 0` (nothing could drop).
  //   * One `Set` of size `after.length`.
  //   * One walk of `before` (Set.has lookups, push on drops).
  //   * No work when no drops (every input ID is in survivors).
  //
  // First-step-wins: an entry whose first element already starts with
  // `"@step:"` is left alone. Once an upstream falls out of the chain
  // it can't be dropped again, so the first writer is the only one
  // that matters; the guard is belt-and-braces against re-entry.
  //
  // Safety: bail out if `before`/`after` contains a non-upstream (e.g.
  // `partition` returns a 2-tuple of arrays). Don't pollute the metric
  // with garbage attributions.
  function _stepDropAttribute(name, before, after) {
    const log = globalThis.__policyLeafReasons;
    if (!log) return;
    if (before.length === 0) return;
    const survivors = new Set();
    for (let i = 0; i < after.length; i++) {
      const u = after[i];
      if (u == null || typeof u.id !== 'string') return;
      survivors.add(u.id);
    }
    const marker = '@step:' + name;
    for (let i = 0; i < before.length; i++) {
      const u = before[i];
      if (u == null || typeof u.id !== 'string') return;
      const id = u.id;
      if (survivors.has(id)) continue;
      let entry = log[id];
      if (!entry) {
        entry = log[id] = [];
      }
      // First-step-wins guard: skip if a step marker is already in slot 0.
      if (entry.length > 0 && typeof entry[0] === 'string' &&
          entry[0].length >= 6 && entry[0].slice(0, 6) === '@step:') {
        continue;
      }
      entry.unshift(marker);
    }
  }

  // ─── Probe verdicts ─────────────────────────────────────────────────
  // Each exclude-family step renders a per-upstream VERDICT about probe
  // eligibility, independent of which step actually dropped the upstream.
  // The chain is first-excluder-wins, so "which step dropped it" is
  // order-sensitive; the verdict matrix is not: every exclude step judges
  // every upstream it WOULD drop — current chain members AND upstreams
  // already removed by earlier steps.
  //
  //   probe-eligible (probe: true)  — the exclusion reverses via fresh
  //     traffic metrics (errors/latency/throttle); probing helps.
  //   probe-blocking (probe: false) — the exclusion is static (tags,
  //     cordons) or otherwise traffic-independent; probing changes
  //     nothing and only burns quota on the excluded upstream.
  //
  // After the eval an excluded upstream is shadow-probed iff it has at
  // least one eligible verdict AND no blocking verdict. Upstreams
  // excluded only by untracked means (raw filters, take, …) carry no
  // verdicts and default to probing — preserving prior behavior.
  // `routing.probe: 'off'` remains the per-upstream hard veto on top.
  function _recordProbeVerdict(id, eligible) {
    const v = globalThis.__policyProbeVerdicts;
    if (!v) return;
    const cur = v[id] || (v[id] = { e: false, b: false });
    if (eligible) cur.e = true; else cur.b = true;
  }
  // Judge upstreams NOT in the current chain (already dropped by earlier
  // steps) so later exclude steps still contribute verdicts for them. A
  // throwing judge is treated as "would not drop" — same defensive
  // posture as the rest of the eval.
  function _sweepAbsentForVerdicts(chain, wouldDrop, eligible) {
    const all = globalThis.__policyAllUpstreams;
    if (!all || all.length === chain.length) return;
    const present = new Set(chain.map(u => u.id));
    for (const u of all) {
      if (present.has(u.id)) continue;
      let drop = false;
      try { drop = !!wouldDrop(u); } catch (_e) { drop = false; }
      if (drop) _recordProbeVerdict(u.id, eligible);
    }
  }

  function define(name, fn) {
    if (proto[name]) return; // idempotent: a re-installed primer must not throw
    const wrapped = function () {
      const before = this;
      const result = fn.apply(this, arguments);
      // Only attribute / record array-in/array-out transitions. Non-array
      // outputs (e.g. `partition` returns a 2-tuple, `at_` returns a single
      // upstream) skip the trail — they're terminal-ish ops anyway.
      const arrayInOut = Array.isArray(before) && Array.isArray(result);
      // HOT-PATH GUARD — pprof on prod showed `_stepDropAttribute`
      // and the supporting array/set work were the largest JS-side
      // CPU consumer (the inner `.some()` / `.every()` shapes in the
      // attribution + the `new Set()` allocation). Most chain steps
      // in a healthy policy DON'T drop anyone (excludeIf passes
      // everything, byTag matches all, etc.) — so when `result.length
      // >= before.length`, there's nothing to attribute and the work
      // is purely wasted. Skip the call entirely on the no-drop path.
      if (arrayInOut && result.length < before.length) {
        // Always-on: low-cost step-name attribution for metrics.
        _stepDropAttribute(name, before, result);
      }
      // Diagnostic-only: full step trail with args / dropped / added.
      // Gated by `__policyStepLogEnabled` (debug-level or simulator);
      // production never pays this cost.
      if (arrayInOut && globalThis.__policyStepLogEnabled) {
        _recordStep(name, before, result, _captureArgs(arguments));
      }
      return result;
    };
    _set(proto, name, { value: wrapped, enumerable: false, writable: false, configurable: false });
  }

  // ─── small helpers ──────────────────────────────────────────────────────

  // toIDs maps an upstream array to its IDs.
  function toIDs(arr) {
    const out = new Array(arr.length);
    for (let i = 0; i < arr.length; i++) out[i] = arr[i].id;
    return out;
  }

  // _finalityBit maps a finality string (the canonical value the Go side
  // puts on `ctx.finality`) to its bit position — mirroring the
  // REALTIME/UNFINALIZED/FINALIZED/UNKNOWN constants installed in Go.
  // Used by `when()` and any future finality-mask primitive. Unknown /
  // missing maps to UNKNOWN's bit so a mask containing UNKNOWN still
  // matches an ambiguous request.
  function _finalityBit(f) {
    switch (f) {
      case 'realtime':    return 1 << 0;
      case 'unfinalized': return 1 << 1;
      case 'finalized':   return 1 << 2;
      default:            return 1 << 3; // unknown / missing
    }
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
  // excludeTag(pat, opts?) — static exclusion by tag. Static exclusions
  // default to probe-BLOCKING: the upstream is out by decision, not by
  // data, so no amount of fresh traffic metrics changes the outcome.
  // Pass `{ probe: true }` to opt the dropped upstreams back into
  // shadow probing.
  define('excludeTag', function (pat, opts) {
    const probe = !!(opts && opts.probe === true);
    const wouldDrop = (u) => hasMatchingTag(u, pat);
    _sweepAbsentForVerdicts(this, wouldDrop, probe);
    return this.filter(u => {
      if (wouldDrop(u)) {
        _recordProbeVerdict(u.id, probe);
        return false;
      }
      return true;
    });
  });
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
    return this.filter(u => u.metrics.errorRate <= max);
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
  // removeCordoned(opts?) — cordons reverse via timers (consensus
  // sit-out) or operator action, never via fresh traffic metrics, so
  // cordoned upstreams default to probe-BLOCKING. `{ probe: true }`
  // opts them back in.
  define('removeCordoned', function (opts) {
    const probe = !!(opts && opts.probe === true);
    const wouldDrop = (u) => !!u.metrics.cordonedReason;
    _sweepAbsentForVerdicts(this, wouldDrop, probe);
    return this.filter(u => {
      if (wouldDrop(u)) {
        _recordProbeVerdict(u.id, probe);
        return false;
      }
      return true;
    });
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

  // ─── 4.3a excludeIf — predicate-driven exclusion ────────────────────────
  //
  // The composable replacement for `keepHealthy`. Each `excludeIf` call
  // takes a predicate function:
  //
  //   .excludeIf(errorRateAbove(0.5))
  //
  // Semantics:
  //   * If `predicate(u)` is true this tick → drop `u`.
  //   * Annotates dropped upstreams with a human-readable reason for
  //     diagnostics. The reason is auto-derived from the predicate:
  //     factory-built predicates (errorRateAbove, latencyDeviationAbove,
  //     all/any/not, …) attach a `policyReason` property describing
  //     what they check. Inline custom predicates can supply a label
  //     as an optional 2nd positional arg:
  //
  //       .excludeIf(u => u.id.startsWith('legacy-'), 'legacy upstream')
  //
  // The annotation shows up in `Decision.Output.Annotations[id]`, in
  // the simulator's policy-history modal as a pill on the excluded
  // row, and in DEBUG eRPC logs — so "which rule trips this upstream
  // out" is always one click / log line away.
  //
  // Re-admission is implicit: an excluded upstream's tracker counters
  // continue to be fed by `probeExcluded` (shadow-mirrored real traffic)
  // and structural pollers (head-lag etc.); once those counters cross
  // back below the excludeIf predicate's threshold the upstream falls
  // out of the excluded set on the next tick. No separate cooldown
  // timer — the same predicate that excluded it is what re-admits it.
  // excludeIf(predicate, reasonOverrideOrOpts?) — conditional exclusion.
  // The second arg is either the legacy reason-override string, or an
  // options object `{ probe?: boolean, reason?: string }`. Conditional
  // exclusions default to probe-ELIGIBLE: the predicate reads traffic
  // metrics that freeze without traffic, so shadow probes are what let
  // the upstream prove recovery. Pass `{ probe: false }` for predicates
  // whose inputs refresh without traffic (e.g. block-head lag, fed by
  // the state poller) — probing those is pure waste.
  define('excludeIf', function (predicate, reasonOverrideOrOpts) {
    if (typeof predicate !== 'function') {
      // Graceful no-op for invalid first arg — keeps the chain alive
      // rather than throwing mid-eval and falling back to the default
      // policy at the engine level.
      return this.slice();
    }
    let reasonOverride;
    let probe = true;
    if (typeof reasonOverrideOrOpts === 'string') {
      reasonOverride = reasonOverrideOrOpts;
    } else if (reasonOverrideOrOpts != null && typeof reasonOverrideOrOpts === 'object') {
      if (typeof reasonOverrideOrOpts.reason === 'string') reasonOverride = reasonOverrideOrOpts.reason;
      if (reasonOverrideOrOpts.probe === false) probe = false;
    }
    _sweepAbsentForVerdicts(this, predicate, probe);
    // Per-upstream leaf-slug attribution for metrics. The Go-side metric
    // emitter reads `__policyLeafReasons[id]` after the eval and emits
    // one `selection_exclusion_total{reason=<slug>}` increment per leaf.
    // Compound predicates (`any`/`all`/`not`) attribute exclusion to
    // their LEAF slugs, never to the combinator boilerplate.
    const leafLog = globalThis.__policyLeafReasons;
    return this.filter(u => {
      if (predicate(u)) {
        _recordProbeVerdict(u.id, probe);
        if (leafLog) {
          let leaves;
          if (typeof reasonOverride === 'string') {
            // Operator overrode the reason — respect it for both display
            // AND attribution; one slug, the override string itself.
            leaves = [reasonOverride];
          } else if (typeof predicate.policyLeaves === 'function') {
            leaves = predicate.policyLeaves(u);
          } else if (predicate.policySlug) {
            leaves = [predicate.policySlug];
          } else {
            leaves = ['custom'];
          }
          if (leaves && leaves.length > 0) {
            const existing = leafLog[u.id];
            if (existing) {
              for (const l of leaves) existing.push(l);
            } else {
              leafLog[u.id] = leaves.slice();
            }
          }
        }
        return false;
      }
      return true;
    });
  });

  // ─── 4.3b shadowExcludeIf — dry-run / observed-only exclusion ──────────
  //
  // Mirrors `excludeIf` exactly — but DOES NOT drop the upstream. Used to
  // safely audition a new exclusion rule (or removal of an existing one) in
  // production before flipping the call to `excludeIf` for real.
  //
  // Per tick, for every upstream whose predicate trips:
  //   * Annotates `shadow:<reason>` (visible in DEBUG logs / the
  //     simulator's policy-history pane / Decision.Output.Annotations).
  //   * Populates `globalThis.__policyShadowReasons[u.id]` with the LEAF
  //     slugs the predicate would have attributed to (mirrors option-(c)
  //     attribution from `excludeIf`).
  //
  // The Go-side metric emitter reads `__policyShadowReasons` after the
  // eval and increments `erpc_selection_shadow_exclusion_total{reason}` —
  // one per leaf, NOT the combinator. Operators compare its rate to
  // `erpc_selection_exclusion_total` over time to decide whether the
  // proposed rule is safe to promote.
  //
  // The upstream stays in the pool in its original position. No effect on
  // `excludedSince`, `stickyPrimary`, or `probeExcluded` — shadow trips
  // never enter the cooldown bookkeeping.
  define('shadowExcludeIf', function (predicate, reasonOverride) {
    if (typeof predicate !== 'function') {
      return this.slice();
    }
    const shadowLog = globalThis.__policyShadowReasons;
    const out = this.slice();
    for (const u of out) {
      if (!predicate(u)) continue;
      if (shadowLog) {
        let leaves;
        if (typeof reasonOverride === 'string') {
          leaves = [reasonOverride];
        } else if (typeof predicate.policyLeaves === 'function') {
          leaves = predicate.policyLeaves(u);
        } else if (predicate.policySlug) {
          leaves = [predicate.policySlug];
        } else {
          leaves = ['custom'];
        }
        if (leaves && leaves.length > 0) {
          const existing = shadowLog[u.id];
          if (existing) {
            for (const l of leaves) existing.push(l);
          } else {
            shadowLog[u.id] = leaves.slice();
          }
        }
      }
    }
    return out;
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

  // Score presets — JS is the single source of truth for ranking weights.
  // Three explicit profiles, each describing the operator's PRIORITY for
  // that request class:
  //
  //   * PREFER_FASTEST       — latency dominates. Default for most
  //                            request paths; the `excludeIf` chain
  //                            already drops broken upstreams, so the
  //                            ranking question is "which of the
  //                            healthy ones answers first?".
  //   * PREFER_FRESHEST      — block-head freshness dominates.
  //                            Realtime reads that can't tolerate a
  //                            stale-head upstream.
  //   * PREFER_LEAST_ERRORS  — error rate dominates. Use for write
  //                            paths or anything where a 5xx costs
  //                            more than a slow response.
  //
  // Each preset emphasizes ONE primary axis (15 weight) while keeping
  // the others balanced enough that an obviously bad upstream on a
  // secondary signal still loses. Operators wanting a fully custom
  // weight map pass an object literal directly to `sortByScore(...)`.
  const PRESETS = {
    PREFER_FASTEST:       { errorRate: 4,  respLatency: 15, throttledRate: 4, blockHeadLag: 1,  finalizationLag: 0, misbehaviors: 2 },
    PREFER_FRESHEST:      { errorRate: 4,  respLatency: 2,  throttledRate: 2, blockHeadLag: 15, finalizationLag: 8, misbehaviors: 3 },
    PREFER_LEAST_ERRORS:  { errorRate: 15, respLatency: 2,  throttledRate: 6, blockHeadLag: 2,  finalizationLag: 1, misbehaviors: 12 },
  };

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

  // computeWeightedPenalty sums each enabled metric × weight. Lower is
  // healthier; `sortByScore` turns it into a higher-is-better score.
  //
  // `m` is the metrics object — `u.metrics` for slot-local scoring,
  // `u.metricsAcrossMethods` when stickyPrimary needs the wildcard
  // aggregate every slot in the scope sees identically.
  function computeWeightedPenalty(m, weights, latencyKey) {
    if (!m) return 0;
    let p = 0;
    if (weights.errorRate)       p += m.errorRate       * weights.errorRate;
    if (weights.respLatency)     p += m[latencyKey]     * weights.respLatency;
    if (weights.throttledRate)   p += m.throttledRate   * weights.throttledRate;
    if (weights.blockHeadLag)    p += m.blockHeadLag    * weights.blockHeadLag;
    if (weights.finalizationLag) p += m.finalizationLag * weights.finalizationLag;
    if (weights.misbehaviors)    p += m.misbehaviorRate * weights.misbehaviors;
    return p;
  }

  // sortByScore(base, opts) — rank upstreams best-first. HIGHER score wins.
  //
  //   score(u) = overall(u) / (1 + Σ metricᵢ(u) × weightᵢ)
  //
  // A clean upstream (zero penalty) scores `overall` (default 1); accrued
  // errors / latency / lag divide that back down. `overall` is the
  // preference dial — >1 prefers an upstream, <1 avoids it.
  //
  // `base` is the BASELINE weight map every upstream starts from:
  //   * a preset (PREFER_FASTEST / PREFER_FRESHEST / PREFER_LEAST_ERRORS),
  //   * a custom `{ errorRate, respLatency, throttledRate, blockHeadLag,
  //     finalizationLag, misbehaviors }` object,
  //   * a function `(u) => preset | weights` for per-upstream baselines,
  //   * or omitted → defaults to PREFER_FASTEST.
  //
  // Per-upstream `routing.scoreMultipliers` (config) arrive as
  // `u.scoreMultipliers` and combine with `base` per `opts.multipliers`:
  //   * 'merge'    (default) — per-upstream keys override the matching
  //                base keys; unset keys inherit base. `overall` lifts the
  //                final score.
  //   * 'override' — configured upstreams rank by THEIR weights only
  //                (base ignored); upstreams without config use base.
  //   * 'off'      — ignore `u.scoreMultipliers` entirely; rank by base.
  //
  // `opts.latencyQuantile` ('p50'..'p99', default p70) picks which
  // response-time quantile feeds `respLatency`. `opts.overall` (a
  // function) is an extra multiplicative dial folded on top of the
  // per-upstream `overall`, kept for advanced/programmatic callers.
  define('sortByScore', function (base, opts) {
    opts = opts || {};
    const latencyKey = quantileKey(opts.latencyQuantile);
    const mode = opts.multipliers || 'merge';
    const optsOverallFn = (typeof opts.overall === 'function') ? opts.overall : null;
    const baseIsFn = (typeof base === 'function');

    const out = this.slice();
    for (const u of out) {
      let baseW = baseIsFn ? base(u) : base;
      if (!baseW) baseW = PRESETS.PREFER_FASTEST;

      // Per-upstream override object (engine-attached), unless disabled.
      const pm = (mode !== 'off') ? u.scoreMultipliers : null;

      let weights = baseW;
      let overallVal = 1;
      if (pm) {
        // Split the non-metric `overall` dial out from the metric weights.
        let pmWeights = null, hasW = false;
        for (const k in pm) {
          if (!Object.prototype.hasOwnProperty.call(pm, k)) continue;
          if (k === 'overall') { if (pm[k] != null) overallVal = pm[k]; continue; }
          if (!pmWeights) pmWeights = {};
          pmWeights[k] = pm[k];
          hasW = true;
        }
        if (mode === 'override') {
          weights = hasW ? pmWeights : baseW;
        } else {
          weights = hasW ? Object.assign({}, baseW, pmWeights) : baseW;
        }
      }

      if (optsOverallFn) overallVal *= optsOverallFn(u);

      const penalty = computeWeightedPenalty(u.metrics, weights, latencyKey);
      u.score = overallVal / (1 + penalty);
    }
    // Descending score; alphabetical id as the stable tiebreak.
    out.sort((a, b) => (b.score - a.score) || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
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

  // Hold the previous primary across ticks unless BOTH
  //   (a) challenger.score > prev.score × (1 + hysteresis), AND
  //   (b) at least `minSwitchInterval` has elapsed since the last switch.
  // (Scores are higher-is-better, so a challenger must be DECISIVELY
  // higher than the incumbent — by the hysteresis margin — to take over.)
  // Both are needed: during an incident the score gap between primary
  // and challenger can be huge, so without (b) sticky becomes a no-op
  // and a degrading primary still flaps every tick. With (b), a recent
  // switch is locked in for the cooldown window regardless of score gap.
  //
  // `opts.scope` controls WHICH set of slots agree on this primary.
  // Cross-method scopes (NETWORK, NETWORK_FINALITY) score the primary
  // off the wildcard `u.metricsAcrossMethods` aggregate — the only
  // metric source every slot in the scope sees identically — so they
  // converge deterministically without "first-writer wins" flapping.
  // Slot-local scope (NETWORK_METHOD_FINALITY) scores off the slot's
  // own per-method `u.score`, current behavior.
  //
  // Convergence: the shared register is keyed by the scope-resolved
  // (network, method-or-*, finality-or-*) tuple; every slot that maps
  // to the same key reads/writes the SAME entry. Hysteresis is
  // evaluated against the SAME challenger score (wildcard) in every
  // slot, so they all reach the same hold/switch verdict.
  define('stickyPrimary', function (opts) {
    opts = opts || {};
    const hysteresis  = (opts.hysteresis != null) ? opts.hysteresis : 0.10;
    const minSwitchMs = (opts.minSwitchInterval != null) ? durationMs(opts.minSwitchInterval) : 30_000;
    const scope       = opts.scope || 'network';
    if (this.length === 0) return this.slice();

    const ctx = globalThis.__policyCtx || {};

    // Resolve previous primary + lastSwitchAt. Coarser-than-slot
    // scopes consult the cross-slot shared register; slot-grain scope
    // falls through to ctx.previousOrder for back-compat with chains
    // that don't go through the shared register (e.g. tests).
    let prevPrimary = null, lastSwitchAt = null;
    if (scope !== 'network-method-finality' && typeof __getSharedSticky === 'function') {
      const shared = __getSharedSticky(scope);
      if (shared && shared.primary) {
        prevPrimary = shared.primary;
        lastSwitchAt = shared.lastSwitchAt;
      }
    }
    if (prevPrimary == null) {
      // Fallback / slot-local scope: read the slot's own previousOrder.
      // The shared register also exists for slot-grain scopes; we just
      // skip the round-trip when ctx already has the value.
      prevPrimary = (ctx.previousOrder && ctx.previousOrder.length) ? ctx.previousOrder[0] : null;
      lastSwitchAt = ctx.lastSwitchAt;
    }

    // Scoring source: WILDCARD aggregate for cross-method scopes (the
    // only metric every participating slot sees identically), or the
    // slot-local `u.score` (set by sortByScore) for slot-grain scopes.
    const useWildcard = (scope === 'network' || scope === 'network-finality');
    function scoreOf(u) {
      if (useWildcard) {
        const m = u.metricsAcrossMethods;
        if (!m) return null;
        // PREFER_FASTEST is hard-coded for the primary picker — it's
        // the canonical "score across methods" formula. Per-upstream
        // scoreMultipliers stay out of the wildcard view because they
        // resolve per (method, finality) and don't share a single
        // canonical value across the scope. We can plumb opts.scoreFn
        // later if operator preference grows beyond this.
        const penalty = computeWeightedPenalty(m, PRESETS.PREFER_FASTEST, 'p70ResponseSeconds');
        return 1 / (1 + penalty);
      }
      return u.score;
    }

    // Note the publish/write happens at the END regardless of branch —
    // this ensures the shared register seeds with the slot's current
    // primary on cold start (no prevPrimary), and updates on switch.
    function publish(primary, switching) {
      if (typeof __setSharedSticky === 'function' && primary) {
        const ts = switching ? ctx.now : (lastSwitchAt != null ? lastSwitchAt : ctx.now);
        __setSharedSticky(scope, primary, ts);
      }
    }

    if (!prevPrimary) {
      // Cold start: current head IS the primary; seed the register.
      publish(this[0].id, true);
      return this.slice();
    }

    const cur = this[0];
    if (cur.id === prevPrimary) {
      publish(cur.id, false);          // confirm — no switch, same lastSwitchAt
      return this.slice();             // already sticky
    }
    const prevIdx = this.findIndex(u => u.id === prevPrimary);
    if (prevIdx < 0) {
      // Prev is gone from this slot's survivor set (excluded for this
      // method or removed upstream). Per-slot fall-through: use the
      // slot's local best WITHOUT updating the shared register — other
      // slots that still have the prev primary in their survivors keep
      // using it. This is the "method-disagreement" escape hatch.
      return this.slice();
    }
    const prevU = this[prevIdx];

    function keepPrev() {
      const out = this.slice();
      const removed = out.splice(prevIdx, 1)[0];
      out.unshift(removed);
      // Flag that sticky actively held the primary this tick. Read by
      // the Go-side metric emitter to increment `selection_sticky_hold_total`
      // for the held upstream. Only flips when we WOULD have switched —
      // the "already sticky" branch above doesn't set it.
      globalThis.__policyStickyHeld = true;
      publish(prevPrimary, false);
      return out;
    }

    // Cooldown not elapsed → keep prev regardless of score gap.
    if (lastSwitchAt != null && (ctx.now - lastSwitchAt) < minSwitchMs) {
      return keepPrev.call(this);
    }

    const curScore = scoreOf(cur);
    const prevScore = scoreOf(prevU);
    if (curScore == null || prevScore == null) {
      // Missing score (no sortByScore ran AND no wildcard metrics
      // available) → positional sticky: keep prev as primary.
      return keepPrev.call(this);
    }

    // Cooldown elapsed; check hysteresis. Higher is better, so the
    // challenger must clear the incumbent by the hysteresis margin.
    if (curScore > prevScore * (1 + hysteresis)) {
      publish(cur.id, true);
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

  // probeExcluded — opt-in shadow-mirror primitive. When this step
  // appears in the chain, the network's probe subsystem mirrors a
  // sampled stream of real incoming requests against any upstream
  // currently in the excluded set. The mirrored calls feed the SAME
  // tracker counters as real traffic, so the upstream is re-admitted
  // implicitly on the next tick once its metrics improve enough to
  // clear the chain's `excludeIf` predicates. There is no
  // "re-admission timer" — the criteria for re-admission is exactly
  // the criteria for exclusion, in reverse: if `excludeIf` no longer
  // trips against the upstream's fresh (shadow-fed) samples, it's
  // back in rotation.
  //
  // This is a NO-OP transform on the upstream array — its real work
  // is in the Go-side prober that subscribes to the network's request
  // feed when this step is present. Omitting `probeExcluded` from the
  // chain disables shadow probing entirely; excluded upstreams stay
  // excluded until structural signals (head lag, state-poller results,
  // etc.) bring their counters back across the threshold OR an
  // operator intervenes manually.
  //
  // Options:
  //   sampleRate    — 0.0–1.0, per-(request, excluded-upstream) probability
  //                    of mirroring. Default 0.1 — 10% of incoming
  //                    requests are probe candidates. At high RPS this
  //                    keeps probe-traffic cost bounded; at low RPS the
  //                    `minSamples` floor below kicks in to ensure
  //                    enough samples accumulate for re-admission.
  //   minSamples    — per-upstream floor on probes within
  //                    `minSamplesWindow`. While the upstream has
  //                    fewer than this many probes in the rolling
  //                    window, the sampleRate gate is bypassed and
  //                    every incoming request is considered — so
  //                    low-traffic networks aren't starved out of
  //                    probe activity. Default 10. Pair with the
  //                    excludeIf chain's `samplesAbove(N)` guard:
  //                    `minSamples` should be ≥ that N so the
  //                    re-admission criterion is reachable.
  //   minSamplesWindow — rolling window for `minSamples` (default
  //                    '60s', matches typical scoreMetricsWindowSize).
  //   maxConcurrent — concurrent in-flight probes per excluded upstream
  //                    (default 4). Bounds worst-case per-upstream
  //                    probe RPS independently of sampleRate.
  //   timeout       — per-probe deadline (default '10s'). Probes that
  //                    overrun are cancelled and counted as failures so
  //                    a hung upstream registers as bad in the tracker.
  //
  // Per-upstream opt-out: set `routing.probe: off` on any upstream
  // config to exclude it from probe traffic entirely (cost-sensitive
  // vendors, etc.). That upstream stays in the excluded set forever
  // once predicates trip, until manually uncordoned.
  function probeExcludedFn(opts) {
    opts = opts || {};
    globalThis.__probeConfig = {
      sampleRate:       opts.sampleRate       != null ? opts.sampleRate       : 0.1,
      minSamples:       opts.minSamples       != null ? opts.minSamples       : 10,
      minSamplesWindow: opts.minSamplesWindow != null ? opts.minSamplesWindow : '60s',
      maxConcurrent:    opts.maxConcurrent    != null ? opts.maxConcurrent    : 4,
      timeout:          opts.timeout          != null ? opts.timeout          : '10s',
    };
    return this.slice();
  }
  define('probeExcluded', probeExcludedFn);

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

  // includeIf — conditionally admit upstreams from the full universe back
  // into the chain. The dual of `excludeIf`: where `excludeIf` DROPS a
  // per-upstream offender, `includeIf` ADDS a selected set of upstreams
  // when an aggregate condition over the surviving pool holds. It never
  // removes anyone.
  //
  // The intended use is a "break-glass" tier: keep a set of upstreams out
  // of normal rotation (e.g. via `excludeTag('tier:<x>')`) and bring them
  // in ONLY when the upstreams that are currently serving become
  // collectively unfit — too few left, all of them lagging, all of them
  // slow. Because it adds rather than excludes, a single degraded primary
  // is never evicted; the reserve set is offered alongside it and ranked
  // by the subsequent `sortByScore`.
  //
  //   target: WHAT to admit — placed first so the policy reads
  //     "include <these> if <condition>" and is never ambiguous. Either:
  //       * a tag pattern (string / array, `!negation` accepted) — the
  //         common case: `includeIf('tier:reserve', cond)`; or
  //       * a selector object `{ id, tag, vendor, type, position }` whose
  //         facets AND together (same semantics as `where`). At least one
  //         facet must resolve to a concrete value — otherwise includeIf is
  //         a no-op (it never pulls the whole universe by accident, e.g. for
  //         `{}` or `{ position: 'head' }`).
  //     `position` ('head' | 'tail', default 'tail') only applies to the
  //     object form; admitted upstreams go to the tail by default so the
  //     surviving pool keeps priority — let `sortByScore` reorder if a
  //     reserve upstream is genuinely better. Already-present upstreams (by
  //     id) are not duplicated.
  //
  //   condition: boolean OR (upstreams, ctx) => boolean. The function form
  //     receives the CURRENT chain array — the pool that survived earlier
  //     steps — so it can ask aggregate ("network-level") questions about
  //     what is left, using native array methods over the existing
  //     per-upstream predicate factories:
  //
  //       // admit when EVERY survivor is lagging (empty pool also admits —
  //       // native `every` is true on []):
  //       .includeIf('tier:reserve', p => p.every(blockSecondsLagAbove(30)))
  //       // ...or when too few survive:
  //       .includeIf('tier:reserve', p => p.length < 2)
  //
  // Defensive: any malformed argument degrades to a no-op (returns the
  // chain unchanged) rather than throwing — a throw mid-eval would drop the
  // whole network back to the engine's default policy.
  define('includeIf', function (target, condition) {
    // 1. Resolve the target into selector facets + position. A string/array
    //    is the tag shorthand; an object carries explicit facets. Gating on
    //    the RESOLVED facets (not on "an object was passed") is what keeps
    //    `{}` / `{ position: 'head' }` a no-op instead of a match-all that
    //    admits the entire universe.
    let idPat, tagPat, vendorPat, typePat, position;
    if (typeof target === 'string' || Array.isArray(target)) {
      tagPat = target;
    } else if (target != null && typeof target === 'object') {
      idPat = target.id;
      tagPat = target.tag;
      vendorPat = target.vendor;
      typePat = target.type;
      position = target.position;
    }
    if (idPat == null && tagPat == null && vendorPat == null && typePat == null) {
      return this.slice();
    }

    // 2. Evaluate the gate. Function form gets (upstreams, ctx); a thrown
    //    predicate is swallowed (treated as "do not include") so one bad
    //    custom condition can't sink the eval.
    let pass;
    if (typeof condition === 'function') {
      try {
        pass = !!condition(this, globalThis.__policyCtx || {});
      } catch (_e) {
        pass = false;
      }
    } else {
      pass = !!condition;
    }
    if (!pass) return this.slice();

    // 3. Union in matching upstreams from the universe, deduped by id.
    const matchFn = function (u) {
      if (idPat     != null && !matchAny(idPat, u.id)) return false;
      if (tagPat    != null && !hasMatchingTag(u, tagPat)) return false;
      if (vendorPat != null && !matchAny(vendorPat, u.vendor)) return false;
      if (typePat   != null && !matchAny(typePat, u.type)) return false;
      return true;
    };
    const all = globalThis.__policyAllUpstreams || [];
    const haves = new Set(this.map(u => u.id));
    const adds = all.filter(u => !haves.has(u.id) && matchFn(u));
    if (adds.length === 0) return this.slice();
    return (position === 'head') ? adds.concat(this) : this.concat(adds);
  });

  // ─── 4.11 Combinators ───────────────────────────────────────────────────

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

  // when(mask, fn) — finality-conditional chain step. The `mask` is a
  // bitwise-OR of finality bit constants (REALTIME | UNFINALIZED |
  // FINALIZED | UNKNOWN). When the request's `ctx.finality` matches the
  // mask, `fn(this)` runs and its result becomes the chain value.
  // Otherwise the array passes through unchanged.
  //
  // Typical use: skip stickyPrimary for finalized reads (no consistency
  // requirement, so route freely on latency):
  //
  //   .when(REALTIME | UNFINALIZED | UNKNOWN,
  //     u => u.stickyPrimary({ scope: NETWORK }))
  //
  // The lambda receives `this` (the array) so chain methods can be
  // tacked on inside (`u => u.stickyPrimary(...)`); ctx is reachable
  // via `globalThis.__policyCtx` if the lambda needs it.
  //
  // Defensive: a non-function `fn` is a no-op (returns the array as-is)
  // rather than throwing — keeps the chain alive on a typo'd policy
  // instead of falling back to the default-policy at the engine level.
  define('when', function (mask, fn) {
    if (typeof fn !== 'function') return this.slice();
    const ctx = globalThis.__policyCtx || {};
    const bit = _finalityBit(ctx.finality);
    if ((Number(mask) & bit) === 0) return this.slice();
    return fn(this);
  });
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

  // ─── 4.13 Debug ─────────────────────────────────────────────────────────

  define('tap',      function (fn) { fn(this); return this; });
  define('label',    function (_name) { return this; }); // no-op for now; decision-record wiring in phase 6
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
  //   excludeIf(all(errorRateAbove(0.2), latencyAbove(99, 2000)),
  //             { outFor: '60s' })
  //
  // Authors can write their own factories in their policy file — they're
  // just functions that return `u => boolean`. Stdlib exports cover the
  // common signals; ad-hoc compounds stay inline as `u => ...`.

  // Every factory below stamps THREE pieces of metadata on the returned
  // closure:
  //   * `policyReason` — human-readable display string carrying the
  //     threshold (e.g. `errorRate>0.5`). Surfaced in DEBUG logs, the
  //     simulator's policy-history pane, and `Decision.Output.Excluded[i].Reason`.
  //   * `policySlug` — stable, threshold-free metric label (e.g.
  //     `error_rate_above`). Used by `erpc_selection_exclusion_total`'s
  //     `reason` label so cardinality stays bounded by the set of
  //     predicate factories (~25) rather than the powerset of thresholds.
  //   * `policyLeaves(u)` — given an upstream that the predicate trips
  //     on, returns the leaf-slug ARRAY for metric attribution. For base
  //     factories this is `[slug]`; for combinators it walks children.
  //     This is what powers option-(c) attribution: a compound like
  //     `any(errorRateAbove(0.5), latencyDeviationAbove(95, 4))` excluding
  //     an upstream attributes the exclusion to the leaf(s) that were
  //     actually true, not the boilerplate `any` wrapper.

  // _leaf builds a base predicate carrying both display reason and
  // metric slug. `policyLeaves(u)` returns `[slug]` when the predicate
  // trips on `u`, `[]` otherwise — `excludeIf` only calls it AFTER the
  // predicate already returned true, so the empty branch is defensive.
  //
  // `isGuard` marks "this predicate is a precondition, not a reason."
  // Guards (e.g. `samplesAbove(20)`) are used inside `all(...)` to gate
  // the REAL exclusion check on having enough signal — operationally
  // they describe sample-volume readiness, not health. When `all` /
  // `any` collect leaves for `erpc_selection_exclusion_total{reason}`,
  // guards are filtered out so the dashboard shows only the meaningful
  // reason (`latency_p95_deviation_pct_above`), not the noise
  // (`samples_above`). Guards fall back into attribution only if EVERY
  // tripping leaf is a guard — defensive so we never lose all signal.
  function _leaf(fn, reason, slug, isGuard) {
    fn.policyReason = reason;
    fn.policySlug = slug;
    fn.policyIsGuard = !!isGuard;
    fn.policyLeaves = function (u) { return fn(u) ? [slug] : []; };
    return fn;
  }

  // Rate-based factories (0..1 fractions).
  globalThis.errorRateAbove       = function (rate) { return _leaf(u => u.metrics.errorRate       > rate, 'errorRate>'       + rate, 'error_rate_above'); };
  globalThis.errorRateBelow       = function (rate) { return _leaf(u => u.metrics.errorRate       < rate, 'errorRate<'       + rate, 'error_rate_below'); };
  globalThis.throttleRateAbove    = function (rate) { return _leaf(u => u.metrics.throttledRate   > rate, 'throttledRate>'   + rate, 'throttle_rate_above'); };
  globalThis.throttleRateBelow    = function (rate) { return _leaf(u => u.metrics.throttledRate   < rate, 'throttledRate<'   + rate, 'throttle_rate_below'); };
  globalThis.misbehaviorRateAbove = function (rate) { return _leaf(u => u.metrics.misbehaviorRate > rate, 'misbehaviorRate>' + rate, 'misbehavior_rate_above'); };

  // Latency factory — millisecond threshold at any quantile.
  //
  // `ms` is the threshold (required); `quantile` is optional and
  // defaults to p70 — matches the quantile `sortByScore(PREFER_FASTEST)`
  // uses for ranking, so the exclusion axis and the rank axis agree
  // on what "fast" means. Quantile accepts `0..1` fractions (`0.95`)
  // or `0..100` numbers (`95`); both normalize inside
  // `u.metrics.latencyP`.
  //
  // Reads like English: `latencyAbove(30_000)` = "out if p70 > 30s";
  // `latencyAbove(10_000, 95)` = "out if p95 > 10s".
  globalThis.latencyAbove = function (ms, quantile) {
    if (quantile == null) quantile = 70;
    return _leaf(u => u.metrics.latencyP(quantile) > ms, 'p' + quantile + '>' + ms + 'ms', 'latency_p' + quantile + '_above');
  };

  // latencyDeviationAbove(multiplier, opts?) — trips when this
  // upstream's latency is significantly above the fastest peer's,
  // compared APPLES-TO-APPLES PER METHOD and then collapsed across
  // methods via the configured mode.
  //
  // Why per-method: each upstream's aggregate p<quantile> is a sample-
  // count-weighted percentile of WHATEVER methods landed in its
  // bucket. A primary with 95% fast eth_call traffic + 5% slow
  // eth_getLogs has an aggregate p70 of ~eth_call latency. A
  // runner-up that only ever sees hedge-fired eth_getLogs has an
  // aggregate p70 of ~eth_getLogs latency. They look 20-40× apart
  // even when their PER-METHOD latencies are identical — the
  // distribution skew is the entire delta. Per-method comparison
  // eliminates this bias.
  //
  // Resolution modes (when methods disagree):
  //   • 'geomean' (DEFAULT) — geometric mean of per-method ratios.
  //     Trips when the typical ratio across methods is ≥ multiplier.
  //     Self-protective against single-method outliers (one slow
  //     method out of many doesn't trip).
  //   • 'majority' — trips when ≥50% of compared methods show the
  //     upstream as ≥ multiplier× slower.
  //   • 'veto' — trips when ANY single method shows the upstream as
  //     ≥ multiplier× slower. Most aggressive: one bad method casts
  //     a vote-out against the upstream. False-positive prone on
  //     specialty methods (vendor good at most things, bad at one).
  //
  // The 2nd argument is polymorphic — pass a number for the common
  // "just change the quantile" case, or an options object for full
  // control:
  //
  //   `latencyDeviationAbove(3)`                              → geomean of p70 ratios > 3
  //   `latencyDeviationAbove(3, 95)`                          → geomean of p95 ratios > 3
  //   `latencyDeviationAbove(3, { mode: 'veto' })`            → trips on ANY method 3× slower
  //   `latencyDeviationAbove(3, { mode: 'majority', quantile: 90 })`
  //
  // Methods with no peer-data on either side are skipped (not
  // counted). Upstreams alone in the pool (no peers with data on the
  // same methods) never trip. Self is excluded from the per-method
  // fastest-peer computation, so a 2-pool with one slow upstream
  // detects the slow one against its peer correctly.
  function _latencyDeviationAbove(quantile, multiplier, mode, minMethodSamples, dampingMs) {
    const ups = globalThis.__policyAllUpstreams || [];
    // metricsByMethod entries expose raw quantile fields (no closure)
    // for performance — compute the right field name once. Snap to the
    // nearest pre-computed bucket, matching the closure-based
    // `latencyP` semantic on `u.metrics`.
    const q = quantile <= 50 ? 50 : quantile <= 70 ? 70 : quantile <= 90 ? 90 : quantile <= 95 ? 95 : 99;
    const field = 'p' + q + 'ms';
    // Pre-compute top-2 fastest p<quantile> PER METHOD across the
    // pool, so the per-upstream closure can pick "fastest PEER (not
    // me)" in O(1).
    //
    // Two safeguards apply before the ratio contributes to the
    // collapse:
    //
    //   • `minMethodSamples` (default 50) — methods with fewer
    //     samples than this are skipped entirely. The p<q> is too
    //     noisy at low n; multiple unstable methods otherwise
    //     conspire on the geomean and falsely-trip healthy
    //     upstreams.
    //
    //   • `dampingMs` (default 100) — exponential damping of the
    //     per-method ratio by THIS upstream's absolute latency:
    //         effective_ratio = raw_ratio × (1 − exp(−my / dampingMs))
    //     At my << dampingMs the ratio fades toward zero (the case
    //     where a 3× spread between 2ms and 6ms isn't human-
    //     noticeable). At my >> dampingMs the damping → 1 and the
    //     raw ratio takes over (a 3× spread between 200ms and 600ms
    //     is a real UX delta). No hard cutoff — graceful transition
    //     means a slightly-mis-tuned dampingMs degrades smoothly
    //     instead of flipping the predicate.
    //
    //     `dampingMs = 0` disables damping entirely; use sparingly,
    //     and only when you've already constrained the comparison
    //     to a regime where absolute latency differences matter.
    const topByMethod = {}; // method → { v1, id1, v2 }
    for (const u of ups) {
      if (!u) continue;
      const byMethod = u.metricsByMethod || {};
      for (const m in byMethod) {
        const mm = byMethod[m];
        if (minMethodSamples > 0 && mm.requestsTotal < minMethodSamples) continue;
        const v = mm[field];
        if (v <= 0) continue;
        const entry = topByMethod[m] || { v1: Infinity, id1: null, v2: Infinity };
        if (v < entry.v1) {
          entry.v2 = entry.v1;
          entry.v1 = v;
          entry.id1 = u.id;
        } else if (v < entry.v2) {
          entry.v2 = v;
        }
        topByMethod[m] = entry;
      }
    }
    return function (u) {
      if (!u) return false;
      const byMethod = u.metricsByMethod || {};
      let logSum = 0, geoCount = 0, slow = 0, compared = 0;
      for (const m in byMethod) {
        const mm = byMethod[m];
        if (minMethodSamples > 0 && mm.requestsTotal < minMethodSamples) continue;
        const my = mm[field];
        if (my <= 0) continue;                       // no signal on this method
        const top = topByMethod[m];
        if (!top) continue;                          // method filtered out by samples gate
        const fastestPeer = (top.id1 === u.id) ? top.v2 : top.v1;
        if (!isFinite(fastestPeer)) continue;        // alone in pool (after gate) on this method
        const rawRatio = my / fastestPeer;
        // Exponential damping by this upstream's absolute latency.
        // The closer `my` sits to 0, the closer the effective ratio
        // sits to 0 — sub-perceptible latency differences don't
        // contribute regardless of raw ratio. At my >> dampingMs the
        // damping factor → 1 and the raw ratio is preserved.
        const ratio = dampingMs > 0
          ? rawRatio * (1 - Math.exp(-my / dampingMs))
          : rawRatio;
        compared++;
        if (ratio > multiplier) slow++;
        if (mode === 'geomean' && ratio > 0) {
          logSum += Math.log(ratio);
          geoCount++;
        }
      }
      if (compared < 1) return false;
      switch (mode) {
        case 'veto':
          return slow > 0;
        case 'majority':
          return (slow / compared) >= 0.5;
        case 'geomean':
        default:
          if (geoCount < 1) return false;
          return Math.exp(logSum / geoCount) > multiplier;
      }
    };
  }
  // `multiplier` first, `optsOrQuantile` second-and-optional.
  // Accepts:
  //   • undefined → { quantile: 70, mode: 'geomean', minMethodSamples: 50, dampingMs: 30 }
  //   • a number  → quantile shorthand, defaults for the rest
  //   • an object → { quantile?, mode?, minMethodSamples?, dampingMs? }
  //
  // Defaults reflect the production lessons:
  //   • minMethodSamples=50 — per-method sample floor. Below this,
  //     the p<q> CI is too wide for cross-upstream comparison.
  //   • dampingMs=30 — exponential damping scale (ms). At my=30ms
  //     damping=0.63 (significant); at my=100ms damping=0.96 (raw
  //     ratio mostly passes through). The choice of 30ms keeps the
  //     comparison sensitive to genuinely slow upstreams (anything
  //     >100ms is full-weight) while still suppressing meaningless
  //     micro-differences (2ms vs 6ms damps to ~0.13× raw). Pair
  //     with a high multiplier (default 10 in the chain) so the
  //     mid-range still has plenty of breathing room. Set to 0 to
  //     disable damping.
  globalThis.latencyDeviationAbove = function (multiplier, optsOrQuantile) {
    let quantile = 70;
    let mode = 'geomean';
    let minMethodSamples = 50;
    let dampingMs = 30;
    if (typeof optsOrQuantile === 'number') {
      quantile = optsOrQuantile;
    } else if (optsOrQuantile && typeof optsOrQuantile === 'object') {
      if (optsOrQuantile.quantile != null) quantile = optsOrQuantile.quantile;
      if (optsOrQuantile.mode != null) mode = optsOrQuantile.mode;
      if (optsOrQuantile.minMethodSamples != null) minMethodSamples = optsOrQuantile.minMethodSamples;
      if (optsOrQuantile.dampingMs != null) dampingMs = optsOrQuantile.dampingMs;
    }
    return _leaf(_latencyDeviationAbove(quantile, multiplier, mode, minMethodSamples, dampingMs),
      'p' + quantile + '>' + multiplier + 'xFastest(' + mode + ')',
      'latency_p' + quantile + '_deviation_above');
  };

  // Lag-based factories — block-count thresholds.
  globalThis.blockNumberLagAbove  = function (blocks) { return _leaf(u => u.metrics.blockHeadLag    > blocks, 'blockHeadLag>'    + blocks, 'block_head_lag_above'); };
  globalThis.finalizationLagAbove = function (blocks) { return _leaf(u => u.metrics.finalizationLag > blocks, 'finalizationLag>' + blocks, 'finalization_lag_above'); };

  // Time-based lag — block-count × network's EMA-estimated block time.
  // Threshold is in SECONDS. Returns 0 (predicate always false) until the
  // tracker has enough block-time samples; on Eth mainnet that's a few
  // seconds after first traffic. Useful when you want a wall-clock SLO
  // ("trip if more than 60s behind tip") instead of a chain-relative
  // block count that means different things on different chains.
  globalThis.blockSecondsLagAbove         = function (seconds) { return _leaf(u => u.metrics.blockHeadLagSeconds    > seconds, 'blockHeadLagSeconds>'    + seconds, 'block_head_lag_seconds_above'); };
  globalThis.finalizationSecondsLagAbove  = function (seconds) { return _leaf(u => u.metrics.finalizationLagSeconds > seconds, 'finalizationLagSeconds>' + seconds, 'finalization_lag_seconds_above'); };

  // Sample-size guard — useful as an AND-term to avoid tripping rules on
  // low-sample-count noise (when an upstream has only had a handful of
  // requests, a single error blows up errorRate to 100%).
  // samplesAbove / samplesBelow are GUARDS — they answer "do we have
  // enough signal yet?" not "is this upstream unhealthy?". Marked as
  // such so `all(samplesAbove(20), latencyDeviationAbove(85, 4))` doesn't
  // bleed `samples_above` into the `reason` label of the per-exclusion
  // metric (operators reading the dashboard saw `samples_above` next
  // to `latency_p85_deviation_above` and rightly wondered what
  // "had enough samples" was supposed to mean as a cause).
  globalThis.samplesBelow = function (n) { return _leaf(u => u.metrics.requestsTotal < n, 'samples<' + n, 'samples_below', /* isGuard */ true); };
  globalThis.samplesAbove = function (n) { return _leaf(u => u.metrics.requestsTotal > n, 'samples>' + n, 'samples_above', /* isGuard */ true); };

  // Logical combinators — flat, variadic. Predicates compose freely:
  //   excludeIf(all(errorRateAbove(0.3), not(samplesBelow(10))))
  // means "trip if errorRate>0.3 AND we actually have enough samples".
  //
  // The composed predicate's:
  //   * `policyReason` joins child reasons so the simulator UI / DEBUG
  //     logs still read clearly ("all(errorRate>0.3,not(samples<10))").
  //   * `policyLeaves(u)` returns the leaf slugs that were ACTUALLY
  //     responsible for the trip on this specific upstream, so the
  //     `erpc_selection_exclusion_total{reason}` metric attributes
  //     exclusion to the underlying signal instead of the combinator.
  //       - `any(A,B)`: leaves = slugs of every predicate that was true
  //       - `all(A,B)`: leaves = slugs of every predicate (all true by definition)
  //       - `not(X)`:   leaves = `["not_" + X.slug]` (or just the slug
  //                     if X is itself a `not(_)` — double-negation
  //                     collapses to the original signal)
  function _reasonFor(pred) { return (pred && pred.policyReason) || '?'; }
  function _leavesFromPred(p, u) {
    if (p && typeof p.policyLeaves === 'function') return p.policyLeaves(u);
    return ['custom'];
  }
  // _collectLeaves splits children into (guard, real) buckets so the
  // caller can prefer "real" leaves (the actual reason) over guards
  // (preconditions like `samplesAbove`). Used by `all()` / `any()` to
  // keep the exclusion `reason` label meaningful — `latency_p95_…`
  // surfaces, `samples_above` doesn't pollute it.
  function _collectLeaves(preds, u, filter) {
    const guards = [];
    const real = [];
    for (const p of preds) {
      if (filter && !p(u)) continue;
      const leaves = _leavesFromPred(p, u);
      if (p && p.policyIsGuard) {
        for (const l of leaves) guards.push(l);
      } else {
        for (const l of leaves) real.push(l);
      }
    }
    // Defensive: if every contributing leaf was a guard, fall back to
    // the guard slugs rather than emit no attribution. Shouldn't happen
    // for well-formed policies (a chain of pure guards has no meaningful
    // exclusion criterion) but better than a silent metric drop.
    return real.length > 0 ? real : guards;
  }
  globalThis.all = function () {
    const preds = Array.prototype.slice.call(arguments);
    const fn = function (u) { return preds.every(p => p(u)); };
    fn.policyReason = 'all(' + preds.map(_reasonFor).join(',') + ')';
    fn.policySlug = 'all';
    fn.policyLeaves = function (u) {
      // AND-semantics: when `all` trips, every child returned true so
      // we attribute to every NON-GUARD leaf (the "real" reasons).
      // `filter=false` because no per-pred truth check is needed —
      // they all tripped to get us here.
      return _collectLeaves(preds, u, /* filter */ false);
    };
    return fn;
  };
  globalThis.any = function () {
    const preds = Array.prototype.slice.call(arguments);
    const fn = function (u) { return preds.some(p => p(u)); };
    fn.policyReason = 'any(' + preds.map(_reasonFor).join(',') + ')';
    fn.policySlug = 'any';
    fn.policyLeaves = function (u) {
      // OR-semantics: only the leaves that ACTUALLY evaluated true
      // contributed to the trip — filter to those, then prefer
      // non-guard ones for the exclusion label.
      return _collectLeaves(preds, u, /* filter */ true);
    };
    return fn;
  };
  globalThis.not = function (pred) {
    const fn = function (u) { return !pred(u); };
    fn.policyReason = 'not(' + _reasonFor(pred) + ')';
    const childSlug = (pred && pred.policySlug) || 'custom';
    fn.policySlug = 'not_' + childSlug;
    fn.policyLeaves = function (_u) {
      // NOT inverts truth, so the "leaf that tripped" is the negation of
      // the child's slug. Use that as the attribution. We do NOT recurse
      // into child leaves — if the child is itself a compound, attributing
      // to "not_all" is more honest than picking one of the inner leaves
      // and pretending it caused the not.
      return [fn.policySlug];
    };
    return fn;
  };
})();
