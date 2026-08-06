package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every rule guards a silent runtime failure: an unknown level enables zero
// checks, an unknown check id does nothing, an unknown behavior keeps the
// default. Validation is the only place a typo is visible.

func TestIntegrityConfigValidate(t *testing.T) {
	// The check-id catalog is fed by the integrity package's init; common's
	// own tests don't link it, so register a couple of ids for the test.
	RegisterIntegrityCheckID("hashStability")
	RegisterIntegrityCheckID("receiptVsBlock")

	t.Run("nil config is valid", func(t *testing.T) {
		var c *IntegrityConfig
		assert.NoError(t, c.Validate())
	})

	t.Run("full valid config passes", func(t *testing.T) {
		c := &IntegrityConfig{
			IntegritySettings: IntegritySettings{
				Level: "authoritative",
				Checks: map[string]*IntegrityCheckConfig{
					"receiptVsBlock": {Enabled: boolPtr(true), OnFailure: "soft-flag"},
				},
				InvalidBehavior: &IntegrityInvalidBehaviorConfig{Finalized: "reject", Unfinalized: "soft-flag"},
				Budget:          &IntegrityBudgetConfig{MaxPerSecond: 50, MaxConcurrent: 8},
				ReorgWindow:     256,
			},
			HeaderMode: "profiles",
			Profiles:   map[string]*IntegritySettings{"strict": {Level: "authoritative"}},
		}
		assert.NoError(t, c.Validate())
	})

	t.Run("level is case-insensitive (runtime normalizes)", func(t *testing.T) {
		c := &IntegrityConfig{IntegritySettings: IntegritySettings{Level: "Intrinsic"}}
		assert.NoError(t, c.Validate())
	})

	t.Run("unknown level rejected", func(t *testing.T) {
		c := &IntegrityConfig{IntegritySettings: IntegritySettings{Level: "intrinsik"}}
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "integrity.level")
	})

	t.Run("unknown check id rejected with the known list", func(t *testing.T) {
		c := &IntegrityConfig{IntegritySettings: IntegritySettings{
			Checks: map[string]*IntegrityCheckConfig{"hashStabilty": {}},
		}}
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "hashStabilty")
		assert.Contains(t, err.Error(), "known ids")
	})

	t.Run("unknown onFailure rejected", func(t *testing.T) {
		c := &IntegrityConfig{IntegritySettings: IntegritySettings{
			Checks: map[string]*IntegrityCheckConfig{"hashStability": {OnFailure: "soft_flagg"}},
		}}
		require.Error(t, c.Validate())
	})

	t.Run("unknown invalidBehavior rejected", func(t *testing.T) {
		c := &IntegrityConfig{IntegritySettings: IntegritySettings{
			InvalidBehavior: &IntegrityInvalidBehaviorConfig{Unfinalized: "rekect"},
		}}
		require.Error(t, c.Validate())
	})

	t.Run("unknown headerMode rejected", func(t *testing.T) {
		c := &IntegrityConfig{HeaderMode: "ful"}
		require.Error(t, c.Validate())
	})

	t.Run("bad profile settings rejected with the profile name", func(t *testing.T) {
		c := &IntegrityConfig{Profiles: map[string]*IntegritySettings{"lenient": {Level: "nope"}}}
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "profiles.lenient")
	})

	t.Run("negative budget rejected", func(t *testing.T) {
		c := &IntegrityConfig{IntegritySettings: IntegritySettings{
			Budget: &IntegrityBudgetConfig{MaxPerSecond: -1},
		}}
		require.Error(t, c.Validate())
	})
}
