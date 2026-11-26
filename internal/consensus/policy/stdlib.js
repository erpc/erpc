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
// noneSelected intersects the filtered network view with the optional
// `selected` list (post-selection candidates) by upstream id — so a
// tag-scoped noneSelected() asks whether that tag still has anyone in this
// round after lag / diversity / ensureMin exclusion.
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

  // items = network snapshot (or a filtered view) for health helpers;
  // punishedOrOperator = fail-closed scan list (narrowed by withTag/…).
  // selectedAll = full post-selection candidate list — never narrowed by
  // chain filters; noneSelected() intersects by upstream id against `arr`.
  // selectedKnown is false when the bridge omitted `selected` — noneSelected
  // must stay false rather than treat missing wire-up as an empty round.
  //
  // Chained filters (withTag / healthy / canServeBlock) narrow items + pin
  // so tag-scoped helpers only see matching nodes — feature §7.2:
  // primaries.anyPunished() must not pin when only a secondary is punished.
  function makeUpstreams(items, punishedOrOperator, selectedAll, selectedKnown) {
    var arr = items || [];
    var pin = punishedOrOperator || [];
    var sel = selectedAll || [];
    return {
      items: arr,
      get length() {
        return arr.length;
      },
      withTag: function (t) {
        function hasTag(u) {
          return u.tags && u.tags.indexOf(t) !== -1;
        }
        return makeUpstreams(arr.filter(hasTag), pin.filter(hasTag), sel, selectedKnown);
      },
      healthy: function () {
        function isHealthy(u) {
          return u.health && u.health.state === "healthy";
        }
        return makeUpstreams(arr.filter(isHealthy), pin.filter(isHealthy), sel, selectedKnown);
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
      // True when the set is non-empty and every member holds an operator
      // (admin) cordon. Scans candidate items (network snapshot when wired),
      // so selection-dropped operator cordons remain visible via withTag —
      // unlike anyOperatorCordon (pin list, "any"), this is the deliberate
      // full-tag drain signal for an opt-in looser named policy. False on an
      // empty set (same as allUnavailable).
      allOperatorCordoned: function () {
        if (arr.length === 0) return false;
        return arr.every(function (u) {
          return hasClass(u, "operator");
        });
      },
      // True when the filtered network set is non-empty and none of those
      // members' ids appear in the post-selection candidate list (lag /
      // diversity / ensureMin exclusion). False on an empty network set
      // (config error) and false when the bridge never stamped `selected`.
      noneSelected: function () {
        if (!selectedKnown) return false;
        if (arr.length === 0) return false;
        // Prototype-free map: an upstream id like "toString" must not hit
        // Object.prototype and falsely look selected.
        var ids = Object.create(null);
        for (var i = 0; i < sel.length; i++) {
          if (sel[i] && sel[i].id) ids[sel[i].id] = true;
        }
        for (var j = 0; j < arr.length; j++) {
          if (arr[j] && arr[j].id && ids[arr[j].id]) return false;
        }
        return true;
      },
      canServeBlock: function (n) {
        function serves(u) {
          return canServe(u, n);
        }
        return makeUpstreams(arr.filter(serves), pin.filter(serves), sel, selectedKnown);
      },
    };
  }

  function makeUser(raw) {
    if (!raw) return null;
    return { auth: raw.auth || "" };
  }

  globalThis.__erpcWrapConsensusCtx = function (raw) {
    raw = raw || {};
    var selectedKnown = Object.prototype.hasOwnProperty.call(raw, "selected");
    return {
      request: raw.request || {},
      user: makeUser(raw.user),
      // punishedOrOperator / selected are NOT exposed on ctx — only consumed
      // by helpers on ctx.upstreams.
      upstreams: makeUpstreams(
        raw.upstreams,
        raw.punishedOrOperator,
        raw.selected,
        selectedKnown
      ),
    };
  };
})();
