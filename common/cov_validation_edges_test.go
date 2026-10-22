package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCovValidation_NetworkDefaultsNilIsValid(t *testing.T) {
	var n *NetworkDefaults
	require.NoError(t, n.Validate())
}

func TestCovValidation_ServedTipGuaranteedMethodsPattern(t *testing.T) {
	e := cvValidEvmNetwork(1).Evm
	e.ServedTip = &EvmServedTipConfig{GuaranteedMethods: []string{"(eth_call"}}
	require.ErrorContains(t, e.Validate(), `network.*.evm.servedTip.guaranteedMethods has invalid pattern "(eth_call"`)
}

func TestCovValidation_ChainTrackerReorgWindow(t *testing.T) {
	e := cvValidEvmNetwork(1).Evm
	e.ChainTracker = &EvmChainTrackerConfig{ReorgWindow: -1}
	require.ErrorContains(t, e.Validate(), "network.*.evm.chainTracker.reorgWindow must be >= 0")
}

func TestCovValidation_SvmBlockAvailabilityUpperBound(t *testing.T) {
	c := &SvmBlockAvailabilityConfig{Upper: &SvmAvailabilityBoundConfig{}}
	require.ErrorContains(t, c.Validate(), "upstream.*.svm.blockAvailability.upper is invalid: bound must set exactly one of")
}

func TestCovValidation_IntegritySettingsEdges(t *testing.T) {
	cvRunCases(t, func() *IntegrityConfig {
		return &IntegrityConfig{
			Profiles: map[string]*IntegritySettings{"lenient": nil, "strict": {Level: "corroborated"}},
		}
	}, (*IntegrityConfig).Validate, []cvCase[IntegrityConfig]{
		{"empty profile name", func(i *IntegrityConfig) { i.Profiles[" "] = &IntegritySettings{} }, "integrity.profiles contains an empty profile name"},
		{"invalid profile", func(i *IntegrityConfig) { i.Profiles["strict"].Level = "max" }, "integrity.profiles.strict: integrity.level 'max' is invalid"},
		{"invalid misbehaviors destination", func(i *IntegrityConfig) {
			i.MisbehaviorsDestination = &MisbehaviorsDestinationConfig{Type: "file", Path: "rel"}
		}, "integrity.misbehaviorsDestination: consensus.misbehaviorsDestination.path must be an absolute path"},
		{"valid misbehaviors destination", func(i *IntegrityConfig) {
			i.MisbehaviorsDestination = &MisbehaviorsDestinationConfig{Type: "file", Path: "/var/erpc"}
		}, ""},
	})
}
