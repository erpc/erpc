package common

import (
	"testing"

	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func init() {
	util.ConfigureTestLogger()
}

func TestParseRequestedCommitment(t *testing.T) {
	assert.Equal(t, CommitmentNone, ParseRequestedCommitment(""))
	assert.Equal(t, CommitmentProcessed, ParseRequestedCommitment("processed"))
	assert.Equal(t, CommitmentConfirmed, ParseRequestedCommitment("CONFIRMED"))
	assert.Equal(t, CommitmentFinalized, ParseRequestedCommitment(" finalized "))
	assert.Equal(t, CommitmentUnknown, ParseRequestedCommitment("recent"))
	assert.Equal(t, CommitmentUnknown, ParseRequestedCommitment("max"))
}

func TestFailsafeConfig_MatchCommitmentValidate(t *testing.T) {
	t.Run("accepts valid tokens", func(t *testing.T) {
		f := &FailsafeConfig{
			MatchMethod:     "*",
			MatchCommitment: []CommitmentLevel{CommitmentNone, CommitmentProcessed, CommitmentConfirmed, CommitmentFinalized},
		}
		require.NoError(t, f.Validate())
	})
	t.Run("rejects unknown", func(t *testing.T) {
		f := &FailsafeConfig{
			MatchMethod:     "*",
			MatchCommitment: []CommitmentLevel{CommitmentUnknown},
		}
		err := f.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "matchCommitment")
	})
	t.Run("yaml unmarshal", func(t *testing.T) {
		var f FailsafeConfig
		err := yaml.Unmarshal([]byte(`
matchMethod: "*"
matchCommitment: [none, finalized]
`), &f)
		require.NoError(t, err)
		assert.Equal(t, []CommitmentLevel{CommitmentNone, CommitmentFinalized}, f.MatchCommitment)
	})
	t.Run("yaml rejects invalid token", func(t *testing.T) {
		var f FailsafeConfig
		err := yaml.Unmarshal([]byte(`
matchMethod: "*"
matchCommitment: [recent]
`), &f)
		require.Error(t, err)
	})
}

func TestMatchCommitments(t *testing.T) {
	assert.True(t, MatchCommitments(nil, []CommitmentLevel{CommitmentFinalized}))
	assert.True(t, MatchCommitments([]CommitmentLevel{CommitmentNone}, nil))
	assert.True(t, MatchCommitments(
		[]CommitmentLevel{CommitmentNone, CommitmentConfirmed},
		[]CommitmentLevel{CommitmentConfirmed},
	))
	assert.False(t, MatchCommitments(
		[]CommitmentLevel{CommitmentProcessed},
		[]CommitmentLevel{CommitmentFinalized},
	))
}

func TestFailsafeConfig_CopyMatchCommitment(t *testing.T) {
	src := &FailsafeConfig{
		MatchMethod:     "*",
		MatchCommitment: []CommitmentLevel{CommitmentConfirmed},
	}
	cp := src.Copy()
	require.NotNil(t, cp.MatchCommitment)
	cp.MatchCommitment[0] = CommitmentNone
	assert.Equal(t, CommitmentConfirmed, src.MatchCommitment[0])
}
