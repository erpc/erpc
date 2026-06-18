package common

// Integrity header-control modes (IntegrityConfig.HeaderMode).
const (
	// IntegrityHeaderModeOff ignores per-request integrity headers entirely.
	IntegrityHeaderModeOff = "off"
	// IntegrityHeaderModeProfiles lets a request select only a named profile.
	IntegrityHeaderModeProfiles = "profiles"
	// IntegrityHeaderModeFull lets a request set a level / per-check overrides.
	IntegrityHeaderModeFull = "full"
)

// IntegritySettings is the reusable body of an integrity configuration: the
// front-door level plus the axes (checks / budget) and the per-finality
// verdict. The top-level/per-network blocks and every named profile share this
// shape — only IntegrityConfig adds the header surface and profiles on top.
type IntegritySettings struct {
	// Level is the front-door preset: off | intrinsic | corroborated | authoritative.
	Level string `yaml:"level,omitempty" json:"level,omitempty"`
	// Checks overrides individual checks by their catalog id (enable/disable,
	// params, per-check failure mode).
	Checks map[string]*IntegrityCheckConfig `yaml:"checks,omitempty" json:"checks,omitempty"`
	// Budget caps the canonical force-fetches the authoritative tier issues.
	Budget *IntegrityBudgetConfig `yaml:"budget,omitempty" json:"budget,omitempty"`
	// InvalidBehavior is the per-finality verdict for reorg-sensitive checks,
	// where invalid data is ambiguously a node bug or a reorg. Deterministic
	// checks ignore it and always reject.
	InvalidBehavior *IntegrityInvalidBehaviorConfig `yaml:"invalidBehavior,omitempty" json:"invalidBehavior,omitempty"`
}

// IntegrityConfig is an IntegritySettings plus the per-request header-control
// surface and the named profiles a request may select. It lives at the project
// level (applies to all networks) and the network level (overrides).
type IntegrityConfig struct {
	IntegritySettings `yaml:",inline" json:",inline"`
	// HeaderMode controls whether/how per-request X-ERPC-Integrity headers may
	// adjust integrity: off | profiles | full. Defaults to off.
	HeaderMode string `yaml:"headerMode,omitempty" json:"headerMode,omitempty"`
	// Profiles are named settings an operator defines; a request may select one
	// by name via header when HeaderMode permits.
	Profiles map[string]*IntegritySettings `yaml:"profiles,omitempty" json:"profiles,omitempty"`
}

// IntegrityCheckConfig overrides a single check by its catalog id.
type IntegrityCheckConfig struct {
	// Enabled forces the check on/off; nil leaves the level's decision intact.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Params are check-specific knobs (e.g. bloom equality-vs-superset).
	Params map[string]string `yaml:"params,omitempty" json:"params,omitempty"`
	// OnFailure overrides this check's failure mode: reject | soft-flag.
	OnFailure string `yaml:"onFailure,omitempty" json:"onFailure,omitempty"`
}

// IntegrityBudgetConfig caps the authoritative tier's canonical fetches.
type IntegrityBudgetConfig struct {
	MaxPerSecond  int `yaml:"maxPerSecond,omitempty" json:"maxPerSecond,omitempty"`
	MaxConcurrent int `yaml:"maxConcurrent,omitempty" json:"maxConcurrent,omitempty"`
}

// IntegrityInvalidBehaviorConfig is the per-finality verdict for reorg-sensitive
// checks: reject | soft-flag | off, split by whether the response's block is
// finalized.
type IntegrityInvalidBehaviorConfig struct {
	Finalized   string `yaml:"finalized,omitempty" json:"finalized,omitempty"`
	Unfinalized string `yaml:"unfinalized,omitempty" json:"unfinalized,omitempty"`
}

// MergeIntegrityConfig overlays over (e.g. a network block) onto base (e.g. the
// project block): set fields in over win; maps union with over winning per key.
// Either argument may be nil. The result is always a fresh deep copy, so callers
// may keep it without aliasing the inputs.
func MergeIntegrityConfig(base, over *IntegrityConfig) *IntegrityConfig {
	if base == nil {
		return over.Copy()
	}
	if over == nil {
		return base.Copy()
	}
	out := base.Copy()

	if over.Level != "" {
		out.Level = over.Level
	}
	if over.Budget != nil {
		out.Budget = over.Budget.Copy()
	}
	if over.InvalidBehavior != nil {
		out.InvalidBehavior = over.InvalidBehavior.Copy()
	}
	for id, c := range over.Checks {
		if out.Checks == nil {
			out.Checks = make(map[string]*IntegrityCheckConfig, len(over.Checks))
		}
		out.Checks[id] = c.Copy()
	}

	if over.HeaderMode != "" {
		out.HeaderMode = over.HeaderMode
	}
	for name, p := range over.Profiles {
		if out.Profiles == nil {
			out.Profiles = make(map[string]*IntegritySettings, len(over.Profiles))
		}
		out.Profiles[name] = p.Copy()
	}
	return out
}

// --- deep copies (match the per-sub-config Copy() convention in config.go) ---

func (c *IntegritySettings) Copy() *IntegritySettings {
	if c == nil {
		return nil
	}
	copied := &IntegritySettings{
		Level:           c.Level,
		Budget:          c.Budget.Copy(),
		InvalidBehavior: c.InvalidBehavior.Copy(),
	}
	if c.Checks != nil {
		copied.Checks = make(map[string]*IntegrityCheckConfig, len(c.Checks))
		for k, v := range c.Checks {
			copied.Checks[k] = v.Copy()
		}
	}
	return copied
}

func (c *IntegrityConfig) Copy() *IntegrityConfig {
	if c == nil {
		return nil
	}
	copied := &IntegrityConfig{HeaderMode: c.HeaderMode}
	if s := c.IntegritySettings.Copy(); s != nil {
		copied.IntegritySettings = *s
	}
	if c.Profiles != nil {
		copied.Profiles = make(map[string]*IntegritySettings, len(c.Profiles))
		for k, v := range c.Profiles {
			copied.Profiles[k] = v.Copy()
		}
	}
	return copied
}

func (c *IntegrityCheckConfig) Copy() *IntegrityCheckConfig {
	if c == nil {
		return nil
	}
	copied := &IntegrityCheckConfig{OnFailure: c.OnFailure}
	if c.Enabled != nil {
		e := *c.Enabled
		copied.Enabled = &e
	}
	if c.Params != nil {
		copied.Params = make(map[string]string, len(c.Params))
		for k, v := range c.Params {
			copied.Params[k] = v
		}
	}
	return copied
}

func (c *IntegrityBudgetConfig) Copy() *IntegrityBudgetConfig {
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}

func (c *IntegrityInvalidBehaviorConfig) Copy() *IntegrityInvalidBehaviorConfig {
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}
