package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// BlockTimeAdaptiveDuration is a duration that is either a fixed value or derived from the
// network's estimated block time. It's the reusable shape for knobs (e.g. cache
// TTLs) that should track each chain's cadence rather than a single constant.
//
// Wire format accepts a scalar shorthand or an object:
//
//	ttl: 2s                                        # fixed
//	ttl: { blockTimeMultiplier: 1 }                # blockTime * 1 (caller default until known)
//	ttl: { blockTimeMultiplier: 1, fallback: 2s }  # with explicit cold-start fallback
//	ttl: { blockTimeMultiplier: 2, min: 1s, max: 10s }  # derived value clamped to [min, max]
//
// See Resolve for how the value is computed.
type BlockTimeAdaptiveDuration struct {
	// Fallback is the fixed value. A scalar shorthand sets it directly; in object
	// form it's the cold-start floor used until the block time is known.
	Fallback Duration `yaml:"fallback,omitempty" json:"fallback,omitempty" tstype:"Duration"`
	// BlockTimeMultiplier, when > 0, derives the value from the network's
	// estimated block time (blockTime * multiplier).
	BlockTimeMultiplier float64 `yaml:"blockTimeMultiplier,omitempty" json:"blockTimeMultiplier,omitempty"`
	// Min, when > 0, is the lower bound for the block-time-derived value.
	Min Duration `yaml:"min,omitempty" json:"min,omitempty" tstype:"Duration"`
	// Max, when > 0, is the upper bound for the block-time-derived value.
	Max Duration `yaml:"max,omitempty" json:"max,omitempty" tstype:"Duration"`
}

// IsZero reports whether no field is set (nil, or a scalar zero), i.e. the
// caller's default should apply.
func (d *BlockTimeAdaptiveDuration) IsZero() bool {
	return d == nil || *d == BlockTimeAdaptiveDuration{}
}

// FixedDuration returns the fixed/fallback component, or 0 when unset. Used by
// block-time-independent consumers (e.g. cache storage expiry).
func (d *BlockTimeAdaptiveDuration) FixedDuration() time.Duration {
	if d == nil {
		return 0
	}
	return d.Fallback.Duration()
}

// Resolve computes the effective duration for a given network block time.
// coldStartDefault is used only when a multiplier is set but the block time is
// unknown and no Fallback is configured, so the result is never unbounded.
func (d *BlockTimeAdaptiveDuration) Resolve(blockTime, coldStartDefault time.Duration) time.Duration {
	if d == nil {
		return 0
	}
	if d.BlockTimeMultiplier > 0 {
		if blockTime > 0 {
			v := time.Duration(float64(blockTime) * d.BlockTimeMultiplier)
			if mn := d.Min.Duration(); mn > 0 && v < mn {
				v = mn
			}
			if mx := d.Max.Duration(); mx > 0 && v > mx {
				v = mx
			}
			return v
		}
		if f := d.Fallback.Duration(); f > 0 {
			return f
		}
		return coldStartDefault
	}
	return d.Fallback.Duration()
}

func (d *BlockTimeAdaptiveDuration) Copy() *BlockTimeAdaptiveDuration {
	if d == nil {
		return nil
	}
	c := *d
	return &c
}

func (d *BlockTimeAdaptiveDuration) validate(field string) error {
	if d == nil {
		return nil
	}
	if d.BlockTimeMultiplier < 0 {
		return fmt.Errorf("%s.blockTimeMultiplier must be >= 0", field)
	}
	if d.Fallback < 0 || d.Min < 0 || d.Max < 0 {
		return fmt.Errorf("%s: fallback, min and max must be >= 0", field)
	}
	if d.Min > 0 && d.Max > 0 && d.Min > d.Max {
		return fmt.Errorf("%s.min (%s) must be <= max (%s)", field, d.Min, d.Max)
	}
	if (d.Min > 0 || d.Max > 0) && d.BlockTimeMultiplier == 0 {
		return fmt.Errorf("%s: min/max only apply with blockTimeMultiplier > 0", field)
	}
	return nil
}

// rejectUnknownBlockTimeKeys errors on any key outside the type's fields, so a
// quantile-style spec (or a typo) fails loudly instead of being silently
// ignored when used in a block-time context.
func rejectUnknownBlockTimeKeys[V any](obj map[string]V) error {
	for k := range obj {
		switch k {
		case "fallback", "blockTimeMultiplier", "min", "max":
		default:
			return fmt.Errorf("unknown field %q for block-time duration (allowed: fallback, blockTimeMultiplier, min, max)", k)
		}
	}
	return nil
}

// UnmarshalYAML accepts a scalar shorthand (string/number -> Fallback) or the
// object form. Unknown object keys are rejected.
func (d *BlockTimeAdaptiveDuration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var scalar Duration
	if err := unmarshal(&scalar); err == nil {
		d.Fallback = scalar
		d.BlockTimeMultiplier = 0
		return nil
	}
	var raw map[string]interface{}
	if err := unmarshal(&raw); err != nil {
		return err
	}
	if err := rejectUnknownBlockTimeKeys(raw); err != nil {
		return err
	}
	type alias BlockTimeAdaptiveDuration
	var a alias
	if err := unmarshal(&a); err != nil {
		return err
	}
	*d = BlockTimeAdaptiveDuration(a)
	return nil
}

// UnmarshalJSON accepts a scalar shorthand (string/number -> Fallback) or the
// object form. Unknown object keys are rejected.
func (d *BlockTimeAdaptiveDuration) UnmarshalJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	if trimmed[0] == '{' {
		// Duration has no UnmarshalJSON (only YAML), so parse the fallback's raw
		// value via parseJSONDuration rather than relying on struct unmarshal.
		var obj map[string]json.RawMessage
		if err := SonicCfg.Unmarshal(trimmed, &obj); err != nil {
			return err
		}
		if err := rejectUnknownBlockTimeKeys(obj); err != nil {
			return err
		}
		if rawMult, ok := obj["blockTimeMultiplier"]; ok {
			if err := SonicCfg.Unmarshal(rawMult, &d.BlockTimeMultiplier); err != nil {
				return err
			}
		}
		for key, dst := range map[string]*Duration{"fallback": &d.Fallback, "min": &d.Min, "max": &d.Max} {
			rawDur, ok := obj[key]
			if !ok {
				continue
			}
			v, err := parseJSONDuration(rawDur)
			if err != nil {
				return err
			}
			*dst = v
		}
		return nil
	}
	dur, err := parseJSONDuration(trimmed)
	if err != nil {
		return err
	}
	d.Fallback = dur
	d.BlockTimeMultiplier = 0
	return nil
}

// isFixed reports whether only the fixed component is set, so marshaling can
// emit the scalar shorthand and round-trip the way the value was written.
func (d BlockTimeAdaptiveDuration) isFixed() bool {
	return d.BlockTimeMultiplier == 0 && d.Min == 0 && d.Max == 0
}

func (d BlockTimeAdaptiveDuration) MarshalYAML() (interface{}, error) {
	if d.isFixed() {
		return d.Fallback.MarshalYAML()
	}
	type alias BlockTimeAdaptiveDuration
	return alias(d), nil
}

func (d BlockTimeAdaptiveDuration) MarshalJSON() ([]byte, error) {
	if d.isFixed() {
		return d.Fallback.MarshalJSON()
	}
	type alias BlockTimeAdaptiveDuration
	return SonicCfg.Marshal(alias(d))
}

// FixedDuration builds a BlockTimeAdaptiveDuration with only a fixed value.
func FixedDuration(d time.Duration) *BlockTimeAdaptiveDuration {
	return &BlockTimeAdaptiveDuration{Fallback: Duration(d)}
}
