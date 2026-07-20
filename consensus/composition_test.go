package consensus

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helpers ------------------------------------------------------------------

func taggedUpstream(id string, tags ...string) common.Upstream {
	return common.NewFakeUpstream(id, common.WithTags(tags...))
}

func resultFrom(t *testing.T, ups common.Upstream, value string, index int) *execResult {
	t.Helper()
	jrpc, err := common.NewJsonRpcResponse(1, value, nil)
	require.NoError(t, err)
	return &execResult{
		Result:   common.NewNormalizedResponse().WithJsonRpcResponse(jrpc),
		Upstream: ups,
		Index:    index,
	}
}

func errorFrom(ups common.Upstream, err error, index int) *execResult {
	return &execResult{Err: err, Upstream: ups, Index: index}
}

func analyze(cfg *config, responses []*execResult) *consensusAnalysis {
	lg := zerolog.Nop()
	return newConsensusAnalysis(&lg, context.Background(), cfg, responses)
}

func winnerOf(cfg *config, analysis *consensusAnalysis) *slotResult {
	lg := zerolog.Nop()
	e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
	return e.determineWinner(&lg, analysis)
}

// dedup ----------------------------------------------------------------------

func TestDedup_SameUpstreamVotesOnce(t *testing.T) {
	// Two slots landed on the same upstream (hedge/retry reselection) and
	// agree on the same value. One node must not self-corroborate past
	// agreementThreshold: with threshold 2, one upstream voting twice plus a
	// dissenting second upstream must NOT produce consensus.
	cfg := &config{maxParticipants: 3, agreementThreshold: 2}
	u1 := taggedUpstream("upstream-1")
	u2 := taggedUpstream("upstream-2")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, u1, "0xaa", 0),
		resultFrom(t, u1, "0xaa", 1), // duplicate upstream, same value
		resultFrom(t, u2, "0xbb", 2),
	})

	assert.Equal(t, 2, analysis.totalParticipants, "duplicate upstream must count once")
	assert.Equal(t, 2, analysis.validParticipants)
	for _, g := range analysis.getValidGroups() {
		assert.LessOrEqual(t, g.Count, 1, "no group may contain a duplicated vote")
	}

	winner := winnerOf(cfg, analysis)
	require.NotNil(t, winner.Error, "1-vs-1 after dedup must not reach threshold 2")
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusDispute))
}

func TestDedup_KeepsBestResponsePerUpstream(t *testing.T) {
	// An early infrastructure error from an upstream must not shadow a later
	// valid response from the same upstream (keep-best, not keep-first).
	cfg := &config{maxParticipants: 2, agreementThreshold: 2}
	u1 := taggedUpstream("upstream-1")
	u2 := taggedUpstream("upstream-2")

	infraErr := common.NewErrEndpointServerSideException(nil, nil, 500)
	analysis := analyze(cfg, []*execResult{
		errorFrom(u1, infraErr, 0),   // slot 0: infra error from u1
		resultFrom(t, u2, "0xaa", 1), // slot 1: valid from u2
		resultFrom(t, u1, "0xaa", 0), // u1 retried elsewhere: valid
	})

	assert.Equal(t, 2, analysis.totalParticipants)
	assert.Equal(t, 2, analysis.validParticipants, "u1's valid retry must replace its earlier infra error")

	winner := winnerOf(cfg, analysis)
	require.Nil(t, winner.Error)
	require.NotNil(t, winner.Result, "both upstreams agree on 0xaa - consensus expected")
}
func TestDedup_EquivocatingUpstreamLatestVoteWins(t *testing.T) {
	// A re-answering upstream near the chain tip can legitimately see
	// advanced state between hedge legs. Its LATEST answer is its vote:
	// keeping the first would drop a fresher vote that agrees with the
	// eventual winner and manufacture a dispute out of thin air.
	cfg := &config{maxParticipants: 3, agreementThreshold: 2}
	u1 := taggedUpstream("upstream-1")
	u2 := taggedUpstream("upstream-2")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, u1, "0x100", 0), // stale first answer
		resultFrom(t, u1, "0x101", 1), // fresher answer after state advanced
		resultFrom(t, u2, "0x101", 2),
	})

	assert.Equal(t, 2, analysis.totalParticipants)
	winner := winnerOf(cfg, analysis)
	require.Nil(t, winner.Error, "u1's latest vote agrees with u2 - consensus expected, got: %v", winner.Error)
	require.NotNil(t, winner.Result)
}

func TestDedup_HasRemainingUsesRawCollectedCount(t *testing.T) {
	// A deduplicated duplicate consumed a participant slot. After all
	// maxParticipants slots have answered, nothing more can arrive -
	// hasRemaining must be false even though distinct-vote count is lower,
	// or short-circuits get suppressed and composition-dispute rounds are
	// held open for responses that can never come.
	cfg := &config{maxParticipants: 3, agreementThreshold: 2}
	u1 := taggedUpstream("upstream-1")
	u2 := taggedUpstream("upstream-2")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, u1, "0xaa", 0),
		resultFrom(t, u1, "0xaa", 1), // duplicate: dropped from votes, but its slot answered
		resultFrom(t, u2, "0xbb", 2),
	})

	assert.Equal(t, 2, analysis.totalParticipants, "votes are deduplicated")
	assert.Equal(t, 3, analysis.collectedResponses, "slots answered are not")
	assert.False(t, analysis.hasRemaining(), "all slots answered - nothing can still arrive")
}

// minAgreement gate -----------------------------------------------------------

func mixedQuota(minAgreement int) []*common.ConsensusRequiredParticipant {
	return []*common.ConsensusRequiredParticipant{
		{Tag: "type:internal", MinParticipants: 1, MinAgreement: minAgreement},
	}
}

// dualQuota mirrors the reference mixed-node config: the winning group must
// contain at least one internal AND one external upstream.
func dualQuota() []*common.ConsensusRequiredParticipant {
	return []*common.ConsensusRequiredParticipant{
		{Tag: "type:internal", MinParticipants: 1, MinAgreement: 1},
		{Tag: "type:external", MinParticipants: 1, MinAgreement: 1},
	}
}

func TestMinAgreement_DualQuota_LargestAllExternalGroupDisputes(t *testing.T) {
	// Reference scenario: three groups all at count 2 with minAgreement: 1
	// for BOTH internal and external, preferLargerResponses enabled and
	// disputeBehavior acceptMostCommon. Groups A and B are mixed
	// (internal+external); group C is 2 externals with the LARGEST
	// response. The size preference picks C, but C fails the internal
	// quota -> composition dispute, not C's value.
	cfg := &config{
		maxParticipants:       6,
		agreementThreshold:    2,
		disputeBehavior:       common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		preferLargerResponses: true,
		requiredParticipants:  dualQuota(),
	}
	int1 := taggedUpstream("internal-1", "type:internal")
	int2 := taggedUpstream("internal-2", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")
	ext3 := taggedUpstream("external-3", "type:external")
	ext4 := taggedUpstream("external-4", "type:external")

	large := "0x300" + "00" // strictly largest payload
	analysis := analyze(cfg, []*execResult{
		resultFrom(t, int1, "0x100", 0), // group A: internal + external
		resultFrom(t, ext1, "0x100", 1),
		resultFrom(t, int2, "0x200", 2), // group B: internal + external
		resultFrom(t, ext2, "0x200", 3),
		resultFrom(t, ext3, large, 4), // group C: 2 externals, largest
		resultFrom(t, ext4, large, 5),
	})
	winner := winnerOf(cfg, analysis)

	require.NotNil(t, winner.Error)
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusCompositionDispute),
		"largest all-external group must not win under dual quota, got: %v", winner.Error)
}

func TestMinAgreement_DualQuota_LargestMixedGroupWins(t *testing.T) {
	// Positive control for the scenario above: when the largest group IS
	// mixed, the size preference and the quota agree and it wins outright.
	cfg := &config{
		maxParticipants:       6,
		agreementThreshold:    2,
		disputeBehavior:       common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		preferLargerResponses: true,
		requiredParticipants:  dualQuota(),
	}
	int1 := taggedUpstream("internal-1", "type:internal")
	int2 := taggedUpstream("internal-2", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")
	ext3 := taggedUpstream("external-3", "type:external")
	ext4 := taggedUpstream("external-4", "type:external")

	large := "0x300" + "00"
	analysis := analyze(cfg, []*execResult{
		resultFrom(t, int1, "0x100", 0),
		resultFrom(t, ext1, "0x100", 1),
		resultFrom(t, ext3, "0x200", 2),
		resultFrom(t, ext4, "0x200", 3),
		resultFrom(t, int2, large, 4), // group C: internal + external, largest
		resultFrom(t, ext2, large, 5),
	})
	winner := winnerOf(cfg, analysis)

	require.Nil(t, winner.Error)
	require.NotNil(t, winner.Result, "largest mixed group satisfies both quotas and wins")
	jrr, err := winner.Result.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, string(jrr.GetResultBytes()), "0x30000", "winner must be the largest (mixed) group's value")
}

func TestMinAgreement_SatisfiedWinnerPasses(t *testing.T) {
	cfg := &config{
		maxParticipants:      2,
		agreementThreshold:   2,
		requiredParticipants: mixedQuota(1),
	}
	internal := taggedUpstream("internal-1", "type:internal")
	external := taggedUpstream("external-1", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, internal, "0xaa", 0),
		resultFrom(t, external, "0xaa", 1),
	})
	winner := winnerOf(cfg, analysis)

	require.Nil(t, winner.Error)
	require.NotNil(t, winner.Result, "internal+external agreement satisfies the quota")
}

func TestMinAgreement_CorrelatedWinnerDisputes(t *testing.T) {
	// Two externals agree and meet agreementThreshold, but the winning group
	// contains no internal upstream -> composition dispute, and
	// disputeBehavior must NOT bypass it.
	cfg := &config{
		maxParticipants:      3,
		agreementThreshold:   2,
		disputeBehavior:      common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		requiredParticipants: mixedQuota(1),
	}
	internal := taggedUpstream("internal-1", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xaa", 1),
		resultFrom(t, internal, "0xbb", 2), // internal dissents
	})
	winner := winnerOf(cfg, analysis)

	require.NotNil(t, winner.Error)
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusCompositionDispute),
		"winner without required internal upstream must be a composition dispute, got: %v", winner.Error)
}

func TestMinAgreement_ZeroIsNoOp(t *testing.T) {
	// minAgreement: 0 keeps today's behavior: the correlated externals win.
	cfg := &config{
		maxParticipants:      3,
		agreementThreshold:   2,
		requiredParticipants: mixedQuota(0),
	}
	internal := taggedUpstream("internal-1", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xaa", 1),
		resultFrom(t, internal, "0xbb", 2),
	})
	winner := winnerOf(cfg, analysis)

	require.Nil(t, winner.Error)
	require.NotNil(t, winner.Result)
}

func TestMinAgreement_ShortCircuitDeferredWhileRemaining(t *testing.T) {
	// Externals reach an unassailable lead before the internal upstream has
	// answered. Without the guard, unassailable_lead would cancel the
	// internal request and lock in a composition dispute; with it, the round
	// keeps collecting and the late internal vote completes the quota.
	cfg := &config{
		maxParticipants:      3,
		agreementThreshold:   2,
		requiredParticipants: mixedQuota(1),
	}
	internal := taggedUpstream("internal-1", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	lg := zerolog.Nop()
	e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}

	// Round 1: only the two externals have responded.
	partial := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xaa", 1),
	})
	provisional := e.determineWinner(&lg, partial)
	require.NotNil(t, provisional.Error)
	require.True(t, common.HasErrorCode(provisional.Error, common.ErrCodeConsensusCompositionDispute))

	reason, ok := e.shouldShortCircuit(provisional, partial)
	assert.False(t, ok, "must not short-circuit a provisional composition dispute (reason=%s)", reason)

	// Round 2: the internal response arrives and joins the winning group.
	full := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xaa", 1),
		resultFrom(t, internal, "0xaa", 2),
	})
	final := e.determineWinner(&lg, full)
	require.Nil(t, final.Error)
	require.NotNil(t, final.Result, "late internal agreement must convert the dispute into a win")
}

func TestMinAgreement_AppliesToPreferHighestValueFor(t *testing.T) {
	// The gate sits after determineWinner's rule table, so the
	// preferHighestValueFor rule needs no composition-awareness of its own.
	cfg := &config{
		maxParticipants:       3,
		agreementThreshold:    2,
		preferHighestValueFor: map[string][]string{"eth_getTransactionCount": {"result"}},
		requiredParticipants:  mixedQuota(1),
	}
	internal := taggedUpstream("internal-1", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0x5", 0),
		resultFrom(t, ext2, "0x5", 1),
		resultFrom(t, internal, "0x3", 2),
	})
	analysis.method = "eth_getTransactionCount"

	winner := winnerOf(cfg, analysis)
	require.NotNil(t, winner.Error)
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusCompositionDispute),
		"highest-value winner without the required internal upstream must dispute, got: %v", winner.Error)
}
func TestMinAgreement_ValueAgreementSpansEncodings(t *testing.T) {
	// preferHighestValueFor counts agreement by numeric value, so "0x05"
	// and "0x5" are the same vote in different hash groups. The tagged
	// upstream agreeing with a different encoding must satisfy the quota —
	// not trigger a false composition dispute.
	cfg := &config{
		maxParticipants:       3,
		agreementThreshold:    2,
		preferHighestValueFor: map[string][]string{"eth_getTransactionCount": {"result"}},
		requiredParticipants:  mixedQuota(1),
	}
	internal := taggedUpstream("internal-1", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0x5", 0),
		resultFrom(t, ext2, "0x5", 1),
		resultFrom(t, internal, "0x05", 2), // same value, different encoding
	})
	analysis.method = "eth_getTransactionCount"

	winner := winnerOf(cfg, analysis)
	require.Nil(t, winner.Error, "value-equal encoding must count toward the quota, got: %v", winner.Error)
	require.NotNil(t, winner.Result)
}

func TestMinAgreement_CompositionDisputeDoesNotPunishDissenter(t *testing.T) {
	// On a composition dispute the count-majority itself was rejected;
	// the quota-tagged dissenter must NOT be recorded as misbehaving
	// against that rejected majority.
	cfg := &config{
		maxParticipants:      3,
		agreementThreshold:   2,
		requiredParticipants: mixedQuota(1),
	}
	internal := taggedUpstream("internal-1", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xaa", 1),
		resultFrom(t, internal, "0xbb", 2),
	})
	lg := zerolog.Nop()
	e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
	winner := e.determineWinner(&lg, analysis)
	require.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusCompositionDispute))

	e.trackAndPunishMisbehavingUpstreams(&lg, nil, metricsLabels{}, winner, analysis)

	tracker := internal.Tracker().(*common.FakeHealthTracker)
	assert.False(t, tracker.MisbehaviorRecorded,
		"dissenting quota-tagged upstream must not be punished against a rejected majority")
}

func TestMinAgreement_SendRawTransactionExempt(t *testing.T) {
	// Broadcasts return the first accepted tx hash by design; a tx accepted
	// by any single node propagates network-wide, so winner composition is
	// meaningless there.
	cfg := &config{
		maxParticipants:      2,
		agreementThreshold:   2,
		requiredParticipants: mixedQuota(1),
	}
	ext1 := taggedUpstream("external-1", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xdeadbeef", 0),
	})
	analysis.method = "eth_sendRawTransaction"

	winner := winnerOf(cfg, analysis)
	require.Nil(t, winner.Error)
	require.NotNil(t, winner.Result)
}

func TestMinAgreement_SynthesizedErrorsPassThrough(t *testing.T) {
	// A dispute produced by the rules themselves has no backing group; the
	// gate must not relabel it as a composition dispute.
	cfg := &config{
		maxParticipants:      3,
		agreementThreshold:   3,
		requiredParticipants: mixedQuota(1),
	}
	internal := taggedUpstream("internal-1", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xbb", 1),
		resultFrom(t, internal, "0xcc", 2), // three-way split, threshold 3
	})
	winner := winnerOf(cfg, analysis)

	require.NotNil(t, winner.Error)
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusDispute),
		"a plain dispute must stay a plain dispute, got: %v", winner.Error)
	assert.False(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusCompositionDispute))
}
func TestQuota_DuplicateUpstreamCountsOnce(t *testing.T) {
	// The wait-cap arming path checks quota coverage on the RAW pre-dedup
	// response slice: one tagged upstream answering twice via hedge must
	// not satisfy minAgreement: 2 by itself.
	reqs := []*common.ConsensusRequiredParticipant{
		{Tag: "type:internal", MinParticipants: 2, MinAgreement: 2},
	}
	int1 := taggedUpstream("internal-1", "type:internal")
	int2 := taggedUpstream("internal-2", "type:internal")

	dup := []*execResult{
		resultFrom(t, int1, "0xaa", 0),
		resultFrom(t, int1, "0xaa", 1), // same upstream, second hedge leg
	}
	assert.False(t, resultsSatisfyAgreementQuotas(dup, reqs),
		"one upstream answering twice must not count as two")

	distinct := append(dup, resultFrom(t, int2, "0xaa", 2))
	assert.True(t, resultsSatisfyAgreementQuotas(distinct, reqs))
}

// config validation ------------------------------------------------------------

func TestMinAgreement_Validation(t *testing.T) {
	base := func() *common.ConsensusPolicyConfig {
		return &common.ConsensusPolicyConfig{
			MaxParticipants:    3,
			AgreementThreshold: 2,
		}
	}

	t.Run("minAgreement above minParticipants is rejected", func(t *testing.T) {
		cfg := base()
		cfg.RequiredParticipants = []*common.ConsensusRequiredParticipant{
			{Tag: "type:internal", MinParticipants: 1, MinAgreement: 2},
		}
		require.ErrorContains(t, cfg.Validate(), "minAgreement")
	})

	t.Run("negative minAgreement is rejected", func(t *testing.T) {
		cfg := base()
		cfg.RequiredParticipants = []*common.ConsensusRequiredParticipant{
			{Tag: "type:internal", MinParticipants: 1, MinAgreement: -1},
		}
		require.ErrorContains(t, cfg.Validate(), "minAgreement")
	})

	t.Run("minAgreement equal to minParticipants is accepted", func(t *testing.T) {
		cfg := base()
		cfg.RequiredParticipants = []*common.ConsensusRequiredParticipant{
			{Tag: "type:internal", MinParticipants: 2, MinAgreement: 2},
		}
		require.NoError(t, cfg.Validate())
	})
}
