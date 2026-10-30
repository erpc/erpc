// Consensus-policy selector stdlib (v1). Installed once per VM.
//
// The Go side hands the eval context in as plain native objects/arrays (so the
// standard Array methods are available), then wraps it with
// __erpcWrapConsensusCtx before calling the operator function. The wrap
// gives ctx.upstreams a small chainable collection API; ctx.user.auth is a
// plain string (auth strategy name) for operator string compares.
//
// anyPunished / anyOperatorCordon scan punishedOrOperator (the network's
// punishment ∪ operator cordons), not the candidate items — selection may
// already have dropped those nodes via removeCordoned.
//
// Grow this surface only when an observed operator config forces it.
(function () {
  "use strict";
  if (globalThis.__erpcWrapConsensusCtx) return;

  function hasClass(u, cls) {
    var h = u && u.health;
    if (!h || !h.cordonClasses) return false;
    return h.cordonClasses.indexOf(cls) !== -1;
  }

  // A member is "unavailable" for allUnavailable() only when it is either
  // unhealthy-but-uncordoned, or cordoned by availability and nothing else.
  // Any punishment/operator (or any future) cordon class makes it false, which
  // is what keeps allUnavailable() a whitelist of availability.
  function availabilityUnavailable(u) {
    var h = (u && u.health) || {};
    var cc = h.cordonClasses || [];
    if (cc.length === 0) return h.state === "unhealthy";
    for (var i = 0; i < cc.length; i++) {
      if (cc[i] !== "availability") return false;
    }
    return true;
  }

  // canServeBlock mirrors the block-availability guard: it excludes an upstream
  // only on positive evidence its range rules the block out. An unknown block
  // (null/undefined/non-number — the same "unknown block" case the request
  // carries for tx-hash/block-hash lookups) is no such evidence, so the
  // upstream is kept rather than excluded by numeric coercion of null to 0.
  // Likewise an upstream with no configured range is kept (attempt allowed); a
  // configured range must contain n (bounds inclusive, a null bound is open on
  // that side).
  function canServe(u, n) {
    if (typeof n !== "number") return true;
    var ba = u && u.blockAvailability;
    if (!ba) return true;
    if (ba.lower !== null && ba.lower !== undefined && n < ba.lower) return false;
    if (ba.upper !== null && ba.upper !== undefined && n > ba.upper) return false;
    return true;
  }

  // items = post-selection candidates (or a filtered view of them);
  // punishedOrOperator = fail-closed scan list. Chained filters (withTag /
  // healthy / canServeBlock) narrow BOTH lists so anyPunished /
  // anyOperatorCordon on a filtered collection only see matching nodes —
  // feature §7.2: internals.anyPunished() must not pin when only an
  // external is punished. The unfiltered ctx.upstreams.anyPunished() still
  // scans the full network pin list.
  function makeUpstreams(items, punishedOrOperator) {
    var arr = items || [];
    var pin = punishedOrOperator || [];
    return {
      items: arr,
      get length() {
        return arr.length;
      },
      withTag: function (t) {
        function hasTag(u) {
          return u.tags && u.tags.indexOf(t) !== -1;
        }
        return makeUpstreams(arr.filter(hasTag), pin.filter(hasTag));
      },
      healthy: function () {
        function isHealthy(u) {
          return u.health && u.health.state === "healthy";
        }
        return makeUpstreams(arr.filter(isHealthy), pin.filter(isHealthy));
      },
      anyPunished: function () {
        return pin.some(function (u) {
          return hasClass(u, "punishment");
        });
      },
      anyOperatorCordon: function () {
        return pin.some(function (u) {
          return hasClass(u, "operator");
        });
      },
      allUnavailable: function () {
        if (arr.length === 0) return false;
        return arr.every(availabilityUnavailable);
      },
      canServeBlock: function (n) {
        function serves(u) {
          return canServe(u, n);
        }
        return makeUpstreams(arr.filter(serves), pin.filter(serves));
      },
    };
  }

  function makeUser(raw) {
    if (!raw) return null;
    return { auth: raw.auth || "" };
  }

  globalThis.__erpcWrapConsensusCtx = function (raw) {
    raw = raw || {};
    return {
      request: raw.request || {},
      user: makeUser(raw.user),
      // punishedOrOperator is NOT exposed on ctx — only consumed by the
      // anyPunished / anyOperatorCordon helpers on ctx.upstreams.
      upstreams: makeUpstreams(raw.upstreams, raw.punishedOrOperator),
    };
  };
})();
