package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelectionPolicyDefaultsAndPrecedence(t *testing.T) {
	t.Run("DeclarativeRulesModeWhenNoEvalFunction", func(t *testing.T) {
		maxErrorRate := 0.5
		cfg := &SelectionPolicyConfig{
			EvalInterval: Duration(30 * time.Second),
			Rules: []*SelectionPolicyRuleConfig{
				{
					Action:       SelectionPolicyRuleActionExclude,
					MaxErrorRate: &maxErrorRate,
				},
			},
		}

		err := cfg.SetDefaults()
		require.NoError(t, err)
		assert.False(t, cfg.UsesEvalFunction())
		assert.Equal(t, "rules", cfg.EffectiveMode())
		assert.NoError(t, cfg.Validate())
	})

	t.Run("EvalFunctionPrecedenceOverRules", func(t *testing.T) {
		evalFn, err := CompileFunction(`
			(upstreams) => upstreams
		`)
		require.NoError(t, err)

		maxErrorRate := 0.1
		cfg := &SelectionPolicyConfig{
			EvalInterval: Duration(30 * time.Second),
			EvalFunction: evalFn,
			Rules: []*SelectionPolicyRuleConfig{
				{
					Action:       SelectionPolicyRuleActionExclude,
					MaxErrorRate: &maxErrorRate,
				},
			},
		}

		err = cfg.SetDefaults()
		require.NoError(t, err)
		assert.True(t, cfg.UsesEvalFunction())
		assert.True(t, cfg.HasIgnoredRules())
		assert.Equal(t, "evalFunction", cfg.EffectiveMode())
		assert.NoError(t, cfg.Validate())
	})

	t.Run("ValidateRequiresEvalFunctionOrRules", func(t *testing.T) {
		cfg := &SelectionPolicyConfig{
			EvalInterval: Duration(1 * time.Minute),
		}

		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "selectionPolicy.evalFunction or selectionPolicy.rules is required")
	})
}
