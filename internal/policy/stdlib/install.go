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
		"REASON_LAG":        "lag",
		"REASON_ERROR_RATE": "errorRate",
		"REASON_LATENCY":    "latency",
		"REASON_THROTTLING": "throttling",
		"REASON_MISBEHAVIOR": "misbehavior",
		"REASON_CORDONED":   "cordoned",
		"REASON_GROUP":      "group",
		"REASON_VENDOR":     "vendor",
		"REASON_USER_FILTER": "user_filter",
	}
	for k, v := range reasons {
		if err := vm.Set(k, v); err != nil {
			return err
		}
	}

	// 3. Finality constants (match `EvalContext.Finality` values).
	finalities := map[string]string{
		"REALTIME":    "realtime",
		"UNFINALIZED": "unfinalized",
		"FINALIZED":   "finalized",
		"UNKNOWN":     "unknown",
	}
	for k, v := range finalities {
		if err := vm.Set(k, v); err != nil {
			return err
		}
	}

	// 4. Install Array.prototype methods + globals.
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
