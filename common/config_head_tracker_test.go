package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestEvmHeadTrackerConfig_ParseDefaultsValidate(t *testing.T) {
	var n EvmNetworkConfig
	require.NoError(t, yaml.Unmarshal([]byte(`
chainId: 1
headTracker:
  enabled: true
  fullBlocks: true
  interval:
    blockTimeMultiplier: 1
    fallback: 2s
`), &n))
	require.True(t, n.HeadTrackerEnabled())
	require.True(t, n.HeadTracker.FullBlocks)
	require.Equal(t, 1.0, n.HeadTracker.Interval.BlockTimeMultiplier)
	require.Equal(t, 2*time.Second, n.HeadTracker.Interval.Fallback.Duration())
	n.HeadTracker.SetDefaults()
	require.Equal(t, DefaultHeadTrackerLeaseTtl, n.HeadTracker.LeaseTtl.Duration())
	require.NoError(t, n.HeadTracker.Validate())

	n.HeadTracker.LeaseTtl = Duration(100 * time.Millisecond)
	require.Error(t, n.HeadTracker.Validate())
}

func TestEvmHeadTrackerConfig_DisabledOrAbsentIsOff(t *testing.T) {
	var nilCfg *EvmNetworkConfig
	require.False(t, nilCfg.HeadTrackerEnabled())
	require.False(t, (&EvmNetworkConfig{}).HeadTrackerEnabled())
	require.False(t, (&EvmNetworkConfig{HeadTracker: &EvmHeadTrackerConfig{}}).HeadTrackerEnabled())
	var c *EvmHeadTrackerConfig
	c.SetDefaults()
	require.NoError(t, c.Validate())
	require.Nil(t, c.Copy())
}

func TestEvmHeadTrackerConfig_CopyIsDeep(t *testing.T) {
	a := &EvmHeadTrackerConfig{Enabled: true, Interval: &BlockTimeAdaptiveDuration{BlockTimeMultiplier: 1}}
	b := a.Copy()
	b.Interval.BlockTimeMultiplier = 2
	require.Equal(t, 1.0, a.Interval.BlockTimeMultiplier)
}
