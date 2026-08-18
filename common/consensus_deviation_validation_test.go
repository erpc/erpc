package common

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func validConsensusPolicyConfig() *ConsensusPolicyConfig {
	return &ConsensusPolicyConfig{
		MaxParticipants:    3,
		AgreementThreshold: 2,
		PreferHighestValueFor: map[string][]string{
			"eth_getTransactionCount": {"result"},
		},
	}
}

func TestConsensusPolicyConfig_Validate_PreferHighestValueForMaxDeviationPct(t *testing.T) {
	t.Run("unset is valid", func(t *testing.T) {
		require.NoError(t, validConsensusPolicyConfig().Validate())
	})
	t.Run("zero is valid", func(t *testing.T) {
		cfg := validConsensusPolicyConfig()
		cfg.PreferHighestValueForMaxDeviationPct = map[string]float64{"eth_getTransactionCount": 0}
		require.NoError(t, cfg.Validate())
	})
	t.Run("positive is valid", func(t *testing.T) {
		cfg := validConsensusPolicyConfig()
		cfg.PreferHighestValueForMaxDeviationPct = map[string]float64{"eth_getTransactionCount": 50}
		require.NoError(t, cfg.Validate())
	})
	t.Run("negative is rejected", func(t *testing.T) {
		cfg := validConsensusPolicyConfig()
		cfg.PreferHighestValueForMaxDeviationPct = map[string]float64{"eth_getTransactionCount": -1}
		err := cfg.Validate()
		require.ErrorContains(t, err, "must not be negative")
	})
	t.Run("NaN is rejected", func(t *testing.T) {
		cfg := validConsensusPolicyConfig()
		cfg.PreferHighestValueForMaxDeviationPct = map[string]float64{"eth_getTransactionCount": math.NaN()}
		err := cfg.Validate()
		require.ErrorContains(t, err, "must be a finite number")
	})
	t.Run("+Inf is rejected", func(t *testing.T) {
		cfg := validConsensusPolicyConfig()
		cfg.PreferHighestValueForMaxDeviationPct = map[string]float64{"eth_getTransactionCount": math.Inf(1)}
		err := cfg.Validate()
		require.ErrorContains(t, err, "must be a finite number")
	})
	t.Run("-Inf is rejected", func(t *testing.T) {
		cfg := validConsensusPolicyConfig()
		cfg.PreferHighestValueForMaxDeviationPct = map[string]float64{"eth_getTransactionCount": math.Inf(-1)}
		err := cfg.Validate()
		require.ErrorContains(t, err, "must be a finite number")
	})
	t.Run("missing corresponding preferHighestValueFor entry is rejected", func(t *testing.T) {
		cfg := validConsensusPolicyConfig()
		cfg.PreferHighestValueForMaxDeviationPct = map[string]float64{"eth_gasPrice": 50}
		err := cfg.Validate()
		require.ErrorContains(t, err, "has no corresponding consensus.preferHighestValueFor")
	})
}
