package evm

import (
	"testing"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompileIntegritySettings(t *testing.T) {
	tru, fls := true, false

	t.Run("nil → empty set + default policy", func(t *testing.T) {
		cs, p := compileIntegritySettings(nil)
		assert.Empty(t, cs)
		assert.Equal(t, integrity.DefaultReorgPolicy(), p)
	})

	t.Run("level authoritative enables every tier", func(t *testing.T) {
		cs, _ := compileIntegritySettings(&common.IntegritySettings{Level: "authoritative"})
		assert.True(t, cs.For("indexMagnitude").Enabled)
		assert.True(t, cs.For("parentHashLinkage").Enabled)
		assert.True(t, cs.For("receiptVsBlock").Enabled)
	})

	t.Run("level intrinsic excludes higher tiers", func(t *testing.T) {
		cs, _ := compileIntegritySettings(&common.IntegritySettings{Level: "intrinsic"})
		assert.True(t, cs.For("indexMagnitude").Enabled)
		assert.False(t, cs.For("receiptVsBlock").Enabled)
	})

	t.Run("explicit disable overrides the level", func(t *testing.T) {
		cs, _ := compileIntegritySettings(&common.IntegritySettings{
			Level:  "authoritative",
			Checks: map[string]*common.IntegrityCheckConfig{"receiptVsBlock": {Enabled: &fls}},
		})
		assert.False(t, cs.For("receiptVsBlock").Enabled)
	})

	t.Run("explicit enable above the level with params", func(t *testing.T) {
		cs, _ := compileIntegritySettings(&common.IntegritySettings{
			Level: "intrinsic",
			Checks: map[string]*common.IntegrityCheckConfig{
				"receiptVsBlock": {Enabled: &tru, Params: map[string]string{"k": "v"}},
			},
		})
		assert.True(t, cs.For("receiptVsBlock").Enabled)
		assert.Equal(t, "v", cs.For("receiptVsBlock").Params["k"])
	})

	t.Run("invalidBehavior maps to the reorg policy", func(t *testing.T) {
		_, p := compileIntegritySettings(&common.IntegritySettings{
			Level:           "authoritative",
			InvalidBehavior: &common.IntegrityInvalidBehaviorConfig{Finalized: "reject", Unfinalized: "off"},
		})
		assert.Equal(t, integrity.BehaviorError, p.Finalized)
		assert.Equal(t, integrity.BehaviorIgnore, p.Unfinalized)
	})

	t.Run("per-check onFailure becomes a fail override", func(t *testing.T) {
		cs, _ := compileIntegritySettings(&common.IntegritySettings{
			Level:  "intrinsic",
			Checks: map[string]*common.IntegrityCheckConfig{"bloomMatch": {OnFailure: "soft-flag"}},
		})
		require.NotNil(t, cs.For("bloomMatch").FailOverride)
		assert.Equal(t, integrity.BehaviorRecord, *cs.For("bloomMatch").FailOverride)
	})
}

func TestResolveRequestSettings(t *testing.T) {
	base := common.IntegritySettings{Level: "intrinsic"}
	profiles := map[string]*common.IntegritySettings{"strict": {Level: "authoritative"}}
	cfgWith := func(mode string) *common.IntegrityConfig {
		return &common.IntegrityConfig{IntegritySettings: base, HeaderMode: mode, Profiles: profiles}
	}

	t.Run("nil config → nil", func(t *testing.T) {
		assert.Nil(t, resolveRequestSettings(nil, "strict"))
	})
	t.Run("empty selector → base unchanged", func(t *testing.T) {
		assert.Equal(t, "intrinsic", resolveRequestSettings(cfgWith("full"), "").Level)
	})
	t.Run("off mode ignores the selector", func(t *testing.T) {
		assert.Equal(t, "intrinsic", resolveRequestSettings(cfgWith("off"), "strict").Level)
		assert.Equal(t, "intrinsic", resolveRequestSettings(cfgWith(""), "strict").Level)
	})
	t.Run("profiles mode selects a named profile", func(t *testing.T) {
		assert.Equal(t, "authoritative", resolveRequestSettings(cfgWith("profiles"), "strict").Level)
	})
	t.Run("profiles mode rejects a bare level word (restricted)", func(t *testing.T) {
		assert.Equal(t, "intrinsic", resolveRequestSettings(cfgWith("profiles"), "authoritative").Level)
	})
	t.Run("profiles mode ignores an unknown profile", func(t *testing.T) {
		assert.Equal(t, "intrinsic", resolveRequestSettings(cfgWith("profiles"), "nope").Level)
	})
	t.Run("full mode accepts a level word", func(t *testing.T) {
		assert.Equal(t, "authoritative", resolveRequestSettings(cfgWith("full"), "authoritative").Level)
	})
	t.Run("full mode selects a profile by name", func(t *testing.T) {
		assert.Equal(t, "authoritative", resolveRequestSettings(cfgWith("full"), "strict").Level)
	})
}

func TestParseBehavior(t *testing.T) {
	cases := map[string]struct {
		want integrity.Behavior
		ok   bool
	}{
		"reject":    {integrity.BehaviorError, true},
		"soft-flag": {integrity.BehaviorRecord, true},
		"off":       {integrity.BehaviorIgnore, true},
		"":          {integrity.BehaviorError, false},
		"nonsense":  {integrity.BehaviorError, false},
	}
	for in, exp := range cases {
		got, ok := parseBehavior(in)
		assert.Equal(t, exp.ok, ok, in)
		if exp.ok {
			assert.Equal(t, exp.want, got, in)
		}
	}
}
