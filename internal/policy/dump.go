package policy

import (
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy/stdlib"
	"github.com/grafana/sobek"
)

// ResolveEffectiveSelectionPolicies makes each network's effective
// selectionPolicy explicit on `cfg` — for diagnostic surfaces like
// `erpc dump`, so operators can read the actual policy that will run
// instead of a nil placeholder or an opaque sentinel:
//
//   - Networks with `selectionPolicy == nil` get the rich default policy
//     filled in (the same source the engine applies at runtime via
//     `upgradeDefaultPolicy`).
//   - The trivial `(upstreams, ctx) => upstreams` placeholder (set by
//     `SetDefaults` when only an empty `selectionPolicy` block is given) is
//     upgraded to the rich default.
//   - TS-function sentinels (`__ts_fn__:<id>`) are best-effort resolved to
//     their actual JS source by evaluating `cfg.UserScript` once in a
//     throwaway sobek runtime and reading the function's `.toString()`.
//     On any error the sentinel is left in place so operators still see
//     the id.
//
// Pure for-display: this mutates the passed `cfg`, so call it AFTER
// `LoadConfig` and immediately before marshalling. It does NOT compile
// programs (the dump only needs source strings) and does NOT touch any
// network whose operator already supplied a non-trivial source.
func ResolveEffectiveSelectionPolicies(cfg *common.Config) {
	if cfg == nil {
		return
	}

	// Lazily evaluate `cfg.UserScript` once when the first TS sentinel is
	// hit. The pooled production runtimes do the same on every acquire;
	// here we only need the function objects in `__erpcFns` so we can
	// read their source. Errors disable resolution silently — dump is
	// best-effort.
	var (
		tsVM        *sobek.Runtime
		tsVMTried   bool
		tsVMReady   bool
	)
	resolveTS := func(id string) (string, bool) {
		if id == "" || cfg.UserScript == nil {
			return "", false
		}
		if !tsVMTried {
			tsVMTried = true
			rt, err := common.NewRuntime()
			if err != nil {
				return "", false
			}
			if err := stdlib.Install(rt); err != nil {
				return "", false
			}
			vm := rt.VM()
			// Seed the registry the user-script writes into. Production
			// loader does the same on every pool runtime; missing here
			// would only matter if the script reads __erpcFns at top
			// level (uncommon, but cheap to guard).
			if _, err := vm.RunString("globalThis.__erpcFns = globalThis.__erpcFns || {};"); err != nil {
				return "", false
			}
			if _, err := vm.RunProgram(cfg.UserScript); err != nil {
				return "", false
			}
			tsVM = vm
			tsVMReady = true
		}
		if !tsVMReady {
			return "", false
		}
		fnsRaw := tsVM.GlobalObject().Get("__erpcFns")
		if fnsRaw == nil || sobek.IsUndefined(fnsRaw) || sobek.IsNull(fnsRaw) {
			return "", false
		}
		fnsObj := fnsRaw.ToObject(tsVM)
		if fnsObj == nil {
			return "", false
		}
		fnVal := fnsObj.Get(id)
		if fnVal == nil || sobek.IsUndefined(fnVal) || sobek.IsNull(fnVal) {
			return "", false
		}
		fnObj := fnVal.ToObject(tsVM)
		if fnObj == nil {
			return "", false
		}
		toStrVal := fnObj.Get("toString")
		toStrFn, ok := sobek.AssertFunction(toStrVal)
		if !ok {
			return "", false
		}
		res, err := toStrFn(fnVal)
		if err != nil || res == nil {
			return "", false
		}
		src := strings.TrimSpace(res.String())
		if src == "" {
			return "", false
		}
		return src, true
	}

	trivial := strings.TrimSpace(common.DefaultSelectionPolicySource)
	rich := DefaultPolicySource()

	for _, prj := range cfg.Projects {
		if prj == nil {
			continue
		}
		for _, nw := range prj.Networks {
			if nw == nil {
				continue
			}
			if nw.SelectionPolicy == nil {
				nw.SelectionPolicy = &common.SelectionPolicyConfig{EvalFunc: rich}
				_ = nw.SelectionPolicy.SetDefaults()
				continue
			}
			if strings.TrimSpace(nw.SelectionPolicy.EvalFunc) == trivial {
				nw.SelectionPolicy.EvalFunc = rich
				nw.SelectionPolicy.EvalFuncOriginal = rich
			}
			if common.IsTSFunctionSentinel(nw.SelectionPolicy.EvalFunc) {
				if src, ok := resolveTS(common.TSFunctionSentinelID(nw.SelectionPolicy.EvalFunc)); ok {
					nw.SelectionPolicy.EvalFunc = src
				}
			}
		}
	}
}
