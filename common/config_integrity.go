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
	// ReorgWindow is how many blocks back from the tip the integrity ChainView
	// keeps a number→hash pin + header and tracks reorgs (default 32). Raise for
	// deep-reorg chains (e.g. polygon 256).
	ReorgWindow int `yaml:"reorgWindow,omitempty" json:"reorgWindow,omitempty"`
	// ObserveOnly runs every enabled check but never lets a verdict touch the
	// response: violations that WOULD have been rejected are recorded with the
	// outcome "would_reject" and served anyway. This is the safe way to enable
	// integrity on a network for the first time — it reveals both bad upstream
	// data AND the module's own gaps on that chain at zero request risk, and
	// the would_reject rate is exactly the client-facing cost enforcement
	// would incur.
	//
	// It is ABSOLUTE and overrides everything else, including a per-check
	// onFailure: reject and invalidBehavior — so a future release adding a new
	// check cannot start rejecting on an observe-only network. Deterministic
	// checks are covered too, which invalidBehavior alone cannot do (they
	// ignore it by design).
	ObserveOnly *bool `yaml:"observeOnly,omitempty" json:"observeOnly,omitempty"`
	// StateProbe proves, per upstream, that the node actually holds the state
	// trie it claims: on each new followed block, probe every upstream with an
	// execution-context call (and eth_getProof where supported) verified
	// against the follower's verified header, and advance that upstream's
	// state-proven head only on success. Routing for state methods (eth_call,
	// eth_getBalance, ...) then refuses to outrun proof. Requires follow to be
	// enabled (the verified header is the trust anchor). Off by default.
	StateProbe *IntegrityStateProbeConfig `yaml:"stateProbe,omitempty" json:"stateProbe,omitempty"`
	// Follow turns the ChainView into an actual chain follower: it walks the
	// chain forward block by block, requires each block to name its
	// predecessor's hash, and reconciles reorgs by finding the common ancestor.
	// Off by default — it costs one eth_getBlockByNumber per block per network.
	Follow *IntegrityFollowConfig `yaml:"follow,omitempty" json:"follow,omitempty"`
	// MisbehaviorsDestination durably archives every integrity catch (reject or
	// soft-flag) as a JSONL record — same file/S3 destination shape as the
	// consensus policy's misbehaviorsDestination. Catches are rare, so the
	// volume is low; the archive is the forensic record metrics can't carry.
	MisbehaviorsDestination *MisbehaviorsDestinationConfig `yaml:"misbehaviorsDestination,omitempty" json:"misbehaviorsDestination,omitempty"`
}

// IntegrityFollowConfig configures the ChainView chain follower.
//
// Following gives the module a CONTIGUOUS, parent-linked view of the chain
// instead of the sparse scatter of heights that client traffic happens to
// touch. That is what lets it answer "is this block canonical" from a verified
// chain rather than from whichever source was observed first, and it is the
// precondition for any check that compares consecutive headers.
type IntegrityFollowConfig struct {
	// Enabled turns the follower on for this network. Default false.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Interval is how often to check for new blocks (default 1s). Keep it below
	// the chain's block time or the follower will run a step behind.
	Interval Duration `yaml:"interval,omitempty" json:"interval" tstype:"Duration"`
	// MaxBlocksPerTick bounds catch-up work per tick (default 16), so a
	// follower starting far behind converges steadily instead of bursting.
	MaxBlocksPerTick int `yaml:"maxBlocksPerTick,omitempty" json:"maxBlocksPerTick,omitempty"`
}

// IntegrityStateProbeConfig configures the per-upstream state-trie probes.
type IntegrityStateProbeConfig struct {
	// Enabled turns the probes on. Default false.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Interval is the minimum time between probes of the same upstream
	// (default 2s) — a bound on probe cost, not a schedule; probes fire on
	// followed-block arrival.
	Interval Duration `yaml:"interval,omitempty" json:"interval" tstype:"Duration"`
}

func (c *IntegrityStateProbeConfig) IsEnabled() bool {
	return c != nil && c.Enabled != nil && *c.Enabled
}

func (c *IntegrityStateProbeConfig) Copy() *IntegrityStateProbeConfig {
	if c == nil {
		return nil
	}
	cp := *c
	if c.Enabled != nil {
		v := *c.Enabled
		cp.Enabled = &v
	}
	return &cp
}

func (c *IntegrityFollowConfig) Copy() *IntegrityFollowConfig {
	if c == nil {
		return nil
	}
	cp := *c
	if c.Enabled != nil {
		v := *c.Enabled
		cp.Enabled = &v
	}
	return &cp
}

// IsEnabled reports whether the follower should run (nil config = off).
func (c *IntegrityFollowConfig) IsEnabled() bool {
	return c != nil && c.Enabled != nil && *c.Enabled
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
	if over.Follow != nil {
		out.Follow = over.Follow
	}
	if over.StateProbe != nil {
		out.StateProbe = over.StateProbe
	}
	if over.ReorgWindow != 0 {
		out.ReorgWindow = over.ReorgWindow
	}
	if over.ObserveOnly != nil {
		out.ObserveOnly = over.ObserveOnly
	}
	if over.MisbehaviorsDestination != nil {
		out.MisbehaviorsDestination = over.MisbehaviorsDestination
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
		ReorgWindow:     c.ReorgWindow,
		ObserveOnly:     c.ObserveOnly,
		Follow:          c.Follow.Copy(),
		StateProbe:      c.StateProbe.Copy(),
		// Shared, not deep-copied: destination configs are read-only after load.
		MisbehaviorsDestination: c.MisbehaviorsDestination,
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
