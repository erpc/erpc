package evm

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
)

// RequestFormatOutcome is what evm.requestFormat did to one request for one
// upstream.
type RequestFormatOutcome string

const (
	// RequestFormatUnchanged: the method has no known override shape, or the
	// params are already in the upstream's form.
	RequestFormatUnchanged RequestFormatOutcome = "unchanged"
	// RequestFormatRewritten: the upstream is sent rewritten params.
	RequestFormatRewritten RequestFormatOutcome = "rewritten"
	// RequestFormatUnreadable: a value eRPC cannot read exactly (a word longer
	// than 32 bytes, a decimal string, keys equal after padding, an unexpected
	// shape). The upstream is sent the params unchanged, so it judges them.
	RequestFormatUnreadable RequestFormatOutcome = "unreadable"
	// RequestFormatRefused: the upstream's form cannot carry the request's
	// meaning, and its client would drop that meaning without an error. The
	// request is not sent to that upstream.
	RequestFormatRefused RequestFormatOutcome = "refused"
)

// Field names Erigon up to v2.60.5 reads, lower-cased: Go's encoding/json
// matches them case-insensitively and ignores every other field silently.
var (
	erigonAccountFields = map[string]bool{
		"nonce": true, "code": true, "balance": true, "state": true, "statediff": true,
	}
	erigonBlockOverrideFields = map[string]bool{
		"blocknumber": true, "coinbase": true, "timestamp": true, "gaslimit": true,
		"difficulty": true, "basefee": true, "blockhash": true,
	}
	// Spec (geth/alloy) blockOverride names and the Erigon name with the same
	// meaning. alloy accepts the Erigon names too.
	erigonBlockOverrideRenames = map[string]string{
		"number":        "blockNumber",
		"time":          "timestamp",
		"feerecipient":  "coinbase",
		"basefeepergas": "baseFee",
	}
)

// FormatRequestParams returns params in format f for method. Methods whose
// override shape eRPC does not know, an unset format, and any request with a
// value eRPC cannot read exactly return params itself, unchanged. A rewrite
// works on a deep copy: params is never modified. A refused request returns
// a reason.
func FormatRequestParams(f common.EvmRequestFormat, method string, params []interface{}) ([]interface{}, RequestFormatOutcome, error) {
	if f != common.EvmRequestFormatCanonical && f != common.EvmRequestFormatErigonLegacy {
		return params, RequestFormatUnchanged, nil
	}
	switch method {
	case "eth_call", "eth_estimateGas", "eth_createAccessList", "eth_callMany",
		"eth_simulateV1", "debug_traceCall", "debug_traceCallMany", "trace_call", "trace_callMany":
	default:
		return params, RequestFormatUnchanged, nil
	}

	p, _ := deepCopyJSON(params).([]interface{})
	fm := &requestFormatter{f: f}
	switch method {
	case "eth_call", "eth_estimateGas", "eth_createAccessList":
		fm.stateOverride(paramAt(p, 2))
	case "eth_callMany":
		fm.stateOverride(paramAt(p, 2))
		fm.bundleBlockOverrides(paramAt(p, 0))
	case "debug_traceCallMany":
		fm.stateOverride(jsonField(paramAt(p, 2), "stateOverrides"))
		fm.bundleBlockOverrides(paramAt(p, 0))
	case "debug_traceCall":
		fm.stateOverride(jsonField(paramAt(p, 2), "stateOverrides"))
	case "eth_simulateV1":
		if calls, ok := jsonField(paramAt(p, 0), "blockStateCalls").([]interface{}); ok {
			for _, bsc := range calls {
				fm.stateOverride(jsonField(bsc, "stateOverrides"))
			}
		}
	case "trace_call":
		fm.traceCallObject(paramAt(p, 0))
		fm.stateOverride(paramAt(p, 3))
	case "trace_callMany":
		if items, ok := paramAt(p, 0).([]interface{}); ok {
			for _, item := range items {
				if pair, ok := item.([]interface{}); ok {
					fm.traceCallObject(paramAt(pair, 0))
				}
			}
		}
	}

	switch {
	case fm.refused != nil:
		return params, RequestFormatRefused, fm.refused
	case fm.unreadable:
		return params, RequestFormatUnreadable, nil
	case fm.changed:
		return p, RequestFormatRewritten, nil
	default:
		return params, RequestFormatUnchanged, nil
	}
}

// FormatRequestForUpstream resolves the params upstream u must receive for r
// under u's evm.requestFormat, starting from what u would be sent otherwise
// (common.WireParams, so it composes with evm.pinLatestTag). rewritten is
// false when u is sent those params unchanged. A non-nil error means u must
// not be sent r; it is an ErrUpstreamRequestSkipped, so another upstream
// answers.
func FormatRequestForUpstream(ctx context.Context, n common.Network, u common.Upstream, r *common.NormalizedRequest) (params []interface{}, rewritten bool, err error) {
	cfg := u.Config()
	if cfg == nil || cfg.Evm == nil || cfg.Evm.RequestFormat == "" {
		return nil, false, nil
	}
	jrq, jerr := r.JsonRpcRequest(ctx)
	if jerr != nil || jrq == nil {
		return nil, false, nil
	}
	jrq.RLock()
	method := jrq.Method
	out, outcome, reason := FormatRequestParams(cfg.Evm.RequestFormat, method, common.WireParams(ctx, r, jrq.Params))
	jrq.RUnlock()

	if outcome != RequestFormatUnchanged {
		projectId := ""
		if n != nil {
			projectId = n.ProjectId()
		}
		telemetry.MetricUpstreamRequestFormatTotal.WithLabelValues(
			projectId, r.NetworkLabel(), u.Id(), u.VendorName(), method, string(outcome),
		).Inc()
	}
	switch outcome {
	case RequestFormatRefused:
		return nil, false, common.NewErrUpstreamRequestSkipped(
			fmt.Errorf("evm.requestFormat %s cannot carry this request: %w", cfg.Evm.RequestFormat, reason), u.Id())
	case RequestFormatRewritten:
		return out, true, nil
	default:
		return nil, false, nil
	}
}

type requestFormatter struct {
	f          common.EvmRequestFormat
	changed    bool
	unreadable bool
	refused    error
}

func (fm *requestFormatter) legacy() bool { return fm.f == common.EvmRequestFormatErigonLegacy }

func (fm *requestFormatter) refuse(format string, args ...interface{}) {
	if fm.refused == nil {
		fm.refused = fmt.Errorf(format, args...)
	}
}

// stateOverride formats a state override: a map of address to account.
// Anything that is not a map is not a state override in this position (for
// example Erigon's eth_createAccessList optimizeGas bool), so it is left alone.
func (fm *requestFormatter) stateOverride(v interface{}) {
	accounts, ok := v.(map[string]interface{})
	if !ok {
		return
	}
	for _, acc := range accounts {
		am, ok := acc.(map[string]interface{})
		if !ok {
			if acc != nil {
				fm.unreadable = true
			}
			continue
		}
		if fm.legacy() {
			for k := range am {
				if !erigonAccountFields[strings.ToLower(k)] {
					fm.refuse("state override field %q", k)
				}
			}
		}
		fm.storage(am, "state")
		fm.storage(am, "stateDiff")
	}
}

// storage formats one account's storage map: keys always as 32-byte words,
// values as 32-byte words (canonical) or minimal quantities (erigonLegacy).
func (fm *requestFormatter) storage(account map[string]interface{}, name string) {
	raw, present := account[name]
	if !present || raw == nil {
		return
	}
	slots, ok := raw.(map[string]interface{})
	if !ok {
		fm.unreadable = true
		return
	}
	out := make(map[string]interface{}, len(slots))
	for k, v := range slots {
		key, ok := hexWord(k)
		vs, isStr := v.(string)
		val, vok := hexWord(vs)
		if !ok || !isStr || !vok {
			fm.unreadable = true
			return
		}
		nk := padWord(key)
		nv := padWord(val)
		if fm.legacy() {
			nv = minimalWord(val)
		}
		if _, dup := out[nk]; dup {
			fm.unreadable = true
			return
		}
		out[nk] = nv
		if nk != k || nv != vs {
			fm.changed = true
		}
	}
	account[name] = out
}

// bundleBlockOverrides renames each bundle's blockOverride fields to Erigon's
// names (erigonLegacy only). A field Erigon has no name for is refused: Erigon
// would ignore it and answer as if it were absent.
func (fm *requestFormatter) bundleBlockOverrides(v interface{}) {
	if !fm.legacy() {
		return
	}
	bundles, ok := v.([]interface{})
	if !ok {
		return
	}
	for _, b := range bundles {
		bm, ok := b.(map[string]interface{})
		if !ok {
			continue
		}
		bo, ok := bm["blockOverride"].(map[string]interface{})
		if !ok {
			continue
		}
		out := make(map[string]interface{}, len(bo))
		seen := make(map[string]bool, len(bo))
		for k, val := range bo {
			lk := strings.ToLower(k)
			nk := k
			if target, ok := erigonBlockOverrideRenames[lk]; ok {
				nk = target
			} else if !erigonBlockOverrideFields[lk] {
				fm.refuse("blockOverride field %q", k)
				continue
			}
			if seen[strings.ToLower(nk)] {
				fm.refuse("blockOverride field %q given twice", nk)
				continue
			}
			seen[strings.ToLower(nk)] = true
			out[nk] = val
			if nk != k {
				fm.changed = true
			}
		}
		bm["blockOverride"] = out
	}
}

// traceCallObject sends a trace_call object's "input" as "data"
// (erigonLegacy only): Erigon's trace_call reads only "data" and traces with
// empty calldata otherwise. Unequal "input" and "data" are refused.
func (fm *requestFormatter) traceCallObject(v interface{}) {
	if !fm.legacy() {
		return
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		return
	}
	in, hasIn := m["input"]
	if !hasIn || in == nil {
		return
	}
	d, hasData := m["data"]
	switch {
	case !hasData || d == nil:
		m["data"] = in
		delete(m, "input")
		fm.changed = true
	case !reflect.DeepEqual(in, d):
		fm.refuse("call object with unequal input and data")
	}
}

// hexWord returns the hex digits of a 0x-prefixed word of at most 32 bytes,
// leading zeros removed. Anything else (no prefix, a decimal string, a
// non-hex digit, more than 32 bytes) is not read.
func hexWord(s string) (string, bool) {
	if len(s) < 3 || s[0] != '0' || (s[1] != 'x' && s[1] != 'X') {
		return "", false
	}
	d := s[2:]
	for i := 0; i < len(d); i++ {
		if !isHexDigit(d[i]) {
			return "", false
		}
	}
	d = strings.TrimLeft(d, "0")
	if len(d) > 64 {
		return "", false
	}
	return d, true
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func padWord(digits string) string {
	return "0x" + strings.Repeat("0", 64-len(digits)) + digits
}

func minimalWord(digits string) string {
	if digits == "" {
		return "0x0"
	}
	return "0x" + digits
}

func paramAt(p []interface{}, i int) interface{} {
	if i < len(p) {
		return p[i]
	}
	return nil
}

func jsonField(v interface{}, name string) interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m[name]
	}
	return nil
}

// deepCopyJSON copies the maps and slices of a decoded JSON value; scalars
// are immutable and shared.
func deepCopyJSON(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, e := range t {
			out[k] = deepCopyJSON(e)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, e := range t {
			out[i] = deepCopyJSON(e)
		}
		return out
	default:
		return v
	}
}
