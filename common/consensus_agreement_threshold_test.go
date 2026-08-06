package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetDefaults_AgreementThresholdFromMinAgreement(t *testing.T) {
	t.Run("derives sum(minAgreement) when agreementThreshold omitted", func(t *testing.T) {
		c := &ConsensusPolicyConfig{
			MaxParticipants: 3,
			RequiredParticipants: []*ConsensusRequiredParticipant{
				{Tag: "type:internal", MinParticipants: 1, MinAgreement: 1},
				{Tag: "type:external", MinParticipants: 1, MinAgreement: 1},
			},
		}
		require.NoError(t, c.SetDefaults())
		assert.Equal(t, 2, c.AgreementThreshold)
		require.NoError(t, c.Validate())
	})

	t.Run("does not overwrite matching explicit agreementThreshold", func(t *testing.T) {
		c := &ConsensusPolicyConfig{
			MaxParticipants:    3,
			AgreementThreshold: 2,
			RequiredParticipants: []*ConsensusRequiredParticipant{
				{Tag: "type:internal", MinParticipants: 1, MinAgreement: 1},
				{Tag: "type:external", MinParticipants: 1, MinAgreement: 1},
			},
		}
		require.NoError(t, c.SetDefaults())
		assert.Equal(t, 2, c.AgreementThreshold)
		require.NoError(t, c.Validate())
	})

	t.Run("explicit agreementThreshold above sum(minAgreement) is honored", func(t *testing.T) {
		c := &ConsensusPolicyConfig{
			MaxParticipants:    4,
			AgreementThreshold: 3,
			RequiredParticipants: []*ConsensusRequiredParticipant{
				{Tag: "type:internal", MinParticipants: 1, MinAgreement: 1},
				{Tag: "type:external", MinParticipants: 1, MinAgreement: 1},
			},
		}
		require.NoError(t, c.SetDefaults())
		assert.Equal(t, 3, c.AgreementThreshold, "an explicit value stricter than the derived default must not be overwritten")
		require.NoError(t, c.Validate())
	})

	t.Run("explicit agreementThreshold below sum(minAgreement) is accepted", func(t *testing.T) {
		// agreementThreshold only gates which groups are even considered as
		// count-winners; enforceWinnerComposition independently and always
		// enforces the true per-tag minAgreement requirement regardless of
		// this value, so a lower explicit value is not unsatisfiable — it's
		// just a looser count gate.
		c := &ConsensusPolicyConfig{
			MaxParticipants:    4,
			AgreementThreshold: 1,
			RequiredParticipants: []*ConsensusRequiredParticipant{
				{Tag: "type:internal", MinParticipants: 1, MinAgreement: 1},
				{Tag: "type:external", MinParticipants: 1, MinAgreement: 1},
			},
		}
		require.NoError(t, c.SetDefaults())
		assert.Equal(t, 1, c.AgreementThreshold, "SetDefaults must not silently overwrite an explicit value")
		require.NoError(t, c.Validate())
	})

	t.Run("single minAgreement entry derives that value", func(t *testing.T) {
		c := &ConsensusPolicyConfig{
			MaxParticipants: 3,
			RequiredParticipants: []*ConsensusRequiredParticipant{
				{Tag: "type:internal", MinParticipants: 2, MinAgreement: 2},
			},
		}
		require.NoError(t, c.SetDefaults())
		assert.Equal(t, 2, c.AgreementThreshold)
		require.NoError(t, c.Validate())
	})

	t.Run("minAgreement zero entries keep default agreementThreshold of 2", func(t *testing.T) {
		c := &ConsensusPolicyConfig{
			MaxParticipants: 3,
			RequiredParticipants: []*ConsensusRequiredParticipant{
				{Tag: "type:internal", MinParticipants: 1, MinAgreement: 0},
				{Tag: "type:external", MinParticipants: 1},
			},
		}
		require.NoError(t, c.SetDefaults())
		assert.Equal(t, 2, c.AgreementThreshold)
		require.NoError(t, c.Validate())
	})

	t.Run("no requiredParticipants keeps default agreementThreshold of 2", func(t *testing.T) {
		c := &ConsensusPolicyConfig{MaxParticipants: 3}
		require.NoError(t, c.SetDefaults())
		assert.Equal(t, 2, c.AgreementThreshold)
		require.NoError(t, c.Validate())
	})
}
