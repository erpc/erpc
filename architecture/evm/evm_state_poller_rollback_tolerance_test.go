package evm

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// How far a block head may move before the move counts as MAJOR is a property of
// the CHAIN (its block time), not of EVM in general: a sub-second chain can
// legitimately produce far more than 1024 blocks between two polls, while a slow
// chain never should. These tests pin that `networks[*].evm.
// toleratedBlockHeadRollback` drives both the poller's chain-identity gate and
// the health tracker's rollback acceptance, and that leaving it unset preserves
// the previous universal 1024.

func toleranceTestPoller(t *testing.T, up common.Upstream) (*EvmStatePoller, *health.Tracker) {
	t.Helper()
	appCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "test", 2*time.Second)
	ssr, err := data.NewSharedStateRegistry(appCtx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	require.NoError(t, err)
	return NewEvmStatePoller("test", appCtx, &logger, up, tracker, ssr), tracker
}

// networkCfgWithTolerance builds a fully defaulted network config, so the tests
// exercise the same value an operator's erpc.yaml would produce.
func networkCfgWithTolerance(t *testing.T, tolerance *int64) *common.NetworkConfig {
	t.Helper()
	cfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId:                    123,
			ToleratedBlockHeadRollback: tolerance,
		},
	}
	require.NoError(t, cfg.SetDefaults(nil, nil))
	return cfg
}

func TestToleratedBlockHeadRollback_DefaultsToSharedConstant(t *testing.T) {
	up := newSuggestGateUpstream(123, "123", nil)
	p, _ := toleranceTestPoller(t, up)

	assert.Equal(t, int64(common.DefaultToleratedBlockHeadRollback), p.toleratedBlockHeadRollback(),
		"a poller with no network config attached must behave exactly as before the knob existed")

	cfg := networkCfgWithTolerance(t, nil)
	require.NotNil(t, cfg.Evm.ToleratedBlockHeadRollback, "SetDefaults must materialize the field")
	assert.Equal(t, int64(1024), *cfg.Evm.ToleratedBlockHeadRollback, "default must stay 1024")

	p.SetNetworkConfig(cfg)
	assert.Equal(t, int64(1024), p.toleratedBlockHeadRollback())
}

func TestToleratedBlockHeadRollback_WiderConfigKeepsBigJumpNormal(t *testing.T) {
	// The upstream answers for ANOTHER chain, so any gated (MAJOR) move would be
	// dropped and the upstream cordoned. With a tolerance wide enough for a fast
	// chain, this jump is an ordinary advance and never reaches the gate.
	up := newSuggestGateUpstream(123, "999", nil)
	p, _ := toleranceTestPoller(t, up)
	p.SetNetworkConfig(networkCfgWithTolerance(t, ptrInt64(1_000_000)))

	p.SuggestLatestBlock(1000)
	require.Equal(t, int64(1000), p.LatestBlock())

	p.SuggestLatestBlock(500_000) // MAJOR under the 1024 default, normal here
	assert.Equal(t, int64(500_000), p.LatestBlock(), "an in-tolerance advance applies inline")
	assert.Never(t, up.isCordoned, 200*time.Millisecond, 20*time.Millisecond,
		"an in-tolerance advance must never reach the chain-identity gate")
}

func TestToleratedBlockHeadRollback_NarrowerConfigMakesSmallJumpMajor(t *testing.T) {
	up := newSuggestGateUpstream(123, "999", nil)
	p, _ := toleranceTestPoller(t, up)
	p.SetNetworkConfig(networkCfgWithTolerance(t, ptrInt64(10)))

	p.SuggestLatestBlock(1000)
	require.Equal(t, int64(1000), p.LatestBlock())

	p.SuggestLatestBlock(1100) // normal under the 1024 default, MAJOR here
	require.Eventually(t, up.isCordoned, 2*time.Second, 10*time.Millisecond,
		"beyond the configured tolerance the jump must be verified, and a cross-wired upstream cordoned")
	assert.Equal(t, int64(1000), p.LatestBlock(), "the unverified jump must not enter the shared counter")
}

// SetNetworkConfig is the only place the poller and the network config meet, so
// it is where the tracker learns the network's tolerance. Without that hand-off
// the tracker would keep judging every network by the 1024 default.
func TestToleratedBlockHeadRollback_TrackerHonorsConfiguredTolerance(t *testing.T) {
	latestGauge := func(up common.Upstream) float64 {
		return promUtil.ToFloat64(telemetry.MetricUpstreamLatestBlockNumber.
			WithLabelValues("test", up.VendorName(), up.NetworkLabel(), up.Id()))
	}

	t.Run("ConfiguredToleranceApplies", func(t *testing.T) {
		up := newSuggestGateUpstream(123, "123", nil)
		up.id = "tolerance-configured-ups"
		p, tracker := toleranceTestPoller(t, up)
		p.SetNetworkConfig(networkCfgWithTolerance(t, ptrInt64(10)))

		tracker.SetLatestBlockNumber(up, 10_000, 0)
		tracker.SetLatestBlockNumber(up, 9_950, 0) // 50 blocks back: > 10 → a correction

		assert.Equal(t, float64(9_950), latestGauge(up),
			"a rollback beyond the configured tolerance must be applied")
	})

	t.Run("DefaultToleranceWithoutNetworkConfig", func(t *testing.T) {
		up := newSuggestGateUpstream(123, "123", nil)
		up.id = "tolerance-default-ups"
		_, tracker := toleranceTestPoller(t, up)

		tracker.SetLatestBlockNumber(up, 10_000, 0)
		tracker.SetLatestBlockNumber(up, 9_950, 0) // within the 1024 default → noise

		assert.Equal(t, float64(10_000), latestGauge(up),
			"without a configured tolerance the previous 1024 behavior must hold")
	})
}

func ptrInt64(v int64) *int64 { return &v }
