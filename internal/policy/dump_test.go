package policy

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/grafana/sobek"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func dumpConfig(script *sobek.Program, policies ...*common.SelectionPolicyConfig) *common.Config {
	nws := []*common.NetworkConfig{nil}
	for _, p := range policies {
		nws = append(nws, &common.NetworkConfig{SelectionPolicy: p})
	}
	return &common.Config{
		UserScript: script,
		Projects:   []*common.ProjectConfig{nil, {Networks: nws}},
	}
}

func TestResolveEffectiveSelectionPolicies(t *testing.T) {
	ResolveEffectiveSelectionPolicies(nil) // nil config is a no-op

	script, err := sobek.Compile("user.js", `
globalThis.__erpcFns['fast'] = function fast(ups) { return ups; };
globalThis.__erpcFns['nostr'] = Object.create(null);
`, false)
	require.NoError(t, err)

	custom := "(ups) => ups.slice(0, 1)"
	missing := &common.SelectionPolicyConfig{EvalFunc: "__ts_fn__:missing"}
	fast := &common.SelectionPolicyConfig{EvalFunc: "__ts_fn__:fast"}
	nostr := &common.SelectionPolicyConfig{EvalFunc: "__ts_fn__:nostr"}
	empty := &common.SelectionPolicyConfig{EvalFunc: "__ts_fn__:"}
	trivial := &common.SelectionPolicyConfig{EvalFunc: "  " + common.DefaultSelectionPolicySource + "\n"}
	user := &common.SelectionPolicyConfig{EvalFunc: custom}
	cfg := dumpConfig(script, nil, missing, fast, nostr, empty, trivial, user)

	ResolveEffectiveSelectionPolicies(cfg)
	nws := cfg.Projects[1].Networks

	rich := DefaultPolicySource()
	require.NotNil(t, nws[1].SelectionPolicy, "nil policy gets the rich default")
	assert.Equal(t, rich, nws[1].SelectionPolicy.EvalFunc)
	assert.NotZero(t, nws[1].SelectionPolicy.EvalInterval, "filled default carries SetDefaults values")

	assert.Equal(t, "__ts_fn__:missing", missing.EvalFunc, "unknown id keeps the sentinel")
	assert.Equal(t, "function fast(ups) { return ups; }", fast.EvalFunc)
	assert.Equal(t, "__ts_fn__:nostr", nostr.EvalFunc)
	assert.Equal(t, "__ts_fn__:", empty.EvalFunc)
	assert.Equal(t, rich, trivial.EvalFunc)
	assert.Equal(t, rich, trivial.EvalFuncOriginal)
	assert.Equal(t, custom, user.EvalFunc, "operator-supplied source is untouched")
}

func TestResolveEffectiveSelectionPolicies_UnresolvableScripts(t *testing.T) {
	broken, err := sobek.Compile("broken.js", `throw new Error("nope")`, false)
	require.NoError(t, err)
	wipes, err := sobek.Compile("wipe.js", `globalThis.__erpcFns = undefined;`, false)
	require.NoError(t, err)

	tests := []struct {
		name   string
		script *sobek.Program
	}{
		{"no user script", nil},
		{"user script throws", broken},
		{"registry removed", wipes},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &common.SelectionPolicyConfig{EvalFunc: "__ts_fn__:a"}
			b := &common.SelectionPolicyConfig{EvalFunc: "__ts_fn__:b"}
			ResolveEffectiveSelectionPolicies(dumpConfig(tc.script, a, b))
			assert.Equal(t, "__ts_fn__:a", a.EvalFunc)
			assert.Equal(t, "__ts_fn__:b", b.EvalFunc, "a failed runtime is not retried for later networks")
		})
	}
}

func TestUpgradeDefaultPolicy(t *testing.T) {
	require.NoError(t, upgradeDefaultPolicy(nil))

	custom := &common.SelectionPolicyConfig{EvalFunc: "(u) => u"}
	require.NoError(t, upgradeDefaultPolicy(custom))
	assert.Equal(t, "(u) => u", custom.EvalFunc)
	assert.Nil(t, custom.CompiledProgram)

	placeholder := &common.SelectionPolicyConfig{EvalFunc: common.DefaultSelectionPolicySource}
	require.NoError(t, upgradeDefaultPolicy(placeholder))
	assert.Equal(t, DefaultPolicySource(), placeholder.EvalFunc)
	assert.Equal(t, DefaultPolicySource(), placeholder.EvalFuncOriginal)
	assert.NotNil(t, placeholder.CompiledProgram)
}
