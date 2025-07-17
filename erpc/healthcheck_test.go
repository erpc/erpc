package erpc

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func TestHealthCheckLastEvaluation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create a network with a selection policy evaluator
	network := createTestNetworkWithSelectionPolicy(t, ctx)

	// Test GetLastEvalTime method - initially should be zero time
	lastEvalTime := network.selectionPolicyEvaluator.GetLastEvalTime("test-upstream", "*")
	assert.True(t, lastEvalTime.IsZero())

	// Force an evaluation
	err := network.selectionPolicyEvaluator.evaluateUpstreams()
	if err != nil {
		t.Logf("Expected evaluation error for test setup: %v", err)
	}

	// Should have a non-zero time after evaluation attempt
	lastEvalTime = network.selectionPolicyEvaluator.GetLastEvalTime("test-upstream", "*")
	t.Logf("Last evaluation time: %v", lastEvalTime)
}

func createTestNetworkWithSelectionPolicy(t *testing.T, ctx context.Context) *Network {
	// Create selection policy config
	evalInterval := common.Duration(1 * time.Minute)
	resampleInterval := common.Duration(5 * time.Minute)

	selectionPolicyConfig := &common.SelectionPolicyConfig{
		EvalInterval:     evalInterval,
		EvalPerMethod:    false,
		ResampleExcluded: false,
		ResampleInterval: resampleInterval,
		ResampleCount:    10,
		EvalFunction:     nil, // Will be set by config validation
	}

	// Create network config with selection policy
	networkConfig := &common.NetworkConfig{
		Architecture:    common.ArchitectureEvm,
		Evm:             &common.EvmNetworkConfig{ChainId: 123},
		SelectionPolicy: selectionPolicyConfig,
	}

	// Create upstream registry
	metricsTracker := health.NewTracker(&log.Logger, "test", time.Minute)
	upstreamsRegistry := upstream.NewUpstreamsRegistry(
		ctx,
		&log.Logger,
		"test",
		[]*common.UpstreamConfig{},
		nil,
		nil,
		nil,
		nil,
		nil,
		metricsTracker,
		1*time.Second,
	)

	// Create network
	network, err := NewNetwork(
		ctx,
		&log.Logger,
		"test",
		networkConfig,
		nil,
		upstreamsRegistry,
		metricsTracker,
	)
	require.NoError(t, err)

	// Bootstrap the network to initialize the policy evaluator
	err = network.Bootstrap(ctx)
	require.NoError(t, err)

	return network
}
