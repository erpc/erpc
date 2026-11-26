package consensus

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func TestConsensusRun_MarksWinnerFromPolicy(t *testing.T) {
	lg := zerolog.Nop()
	yes, no := true, false

	runOnce := func(t *testing.T, c *Consensus) *common.NormalizedResponse {
		t.Helper()
		req := newTestRequest()
		resp, err := c.Run(context.Background(), req, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return validResponseWithValue("0x1"), nil
		})
		require.NoError(t, err)
		require.NotNil(t, resp)
		return resp
	}

	t.Run("defaultTrue", func(t *testing.T) {
		c := newBuilder().
			WithLogger(&lg).
			WithMaxParticipants(1).
			WithAgreementThreshold(1).
			WithLowParticipantsBehavior(common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult).
			WithCacheableResponses(&yes).
			Build()
		resp := runOnce(t, c)
		require.NotNil(t, resp.Cacheable())
		assert.True(t, *resp.Cacheable())
	})

	t.Run("fallbackFalse", func(t *testing.T) {
		c := newBuilder().
			WithLogger(&lg).
			WithMaxParticipants(1).
			WithAgreementThreshold(1).
			WithLowParticipantsBehavior(common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult).
			WithCacheableResponses(&no).
			Build()
		resp := runOnce(t, c)
		require.NotNil(t, resp.Cacheable())
		assert.False(t, *resp.Cacheable())
	})

	t.Run("namedPolicyMarkViaSelector", func(t *testing.T) {
		c, err := NewConsensus(&common.ConsensusPolicyConfig{
			MaxParticipants:         1,
			AgreementThreshold:      1,
			LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
			CacheableResponses:      &yes,
			CustomPolicy: &common.ConsensusCustomPolicyConfig{
				EvalFunction: `(ctx) => "fallback"`,
			},
			Policies: map[string]*common.ConsensusPolicyConfig{
				"fallback": {
					MaxParticipants:         1,
					AgreementThreshold:      1,
					LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
					CacheableResponses:      &no,
				},
			},
		}, &lg, "")
		require.NoError(t, err)
		resp := runOnce(t, c)
		require.NotNil(t, resp.Cacheable())
		assert.False(t, *resp.Cacheable(), "selector-chosen fallback must stamp false")
	})
}
