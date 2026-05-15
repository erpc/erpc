package erpc

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/internal/policy/stdlib"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// TestHealthCheckLastEvaluation pins: after the policy engine has run at
// least one tick, the network exposes a non-zero "last eval" timestamp.
// Health-check exporters use this signal to flag stale selection state.
func TestHealthCheckLastEvaluation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	network := createTestNetworkWithSelectionPolicy(t, ctx)

	// Network.Bootstrap registers the network with the engine, which runs
	// an initial synchronous eval — that decision is the LastEvalAt anchor
	// health-check exporters use.
	last := network.policyEngine.LastEvalAt("evm:123", "*")
	assert.False(t, last.IsZero(), "LastEvalAt should be set after Bootstrap's initial tick")
	assert.WithinDuration(t, time.Now(), last, time.Second,
		"LastEvalAt should be approximately now")

	// A subsequent tick advances the timestamp.
	first := last
	time.Sleep(5 * time.Millisecond)
	policy.TickForTest(network.policyEngine, "evm:123", "*")
	last = network.policyEngine.LastEvalAt("evm:123", "*")
	assert.True(t, last.After(first), "LastEvalAt should advance after another tick")
}

func createTestNetworkWithSelectionPolicy(t *testing.T, ctx context.Context) *Network {
	tracker := health.NewTracker(&log.Logger, "test", time.Minute)
	tracker.Bootstrap(ctx)

	upr := upstream.NewUpstreamsRegistry(
		ctx, &log.Logger, "test",
		[]*common.UpstreamConfig{},
		nil, nil, nil, nil, nil,
		tracker,
		nil,
	)

	engine := policy.NewEngine(ctx, &log.Logger, "test", tracker, stdlib.Install)

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		SelectionPolicy: &common.SelectionPolicyConfig{
			EvalInterval:    common.Duration(0), // frozen — tests drive ticks manually
			EvalTimeout:     common.Duration(50 * time.Millisecond),
		},
	}
	require.NoError(t, networkConfig.SelectionPolicy.SetDefaults())

	network, err := NewNetwork(ctx, &log.Logger, "test", networkConfig, nil, upr, tracker, engine)
	require.NoError(t, err)

	require.NoError(t, network.Bootstrap(ctx))
	network.PinUpstreamOrderForTest()
	return network
}
