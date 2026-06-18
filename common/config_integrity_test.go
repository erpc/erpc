package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestIntegrityConfig_YAMLInlineRoundTrip(t *testing.T) {
	src := `
level: corroborated
headerMode: profiles
budget:
  maxPerSecond: 50
  maxConcurrent: 8
invalidBehavior:
  finalized: reject
  unfinalized: soft-flag
checks:
  receiptVsBlock:
    enabled: true
    onFailure: reject
  bloomMatch:
    params:
      mode: superset
profiles:
  strict:
    level: authoritative
    invalidBehavior:
      finalized: reject
      unfinalized: reject
`
	var cfg IntegrityConfig
	require.NoError(t, yaml.Unmarshal([]byte(src), &cfg))

	// Inline IntegritySettings fields must hoist to the top level.
	assert.Equal(t, "corroborated", cfg.Level)
	assert.Equal(t, "profiles", cfg.HeaderMode)

	require.NotNil(t, cfg.Budget)
	assert.Equal(t, 50, cfg.Budget.MaxPerSecond)
	assert.Equal(t, 8, cfg.Budget.MaxConcurrent)

	require.NotNil(t, cfg.InvalidBehavior)
	assert.Equal(t, "reject", cfg.InvalidBehavior.Finalized)
	assert.Equal(t, "soft-flag", cfg.InvalidBehavior.Unfinalized)

	require.Contains(t, cfg.Checks, "receiptVsBlock")
	require.NotNil(t, cfg.Checks["receiptVsBlock"].Enabled)
	assert.True(t, *cfg.Checks["receiptVsBlock"].Enabled)
	assert.Equal(t, "reject", cfg.Checks["receiptVsBlock"].OnFailure)
	assert.Equal(t, "superset", cfg.Checks["bloomMatch"].Params["mode"])

	require.Contains(t, cfg.Profiles, "strict")
	assert.Equal(t, "authoritative", cfg.Profiles["strict"].Level)
	require.NotNil(t, cfg.Profiles["strict"].InvalidBehavior)
	assert.Equal(t, "reject", cfg.Profiles["strict"].InvalidBehavior.Unfinalized)
}

func TestMergeIntegrityConfig(t *testing.T) {
	project := &IntegrityConfig{
		IntegritySettings: IntegritySettings{Level: "corroborated", Budget: &IntegrityBudgetConfig{MaxPerSecond: 10}},
		HeaderMode:        "off",
		Profiles:          map[string]*IntegritySettings{"a": {Level: "intrinsic"}},
	}
	network := &IntegrityConfig{
		IntegritySettings: IntegritySettings{Level: "authoritative"}, // overrides level only
		Profiles:          map[string]*IntegritySettings{"b": {Level: "off"}},
	}

	t.Run("nil base returns over", func(t *testing.T) {
		assert.Equal(t, "authoritative", MergeIntegrityConfig(nil, network).Level)
	})
	t.Run("nil over returns base", func(t *testing.T) {
		assert.Equal(t, "corroborated", MergeIntegrityConfig(project, nil).Level)
	})
	t.Run("network overrides project; unspecified fields inherited", func(t *testing.T) {
		m := MergeIntegrityConfig(project, network)
		assert.Equal(t, "authoritative", m.Level) // network wins
		require.NotNil(t, m.Budget)
		assert.Equal(t, 10, m.Budget.MaxPerSecond) // inherited from project
		assert.Equal(t, "off", m.HeaderMode)       // inherited (network unset)
		assert.Contains(t, m.Profiles, "a")        // project profile kept
		assert.Contains(t, m.Profiles, "b")        // network profile added
	})
	t.Run("result is a deep copy of inputs", func(t *testing.T) {
		m := MergeIntegrityConfig(project, network)
		m.Budget.MaxPerSecond = 999
		assert.Equal(t, 10, project.Budget.MaxPerSecond)
	})
}

func TestIntegrityConfig_CopyIsDeep(t *testing.T) {
	tru := true
	orig := &IntegrityConfig{
		IntegritySettings: IntegritySettings{
			Level:           "authoritative",
			Checks:          map[string]*IntegrityCheckConfig{"x": {Enabled: &tru, Params: map[string]string{"a": "b"}}},
			Budget:          &IntegrityBudgetConfig{MaxPerSecond: 10},
			InvalidBehavior: &IntegrityInvalidBehaviorConfig{Finalized: "reject", Unfinalized: "soft-flag"},
		},
		HeaderMode: "full",
		Profiles:   map[string]*IntegritySettings{"p": {Level: "intrinsic"}},
	}
	cp := orig.Copy()

	// Mutate every nested field of the original; the copy must be unaffected.
	orig.Level = "off"
	*orig.Checks["x"].Enabled = false
	orig.Checks["x"].Params["a"] = "z"
	orig.Budget.MaxPerSecond = 0
	orig.InvalidBehavior.Finalized = "off"
	orig.Profiles["p"].Level = "off"

	assert.Equal(t, "authoritative", cp.Level)
	assert.Equal(t, "full", cp.HeaderMode)
	assert.True(t, *cp.Checks["x"].Enabled)
	assert.Equal(t, "b", cp.Checks["x"].Params["a"])
	assert.Equal(t, 10, cp.Budget.MaxPerSecond)
	assert.Equal(t, "reject", cp.InvalidBehavior.Finalized)
	assert.Equal(t, "intrinsic", cp.Profiles["p"].Level)
}
