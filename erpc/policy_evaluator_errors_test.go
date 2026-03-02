package erpc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

func TestPolicyEvaluator_EvaluateUpstreams_ReturnsErrorWhenPerMethodEvaluationFails(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ntw, ups1, _, _ := createTestNetwork(t, ctx)

	evalFn, err := common.CompileFunction(`
		(_upstreams, _method) => {
			throw new Error("boom");
		}
	`)
	require.NoError(t, err)

	cfg := &common.SelectionPolicyConfig{
		EvalInterval:     common.Duration(50 * time.Millisecond),
		EvalPerMethod:    true,
		EvalFunction:     evalFn,
		ResampleInterval: common.Duration(100 * time.Millisecond),
		ResampleCount:    2,
	}

	logger := log.Logger
	mt := ntw.metricsTracker
	evaluator, err := NewPolicyEvaluator("evm:123", &logger, cfg, ntw.upstreamsRegistry, mt)
	require.NoError(t, err)

	// Ensure at least one method exists in GetUpstreamMetrics() map.
	mt.RecordUpstreamRequest(ups1, "method1")
	mt.RecordUpstreamFailure(ups1, "method1", fmt.Errorf("forced-failure"))

	err = evaluator.evaluateUpstreams()
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to evaluate user-defined selectionPolicy")
}
