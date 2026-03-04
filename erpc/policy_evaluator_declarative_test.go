package erpc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPolicyEvaluator_DeclarativeRules(t *testing.T) {
	logger := log.Logger

	t.Run("RulesOnlyModeSelectsHealthyUpstreams", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ntw, ups1, ups2, _ := createTestNetwork(t, ctx)

		ntw.metricsTracker.RecordUpstreamRequest(ups1, "eth_call")
		ntw.metricsTracker.RecordUpstreamFailure(ups1, "eth_call", fmt.Errorf("boom"))

		ntw.metricsTracker.RecordUpstreamRequest(ups2, "eth_call")
		ntw.metricsTracker.RecordUpstreamDuration(ups2, "eth_call", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")

		cfg := &common.SelectionPolicyConfig{
			EvalInterval: common.Duration(50 * time.Millisecond),
			Rules: []*common.SelectionPolicyRuleConfig{
				{
					MatchUpstreamID: "*",
					Action:          common.SelectionPolicyRuleActionExclude,
				},
				{
					MatchUpstreamID: ups2.Id(),
					Action:          common.SelectionPolicyRuleActionInclude,
				},
			},
		}
		require.NoError(t, cfg.SetDefaults())
		require.False(t, cfg.UsesEvalFunction())

		evaluator, err := NewPolicyEvaluator("evm:123", &logger, cfg, ntw.upstreamsRegistry, ntw.metricsTracker)
		require.NoError(t, err)
		require.NoError(t, evaluator.Start(ctx))

		require.Eventually(t, func() bool {
			err := evaluator.AcquirePermit(&logger, ups1, "eth_call")
			var excluded *common.ErrUpstreamExcludedByPolicy
			return errors.As(err, &excluded)
		}, 2*time.Second, 20*time.Millisecond)

		var excluded *common.ErrUpstreamExcludedByPolicy
		require.ErrorAs(t, evaluator.AcquirePermit(&logger, ups1, "eth_call"), &excluded)
		assert.NoError(t, evaluator.AcquirePermit(&logger, ups2, "eth_call"))
	})

	t.Run("EvalFunctionHasPrecedenceWhenBothConfigured", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ntw, ups1, ups2, _ := createTestNetwork(t, ctx)

		ntw.metricsTracker.RecordUpstreamRequest(ups1, "eth_call")
		ntw.metricsTracker.RecordUpstreamFailure(ups1, "eth_call", fmt.Errorf("boom"))
		ntw.metricsTracker.RecordUpstreamRequest(ups2, "eth_call")
		ntw.metricsTracker.RecordUpstreamDuration(ups2, "eth_call", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")

		evalFn, err := common.CompileFunction(`
			(upstreams) => upstreams
		`)
		require.NoError(t, err)

		cfg := &common.SelectionPolicyConfig{
			EvalInterval: common.Duration(50 * time.Millisecond),
			EvalFunction: evalFn,
			Rules: []*common.SelectionPolicyRuleConfig{
				{
					MatchUpstreamID: "*",
					Action:          common.SelectionPolicyRuleActionExclude,
				},
				{
					MatchUpstreamID: ups2.Id(),
					Action:          common.SelectionPolicyRuleActionInclude,
				},
			},
		}
		require.NoError(t, cfg.SetDefaults())
		require.True(t, cfg.UsesEvalFunction())

		evaluator, err := NewPolicyEvaluator("evm:123", &logger, cfg, ntw.upstreamsRegistry, ntw.metricsTracker)
		require.NoError(t, err)
		require.NoError(t, evaluator.Start(ctx))

		require.Eventually(t, func() bool {
			return evaluator.AcquirePermit(&logger, ups1, "eth_call") == nil &&
				evaluator.AcquirePermit(&logger, ups2, "eth_call") == nil
		}, 2*time.Second, 20*time.Millisecond)

		assert.NoError(t, evaluator.AcquirePermit(&logger, ups1, "eth_call"))
		assert.NoError(t, evaluator.AcquirePermit(&logger, ups2, "eth_call"))
	})
}

func TestPolicyEvaluator_DeclarativeRules_InvalidRuntimePatternReturnsError(t *testing.T) {
	evaluator := &PolicyEvaluator{
		config: &common.SelectionPolicyConfig{
			Rules: []*common.SelectionPolicyRuleConfig{
				{
					MatchUpstreamID: "(invalid",
					Action:          common.SelectionPolicyRuleActionExclude,
				},
			},
		},
	}

	_, err := evaluator.evaluateWithRules("eth_call", []metricData{
		{
			"id":     "rpc1",
			"config": &common.UpstreamConfig{Group: "primary"},
			"metrics": map[string]interface{}{
				"errorRate":          0.0,
				"blockHeadLag":       0.0,
				"finalizationLag":    0.0,
				"p90ResponseSeconds": 0.0,
				"p95ResponseSeconds": 0.0,
				"p99ResponseSeconds": 0.0,
				"throttledRate":      0.0,
			},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "matchUpstreamId")
}
