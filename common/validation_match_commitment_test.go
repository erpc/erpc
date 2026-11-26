package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFailsafeConfig_Validate_MatchCommitment(t *testing.T) {
	for _, c := range []string{"none", "processed", "confirmed", "finalized"} {
		require.NoError(t, (&FailsafeConfig{MatchMethod: "*", MatchCommitment: []string{c}}).Validate(), c)
	}

	// "recent" is a deprecated alias the matcher would never see on the wire.
	err := (&FailsafeConfig{MatchMethod: "*", MatchCommitment: []string{"recent"}}).Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failsafe.matchCommitment 'recent' is invalid")
}

func TestUpstreamConfig_Validate_RejectsMatchCommitment(t *testing.T) {
	u := &UpstreamConfig{
		Id:       "u1",
		Endpoint: "http://rpc1.localhost",
		Failsafe: []*FailsafeConfig{{MatchMethod: "*", MatchCommitment: []string{"confirmed"}}},
	}
	err := u.Validate(&Config{}, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upstream 'u1': failsafe.matchCommitment is only supported for network-level failsafe")
}
