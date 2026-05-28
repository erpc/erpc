// Package stdlib wires the JS-side chainable selection-policy std-lib into
// a sobek runtime. Most of the surface area is in stdlib.js — installed
// here via a single Evaluate; Go contributes a handful of helpers
// (duration parsing, reasons, constants) that are clumsy to express in
// pure JS.
package stdlib

import (
	_ "embed"
	"fmt"
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
	if err := vm.Set("durationMs", func(d sobek.Value) int64 {
		if d == nil || sobek.IsUndefined(d) || sobek.IsNull(d) {
			return 0
		}
		switch v := d.Export().(type) {
		case string:
			return parseDurationMs(v)
		case int64:
			return v
		case float64:
			return int64(v)
		default:
			return 0
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

// parseDurationMs accepts the duration formats common across the eRPC
// config (1s, 100ms, 5m, 1h, plus bare integer ms). On any failure, 0.
func parseDurationMs(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d.Milliseconds()
	}
	return 0
}
