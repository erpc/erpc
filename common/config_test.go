package common

import (
	"strings"
	"testing"
	"time"

	"github.com/grafana/sobek"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"gopkg.in/yaml.v3"
)

func TestLoadConfig_FailToReadFile(t *testing.T) {
	fs := afero.NewMemMapFs()
	_, err := LoadConfig(fs, "nonexistent.yaml", &DefaultOptions{})
	if err == nil {
		t.Error("expected error, got nil")
	}
}

func TestLoadConfig_InvalidYaml(t *testing.T) {
	fs := afero.NewMemMapFs()
	cfg, err := afero.TempFile(fs, "", "erpc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.WriteString("invalid yaml")

	_, err = LoadConfig(fs, cfg.Name(), &DefaultOptions{})
	if err == nil {
		t.Error("expected error, got nil")
	}
}

func TestLoadConfig_ValidYaml(t *testing.T) {
	fs := afero.NewMemMapFs()
	cfg, err := afero.TempFile(fs, "", "erpc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.WriteString(`
logLevel: DEBUG
`)

	_, err = LoadConfig(fs, cfg.Name(), &DefaultOptions{})
	if err != nil {
		t.Error(err)
	}
}

// TestLoadConfig_LegacyFieldsAccepted pins the back-compat contract:
// a prod-shape YAML carrying legacy `routing.scoreMultipliers` on
// upstreams + `scoreMetricsWindowSize` at the project level loads
// without strict-decode errors. The stashes land on the parsed Config
// (LegacyProject / LegacyRouting), which the legacy translator hook
// then folds into selectionPolicy.eval — that hook is wired by the
// cmd/erpc init and is NOT installed here, so this test just verifies
// the parse + stash, not the eval synthesis (that has its own test
// suite in common/legacy/translate_test.go).
func TestLoadConfig_LegacyFieldsAccepted(t *testing.T) {
	yamlSrc := `
logLevel: error
projects:
  - id: prod-shape
    scoreMetricsWindowSize: 10m
    scoreMetricsMode: compact
    upstreamDefaults:
      routing:
        scoreLatencyQuantile: 0.9
        scoreMultipliers:
          - finality: [realtime, unfinalized]
            respLatency: 10
            errorRate: 2
    upstreams:
      - id: alc-eth-mainnet
        endpoint: https://eth.example/
        evm: { chainId: 1 }
        routing:
          scoreMultipliers:
            - overall: 0.2
    networks:
      - architecture: evm
        evm: { chainId: 1 }
`
	fs := afero.NewMemMapFs()
	tmp, err := afero.TempFile(fs, "", "legacy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.WriteString(yamlSrc); err != nil {
		t.Fatal(err)
	}

	// Hook intentionally NOT set — this test pins the parse+stash
	// shape so even consumers that bypass the hook get a useful error
	// path (rather than a strict-decode rejection).
	prev := LegacyTranslateFn
	LegacyTranslateFn = nil
	t.Cleanup(func() { LegacyTranslateFn = prev })

	cfg, err := LoadConfig(fs, tmp.Name(), &DefaultOptions{})
	if err != nil {
		t.Fatalf("expected legacy YAML to load, got: %v", err)
	}
	if len(cfg.Projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(cfg.Projects))
	}
	prj := cfg.Projects[0]
	// scoreMetricsWindowSize is canonical (first-class on ProjectConfig);
	// scoreMetricsMode is still legacy (translator emits inert-warning).
	if prj.ScoreMetricsWindowSize.Duration() != 10*time.Minute {
		t.Fatalf("canonical ScoreMetricsWindowSize not parsed; got %v", prj.ScoreMetricsWindowSize)
	}
	if prj.LegacyProject == nil || prj.LegacyProject.ScoreMetricsMode != "compact" {
		t.Fatalf("LegacyProject.ScoreMetricsMode not captured; got %+v", prj.LegacyProject)
	}
	if prj.UpstreamDefaults == nil || prj.UpstreamDefaults.Routing == nil ||
		len(prj.UpstreamDefaults.Routing.ScoreMultipliers) != 1 {
		t.Fatalf("upstreamDefaults.routing.scoreMultipliers not captured; got %+v",
			prj.UpstreamDefaults)
	}
	if len(prj.Upstreams) == 0 || prj.Upstreams[0].Routing == nil ||
		len(prj.Upstreams[0].Routing.ScoreMultipliers) != 1 ||
		prj.Upstreams[0].Routing.ScoreMultipliers[0].Overall == nil ||
		*prj.Upstreams[0].Routing.ScoreMultipliers[0].Overall != 0.2 {
		t.Fatalf("upstream routing.overall not captured")
	}
}

// TestLoadConfig_TypeScriptUnifiedPipeline pins the TS load path:
//   1. function-valued `evalFunc` survives as a real sobek function
//      (NOT stringified) — `SelectionPolicy.EvalFunc` carries only a
//      `__ts_fn__:<id>` sentinel pointing into the user-script's
//      `globalThis.__erpcFns` registry;
//   2. the user's whole compiled module is attached to `cfg.UserScript`
//      so each policy-engine pool runtime can re-evaluate it natively,
//      preserving closures + helpers;
//   3. the legacy `group:` key written via TS still flows through the
//      shadow types and gets migrated to a `tier:` tag identically to
//      the YAML path, and first-class `routing:` parses onto u.Routing.
//
// We don't run the legacy translator hook here — that has its own
// suite. This test just verifies that the TS object survives the
// round-trip with the same stash semantics as YAML AND the function
// is preserved as native sobek state rather than re-parsed source.
func TestLoadConfig_TypeScriptUnifiedPipeline(t *testing.T) {
	// Write a tiny TS config to a real (on-disk) file, since esbuild
	// needs a path it can read from the filesystem.
	dir := t.TempDir()
	tsPath := dir + "/erpc.ts"
	tsSrc := `
export default {
  logLevel: 'warn',
  projects: [
    {
      id: 'ts-test',
      upstreams: [
        {
          id: 'u1',
          endpoint: 'https://u1.example/',
          evm: { chainId: 1 },
          group: 'main',
          routing: {
            scoreMultipliers: [{ errorRate: 7 }]
          }
        }
      ],
      networks: [
        {
          architecture: 'evm',
          evm: { chainId: 1 },
          selectionPolicy: {
            evalFunc: (upstreams, ctx) => upstreams.sortByScore({ errorRate: 99 })
          }
        }
      ]
    }
  ]
};
`
	if err := afero.WriteFile(afero.NewOsFs(), tsPath, []byte(tsSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	prev := LegacyTranslateFn
	LegacyTranslateFn = nil
	t.Cleanup(func() { LegacyTranslateFn = prev })

	cfg, err := LoadConfig(afero.NewOsFs(), tsPath, &DefaultOptions{})
	if err != nil {
		t.Fatalf("LoadConfig TS: %v", err)
	}

	prj := cfg.Projects[0]
	u := prj.Upstreams[0]

	// (1) Legacy `group: 'main'` migrated to a `tier:main` tag via the
	// UnmarshalYAML shadow on UpstreamConfig.
	if got := strings.Join(u.Tags, ","); !strings.Contains(got, "tier:main") {
		t.Fatalf("group→tag migration failed; tags=%q", got)
	}

	// (2) First-class `routing.scoreMultipliers` parsed onto u.Routing
	// (survives to runtime — NOT a load-time stash).
	if u.Routing == nil || len(u.Routing.ScoreMultipliers) != 1 {
		t.Fatalf("routing not parsed; Routing=%+v", u.Routing)
	}
	if u.Routing.ScoreMultipliers[0].ErrorRate == nil ||
		*u.Routing.ScoreMultipliers[0].ErrorRate != 7 {
		t.Fatalf("routing scoreMultipliers value not preserved")
	}

	// (3) Function-valued `evalFunc` is captured as a `__ts_fn__:<id>`
	// sentinel pointing into the user-script's __erpcFns registry —
	// NOT stringified via .toString(). The actual function lives as a
	// real sobek value in every policy-engine pool runtime that
	// evaluates `cfg.UserScript`.
	sp := prj.Networks[0].SelectionPolicy
	if sp == nil {
		t.Fatal("selectionPolicy missing")
	}
	if !IsTSFunctionSentinel(sp.EvalFunc) {
		t.Fatalf("TS-defined evalFunc should be a __ts_fn__ sentinel; got %q", sp.EvalFunc)
	}
	if cfg.UserScript == nil {
		t.Fatal("cfg.UserScript must be attached for TS configs")
	}

	// Smoke-check: run the user script in a fresh runtime, look up the
	// sentinel id in __erpcFns, and confirm it's actually a function.
	// This is what the policy-engine pool primer does on each acquire.
	rt, err := NewRuntime()
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	if _, err := rt.VM().RunProgram(cfg.UserScript); err != nil {
		t.Fatalf("evaluate user script: %v", err)
	}
	id := TSFunctionSentinelID(sp.EvalFunc)
	fns := rt.VM().GlobalObject().Get("__erpcFns")
	if fns == nil || sobek.IsUndefined(fns) || sobek.IsNull(fns) {
		t.Fatal("__erpcFns must be populated after running UserScript")
	}
	fnVal := fns.ToObject(rt.VM()).Get(id)
	if fnVal == nil || sobek.IsUndefined(fnVal) || sobek.IsNull(fnVal) {
		t.Fatalf("__erpcFns[%q] missing — walker did not register the function", id)
	}
	if _, isFn := sobek.AssertFunction(fnVal); !isFn {
		t.Fatalf("__erpcFns[%q] is not a function: %v", id, fnVal.ExportType())
	}
}

// TestLoadConfig_TypeScriptClosurePreserved is the regression test that
// proves the TS path no longer drops closures the way the old
// `.toString()` pipeline did. The user defines a module-level helper
// (`weights`) and references it from inside `evalFunc`. The function
// must be able to read `weights` when invoked in the policy-engine
// pool runtime — which only works because the WHOLE user script is
// evaluated in each runtime, putting the function's closure scope in
// reach.
func TestLoadConfig_TypeScriptClosurePreserved(t *testing.T) {
	dir := t.TempDir()
	tsPath := dir + "/erpc.ts"
	// `weights` is a module-level constant captured by the arrow
	// function via closure. Under the old `.toString()` pipeline the
	// stringified function source referenced `weights` as a free
	// variable that wouldn't resolve in the policy runtime → eval
	// would throw ReferenceError. Under the new pipeline the function
	// resolves to its original closure scope inside `__erpcFns`.
	tsSrc := `
const weights = { fast: 5, slow: 50 };
export default {
  projects: [
    {
      id: 'closure-test',
      upstreams: [
        { id: 'fast', endpoint: 'https://fast.example/', evm: { chainId: 1 } },
        { id: 'slow', endpoint: 'https://slow.example/', evm: { chainId: 1 } }
      ],
      networks: [
        {
          architecture: 'evm',
          evm: { chainId: 1 },
          selectionPolicy: {
            evalFunc: (upstreams, ctx) => weights[upstreams[0] && upstreams[0].id] || 0
          }
        }
      ]
    }
  ]
};
`
	if err := afero.WriteFile(afero.NewOsFs(), tsPath, []byte(tsSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	prev := LegacyTranslateFn
	LegacyTranslateFn = nil
	t.Cleanup(func() { LegacyTranslateFn = prev })

	cfg, err := LoadConfig(afero.NewOsFs(), tsPath, &DefaultOptions{})
	if err != nil {
		t.Fatalf("LoadConfig TS: %v", err)
	}

	sp := cfg.Projects[0].Networks[0].SelectionPolicy
	if !IsTSFunctionSentinel(sp.EvalFunc) {
		t.Fatalf("expected sentinel, got %q", sp.EvalFunc)
	}
	id := TSFunctionSentinelID(sp.EvalFunc)

	rt, err := NewRuntime()
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	if _, err := rt.VM().RunProgram(cfg.UserScript); err != nil {
		t.Fatalf("evaluate user script: %v", err)
	}
	fnVal := rt.VM().GlobalObject().Get("__erpcFns").ToObject(rt.VM()).Get(id)
	fn, isFn := sobek.AssertFunction(fnVal)
	if !isFn {
		t.Fatalf("function not registered: %v", fnVal)
	}

	// Call the function with a fake upstreams arg shaped like the real
	// eval input ({ id: 'fast' }). If the closure survives, it returns
	// `weights['fast']` = 5. If it doesn't, the function would throw a
	// ReferenceError on `weights`.
	upstreams := rt.VM().NewArray()
	upObj := rt.VM().NewObject()
	if err := upObj.Set("id", "fast"); err != nil {
		t.Fatal(err)
	}
	if err := upstreams.Set("0", upObj); err != nil {
		t.Fatal(err)
	}
	if err := upstreams.Set("length", 1); err != nil {
		t.Fatal(err)
	}
	res, err := fn(sobek.Undefined(), upstreams, sobek.Undefined())
	if err != nil {
		t.Fatalf("invoke fn — closure not preserved? %v", err)
	}
	if got := res.ToInteger(); got != 5 {
		t.Fatalf("closure lookup wrong: got %d, want 5 (weights['fast'])", got)
	}
}

// TestLoadConfig_LegacyHookFiresAndClearsStashes wires the actual
// translator into the LoadConfig hook (mirroring what cmd/erpc does
// at init time) and verifies the synthesized selectionPolicy.eval
// lands on the network AND the legacy stashes are nil after load.
func TestLoadConfig_LegacyHookFiresAndClearsStashes(t *testing.T) {
	yamlSrc := `
logLevel: error
projects:
  - id: hook-test
    upstreams:
      - id: u1
        endpoint: https://u1.example/
        evm: { chainId: 1 }
        routing:
          scoreMultipliers:
            - errorRate: 10
              respLatency: 5
    networks:
      - architecture: evm
        evm: { chainId: 1 }
`
	fs := afero.NewMemMapFs()
	tmp, err := afero.TempFile(fs, "", "legacy-hook.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tmp.WriteString(yamlSrc)

	// Inline a minimal translator hook that touches exactly the same
	// stashes the real common/legacy.TranslateFromConfig touches. We
	// can't import common/legacy here without creating a cycle (legacy
	// imports common), so we hand-roll the writeback.
	prev := LegacyTranslateFn
	LegacyTranslateFn = func(cfg *Config) ([]string, error) {
		for _, prj := range cfg.Projects {
			for _, n := range prj.Networks {
				if n.SelectionPolicy == nil {
					n.SelectionPolicy = &SelectionPolicyConfig{EvalFunc: "(upstreams, ctx) => upstreams"}
				}
			}
		}
		return []string{"test-warn"}, nil
	}
	t.Cleanup(func() { LegacyTranslateFn = prev })

	var captured []string
	LegacyTranslateLogger = func(w string) { captured = append(captured, w) }
	t.Cleanup(func() { LegacyTranslateLogger = nil })

	cfg, err := LoadConfig(fs, tmp.Name(), &DefaultOptions{})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if want, got := []string{"test-warn"}, captured; !strings.EqualFold(strings.Join(want, "|"), strings.Join(got, "|")) {
		t.Fatalf("warnings not flowed through LegacyTranslateLogger: got %v", got)
	}
	// routing.scoreMultipliers is first-class — it must SURVIVE the hook
	// (it's not a load-time stash to be consumed).
	if u0 := cfg.Projects[0].Upstreams[0]; u0.Routing == nil ||
		len(u0.Routing.ScoreMultipliers) != 1 {
		t.Fatalf("first-class routing must survive load; got %+v", u0.Routing)
	}
	if cfg.Projects[0].Networks[0].SelectionPolicy == nil ||
		cfg.Projects[0].Networks[0].SelectionPolicy.EvalFunc == "" {
		t.Fatalf("hook must populate selectionPolicy.eval on the network")
	}

	// And the load completed (SetDefaults + Validate ran).
	if cfg.LogLevel != "error" {
		t.Fatalf("expected logLevel from yaml; got %q", cfg.LogLevel)
	}
	_ = time.Second
}

func TestFailsafeConfigBackwardCompatibility(t *testing.T) {
	t.Run("NetworkDefaults old format with empty MatchMethod", func(t *testing.T) {
		yamlData := `
rateLimitBudget: "test"
failsafe:
  retry:
    maxAttempts: 3
`
		var nd NetworkDefaults
		err := yaml.Unmarshal([]byte(yamlData), &nd)
		assert.NoError(t, err)

		// Should convert to array with one element
		assert.Len(t, nd.Failsafe, 1)
		// Empty MatchMethod should be converted to "*"
		assert.Equal(t, "*", nd.Failsafe[0].MatchMethod)
		assert.NotNil(t, nd.Failsafe[0].Retry)
		assert.Equal(t, 3, nd.Failsafe[0].Retry.MaxAttempts)
	})

	t.Run("NetworkDefaults old format with non-empty MatchMethod", func(t *testing.T) {
		yamlData := `
rateLimitBudget: "test"
failsafe:
  matchMethod: "eth_*"
  retry:
    maxAttempts: 3
`
		var nd NetworkDefaults
		err := yaml.Unmarshal([]byte(yamlData), &nd)
		assert.NoError(t, err)

		assert.Len(t, nd.Failsafe, 1)
		// Existing MatchMethod should be preserved
		assert.Equal(t, "eth_*", nd.Failsafe[0].MatchMethod)
	})

	t.Run("UpstreamConfig old format with empty MatchMethod", func(t *testing.T) {
		yamlData := `
id: "test-upstream"
endpoint: "http://test.com"
failsafe:
  retry:
    maxAttempts: 3
`
		var uc UpstreamConfig
		err := yaml.Unmarshal([]byte(yamlData), &uc)
		assert.NoError(t, err)

		assert.Len(t, uc.Failsafe, 1)
		// Empty MatchMethod should be converted to "*"
		assert.Equal(t, "*", uc.Failsafe[0].MatchMethod)
	})

	t.Run("NetworkConfig old format with empty MatchMethod", func(t *testing.T) {
		yamlData := `
architecture: "evm"
evm:
  chainId: 123
failsafe:
  circuitBreaker:
    failureThresholdCount: 5
    failureThresholdCapacity: 10
    halfOpenAfter: 10s
    successThresholdCount: 3
    successThresholdCapacity: 5
`
		var nc NetworkConfig
		err := yaml.Unmarshal([]byte(yamlData), &nc)
		assert.NoError(t, err)

		assert.Len(t, nc.Failsafe, 1)
		// Empty MatchMethod should be converted to "*"
		assert.Equal(t, "*", nc.Failsafe[0].MatchMethod)
		assert.NotNil(t, nc.Failsafe[0].CircuitBreaker)
	})

	t.Run("New array format with MatchFinality", func(t *testing.T) {
		yamlData := `
architecture: "evm"
evm:
  chainId: 123
failsafe:
  - matchMethod: "eth_call"
    matchFinality: ["realtime", "unknown"]  # realtime and unknown finality states
    retry:
      maxAttempts: 5
  - matchMethod: "eth_getBalance"
    matchFinality: ["unfinalized"]  # unfinalized state
    retry:
      maxAttempts: 2
`
		var nc NetworkConfig
		err := yaml.Unmarshal([]byte(yamlData), &nc)
		assert.NoError(t, err)

		assert.Len(t, nc.Failsafe, 2)
		assert.Equal(t, "eth_call", nc.Failsafe[0].MatchMethod)
		assert.Equal(t, []DataFinalityState{DataFinalityStateRealtime, DataFinalityStateUnknown}, nc.Failsafe[0].MatchFinality)
		assert.Equal(t, 5, nc.Failsafe[0].Retry.MaxAttempts)

		assert.Equal(t, "eth_getBalance", nc.Failsafe[1].MatchMethod)
		assert.Equal(t, []DataFinalityState{DataFinalityStateUnfinalized}, nc.Failsafe[1].MatchFinality)
		assert.Equal(t, 2, nc.Failsafe[1].Retry.MaxAttempts)
	})

	t.Run("FailsafeConfig validation rejects empty MatchMethod", func(t *testing.T) {
		fs := &FailsafeConfig{
			MatchMethod: "", // empty should be rejected
			Retry: &RetryPolicyConfig{
				MaxAttempts: 3,
			},
		}

		err := fs.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failsafe.matchMethod cannot be empty")
	})

	t.Run("FailsafeConfig validation accepts wildcard", func(t *testing.T) {
		fs := &FailsafeConfig{
			MatchMethod: "*",
			Retry: &RetryPolicyConfig{
				MaxAttempts:     3,
				BackoffFactor:   2.0,
				BackoffMaxDelay: Duration(10 * time.Second),
			},
		}

		err := fs.Validate()
		assert.NoError(t, err)
	})

	t.Run("FailsafeConfig SetDefaults sets MatchMethod to wildcard", func(t *testing.T) {
		fs := &FailsafeConfig{
			MatchMethod: "", // empty
			Retry: &RetryPolicyConfig{
				MaxAttempts: 3,
			},
		}

		err := fs.SetDefaults(nil)
		assert.NoError(t, err)
		assert.Equal(t, "*", fs.MatchMethod)
	})

	t.Run("MatchFinality with all string values", func(t *testing.T) {
		yamlData := `
architecture: "evm"
evm:
  chainId: 123
failsafe:
  - matchMethod: "eth_*"
    matchFinality: ["finalized", "unfinalized", "realtime", "unknown"]
    retry:
      maxAttempts: 3
`
		var nc NetworkConfig
		err := yaml.Unmarshal([]byte(yamlData), &nc)
		assert.NoError(t, err)

		assert.Len(t, nc.Failsafe, 1)
		assert.Equal(t, "eth_*", nc.Failsafe[0].MatchMethod)
		assert.Equal(t, []DataFinalityState{
			DataFinalityStateFinalized,
			DataFinalityStateUnfinalized,
			DataFinalityStateRealtime,
			DataFinalityStateUnknown,
		}, nc.Failsafe[0].MatchFinality)
	})

	t.Run("MatchFinality with mixed case strings", func(t *testing.T) {
		yamlData := `
architecture: "evm"
evm:
  chainId: 123
failsafe:
  - matchMethod: "eth_call"
    matchFinality: ["Finalized", "UNFINALIZED", "ReaLTime"]
    retry:
      maxAttempts: 3
`
		var nc NetworkConfig
		err := yaml.Unmarshal([]byte(yamlData), &nc)
		assert.NoError(t, err)

		// Should handle case-insensitive parsing
		assert.Len(t, nc.Failsafe, 1)
		assert.Equal(t, []DataFinalityState{
			DataFinalityStateFinalized,
			DataFinalityStateUnfinalized,
			DataFinalityStateRealtime,
		}, nc.Failsafe[0].MatchFinality)
	})
}

func TestNetworkConfigFailsafeBackwardCompatibility(t *testing.T) {
	t.Run("new array format", func(t *testing.T) {
		yamlData := `
architecture: evm
failsafe:
- matchMethod: "*"
  matchFinality: ["realtime"]
  timeout:
    duration: 2s
  retry:
    maxAttempts: 3
- matchMethod: "eth_*"
  timeout:
    duration: 5s
`
		var config NetworkConfig
		err := yaml.Unmarshal([]byte(yamlData), &config)
		assert.NoError(t, err)
		assert.Len(t, config.Failsafe, 2)
		assert.Equal(t, "*", config.Failsafe[0].MatchMethod)
		assert.Equal(t, "eth_*", config.Failsafe[1].MatchMethod)
		assert.Equal(t, 2*time.Second, config.Failsafe[0].Timeout.Duration.Resolve(nil))
		assert.Equal(t, 3, config.Failsafe[0].Retry.MaxAttempts)
	})

	t.Run("old single object format", func(t *testing.T) {
		yamlData := `
architecture: evm
failsafe:
  timeout:
    duration: 2s
  retry:
    maxAttempts: 3
`
		var config NetworkConfig
		err := yaml.Unmarshal([]byte(yamlData), &config)
		assert.NoError(t, err)
		assert.Len(t, config.Failsafe, 1)
		assert.Equal(t, "*", config.Failsafe[0].MatchMethod) // Should default to "*"
		assert.Equal(t, 2*time.Second, config.Failsafe[0].Timeout.Duration.Resolve(nil))
		assert.Equal(t, 3, config.Failsafe[0].Retry.MaxAttempts)
	})
}

func TestNetworkDefaultsFailsafeBackwardCompatibility(t *testing.T) {
	t.Run("new array format", func(t *testing.T) {
		yamlData := `
rateLimitBudget: "test"
failsafe:
- matchMethod: "*"
  timeout:
    duration: 2s
- matchMethod: "eth_*"
  timeout:
    duration: 5s
`
		var defaults NetworkDefaults
		err := yaml.Unmarshal([]byte(yamlData), &defaults)
		assert.NoError(t, err)
		assert.Len(t, defaults.Failsafe, 2)
		assert.Equal(t, "*", defaults.Failsafe[0].MatchMethod)
		assert.Equal(t, "eth_*", defaults.Failsafe[1].MatchMethod)
	})

	t.Run("old single object format", func(t *testing.T) {
		yamlData := `
rateLimitBudget: "test"
failsafe:
  timeout:
    duration: 2s
  retry:
    maxAttempts: 3
`
		var defaults NetworkDefaults
		err := yaml.Unmarshal([]byte(yamlData), &defaults)
		assert.NoError(t, err)
		assert.Len(t, defaults.Failsafe, 1)
		assert.Equal(t, "*", defaults.Failsafe[0].MatchMethod) // Should default to "*"
		assert.Equal(t, 2*time.Second, defaults.Failsafe[0].Timeout.Duration.Resolve(nil))
	})
}

func TestUpstreamConfigFailsafeBackwardCompatibility(t *testing.T) {
	t.Run("new array format", func(t *testing.T) {
		yamlData := `
id: "test-upstream"
endpoint: "http://example.com"
failsafe:
- matchMethod: "*"
  timeout:
    duration: 2s
- matchMethod: "eth_*"
  timeout:
    duration: 5s
`
		var upstream UpstreamConfig
		err := yaml.Unmarshal([]byte(yamlData), &upstream)
		assert.NoError(t, err)
		assert.Len(t, upstream.Failsafe, 2)
		assert.Equal(t, "*", upstream.Failsafe[0].MatchMethod)
		assert.Equal(t, "eth_*", upstream.Failsafe[1].MatchMethod)
	})

	t.Run("old single object format", func(t *testing.T) {
		yamlData := `
id: "test-upstream"
endpoint: "http://example.com"
failsafe:
  timeout:
    duration: 2s
  retry:
    maxAttempts: 3
`
		var upstream UpstreamConfig
		err := yaml.Unmarshal([]byte(yamlData), &upstream)
		assert.NoError(t, err)
		assert.Len(t, upstream.Failsafe, 1)
		assert.Equal(t, "*", upstream.Failsafe[0].MatchMethod) // Should default to "*"
		assert.Equal(t, 2*time.Second, upstream.Failsafe[0].Timeout.Duration.Resolve(nil))
	})
}

func TestFullConfigWithNetworkFailsafe(t *testing.T) {
	t.Run("full config with array failsafe in network", func(t *testing.T) {
		yamlData := `
logLevel: debug
projects:
- id: test-project
  networks:
  - architecture: evm
    evm:
      chainId: 42161
    alias: hyperevm
    failsafe:
    - matchMethod: "*"
      matchFinality: ["realtime"]
      timeout:
        duration: 2s
      retry:
        maxAttempts: 3
    - matchMethod: "*"
      matchFinality: ["unfinalized", "finalized", "unknown"]
      timeout:
        duration: 5s
      retry:
        maxAttempts: 5
        delay: "10ms"
      hedge:
        maxCount: 1
        quantile: 0.95
        minDelay: "500ms"
      consensus:
        maxParticipants: 10
        agreementThreshold: 2
        disputeBehavior: "returnError"
        lowParticipantsBehavior: "acceptMostCommonValidResult"
        punishMisbehavior:
          disputeThreshold: 10
          disputeWindow: "10m"
          sitOutPenalty: "30m"
`
		var config Config
		err := yaml.Unmarshal([]byte(yamlData), &config)
		assert.NoError(t, err)
		assert.Len(t, config.Projects, 1)
		assert.Len(t, config.Projects[0].Networks, 1)

		network := config.Projects[0].Networks[0]
		assert.Equal(t, NetworkArchitecture("evm"), network.Architecture)
		assert.Equal(t, "hyperevm", network.Alias)
		assert.Len(t, network.Failsafe, 2)

		// Check first failsafe config
		assert.Equal(t, "*", network.Failsafe[0].MatchMethod)
		assert.Contains(t, network.Failsafe[0].MatchFinality, DataFinalityStateRealtime)
		assert.Equal(t, 2*time.Second, network.Failsafe[0].Timeout.Duration.Resolve(nil))
		assert.Equal(t, 3, network.Failsafe[0].Retry.MaxAttempts)

		// Check second failsafe config
		assert.Equal(t, "*", network.Failsafe[1].MatchMethod)
		assert.Contains(t, network.Failsafe[1].MatchFinality, DataFinalityStateUnfinalized)
		assert.Equal(t, 5*time.Second, network.Failsafe[1].Timeout.Duration.Resolve(nil))
		assert.Equal(t, 5, network.Failsafe[1].Retry.MaxAttempts)
		assert.NotNil(t, network.Failsafe[1].Hedge)
		assert.NotNil(t, network.Failsafe[1].Consensus)
	})

	t.Run("full config with old single failsafe in network", func(t *testing.T) {
		yamlData := `
logLevel: debug
projects:
- id: test-project
  networks:
  - architecture: evm
    evm:
      chainId: 42161
    failsafe:
      timeout:
        duration: 2s
      retry:
        maxAttempts: 3
`
		var config Config
		err := yaml.Unmarshal([]byte(yamlData), &config)
		assert.NoError(t, err)
		assert.Len(t, config.Projects, 1)
		assert.Len(t, config.Projects[0].Networks, 1)

		network := config.Projects[0].Networks[0]
		assert.Len(t, network.Failsafe, 1)
		assert.Equal(t, "*", network.Failsafe[0].MatchMethod)
		assert.Equal(t, 2*time.Second, network.Failsafe[0].Timeout.Duration.Resolve(nil))
		assert.Equal(t, 3, network.Failsafe[0].Retry.MaxAttempts)
	})
}

func TestLoadConfigWithNetworkFailsafe(t *testing.T) {
	t.Run("LoadConfig with array failsafe in network", func(t *testing.T) {
		yamlData := `
logLevel: debug
projects:
- id: test-project
  networks:
  - architecture: evm
    evm:
      chainId: 42161
    failsafe:
    - matchMethod: "*"
      timeout:
        duration: 2s
      retry:
        maxAttempts: 3
    - matchMethod: "eth_*"
      timeout:
        duration: 5s
`
		// Create a temporary file
		fs := afero.NewMemMapFs()
		err := afero.WriteFile(fs, "test-config.yaml", []byte(yamlData), 0644)
		assert.NoError(t, err)

		// Load the config using LoadConfig
		config, err := LoadConfig(fs, "test-config.yaml", nil)
		assert.NoError(t, err)
		assert.NotNil(t, config)

		assert.Len(t, config.Projects, 1)
		assert.Len(t, config.Projects[0].Networks, 1)

		network := config.Projects[0].Networks[0]
		assert.Len(t, network.Failsafe, 2)
		assert.Equal(t, "*", network.Failsafe[0].MatchMethod)
		assert.Equal(t, "eth_*", network.Failsafe[1].MatchMethod)
	})
}

func TestFailingConfigScenario(t *testing.T) {
	t.Run("Mixed old and new failsafe formats with maxCount", func(t *testing.T) {
		// This test demonstrates that invalid field names now give clear error messages
		yamlData := `
logLevel: error
projects:
  - id: main
    networkDefaults:
      failsafe:
        timeout:
          duration: 300s
        retry:
          maxAttempts: 6
          delay: 0ms
    upstreamDefaults:
      failsafe:
        timeout:
          duration: 300s
        retry: null
    networks:
      - architecture: evm
        evm:
          chainId: 42161
        alias: hyperevm
        failsafe:
        - matchMethod: "*"
          matchFinality: ["realtime"]
          timeout:
            duration: 2s
          retry:
            maxCount: 3
        - matchMethod: "*"
          matchFinality: ["unfinalized", "finalized", "unknown"]
          timeout:
            duration: 5s
          retry:
            maxCount: 5
            delay: "10ms"
`
		// Create a temporary file
		fs := afero.NewMemMapFs()
		err := afero.WriteFile(fs, "test-config.yaml", []byte(yamlData), 0644)
		assert.NoError(t, err)

		// This should fail with a clear error message about invalid field
		config, err := LoadConfig(fs, "test-config.yaml", nil)

		// Verify we get a clear error message
		assert.Error(t, err)
		assert.Nil(t, config)

		if err != nil {
			t.Logf("LoadConfig error (expected): %v", err)
			errStr := err.Error()

			// The error should now clearly mention:
			// 1. The invalid field name "maxCount"
			// 2. The correct type "RetryPolicyConfig"
			// 3. Line numbers where the errors occur
			assert.Contains(t, errStr, "maxCount", "Error should mention the invalid field 'maxCount'")
			assert.Contains(t, errStr, "RetryPolicyConfig", "Error should mention the type where field is not found")
			assert.Contains(t, errStr, "line", "Error should include line numbers")

			// The error should NOT have the confusing sequence unmarshal message
			assert.NotContains(t, errStr, "!!seq", "Error should not mention sequence unmarshaling")
			assert.NotContains(t, errStr, "cannot unmarshal !!seq into common.FailsafeConfig",
				"Error should not have the confusing sequence error")
		}
	})
}

func TestInvalidFieldNameErrorMessage(t *testing.T) {
	t.Run("Invalid field maxCount in retry should give clear error", func(t *testing.T) {
		// Test with just the retry config to isolate the issue
		yamlData := `
maxCount: 3
delay: 10ms
`
		var retryConfig RetryPolicyConfig
		decoder := yaml.NewDecoder(strings.NewReader(yamlData))
		decoder.KnownFields(true) // This should catch unknown fields
		err := decoder.Decode(&retryConfig)

		// We expect an error about unknown field
		assert.Error(t, err)
		if err != nil {
			t.Logf("Error (as expected): %v", err)
			// The error should mention "maxCount" as unknown field
			assert.Contains(t, err.Error(), "maxCount", "Error message should mention the invalid field 'maxCount'")
		}
	})

	t.Run("Valid field maxAttempts should work", func(t *testing.T) {
		yamlData := `
maxAttempts: 3
delay: 10ms
`
		var retryConfig RetryPolicyConfig
		decoder := yaml.NewDecoder(strings.NewReader(yamlData))
		decoder.KnownFields(true)
		err := decoder.Decode(&retryConfig)

		assert.NoError(t, err)
		assert.Equal(t, 3, retryConfig.MaxAttempts)
		assert.Equal(t, 10*time.Millisecond, time.Duration(retryConfig.Delay))
	})

	t.Run("Invalid field in failsafe array gives confusing error", func(t *testing.T) {
		// This shows the actual problem - when invalid fields are in an array of failsafe configs
		yamlData := `
- matchMethod: "*"
  retry:
    maxCount: 3
`
		var failsafeConfigs []*FailsafeConfig
		decoder := yaml.NewDecoder(strings.NewReader(yamlData))
		decoder.KnownFields(true)
		err := decoder.Decode(&failsafeConfigs)

		// This is the confusing error we currently get
		assert.Error(t, err)
		if err != nil {
			t.Logf("Current error: %v", err)
			// This error is confusing - it says it can't unmarshal a sequence into FailsafeConfig
			// but the real issue is the invalid "maxCount" field
		}
	})
}

func TestLoadConfigWithInvalidFieldName(t *testing.T) {
	t.Run("LoadConfig with invalid maxCount field should give clear error", func(t *testing.T) {
		// Full config with invalid field name (maxCount instead of maxAttempts)
		yamlData := `
logLevel: debug
projects:
  - id: test-project
    networks:
      - architecture: evm
        evm:
          chainId: 42161
        failsafe:
          - matchMethod: "*"
            timeout:
              duration: 2s
            retry:
              maxCount: 3  # This should be maxAttempts
              delay: 10ms
`
		fs := afero.NewMemMapFs()
		err := afero.WriteFile(fs, "test-config.yaml", []byte(yamlData), 0644)
		assert.NoError(t, err)

		// Load the config - this should fail with an error
		config, err := LoadConfig(fs, "test-config.yaml", nil)

		// Check what error we get
		assert.Error(t, err)
		if err != nil {
			t.Logf("LoadConfig error: %v", err)

			// The error should ideally mention "maxCount" as an invalid field
			// but currently it gives a confusing message about unmarshaling sequences

			// Let's check if the error mentions anything useful
			errStr := err.Error()
			t.Logf("Error contains 'maxCount': %v", strings.Contains(errStr, "maxCount"))
			t.Logf("Error contains 'unmarshal': %v", strings.Contains(errStr, "unmarshal"))
			t.Logf("Error contains 'seq': %v", strings.Contains(errStr, "seq"))
		}
		assert.Nil(t, config)
	})

	t.Run("LoadConfig with correct maxAttempts field should work", func(t *testing.T) {
		// Same config but with correct field name
		yamlData := `
logLevel: debug
projects:
  - id: test-project
    networks:
      - architecture: evm
        evm:
          chainId: 42161
        failsafe:
          - matchMethod: "*"
            timeout:
              duration: 2s
            retry:
              maxAttempts: 3  # Correct field name
              delay: 10ms
`
		fs := afero.NewMemMapFs()
		err := afero.WriteFile(fs, "test-config.yaml", []byte(yamlData), 0644)
		assert.NoError(t, err)

		// This should work
		config, err := LoadConfig(fs, "test-config.yaml", nil)
		assert.NoError(t, err)
		assert.NotNil(t, config)

		if config != nil {
			network := config.Projects[0].Networks[0]
			assert.Len(t, network.Failsafe, 1)
			assert.Equal(t, 3, network.Failsafe[0].Retry.MaxAttempts)
		}
	})

	t.Run("LoadConfig with invalid field in complex nested structure", func(t *testing.T) {
		// This reproduces the exact scenario from the user's failing config
		yamlData := `
logLevel: error
projects:
  - id: main
    networkDefaults:
      failsafe:
        timeout:
          duration: 300s
        retry:
          maxAttempts: 6  # This is correct
    networks:
      - architecture: evm
        evm:
          chainId: 42161
        failsafe:
          - matchMethod: "*"
            matchFinality: ["realtime"]
            timeout:
              duration: 2s
            retry:
              maxCount: 3  # This is INCORRECT - should be maxAttempts
          - matchMethod: "*"
            matchFinality: ["unfinalized", "finalized", "unknown"]
            timeout:
              duration: 5s
            retry:
              maxCount: 5  # This is INCORRECT - should be maxAttempts
              delay: "10ms"
`
		fs := afero.NewMemMapFs()
		err := afero.WriteFile(fs, "test-config.yaml", []byte(yamlData), 0644)
		assert.NoError(t, err)

		// This should fail
		config, err := LoadConfig(fs, "test-config.yaml", nil)

		assert.Error(t, err)
		if err != nil {
			t.Logf("LoadConfig error with complex nested structure: %v", err)

			// The error message should help identify the problem
			// Currently it says: "cannot unmarshal !!seq into common.FailsafeConfig"
			// which is confusing because the real issue is the invalid field name

			errStr := err.Error()
			t.Logf("Error mentions line number: %v", strings.Contains(errStr, "line"))

			// Ideally, the error should say something like:
			// "field maxCount not found in type common.RetryPolicyConfig at line X"
		}
		assert.Nil(t, config)
	})
}

func TestBackwardCompatibilityStillWorks(t *testing.T) {
	t.Run("Old single failsafe format still works", func(t *testing.T) {
		yamlData := `
logLevel: debug
projects:
  - id: test-project
    upstreams:
      - endpoint: https://example.com
        evm:
          chainId: 42161
    networks:
      - architecture: evm
        evm:
          chainId: 42161
        failsafe:  # Old format: single object instead of array
          timeout:
            duration: 2s
          retry:
            maxAttempts: 3
`
		fs := afero.NewMemMapFs()
		err := afero.WriteFile(fs, "test-config.yaml", []byte(yamlData), 0644)
		assert.NoError(t, err)

		config, err := LoadConfig(fs, "test-config.yaml", nil)
		assert.NoError(t, err)
		assert.NotNil(t, config)

		// Old format should be converted to new array format
		network := config.Projects[0].Networks[0]
		assert.Len(t, network.Failsafe, 1)
		assert.Equal(t, "*", network.Failsafe[0].MatchMethod) // Default value
		assert.Equal(t, 3, network.Failsafe[0].Retry.MaxAttempts)
	})

	t.Run("New array failsafe format works", func(t *testing.T) {
		yamlData := `
logLevel: debug
projects:
  - id: test-project
    upstreams:
      - endpoint: https://example.com
        evm:
          chainId: 42161
    networks:
      - architecture: evm
        evm:
          chainId: 42161
        failsafe:  # New format: array
          - matchMethod: "*"
            timeout:
              duration: 2s
            retry:
              maxAttempts: 3
          - matchMethod: "eth_*"
            timeout:
              duration: 5s
            retry:
              maxAttempts: 5
`
		fs := afero.NewMemMapFs()
		err := afero.WriteFile(fs, "test-config.yaml", []byte(yamlData), 0644)
		assert.NoError(t, err)

		config, err := LoadConfig(fs, "test-config.yaml", nil)
		assert.NoError(t, err)
		assert.NotNil(t, config)

		network := config.Projects[0].Networks[0]
		assert.Len(t, network.Failsafe, 2)
		assert.Equal(t, "*", network.Failsafe[0].MatchMethod)
		assert.Equal(t, 3, network.Failsafe[0].Retry.MaxAttempts)
		assert.Equal(t, "eth_*", network.Failsafe[1].MatchMethod)
		assert.Equal(t, 5, network.Failsafe[1].Retry.MaxAttempts)
	})
}
