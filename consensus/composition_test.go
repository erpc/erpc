package consensus

import (
	"context"
	"fmt"
	"math/big"
	"net/url"
	"testing"
	"time"

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
	return e.determineWinner(context.Background(), &lg, analysis)
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
	provisional := e.determineWinner(context.Background(), &lg, partial)
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
	final := e.determineWinner(context.Background(), &lg, full)
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
	winner := e.determineWinner(context.Background(), &lg, analysis)
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

// MissingData waiver ----------------------------------------------------------

// waiverQuota mirrors R9's minimum viable waiver config: a waivable internal
// quota kept honest by a never-waivable external floor.
func waiverQuota() []*common.ConsensusRequiredParticipant {
	return []*common.ConsensusRequiredParticipant{
		{Tag: "type:internal", MinParticipants: 1, MinAgreement: 1, WaiveAgreementOnMissingData: true},
		{Tag: "type:external", MinParticipants: 1, MinAgreement: 1},
	}
}

func missingDataErr() error {
	return common.NewErrEndpointMissingData(fmt.Errorf("missing trie node"), nil)
}

func TestMissingDataWaiver_FiresWhenAllTaggedParticipantsMissing(t *testing.T) {
	// Round complete. Two externals agree on 0xaa (winner). One internal
	// returned ErrEndpointMissingData — at least minAgreement (1) missing-data
	// answers. The internal quota is waived and the external value wins.
	cfg := &config{
		maxParticipants:      3,
		agreementThreshold:   2,
		requiredParticipants: waiverQuota(),
	}
	internal := taggedUpstream("internal-1", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xaa", 1),
		errorFrom(internal, missingDataErr(), 2),
	})
	require.False(t, analysis.hasRemaining(), "all 3 slots answered")

	winner := winnerOf(cfg, analysis)
	require.Nil(t, winner.Error, "minAgreement missing-data must waive the internal quota, got: %v", winner.Error)
	require.NotNil(t, winner.Result)
}

func TestMissingDataWaiver_FiresWithSiblingTransportError(t *testing.T) {
	// Worldchain-shaped: one internal MissingData, one internal transport
	// failure. With minAgreement:1 the missing-data count still meets the
	// threshold — a down sibling must not block the waiver.
	cfg := &config{
		maxParticipants:      4,
		agreementThreshold:   2,
		requiredParticipants: waiverQuota(),
	}
	int1 := taggedUpstream("internal-1", "type:internal")
	int2 := taggedUpstream("internal-2", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xaa", 1),
		errorFrom(int1, missingDataErr(), 2),
		errorFrom(int2, common.NewErrEndpointTransportFailure(&url.URL{Host: "internal-2"}, fmt.Errorf("connection refused")), 3),
	})
	require.False(t, analysis.hasRemaining())

	winner := winnerOf(cfg, analysis)
	require.Nil(t, winner.Error, "sibling transport error must not block minAgreement missing-data waiver, got: %v", winner.Error)
	require.NotNil(t, winner.Result)
}

func TestMissingDataWaiver_HoldsWhenSiblingReturnedData(t *testing.T) {
	// One internal MissingData meets minAgreement:1, but a second internal
	// returned real data that contradicts the archives. Waiving would drop
	// that vote and accept the external winner.
	cfg := &config{
		maxParticipants:      4,
		agreementThreshold:   2,
		requiredParticipants: waiverQuota(),
	}
	int1 := taggedUpstream("internal-1", "type:internal")
	int2 := taggedUpstream("internal-2", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xaa", 1),
		errorFrom(int1, missingDataErr(), 2),
		resultFrom(t, int2, "0xbb", 3),
	})
	require.False(t, analysis.hasRemaining())

	winner := winnerOf(cfg, analysis)
	require.NotNil(t, winner.Error)
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusCompositionDispute),
		"matching internal data must hold the quota, got: %v", winner.Error)
}

func TestMissingDataWaiver_HoldsWhenEmptyCountBelowMinAgreement(t *testing.T) {
	// minAgreement:2 with only one MissingData answer — hold even if a sibling
	// returned disagreeing data (that sibling does not count toward the threshold).
	reqs := []*common.ConsensusRequiredParticipant{
		{Tag: "type:internal", MinParticipants: 1, MinAgreement: 2, WaiveAgreementOnMissingData: true},
		{Tag: "type:external", MinParticipants: 1, MinAgreement: 1},
	}
	cfg := &config{
		maxParticipants:      4,
		agreementThreshold:   2,
		requiredParticipants: reqs,
	}
	int1 := taggedUpstream("internal-1", "type:internal")
	int2 := taggedUpstream("internal-2", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xaa", 1),
		errorFrom(int1, missingDataErr(), 2),
		resultFrom(t, int2, "0xbb", 3),
	})
	require.False(t, analysis.hasRemaining())

	winner := winnerOf(cfg, analysis)
	require.NotNil(t, winner.Error)
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusCompositionDispute),
		"below minAgreement missing-data count must hold the quota, got: %v", winner.Error)
}

func TestMissingDataWaiver_DeferredWhileRoundHasRemaining(t *testing.T) {
	// The lone internal so far returned MissingData, but the round is not
	// complete (maxParticipants 4, only 3 answered): a slower internal could
	// still return data. The waiver is round-complete by design, so it holds
	// off and the provisional winner stays a composition dispute.
	cfg := &config{
		maxParticipants:      4,
		agreementThreshold:   2,
		requiredParticipants: waiverQuota(),
	}
	internal := taggedUpstream("internal-1", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xaa", 1),
		errorFrom(internal, missingDataErr(), 2),
	})
	require.True(t, analysis.hasRemaining(), "a 4th slot can still arrive")

	winner := winnerOf(cfg, analysis)
	require.NotNil(t, winner.Error)
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusCompositionDispute),
		"waiver must not fire while responses are still outstanding, got: %v", winner.Error)
}

func TestMissingDataWaiver_FiresWhenCollectionSealedShortOfMaxParticipants(t *testing.T) {
	// Worldchain-shaped wait-cap: maxParticipants 4, only 3 answered (one
	// sibling down/hanging), remaining cancelled. Sealing the bag must let
	// the MissingData waiver evaluate — cancelled slots cannot return data.
	cfg := &config{
		maxParticipants:      4,
		agreementThreshold:   2,
		requiredParticipants: waiverQuota(),
	}
	internal := taggedUpstream("internal-1", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xaa", 1),
		errorFrom(internal, missingDataErr(), 2),
	})
	require.True(t, analysis.hasRemaining())
	analysis.sealCollection()
	require.False(t, analysis.hasRemaining(), "sealed bag is round-complete for waivers")

	winner := winnerOf(cfg, analysis)
	require.Nil(t, winner.Error, "sealed MissingData waiver must release the internal quota, got: %v", winner.Error)
	require.NotNil(t, winner.Result)
}

func TestMissingDataWaiver_OptInOnly(t *testing.T) {
	// Same unanimous-missing-data shape, but the internal quota does NOT opt in
	// to the waiver. Without waiveAgreementOnMissingData the quota stays strict
	// and the round is a composition dispute.
	cfg := &config{
		maxParticipants:    3,
		agreementThreshold: 2,
		requiredParticipants: []*common.ConsensusRequiredParticipant{
			{Tag: "type:internal", MinParticipants: 1, MinAgreement: 1}, // no waiver opt-in
			{Tag: "type:external", MinParticipants: 1, MinAgreement: 1},
		},
	}
	internal := taggedUpstream("internal-1", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xaa", 1),
		errorFrom(internal, missingDataErr(), 2),
	})

	winner := winnerOf(cfg, analysis)
	require.NotNil(t, winner.Error)
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusCompositionDispute),
		"waiver must not fire without opt-in, got: %v", winner.Error)
}

// TestMissingDataWaiver_StgShape_TwoInternalsWrappedMissingData is the
// production shape that composition-disputed on stg (TECHOPS-30404):
// agreementThreshold 2, maxParticipants 4, internal minParticipants 2 /
// minAgreement 1 with BOTH waiver flags, external floor minParticipants 2 /
// minAgreement 1, both internals wrapped ErrUpstreamRequest/ErrEndpointMissingData
// (jsonrpc:-32014), both externals agree on one non-empty value, method has no
// blockEvidenceFields (empty-waiver cannot prove). PreferNonEmpty+ReturnError
// without AcceptMostCommon used to flake: the generic threshold rule picked the
// MissingData ConsensusError group by map iteration, then composition-disputed
// because the MissingData waiver releases the internal quota on the wrong winner.
func TestMissingDataWaiver_StgShape_TwoInternalsWrappedMissingData(t *testing.T) {
	preferNE := true
	preferLR := false
	cfg := &config{
		maxParticipants:         4,
		agreementThreshold:      2,
		preferNonEmpty:          preferNE,
		preferLargerResponses:   preferLR,
		disputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
		lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
		requiredParticipants: []*common.ConsensusRequiredParticipant{
			{
				Tag: "type:internal", MinParticipants: 2, MinAgreement: 1,
				WaiveAgreementOnMissingData:            true,
				WaiveAgreementOnEmptyOutsideRetention:  true,
				RetentionBlocks:                        2350000,
			},
			{Tag: "type:external", MinParticipants: 2, MinAgreement: 1},
		},
		// eth_getBalance is not in blockEvidenceFields — empty waiver cannot prove.
	}
	int1 := taggedUpstream("internal-blue", "type:internal")
	int2 := taggedUpstream("internal-green", "type:internal")
	ext1 := taggedUpstream("alchemy", "type:external")
	ext2 := taggedUpstream("quicknode", "type:external")

	inner := common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(-32000, common.JsonRpcErrorMissingData, "historical state is not available", nil, nil),
		nil,
	)
	wrapped := func(ups common.Upstream) error {
		return common.NewErrUpstreamRequest(inner, ups, "evm:1", "eth_getBalance", time.Millisecond, 1, 0, 0)
	}

	for i := 0; i < 50; i++ {
		analysis := analyze(cfg, []*execResult{
			errorFrom(int1, wrapped(int1), 0),
			errorFrom(int2, wrapped(int2), 1),
			resultFrom(t, ext1, "0x1234", 2),
			resultFrom(t, ext2, "0x1234", 3),
		})
		require.False(t, analysis.hasRemaining())
		require.True(t, common.HasErrorCode(wrapped(int1), common.ErrCodeEndpointMissingData))

		winner := winnerOf(cfg, analysis)
		require.Nil(t, winner.Error, "iter %d: MissingData waiver must release internal over PreferNonEmpty+ReturnError, got: %v", i, winner.Error)
		require.NotNil(t, winner.Result)
	}
}

// TestPreferNonEmpty_ReturnError_DoesNotMaskAgreedExecutionErrorTie locks
// Copilot's concern: PreferNonEmpty+ReturnError must not prefer a NonEmpty
// value over a tied ConsensusError that is a genuine execution revert. Only
// MissingData abstention (via dispute→promoteAbstentionWinner + waiver) may
// release that standoff — AcceptMostCommon is the other intentional override.
func TestPreferNonEmpty_ReturnError_DoesNotMaskAgreedExecutionErrorTie(t *testing.T) {
	preferNE := true
	cfg := &config{
		maxParticipants:         4,
		agreementThreshold:      2,
		preferNonEmpty:          preferNE,
		preferLargerResponses:   false,
		disputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
		lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
	}
	u1 := taggedUpstream("rpc-1")
	u2 := taggedUpstream("rpc-2")
	u3 := taggedUpstream("rpc-3")
	u4 := taggedUpstream("rpc-4")
	execErr := common.NewErrEndpointExecutionException(
		common.NewErrJsonRpcExceptionInternal(3, 3, "execution reverted", nil, nil),
	)
	for i := 0; i < 50; i++ {
		analysis := analyze(cfg, []*execResult{
			errorFrom(u1, execErr, 0),
			errorFrom(u2, execErr, 1),
			resultFrom(t, u3, "0xdata", 2),
			resultFrom(t, u4, "0xdata", 3),
		})
		winner := winnerOf(cfg, analysis)
		require.NotNil(t, winner.Error, "iter %d: PreferNonEmpty+ReturnError must not mask tied execution revert", i)
		assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusDispute),
			"iter %d: expected dispute, got: %v", i, winner.Error)
	}
}

// PreferNonEmpty+ReturnError must not synthesize a dispute when NonEmpty
// already has a strict count majority over Empty (e.g. empty=2, non-empty=4
// at threshold 2) — the non-empty is the clear winner.
func TestPreferNonEmpty_ReturnError_NonEmptyMajorityOverEmptyWins(t *testing.T) {
	preferNE := true
	cfg := &config{
		maxParticipants:         6,
		agreementThreshold:      2,
		preferNonEmpty:          preferNE,
		preferLargerResponses:   false,
		disputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
		lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
	}
	ups := make([]common.Upstream, 6)
	for i := range ups {
		ups[i] = taggedUpstream(fmt.Sprintf("rpc-%d", i+1))
	}
	for i := 0; i < 30; i++ {
		analysis := analyze(cfg, []*execResult{
			emptyResultFrom(t, ups[0], 0),
			emptyResultFrom(t, ups[1], 1),
			resultFrom(t, ups[2], "0xdata", 2),
			resultFrom(t, ups[3], "0xdata", 3),
			resultFrom(t, ups[4], "0xdata", 4),
			resultFrom(t, ups[5], "0xdata", 5),
		})
		winner := winnerOf(cfg, analysis)
		require.Nil(t, winner.Error, "iter %d: NonEmpty majority must win without dispute, got: %v", i, winner.Error)
		require.NotNil(t, winner.Result)
	}
}

// An unrelated MissingData abstention on a below-threshold sibling must not
// let promoteAbstentionWinner mask a tied at-threshold execution revert vs
// NonEmpty standoff — only a MissingData group that itself meets threshold
// (covered by the fired waiver) may be promoted over.
func TestPromoteAbstentionWinner_UnrelatedMissingDataDoesNotMaskExecutionRevert(t *testing.T) {
	preferNE := true
	cfg := &config{
		maxParticipants:         5,
		agreementThreshold:      2,
		preferNonEmpty:          preferNE,
		preferLargerResponses:   false,
		disputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
		lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
		requiredParticipants: []*common.ConsensusRequiredParticipant{
			{Tag: "type:internal", MinParticipants: 1, MinAgreement: 1, WaiveAgreementOnMissingData: true},
			{Tag: "type:external", MinParticipants: 2, MinAgreement: 2},
		},
	}
	u1 := taggedUpstream("rpc-1")
	u2 := taggedUpstream("rpc-2")
	u3 := taggedUpstream("rpc-3", "type:external")
	u4 := taggedUpstream("rpc-4", "type:external")
	u5 := taggedUpstream("rpc-5", "type:internal")
	execErr := common.NewErrEndpointExecutionException(
		common.NewErrJsonRpcExceptionInternal(3, 3, "execution reverted", nil, nil),
	)
	for i := 0; i < 50; i++ {
		analysis := analyze(cfg, []*execResult{
			errorFrom(u1, execErr, 0),
			errorFrom(u2, execErr, 1),
			resultFrom(t, u3, "0xdata", 2),
			resultFrom(t, u4, "0xdata", 3),
			errorFrom(u5, missingDataErr(), 4),
		})
		require.False(t, analysis.hasRemaining())
		winner := winnerOf(cfg, analysis)
		require.NotNil(t, winner.Error, "iter %d: unrelated MissingData must not promote NonEmpty over tied execution revert", i)
		assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusDispute),
			"iter %d: expected dispute, got: %v", i, winner.Error)
	}
}

// PreferNonEmpty+ReturnError must not synthesize a dispute when NonEmpty
// already has a strict count majority over Empty (e.g. empty=2, non-empty=4
// at threshold 2). Dispute is only for competitive empty/ConsensusError
// standoffs that promoteAbstentionWinner may rescue.
func TestPreferNonEmpty_ReturnError_NonEmptyMajorityBeatsEmptyThreshold(t *testing.T) {
	preferNE := true
	cfg := &config{
		maxParticipants:         6,
		agreementThreshold:      2,
		preferNonEmpty:          preferNE,
		preferLargerResponses:   false,
		disputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
		lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
	}
	ups := make([]common.Upstream, 6)
	for i := range ups {
		ups[i] = taggedUpstream(fmt.Sprintf("rpc-%d", i+1))
	}
	analysis := analyze(cfg, []*execResult{
		emptyResultFrom(t, ups[0], 0),
		emptyResultFrom(t, ups[1], 1),
		resultFrom(t, ups[2], "0xdata", 2),
		resultFrom(t, ups[3], "0xdata", 3),
		resultFrom(t, ups[4], "0xdata", 4),
		resultFrom(t, ups[5], "0xdata", 5),
	})
	winner := winnerOf(cfg, analysis)
	require.Nil(t, winner.Error, "NonEmpty majority must win without dispute, got: %v", winner.Error)
	require.NotNil(t, winner.Result)
}

func TestMissingDataWaivedQuotas_Unit(t *testing.T) {
	internal := taggedUpstream("internal-1", "type:internal")
	int2 := taggedUpstream("internal-2", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")

	waivable := &common.ConsensusRequiredParticipant{Tag: "type:internal", MinParticipants: 1, MinAgreement: 1, WaiveAgreementOnMissingData: true}
	reqs := []*common.ConsensusRequiredParticipant{
		waivable,
		{Tag: "type:external", MinParticipants: 1, MinAgreement: 1},
	}

	t.Run("waives when missing-data count meets minAgreement", func(t *testing.T) {
		cfg := &config{maxParticipants: 2, agreementThreshold: 1, requiredParticipants: reqs}
		analysis := analyze(cfg, []*execResult{
			errorFrom(internal, missingDataErr(), 0),
			resultFrom(t, ext1, "0xaa", 1),
		})
		waived := missingDataWaivedQuotas(analysis, reqs)
		_, ok := waived[waivable]
		assert.True(t, ok, "the opt-in internal quota should be waived")
		assert.Len(t, waived, 1, "the never-waivable external quota must never be waived")
	})

	t.Run("waives when sibling empty does not count as a value vote", func(t *testing.T) {
		// Feature §7.1: only a non-empty value vote holds the MissingData
		// quota. An empty success is a separate abstention shape.
		cfg := &config{maxParticipants: 2, agreementThreshold: 1, requiredParticipants: reqs}
		analysis := analyze(cfg, []*execResult{
			errorFrom(internal, missingDataErr(), 0),
			emptyResultFrom(t, int2, 1),
		})
		waived := missingDataWaivedQuotas(analysis, reqs)
		_, ok := waived[waivable]
		assert.True(t, ok, "empty sibling must not block MissingData waiver")
	})

	t.Run("holds when a matching sibling returned data", func(t *testing.T) {
		cfg := &config{maxParticipants: 2, agreementThreshold: 1, requiredParticipants: reqs}
		analysis := analyze(cfg, []*execResult{
			errorFrom(internal, missingDataErr(), 0),
			resultFrom(t, int2, "0xbb", 1),
		})
		assert.Empty(t, missingDataWaivedQuotas(analysis, reqs),
			"a matching non-empty result is a value vote — hold the quota")
	})

	t.Run("holds when missing-data count is below minAgreement", func(t *testing.T) {
		strict := &common.ConsensusRequiredParticipant{Tag: "type:internal", MinParticipants: 1, MinAgreement: 2, WaiveAgreementOnMissingData: true}
		reqs2 := []*common.ConsensusRequiredParticipant{
			strict,
			{Tag: "type:external", MinParticipants: 1, MinAgreement: 1},
		}
		cfg := &config{maxParticipants: 2, agreementThreshold: 1, requiredParticipants: reqs2}
		analysis := analyze(cfg, []*execResult{
			errorFrom(internal, missingDataErr(), 0),
			resultFrom(t, int2, "0xbb", 1),
		})
		assert.Empty(t, missingDataWaivedQuotas(analysis, reqs2),
			"one MissingData is below minAgreement:2")
	})

	t.Run("no waiver when the tag matched zero participants", func(t *testing.T) {
		cfg := &config{maxParticipants: 1, agreementThreshold: 1, requiredParticipants: reqs}
		analysis := analyze(cfg, []*execResult{
			resultFrom(t, ext1, "0xaa", 0), // only an external answered
		})
		assert.Empty(t, missingDataWaivedQuotas(analysis, reqs), "an absent tag cannot meet minAgreement")
	})
}

// empty-outside-retention waiver (v1.1) --------------------------------------

// emptyResultFrom builds a participant whose result is JSON null — the shape
// classifyAndHashResponse tags as ResponseTypeEmpty (the abstention signal the
// empty-waiver keys on).
func emptyResultFrom(t *testing.T, ups common.Upstream, index int) *execResult {
	t.Helper()
	jrpc, err := common.NewJsonRpcResponse(1, nil, nil)
	require.NoError(t, err)
	return &execResult{
		Result:   common.NewNormalizedResponse().WithJsonRpcResponse(jrpc),
		Upstream: ups,
		Index:    index,
	}
}

// objResultFrom builds a participant whose result is a JSON object (e.g. a
// tx/receipt carrying its own blockNumber), used as the non-empty winner.
func objResultFrom(t *testing.T, ups common.Upstream, obj map[string]interface{}, index int) *execResult {
	t.Helper()
	jrpc, err := common.NewJsonRpcResponse(1, obj, nil)
	require.NoError(t, err)
	return &execResult{
		Result:   common.NewNormalizedResponse().WithJsonRpcResponse(jrpc),
		Upstream: ups,
		Index:    index,
	}
}

// emptyWaiverQuota mirrors waiverQuota for the empty-waiver: a waivable internal
// quota with a retention boundary, kept honest by a never-waivable external floor.
func emptyWaiverQuota() []*common.ConsensusRequiredParticipant {
	return []*common.ConsensusRequiredParticipant{
		{Tag: "type:internal", MinParticipants: 1, MinAgreement: 1, WaiveAgreementOnEmptyOutsideRetention: true, RetentionBlocks: 100},
		{Tag: "type:external", MinParticipants: 1, MinAgreement: 1},
	}
}

func TestEmptyOutsideRetentionWaivedQuotas_Unit(t *testing.T) {
	internal := taggedUpstream("internal-1", "type:internal")
	int2 := taggedUpstream("internal-2", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")

	waivable := &common.ConsensusRequiredParticipant{Tag: "type:internal", MinParticipants: 1, MinAgreement: 1, WaiveAgreementOnEmptyOutsideRetention: true, RetentionBlocks: 100}
	reqs := []*common.ConsensusRequiredParticipant{
		waivable,
		{Tag: "type:external", MinParticipants: 1, MinAgreement: 1},
	}
	// head 1000, winner at block 0x10 (16) → depth 984 > retention 100 → outside.
	head := int64(1000)
	oldBlock := big.NewInt(16)

	t.Run("waives when empty count meets minAgreement and winner outside retention", func(t *testing.T) {
		cfg := &config{maxParticipants: 2, agreementThreshold: 1, requiredParticipants: reqs}
		analysis := analyze(cfg, []*execResult{
			emptyResultFrom(t, internal, 0),
			objResultFrom(t, ext1, map[string]interface{}{"blockNumber": "0x10"}, 1),
		})
		waived := emptyOutsideRetentionWaivedQuotas(analysis, reqs, oldBlock, head)
		_, ok := waived[waivable]
		assert.True(t, ok, "minAgreement empty on a pruned block should waive")
		assert.Len(t, waived, 1, "the never-waivable external quota must never be waived")
	})

	t.Run("holds for a recent block inside retention (anti-fabrication)", func(t *testing.T) {
		cfg := &config{maxParticipants: 2, agreementThreshold: 1, requiredParticipants: reqs}
		analysis := analyze(cfg, []*execResult{
			emptyResultFrom(t, internal, 0),
			objResultFrom(t, ext1, map[string]interface{}{"blockNumber": "0x3e6"}, 1), // block 998, depth 2
		})
		recentBlock := big.NewInt(998)
		assert.Empty(t, emptyOutsideRetentionWaivedQuotas(analysis, reqs, recentBlock, head),
			"a null on a recent block must hold the quota — the internal should still have it")
	})

	t.Run("waives with sibling error when minAgreement is 1", func(t *testing.T) {
		cfg := &config{maxParticipants: 3, agreementThreshold: 1, requiredParticipants: reqs}
		analysis := analyze(cfg, []*execResult{
			emptyResultFrom(t, internal, 0),
			errorFrom(int2, common.NewErrEndpointTransportFailure(&url.URL{Host: "internal-2"}, fmt.Errorf("connection refused")), 1),
			objResultFrom(t, ext1, map[string]interface{}{"blockNumber": "0x10"}, 2),
		})
		waived := emptyOutsideRetentionWaivedQuotas(analysis, reqs, oldBlock, head)
		_, ok := waived[waivable]
		assert.True(t, ok, "one empty meets minAgreement:1 even if a sibling errored")
	})

	t.Run("holds when a matching sibling returned data", func(t *testing.T) {
		// minAgreement:1 is already met by one empty, but a second internal
		// returned a real value. Waiving would drop that vote and let an
		// external winner mask the disagreement.
		cfg := &config{maxParticipants: 3, agreementThreshold: 1, requiredParticipants: reqs}
		analysis := analyze(cfg, []*execResult{
			emptyResultFrom(t, internal, 0),
			resultFrom(t, int2, "0xbb", 1),
			objResultFrom(t, ext1, map[string]interface{}{"blockNumber": "0x10"}, 2),
		})
		assert.Empty(t, emptyOutsideRetentionWaivedQuotas(analysis, reqs, oldBlock, head),
			"a matching non-empty result is a real vote — hold the quota")
	})

	t.Run("holds when empty count is below minAgreement", func(t *testing.T) {
		strict := &common.ConsensusRequiredParticipant{Tag: "type:internal", MinParticipants: 1, MinAgreement: 2, WaiveAgreementOnEmptyOutsideRetention: true, RetentionBlocks: 100}
		reqs2 := []*common.ConsensusRequiredParticipant{
			strict,
			{Tag: "type:external", MinParticipants: 1, MinAgreement: 1},
		}
		cfg := &config{maxParticipants: 3, agreementThreshold: 1, requiredParticipants: reqs2}
		analysis := analyze(cfg, []*execResult{
			emptyResultFrom(t, internal, 0),
			resultFrom(t, int2, "0xbb", 1),
			objResultFrom(t, ext1, map[string]interface{}{"blockNumber": "0x10"}, 2),
		})
		assert.Empty(t, emptyOutsideRetentionWaivedQuotas(analysis, reqs2, oldBlock, head),
			"one empty is below minAgreement:2")
	})

	t.Run("no waiver without a known head", func(t *testing.T) {
		cfg := &config{maxParticipants: 2, agreementThreshold: 1, requiredParticipants: reqs}
		analysis := analyze(cfg, []*execResult{
			emptyResultFrom(t, internal, 0),
			objResultFrom(t, ext1, map[string]interface{}{"blockNumber": "0x10"}, 1),
		})
		assert.Empty(t, emptyOutsideRetentionWaivedQuotas(analysis, reqs, oldBlock, 0),
			"no head and no avail bounds means no proof — fail closed")
	})

	t.Run("waives via availability lower bound when head is unknown", func(t *testing.T) {
		// lower ExactBlock 1000 → winner at 16 is outside that upstream's range.
		lower := int64(1000)
		bounded := common.NewFakeUpstream("internal-bound",
			common.WithTags("type:internal"),
			common.WithEvmBlockAvailability(&common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{ExactBlock: &lower},
			}),
		)
		cfg := &config{maxParticipants: 2, agreementThreshold: 1, requiredParticipants: reqs}
		analysis := analyze(cfg, []*execResult{
			emptyResultFrom(t, bounded, 0),
			objResultFrom(t, ext1, map[string]interface{}{"blockNumber": "0x10"}, 1),
		})
		waived := emptyOutsideRetentionWaivedQuotas(analysis, reqs, oldBlock, 0)
		_, ok := waived[waivable]
		assert.True(t, ok, "finite avail lower bound must prove outside retention without a network head")
	})

	t.Run("avail bound holds for a recent block inside the lower bound", func(t *testing.T) {
		lower := int64(100)
		bounded := common.NewFakeUpstream("internal-bound",
			common.WithTags("type:internal"),
			common.WithEvmBlockAvailability(&common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{ExactBlock: &lower},
			}),
		)
		cfg := &config{maxParticipants: 2, agreementThreshold: 1, requiredParticipants: reqs}
		recentBlock := big.NewInt(500) // above lower 100
		analysis := analyze(cfg, []*execResult{
			emptyResultFrom(t, bounded, 0),
			objResultFrom(t, ext1, map[string]interface{}{"blockNumber": "0x1f4"}, 1),
		})
		assert.Empty(t, emptyOutsideRetentionWaivedQuotas(analysis, reqs, recentBlock, 0),
			"winner inside avail lower bound must not waive even when head is unknown")
	})

	t.Run("finite bound in-range blocks chain-head fallback", func(t *testing.T) {
		// Bound says winner is still served (lower <= winnerBlock); head depth
		// alone would prove outside retention. Prefer bound evidence — do not
		// waive via the chain-head fallback (feature §7.1).
		lower := int64(100)
		bounded := common.NewFakeUpstream("internal-bound",
			common.WithTags("type:internal"),
			common.WithEvmBlockAvailability(&common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{ExactBlock: &lower},
			}),
		)
		cfg := &config{maxParticipants: 2, agreementThreshold: 1, requiredParticipants: reqs}
		recentBlock := big.NewInt(500)
		analysis := analyze(cfg, []*execResult{
			emptyResultFrom(t, bounded, 0),
			objResultFrom(t, ext1, map[string]interface{}{"blockNumber": "0x1f4"}, 1),
		})
		assert.Empty(t, emptyOutsideRetentionWaivedQuotas(analysis, reqs, recentBlock, head),
			"contradictory finite avail bound must not be overridden by chain-head depth")
	})

	t.Run("no waiver without winner block evidence", func(t *testing.T) {
		cfg := &config{maxParticipants: 2, agreementThreshold: 1, requiredParticipants: reqs}
		analysis := analyze(cfg, []*execResult{
			emptyResultFrom(t, internal, 0),
			objResultFrom(t, ext1, map[string]interface{}{"blockNumber": "0x10"}, 1),
		})
		assert.Empty(t, emptyOutsideRetentionWaivedQuotas(analysis, reqs, nil, head),
			"no extractable winner block means no proof — fail closed")
	})

	t.Run("no waiver without opt-in", func(t *testing.T) {
		strict := []*common.ConsensusRequiredParticipant{
			{Tag: "type:internal", MinParticipants: 1, MinAgreement: 1}, // no opt-in
			{Tag: "type:external", MinParticipants: 1, MinAgreement: 1},
		}
		cfg := &config{maxParticipants: 2, agreementThreshold: 1, requiredParticipants: strict}
		analysis := analyze(cfg, []*execResult{
			emptyResultFrom(t, internal, 0),
			objResultFrom(t, ext1, map[string]interface{}{"blockNumber": "0x10"}, 1),
		})
		assert.Empty(t, emptyOutsideRetentionWaivedQuotas(analysis, strict, oldBlock, head))
	})

	t.Run("deferred while the round can still receive responses", func(t *testing.T) {
		cfg := &config{maxParticipants: 3, agreementThreshold: 1, requiredParticipants: reqs}
		analysis := analyze(cfg, []*execResult{
			emptyResultFrom(t, internal, 0),
			objResultFrom(t, ext1, map[string]interface{}{"blockNumber": "0x10"}, 1),
		})
		require.True(t, analysis.hasRemaining(), "a 3rd slot can still arrive")
		assert.Empty(t, emptyOutsideRetentionWaivedQuotas(analysis, reqs, oldBlock, head),
			"the waiver is round-complete — a slower internal could still return data")
	})

	t.Run("waives when collection is sealed short of maxParticipants", func(t *testing.T) {
		// Wait-cap sealed 3 of 4: one internal empty + two agreeing archives;
		// the 4th slot was cancelled and cannot return data.
		ext2 := taggedUpstream("external-2", "type:external")
		cfg := &config{maxParticipants: 4, agreementThreshold: 2, requiredParticipants: reqs}
		analysis := analyze(cfg, []*execResult{
			emptyResultFrom(t, internal, 0),
			objResultFrom(t, ext1, map[string]interface{}{"blockNumber": "0x10"}, 1),
			objResultFrom(t, ext2, map[string]interface{}{"blockNumber": "0x10"}, 2),
		})
		require.True(t, analysis.hasRemaining())
		assert.Empty(t, emptyOutsideRetentionWaivedQuotas(analysis, reqs, oldBlock, head),
			"unsealed short bag must still defer")
		analysis.sealCollection()
		require.False(t, analysis.hasRemaining())
		waived := emptyOutsideRetentionWaivedQuotas(analysis, reqs, oldBlock, head)
		_, ok := waived[waivable]
		assert.True(t, ok, "sealed short bag must allow the empty waiver")
	})
}

func TestWinnerBlockEvidence(t *testing.T) {
	ext1 := taggedUpstream("external-1", "type:external")
	cfg := &config{
		maxParticipants:     2,
		agreementThreshold:  1,
		blockEvidenceFields: map[string][]string{"eth_getTransactionByHash": {"blockNumber"}},
	}
	lg := zerolog.Nop()
	e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}

	nonEmptyGroup := func(analysis *consensusAnalysis) *responseGroup {
		for _, g := range analysis.groups {
			if g.ResponseType == ResponseTypeNonEmpty {
				return g
			}
		}
		return nil
	}

	t.Run("extracts the winner block for a configured method", func(t *testing.T) {
		analysis := analyze(cfg, []*execResult{
			objResultFrom(t, ext1, map[string]interface{}{"blockNumber": "0x10"}, 0),
		})
		analysis.method = "eth_getTransactionByHash"
		got := e.winnerBlockEvidence(analysis, nonEmptyGroup(analysis))
		require.NotNil(t, got)
		assert.Equal(t, int64(16), got.Int64())
	})

	t.Run("nil for an unlisted method", func(t *testing.T) {
		analysis := analyze(cfg, []*execResult{
			objResultFrom(t, ext1, map[string]interface{}{"blockNumber": "0x10"}, 0),
		})
		analysis.method = "eth_getBalance" // not in blockEvidenceFields
		assert.Nil(t, e.winnerBlockEvidence(analysis, nonEmptyGroup(analysis)))
	})

	t.Run("nil for negative block evidence", func(t *testing.T) {
		analysis := analyze(cfg, []*execResult{
			objResultFrom(t, ext1, map[string]interface{}{"blockNumber": "-1"}, 0),
		})
		analysis.method = "eth_getTransactionByHash"
		assert.Nil(t, e.winnerBlockEvidence(analysis, nonEmptyGroup(analysis)),
			"negative block numbers must not prove outside retention")
	})

	t.Run("first independently resolved field wins", func(t *testing.T) {
		multi := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: &config{
			maxParticipants:    2,
			agreementThreshold: 1,
			blockEvidenceFields: map[string][]string{
				"eth_getTransactionByHash": {"missingField", "blockNumber", "slot"},
			},
		}}}
		analysis := analyze(cfg, []*execResult{
			objResultFrom(t, ext1, map[string]interface{}{"blockNumber": "0x10", "slot": "0x99"}, 0),
		})
		analysis.method = "eth_getTransactionByHash"
		got := multi.winnerBlockEvidence(analysis, nonEmptyGroup(analysis))
		require.NotNil(t, got)
		assert.Equal(t, int64(16), got.Int64(), "must use first present field, not require all fields")
	})
}

func TestEmptyOutsideRetentionWaiver_FailsClosedWithoutNetworkHead(t *testing.T) {
	// End-to-end through enforceWinnerComposition: two externals agree on a
	// tx, the lone internal returned empty. But the test analysis carries no
	// network, so the chain head is unknown (0) and the block proof cannot be
	// established — the waiver must not fire and the round is a composition
	// dispute. Confirms the fail-closed wiring.
	cfg := &config{
		maxParticipants:      3,
		agreementThreshold:   2,
		requiredParticipants: emptyWaiverQuota(),
		blockEvidenceFields:  map[string][]string{"eth_getTransactionByHash": {"blockNumber"}},
	}
	internal := taggedUpstream("internal-1", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		objResultFrom(t, ext1, map[string]interface{}{"blockNumber": "0x10"}, 0),
		objResultFrom(t, ext2, map[string]interface{}{"blockNumber": "0x10"}, 1),
		emptyResultFrom(t, internal, 2),
	})
	analysis.method = "eth_getTransactionByHash"
	require.False(t, analysis.hasRemaining())

	winner := winnerOf(cfg, analysis)
	require.NotNil(t, winner.Error)
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusCompositionDispute),
		"without a known head the empty-waiver cannot prove retention and must hold, got: %v", winner.Error)
}

func TestParticipantLatestBlock(t *testing.T) {
	cfg := &config{maxParticipants: 3, agreementThreshold: 1}

	t.Run("min of two corroborated pollers", func(t *testing.T) {
		low := common.NewFakeEvmStatePoller(20_000_000, 20_000_000)
		high := common.NewFakeEvmStatePoller(50_000_000, 50_000_000)
		a := common.NewFakeUpstream("a", common.WithEvmStatePoller(low))
		b := common.NewFakeUpstream("b", common.WithEvmStatePoller(high))
		analysis := analyze(cfg, []*execResult{
			resultFrom(t, a, "0x1", 0),
			resultFrom(t, b, "0x1", 1),
		})
		assert.Equal(t, int64(20_000_000), participantLatestBlock(analysis),
			"an inflated poller must not raise the fallback head")
	})

	t.Run("single poller fails closed", func(t *testing.T) {
		p := common.NewFakeEvmStatePoller(20_000_000, 20_000_000)
		a := common.NewFakeUpstream("a", common.WithEvmStatePoller(p))
		b := common.NewFakeUpstream("b")
		analysis := analyze(cfg, []*execResult{
			resultFrom(t, a, "0x1", 0),
			resultFrom(t, b, "0x1", 1),
		})
		assert.Equal(t, int64(0), participantLatestBlock(analysis),
			"one uncorroborated poller is not a chain head")
	})

	t.Run("duplicate upstream id counted once", func(t *testing.T) {
		p := common.NewFakeEvmStatePoller(20_000_000, 20_000_000)
		a := common.NewFakeUpstream("same", common.WithEvmStatePoller(p))
		analysis := analyze(cfg, []*execResult{
			resultFrom(t, a, "0x1", 0),
			resultFrom(t, a, "0x2", 1),
		})
		assert.Equal(t, int64(0), participantLatestBlock(analysis),
			"two results from one upstream are still a single poller")
	})
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
