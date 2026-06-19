package evm

import (
	"strings"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
)

// compileIntegritySettings turns a resolved integrity configuration into the
// engine's inputs: the level preset, narrowed/widened by per-check overrides,
// plus the per-finality ReorgPolicy. nil settings → no checks, default policy.
//
// This is the single bridge from the common config vocabulary to the integrity
// engine; it keeps the integrity package free of config types.
func compileIntegritySettings(s *common.IntegritySettings) (integrity.CheckSet, integrity.ReorgPolicy) {
	policy := integrity.DefaultReorgPolicy()
	if s == nil {
		return integrity.CheckSet{}, policy
	}

	cs := integrity.CheckSetForLevel(integrity.Level(s.Level))
	for id, oc := range s.Checks {
		if oc != nil {
			applyCheckOverride(cs, id, oc)
		}
	}

	if ib := s.InvalidBehavior; ib != nil {
		if b, ok := parseBehavior(ib.Finalized); ok {
			policy.Finalized = b
		}
		if b, ok := parseBehavior(ib.Unfinalized); ok {
			policy.Unfinalized = b
		}
	}
	return cs, policy
}

// applyCheckOverride mutates cs for one per-check override: enable/disable,
// parameters, and an optional per-check failure mode.
func applyCheckOverride(cs integrity.CheckSet, id string, oc *common.IntegrityCheckConfig) {
	switch {
	case oc.Enabled != nil && !*oc.Enabled:
		delete(cs, id) // explicit off wins, regardless of level
		return
	case oc.Enabled != nil && *oc.Enabled:
		cs.Enable(id, oc.Params) // turn on above the level (or override params)
	default:
		// No explicit enable flag: only act if the level already enabled it.
		if !cs.For(id).Enabled {
			return
		}
		if len(oc.Params) > 0 {
			cs.Enable(id, oc.Params)
		}
	}

	if b, ok := parseBehavior(oc.OnFailure); ok {
		cfg := cs[id]
		cfg.FailOverride = &b
		cs[id] = cfg
	}
}

// resolveIntegrity computes the effective CheckSet and ReorgPolicy for a request.
// The network's integrity config is the single source: its level/profiles plus
// the per-request header selector. With no config, nothing runs (opt-in).
func resolveIntegrity(n common.Network, dirs *common.RequestDirectives) (integrity.CheckSet, integrity.ReorgPolicy) {
	// Opt-in: with no integrity config, nothing runs.
	if n == nil || n.Config() == nil || n.Config().Integrity == nil {
		return nil, integrity.ReorgPolicy{}
	}
	selector := ""
	if dirs != nil {
		selector = dirs.IntegritySelector
	}
	return compileIntegritySettings(resolveRequestSettings(n.Config().Integrity, selector))
}

// resolveRequestSettings computes the effective settings for one request: the
// configured base, with the per-request header selector overlaid when headerMode
// permits. In profiles mode a request may only select a named profile; in full
// mode it may also set a level word; off ignores the selector.
func resolveRequestSettings(cfg *common.IntegrityConfig, selector string) *common.IntegritySettings {
	if cfg == nil {
		return nil
	}
	base := cfg.IntegritySettings.Copy()
	if base == nil {
		base = &common.IntegritySettings{}
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return base
	}
	switch strings.ToLower(strings.TrimSpace(cfg.HeaderMode)) {
	case common.IntegrityHeaderModeProfiles:
		overlaySettings(base, cfg.Profiles[selector])
	case common.IntegrityHeaderModeFull:
		if isIntegrityLevel(selector) {
			base.Level = strings.ToLower(selector)
		} else {
			overlaySettings(base, cfg.Profiles[selector])
		}
	}
	return base
}

// overlaySettings merges over's set (non-zero) fields onto base. Reused for both
// project⊕network precedence and applying a selected profile.
func overlaySettings(base, over *common.IntegritySettings) {
	if base == nil || over == nil {
		return
	}
	if over.Level != "" {
		base.Level = over.Level
	}
	if over.Budget != nil {
		base.Budget = over.Budget.Copy()
	}
	if over.InvalidBehavior != nil {
		base.InvalidBehavior = over.InvalidBehavior.Copy()
	}
	for id, c := range over.Checks {
		if base.Checks == nil {
			base.Checks = make(map[string]*common.IntegrityCheckConfig, len(over.Checks))
		}
		base.Checks[id] = c.Copy()
	}
}

func isIntegrityLevel(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "intrinsic", "corroborated", "authoritative":
		return true
	}
	return false
}

// parseBehavior maps the config/header vocabulary (reject | soft-flag | off) to
// an engine Behavior. ok=false when the string is empty/unrecognized so callers
// keep their default.
func parseBehavior(s string) (integrity.Behavior, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "reject", "error", "hard-fail":
		return integrity.BehaviorError, true
	case "soft-flag", "softflag", "record", "warn":
		return integrity.BehaviorRecord, true
	case "off", "ignore", "none":
		return integrity.BehaviorIgnore, true
	default:
		return integrity.BehaviorError, false
	}
}
