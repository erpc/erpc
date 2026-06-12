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
//
// See Resolve for how the value is computed.
type BlockTimeAdaptiveDuration struct {
	// Fallback is the fixed value. A scalar shorthand sets it directly; in object
	// form it's the cold-start floor used until the block time is known.
	Fallback Duration `yaml:"fallback,omitempty" json:"fallback,omitempty" tstype:"Duration"`
	// BlockTimeMultiplier, when > 0, derives the value from the network's
	// estimated block time (blockTime * multiplier).
	BlockTimeMultiplier float64 `yaml:"blockTimeMultiplier,omitempty" json:"blockTimeMultiplier,omitempty"`
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
			return time.Duration(float64(blockTime) * d.BlockTimeMultiplier)
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
	return nil
}

// rejectUnknownBlockTimeKeys errors on any key outside the type's fields, so a
// quantile-style spec (or a typo) fails loudly instead of being silently
// ignored when used in a block-time context.
func rejectUnknownBlockTimeKeys[V any](obj map[string]V) error {
	for k := range obj {
		switch k {
		case "fallback", "blockTimeMultiplier":
		default:
			return fmt.Errorf("unknown field %q for block-time duration (allowed: fallback, blockTimeMultiplier)", k)
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
		if rawFallback, ok := obj["fallback"]; ok {
			f, err := parseJSONDuration(rawFallback)
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

// FixedDuration builds a BlockTimeAdaptiveDuration with only a fixed value.
func FixedDuration(d time.Duration) *BlockTimeAdaptiveDuration {
	return &BlockTimeAdaptiveDuration{Fallback: Duration(d)}
}
