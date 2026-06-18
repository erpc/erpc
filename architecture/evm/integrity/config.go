package integrity

// CheckConfig is the resolved state of a single check: whether it runs and the
// parameters it runs with. Parameters are string-keyed so the same shape serves
// config, the generic header grammar, and chain profiles uniformly; each check
// interprets only the keys it understands.
type CheckConfig struct {
	Enabled bool
	Params  map[string]string
	// Data carries structured inputs a check needs that don't fit string
	// params — e.g. ground-truth transactions for cross-validation, supplied by
	// the caller (library mode) or, later, by the authoritative resolver. The
	// consuming check type-asserts it.
	Data any
	// FailOverride, when non-nil, overrides this check's default verdict on a
	// violation. The default is the class verdict: Deterministic → reject,
	// ReorgSensitive → per-finality (the ReorgPolicy). Set from a per-check
	// onFailure config; lets an operator soft-flag a check that would otherwise
	// reject (e.g. for a legitimate chain quirk) or vice versa.
	FailOverride *Behavior
}

// CheckSet is the resolved configuration for a request: check id -> CheckConfig.
// It is produced by the caller (from a level preset, request directives, the
// generic header, and the chain profile) and consumed by Validate. The engine
// does not know or care how it was assembled.
type CheckSet map[string]CheckConfig

// For returns the resolved config for a check id; a missing id is disabled.
func (s CheckSet) For(id string) CheckConfig {
	if s == nil {
		return CheckConfig{}
	}
	return s[id]
}

// Enable turns a check on (with optional parameters) and returns the set for
// fluent assembly by adapters.
func (s CheckSet) Enable(id string, params map[string]string) CheckSet {
	s[id] = CheckConfig{Enabled: true, Params: params}
	return s
}

// param returns a string parameter or a default.
func (c CheckConfig) param(key, def string) string {
	if c.Params == nil {
		return def
	}
	if v, ok := c.Params[key]; ok {
		return v
	}
	return def
}

// boolParam returns a boolean parameter or a default, accepting the same
// truthy/falsey spellings as the header grammar.
func (c CheckConfig) boolParam(key string, def bool) bool {
	switch c.param(key, "") {
	case "true", "on", "1", "yes":
		return true
	case "false", "off", "0", "no":
		return false
	default:
		return def
	}
}
