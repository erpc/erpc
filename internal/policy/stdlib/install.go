// Package stdlib wires the JS-side chainable selection-policy std-lib into
// a sobek runtime. Most of the surface area is in stdlib.js — installed
// here via a single Evaluate; Go contributes a handful of helpers
// (duration parsing, reasons, constants) that are clumsy to express in
// pure JS.
package stdlib

import (
	_ "embed"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/grafana/sobek"
)

//go:embed stdlib.js
var stdlibSource string

// Install primes a sobek runtime with the selection-policy std-lib. Safe
// to call once per runtime — the JS side is idempotent (each `define`
// short-circuits if the method is already installed).
func Install(rt *common.Runtime) error {
	vm := rt.VM()

	// 1. Module-level globals exposed to the JS side.
	//
	// durationMs answers how many milliseconds a value means, or null when it
	// cannot read one. Null is not zero: a zero cooldown is a real instruction,
	// and an unreadable value is not. The caller decides what an unreadable
	// value costs, because only the caller knows its own default.
	if err := vm.Set("durationMs", func(d sobek.Value) sobek.Value {
		if d == nil || sobek.IsUndefined(d) || sobek.IsNull(d) {
			return sobek.Null()
		}
		switch v := d.Export().(type) {
		case string:
			if ms, ok := parseDurationMs(v); ok {
				return vm.ToValue(ms)
			}
			return sobek.Null()
		case int64:
			return vm.ToValue(v)
		case float64:
			// NaN and +/-Inf are JS numbers (durationMs(a / b) with b == 0).
			// Their int64 conversion is undefined in Go, so they are unreadable.
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return sobek.Null()
			}
			return vm.ToValue(int64(v))
		default:
			return sobek.Null()
		}
	}); err != nil {
		return err
	}

	// 2. Reason constants (used by std-lib steps when annotating).
	reasons := map[string]string{
		"REASON_LAG":         "lag",
		"REASON_ERROR_RATE":  "errorRate",
		"REASON_LATENCY":     "latency",
		"REASON_THROTTLING":  "throttling",
		"REASON_MISBEHAVIOR": "misbehavior",
		"REASON_CORDONED":    "cordoned",
		"REASON_GROUP":       "group",
		"REASON_VENDOR":      "vendor",
		"REASON_USER_FILTER": "user_filter",
	}
	for k, v := range reasons {
		if err := vm.Set(k, v); err != nil {
			return err
		}
	}

	// 3. Finality bit-flag constants — used with `when(mask, fn)` and other
	// finality-conditional sugar. Bitwise-OR composes a multi-finality mask:
	//
	//   .when(REALTIME | UNFINALIZED | UNKNOWN, u => u.stickyPrimary({...}))
	//
	// The mask is OR'd with the request's finality-bit (looked up from
	// `ctx.finality` at run time). Raw string-finality comparisons still work
	// directly against `ctx.finality` (e.g. `ctx.finality === 'realtime'`) —
	// these constants are purely the bitmask form.
	for k, v := range map[string]int64{
		"REALTIME":    1 << 0,
		"UNFINALIZED": 1 << 1,
		"FINALIZED":   1 << 2,
		"UNKNOWN":     1 << 3,
	} {
		if err := vm.Set(k, v); err != nil {
			return err
		}
	}

	// 4. Scope constants — the grouping grain for stickyPrimary + the
	// engine's `evalScope` config field. Values are kebab-case strings so
	// they round-trip through YAML / JSON / Go enums cleanly. The TS SDK
	// exports the same CAPITAL_SNAKE_CASE names with the same string
	// values, so an operator writes `scope: NETWORK` in both the eval
	// function and (via the SDK) the YAML config.
	//
	//   NETWORK                   one primary per network across everything
	//   NETWORK_METHOD            one primary per (network, method)
	//   NETWORK_FINALITY          one primary per (network, finality)
	//   NETWORK_METHOD_FINALITY   one primary per slot (most granular, no sharing)
	for k, v := range map[string]string{
		"NETWORK":                 "network",
		"NETWORK_METHOD":          "network-method",
		"NETWORK_FINALITY":        "network-finality",
		"NETWORK_METHOD_FINALITY": "network-method-finality",
	} {
		if err := vm.Set(k, v); err != nil {
			return err
		}
	}

	// 5. Install Array.prototype methods + globals.
	if _, err := vm.RunString(stdlibSource); err != nil {
		return fmt.Errorf("install policy stdlib: %w", err)
	}
	return nil
}

// parseDurationMs accepts the duration strings common across the eRPC
// config (1s, 100ms, 5m, 1h). The second return is false when the string
// names no duration: an empty string, or one time.ParseDuration refuses.
func parseDurationMs(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d.Milliseconds(), true
	}
	return 0, false
}
