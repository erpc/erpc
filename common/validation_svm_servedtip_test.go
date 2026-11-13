package common

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestSvmNetworkConfig_ServedTip(t *testing.T) {
	base := func() *SvmNetworkConfig { return &SvmNetworkConfig{Cluster: "mainnet-beta"} }

	t.Run("unset quorum is majority", func(t *testing.T) {
		var nilCfg *SvmNetworkConfig
		assert.Equal(t, ServedTipQuorumMajority, nilCfg.ServedTipQuorum())
		assert.Equal(t, ServedTipQuorumMajority, base().ServedTipQuorum())
		s := base()
		s.ServedTip = &SvmServedTipConfig{}
		require.NoError(t, s.Validate())
		assert.Equal(t, ServedTipQuorumMajority, s.ServedTipQuorum())
	})

	t.Run("explicit quorum is kept", func(t *testing.T) {
		s := base()
		s.ServedTip = &SvmServedTipConfig{Quorum: 1}
		require.NoError(t, s.Validate())
		assert.Equal(t, ServedTipQuorum(1), s.ServedTipQuorum())
	})

	t.Run("quorum below -1 rejected", func(t *testing.T) {
		s := base()
		s.ServedTip = &SvmServedTipConfig{Quorum: -2}
		err := s.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "svm.servedTip.quorum")
	})

	t.Run("YAML", func(t *testing.T) {
		decode := func(src string) (*SvmNetworkConfig, error) {
			var s SvmNetworkConfig
			dec := yaml.NewDecoder(bytes.NewBufferString(src))
			dec.KnownFields(true)
			return &s, dec.Decode(&s)
		}
		s, err := decode("cluster: mainnet-beta\nservedTip:\n  quorum: 1\n")
		require.NoError(t, err)
		assert.Equal(t, ServedTipQuorum(1), s.ServedTipQuorum())

		s, err = decode("cluster: mainnet-beta\nservedTip:\n  quorum: majority\n")
		require.NoError(t, err)
		assert.Equal(t, ServedTipQuorumMajority, s.ServedTipQuorum())

		_, err = decode("cluster: mainnet-beta\nservedTip:\n  quorum: 0\n")
		require.Error(t, err)
	})

	t.Run("networkDefaults copy, not alias", func(t *testing.T) {
		defaults := &SvmNetworkConfig{ServedTip: &SvmServedTipConfig{Quorum: 1}}
		a, b := base(), base()
		mergeSvmNetworkDefaults(a, defaults)
		mergeSvmNetworkDefaults(b, defaults)
		require.NotNil(t, a.ServedTip)
		assert.Equal(t, ServedTipQuorum(1), a.ServedTipQuorum())
		a.ServedTip.Quorum = 3
		assert.Equal(t, ServedTipQuorum(1), b.ServedTipQuorum())
		assert.Equal(t, ServedTipQuorum(1), defaults.ServedTip.Quorum)

		own := base()
		own.ServedTip = &SvmServedTipConfig{Quorum: 2}
		mergeSvmNetworkDefaults(own, defaults)
		assert.Equal(t, ServedTipQuorum(2), own.ServedTipQuorum(), "a network's own value wins")
	})
}
