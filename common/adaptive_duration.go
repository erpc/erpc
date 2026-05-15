package common

import (
	"encoding/json"
	"fmt"
	"time"
)

// parseJSONDuration accepts a raw JSON value that's either a string
// ("500ms"), a number (milliseconds), or empty/null (returns zero).
func parseJSONDuration(raw json.RawMessage) (Duration, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	if raw[0] == '"' {
		var s string
		if err := SonicCfg.Unmarshal(raw, &s); err != nil {
			return 0, err
		}
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", s, err)
		}
		return Duration(parsed), nil
	}
	var f float64
	if err := SonicCfg.Unmarshal(raw, &f); err != nil {
		return 0, err
	}
	return Duration(time.Duration(f) * time.Millisecond), nil
}

// AdaptiveDuration describes a duration that may be static, derived from a
// per-method latency quantile, or both. It's the reusable building block
// for any failsafe knob that wants "fixed base + adaptive component
// clamped between min/max" semantics — currently consensus wait caps,
// with timeout/hedge supporting it as an alternative entry-point.
//
// Resolution rules:
//
//   final = Base + adaptive
//
// where `adaptive` is:
//   - `qt.GetQuantile(Quantile)` when Quantile > 0 and quantile data exists
//   - `Min` (the floor) when Quantile > 0 but quantile data is cold (no
//     observations yet) — this gives a sensible non-zero cap immediately
//     after boot
//   - `0` when Quantile is unset
//
// After `Base + adaptive`, the result is clamped to [Min, Max] when those
// are set. A nil or all-zero AdaptiveDuration returns 0 (the caller treats
// that as "no cap" / "disabled").
//
// Wire format accepts both shorthand and object form:
//
//	caps: 500ms                                   # shorthand: Base only
//	caps: { base: 500ms }                         # explicit Base
//	caps: { quantile: 0.5, min: 5ms, max: 1s }    # quantile with bounds
//	caps: { base: 100ms, quantile: 0.9, max: 2s } # combined
type AdaptiveDuration struct {
	Base     Duration `yaml:"base,omitempty" json:"base,omitempty" tstype:"Duration"`
	Quantile float64  `yaml:"quantile,omitempty" json:"quantile,omitempty"`
	Min      Duration `yaml:"min,omitempty" json:"min,omitempty" tstype:"Duration"`
	Max      Duration `yaml:"max,omitempty" json:"max,omitempty" tstype:"Duration"`
}

// IsZero reports whether the spec has no fields set (caller should
// treat as "not configured" / disabled).
func (d *AdaptiveDuration) IsZero() bool {
	if d == nil {
		return true
	}
	return d.Base == 0 && d.Quantile == 0 && d.Min == 0 && d.Max == 0
}

// Resolve computes the effective duration. qt may be nil (cold start);
// returns 0 when the spec is zero so callers can use it as a "disabled"
// signal.
//
// Min/Max only apply when Quantile > 0 — they're floor/ceiling for the
// adaptive component. Static configs (Quantile == 0) return Base
// unchanged, so a user-supplied scalar like "10ms" is honored exactly.
func (d *AdaptiveDuration) Resolve(qt QuantileTracker) time.Duration {
	if d == nil || d.IsZero() {
		return 0
	}

	if d.Quantile <= 0 {
		return d.Base.Duration()
	}

	var adaptive time.Duration
	if qt != nil {
		adaptive = qt.GetQuantile(d.Quantile)
	}
	if adaptive <= 0 {
		adaptive = d.Min.Duration()
	}

	v := d.Base.Duration() + adaptive

	if min := d.Min.Duration(); min > 0 && v < min {
		v = min
	}
	if max := d.Max.Duration(); max > 0 && v > max {
		v = max
	}
	return v
}

// Copy returns a deep copy. Safe to call on nil (returns nil).
func (d *AdaptiveDuration) Copy() *AdaptiveDuration {
	if d == nil {
		return nil
	}
	c := *d
	return &c
}

// validate ensures the spec is internally consistent. `field` is a
// dotted path for error messages (e.g. "upstream.failsafe.timeout.duration").
func (d *AdaptiveDuration) validate(field string) error {
	if d == nil {
		return nil
	}
	if d.Quantile < 0 || d.Quantile > 1 {
		return fmt.Errorf("%s.quantile must be between 0 and 1", field)
	}
	if d.Quantile > 0 && d.Base == 0 && d.Max == 0 {
		return fmt.Errorf("%s requires base or max when quantile is set", field)
	}
	if d.Quantile == 0 && d.Base == 0 && d.Min == 0 && d.Max == 0 {
		return fmt.Errorf("%s must specify at least one of base/quantile/min/max", field)
	}
	if d.Min > 0 && d.Max > 0 && d.Min > d.Max {
		return fmt.Errorf("%s.min must be <= %s.max", field, field)
	}
	return nil
}

// inheritFrom fills any zero field in d with the corresponding value
// from src. Used by SetDefaults to merge per-policy defaults without
// clobbering user-supplied values. Safe on nil src.
func (d *AdaptiveDuration) inheritFrom(src *AdaptiveDuration) {
	if d == nil || src == nil {
		return
	}
	if d.Base == 0 {
		d.Base = src.Base
	}
	if d.Quantile == 0 {
		d.Quantile = src.Quantile
	}
	if d.Min == 0 {
		d.Min = src.Min
	}
	if d.Max == 0 {
		d.Max = src.Max
	}
}

// UnmarshalYAML accepts either a scalar (string "500ms", number 500ms)
// or an object ({base, quantile, min, max}). Scalars populate Base.
func (d *AdaptiveDuration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err == nil {
		parsed, perr := time.ParseDuration(s)
		if perr == nil {
			d.Base = Duration(parsed)
			return nil
		}
	}
	var i int64
	if err := unmarshal(&i); err == nil {
		d.Base = Duration(time.Duration(i) * time.Millisecond)
		return nil
	}
	var f float64
	if err := unmarshal(&f); err == nil {
		d.Base = Duration(time.Duration(f) * time.Millisecond)
		return nil
	}
	type alias AdaptiveDuration
	var obj alias
	if err := unmarshal(&obj); err != nil {
		return fmt.Errorf("AdaptiveDuration must be a duration scalar or {base, quantile, min, max} object: %w", err)
	}
	*d = AdaptiveDuration(obj)
	return nil
}

// UnmarshalJSON accepts either a scalar or an object, same shape as
// the YAML side. Strings use time.ParseDuration; numbers are treated as
// milliseconds (matching Duration's YAML semantics).
func (d *AdaptiveDuration) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	switch data[0] {
	case '"':
		var s string
		if err := SonicCfg.Unmarshal(data, &s); err != nil {
			return err
		}
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration scalar %q: %w", s, err)
		}
		d.Base = Duration(parsed)
		return nil
	case '{':
		// Duration has no UnmarshalJSON, so we parse each duration field
		// from its JSON representation (string "500ms" or number-as-ms).
		var raw struct {
			Base     json.RawMessage `json:"base"`
			Quantile float64         `json:"quantile"`
			Min      json.RawMessage `json:"min"`
			Max      json.RawMessage `json:"max"`
		}
		if err := SonicCfg.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("AdaptiveDuration object: %w", err)
		}
		base, err := parseJSONDuration(raw.Base)
		if err != nil {
			return fmt.Errorf("AdaptiveDuration.base: %w", err)
		}
		minD, err := parseJSONDuration(raw.Min)
		if err != nil {
			return fmt.Errorf("AdaptiveDuration.min: %w", err)
		}
		maxD, err := parseJSONDuration(raw.Max)
		if err != nil {
			return fmt.Errorf("AdaptiveDuration.max: %w", err)
		}
		d.Base = base
		d.Quantile = raw.Quantile
		d.Min = minD
		d.Max = maxD
		return nil
	default:
		var asFloat float64
		if err := SonicCfg.Unmarshal(data, &asFloat); err != nil {
			return fmt.Errorf("AdaptiveDuration must be a duration scalar (\"500ms\" or 500) or {base, quantile, min, max} object: %w", err)
		}
		d.Base = Duration(time.Duration(asFloat) * time.Millisecond)
		return nil
	}
}

// MarshalJSON always emits the object form for round-trip stability —
// the scalar shorthand is input-only.
func (d *AdaptiveDuration) MarshalJSON() ([]byte, error) {
	if d == nil {
		return []byte("null"), nil
	}
	type alias AdaptiveDuration
	return SonicCfg.Marshal(alias(*d))
}

// NewStaticDuration is a convenience constructor for tests and
// callers that only want a static base value.
func NewStaticDuration(d time.Duration) *AdaptiveDuration {
	return &AdaptiveDuration{Base: Duration(d)}
}

// ResolveForRequest is a convenience wrapper for Resolve that pulls
// the per-method QuantileTracker off the request's network. Returns 0
// when the spec is zero/nil or when the request lacks a network — the
// caller treats 0 as "no cap" / disabled.
func (d *AdaptiveDuration) ResolveForRequest(req *NormalizedRequest) time.Duration {
	if d.IsZero() || req == nil {
		return 0
	}
	if d.Quantile <= 0 {
		return d.Resolve(nil)
	}
	ntw := req.Network()
	if ntw == nil {
		return d.Resolve(nil)
	}
	m, _ := req.Method()
	if m == "" {
		return d.Resolve(nil)
	}
	mt := ntw.GetMethodMetrics(m)
	if mt == nil {
		return d.Resolve(nil)
	}
	return d.Resolve(mt.GetResponseQuantiles())
}
