package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// DynamicDuration is a duration that is either a fixed value or derived from the
// network's estimated block time. It's the reusable shape for knobs (e.g. cache
// TTLs) that should track each chain's cadence rather than a single constant.
//
// Wire format accepts a scalar shorthand or an object:
//
//	ttl: 2s                                        # fixed
//	ttl: { blockTimeMultiplier: 1 }                # blockTime * 1 (caller default until known)
//	ttl: { blockTimeMultiplier: 1, fallback: 2s }  # with explicit cold-start fallback
//
// See Resolve for how the value is computed.
type DynamicDuration struct {
	// Fallback is the fixed value. A scalar shorthand sets it directly; in object
	// form it's the cold-start floor used until the block time is known.
	Fallback Duration `yaml:"fallback,omitempty" json:"fallback,omitempty" tstype:"Duration"`
	// BlockTimeMultiplier, when > 0, derives the value from the network's
	// estimated block time (blockTime * multiplier).
	BlockTimeMultiplier float64 `yaml:"blockTimeMultiplier,omitempty" json:"blockTimeMultiplier,omitempty"`
}

// FixedDuration returns the fixed/fallback component, or 0 when unset. Used by
// block-time-independent consumers (e.g. cache storage expiry).
func (d *DynamicDuration) FixedDuration() time.Duration {
	if d == nil {
		return 0
	}
	return d.Fallback.Duration()
}

// Resolve computes the effective duration for a given network block time.
// coldStartDefault is used only when a multiplier is set but the block time is
// unknown and no Fallback is configured, so the result is never unbounded.
func (d *DynamicDuration) Resolve(blockTime, coldStartDefault time.Duration) time.Duration {
	if d == nil {
		return 0
	}
	if d.BlockTimeMultiplier > 0 {
		if blockTime > 0 {
			return time.Duration(float64(blockTime) * d.BlockTimeMultiplier)
		}
		if f := d.Fallback.Duration(); f > 0 {
			return f
		}
		return coldStartDefault
	}
	return d.Fallback.Duration()
}

func (d *DynamicDuration) Copy() *DynamicDuration {
	if d == nil {
		return nil
	}
	c := *d
	return &c
}

func (d *DynamicDuration) validate(field string) error {
	if d == nil {
		return nil
	}
	if d.BlockTimeMultiplier < 0 {
		return fmt.Errorf("%s.blockTimeMultiplier must be >= 0", field)
	}
	return nil
}

// UnmarshalYAML accepts a scalar shorthand (string/number -> Fallback) or the object form.
func (d *DynamicDuration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var scalar Duration
	if err := unmarshal(&scalar); err == nil {
		d.Fallback = scalar
		d.BlockTimeMultiplier = 0
		return nil
	}
	type alias DynamicDuration
	var a alias
	if err := unmarshal(&a); err != nil {
		return err
	}
	*d = DynamicDuration(a)
	return nil
}

// UnmarshalJSON accepts a scalar shorthand (string/number -> Fallback) or the object form.
func (d *DynamicDuration) UnmarshalJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	if trimmed[0] == '{' {
		// Duration has no UnmarshalJSON (only YAML), so parse the fallback's raw
		// value via parseJSONDuration rather than relying on struct unmarshal.
		var obj struct {
			Fallback            json.RawMessage `json:"fallback"`
			BlockTimeMultiplier float64         `json:"blockTimeMultiplier"`
		}
		if err := SonicCfg.Unmarshal(trimmed, &obj); err != nil {
			return err
		}
		d.BlockTimeMultiplier = obj.BlockTimeMultiplier
		if len(obj.Fallback) > 0 {
			f, err := parseJSONDuration(obj.Fallback)
			if err != nil {
				return err
			}
			d.Fallback = f
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

// FixedDuration builds a DynamicDuration with only a fixed value.
func FixedDuration(d time.Duration) *DynamicDuration {
	return &DynamicDuration{Fallback: Duration(d)}
}
