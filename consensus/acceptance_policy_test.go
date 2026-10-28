package consensus

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The scenario every test below is a variation of: an operator wants a mixed
// internal+external agreement, and would rather serve an answer two
// independent externals agree on than hard-fail when the internal nodes are
// absent or dissenting — provided the relaxed answer is labelled as such.
func mixedThenExternalOnly() []*common.ConsensusAcceptancePolicy {
	return []*common.ConsensusAcceptancePolicy{
		{
			Name: "standard",
			RequiredAgreement: []*common.ConsensusAgreementQuota{
				{Tag: "type:internal", MinAgreement: 1},
				{Tag: "type:external", MinAgreement: 1},
			},
		},
		{
			Name: "degraded",
			RequiredAgreement: []*common.ConsensusAgreementQuota{
				{Tag: "type:external", MinAgreement: 2},
			},
		},
	}
}

func cascadeConfig() *config {
	return &config{
		maxParticipants:         5,
		agreementThreshold:      2,
		acceptancePolicyConfigs: mixedThenExternalOnly(),
	}
}

// grade selection -------------------------------------------------------------

func TestAcceptance_StrictGradeWinsWhenMixedAgreementExists(t *testing.T) {
	cfg := cascadeConfig()
	int1 := taggedUpstream("internal-1", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	// All three agree, so BOTH grades are satisfiable. Order must decide.
	analysis := analyze(cfg, []*execResult{
		resultFrom(t, int1, "0xaa", 0),
		resultFrom(t, ext1, "0xaa", 1),
		resultFrom(t, ext2, "0xaa", 2),
	})
	winner := winnerOf(cfg, analysis)

	require.Nil(t, winner.Error)
	assert.Equal(t, "standard", winner.Policy,
		"when both grades match, the strictest (first) one must be reported")
}

func TestAcceptance_FallsToRelaxedGradeWhenInternalAbsent(t *testing.T) {
	// The availability case: internal nodes down/cordoned, two independent
	// externals agree. Strict is unsatisfiable; the round is still served,
	// and is labelled so the operator can tell it apart.
	cfg := cascadeConfig()
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xaa", 1),
	})
	winner := winnerOf(cfg, analysis)

	require.Nil(t, winner.Error, "two agreeing externals must satisfy the relaxed grade")
	assert.Equal(t, "degraded", winner.Policy)
}

func TestAcceptance_FallsToRelaxedGradeWhenInternalDissents(t *testing.T) {
	// The integrity case André raised: internal is UP but serving forked
	// data while two externals agree. Strict cannot be met (the winning
	// group holds no internal), so the relaxed grade serves — labelled.
	// This is the behaviour a dispute-rate breaker cannot distinguish from
	// the absent case; here it is decided per round from the votes.
	cfg := cascadeConfig()
	int1 := taggedUpstream("internal-1", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, int1, "0xdead", 0), // forked
		resultFrom(t, ext1, "0xaa", 1),
		resultFrom(t, ext2, "0xaa", 2),
	})
	winner := winnerOf(cfg, analysis)

	require.Nil(t, winner.Error)
	assert.Equal(t, "degraded", winner.Policy)
}

func TestAcceptance_GenuineDisagreementStaysAPlainDispute(t *testing.T) {
	// One internal and one external that disagree: no group reaches any
	// grade's threshold, so the rules engine produces a plain dispute and
	// the gate passes it through untouched. Converting it into a
	// composition dispute would mask the real failure — the upstreams
	// disagreed, which is not a composition problem.
	cfg := cascadeConfig()
	int1 := taggedUpstream("internal-1", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, int1, "0xaa", 0),
		resultFrom(t, ext1, "0xbb", 1),
	})
	winner := winnerOf(cfg, analysis)

	require.NotNil(t, winner.Error)
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusDispute),
		"got: %v", winner.Error)
	assert.False(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusCompositionDispute))
	assert.Empty(t, winner.Policy, "a disputed round is served under no grade")
}

func TestAcceptance_CountWinnerFailingEveryGradeIsCompositionDispute(t *testing.T) {
	// Here a group DOES reach the threshold — two correlated externals
	// agree — but the operator required an internal in the winner and only
	// configured the strict grade. That is a composition failure, and must
	// be labelled distinctly from a plain disagreement.
	cfg := &config{
		maxParticipants:    5,
		agreementThreshold: 2,
		acceptancePolicyConfigs: []*common.ConsensusAcceptancePolicy{
			{Name: "standard", RequiredAgreement: []*common.ConsensusAgreementQuota{
				{Tag: "type:internal", MinAgreement: 1},
				{Tag: "type:external", MinAgreement: 1},
			}},
		},
	}
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xaa", 1),
	})
	winner := winnerOf(cfg, analysis)

	require.NotNil(t, winner.Error)
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusCompositionDispute),
		"got: %v", winner.Error)
	assert.Empty(t, winner.Policy)
}

func TestAcceptance_RelaxedGradeResolvesRoundTooThinForStrict(t *testing.T) {
	// The low-participants case. Strict needs 3 agreeing upstreams; only two
	// externals answered at all. Without per-grade thresholds the rules
	// engine would declare low-participants/dispute and the relaxed grade
	// would never get a chance.
	cfg := &config{
		maxParticipants:    5,
		agreementThreshold: 3,
		acceptancePolicyConfigs: []*common.ConsensusAcceptancePolicy{
			{
				Name: "standard",
				RequiredAgreement: []*common.ConsensusAgreementQuota{
					{Tag: "type:internal", MinAgreement: 1},
					{Tag: "type:external", MinAgreement: 2},
				},
			},
			{
				Name: "degraded",
				RequiredAgreement: []*common.ConsensusAgreementQuota{
					{Tag: "type:external", MinAgreement: 2},
				},
			},
		},
	}
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xaa", 1),
	})
	winner := winnerOf(cfg, analysis)

	require.Nil(t, winner.Error, "relaxed grade needs only 2 agreeing upstreams, got: %v", winner.Error)
	assert.Equal(t, "degraded", winner.Policy)
}

func TestAcceptance_GradeThresholdIsStillEnforced(t *testing.T) {
	// The flip side of lowering the rules threshold: a single external must
	// NOT be served just because the rules engine now nominates it. The
	// relaxed grade's own bar (2 externals) still applies.
	cfg := cascadeConfig()
	ext1 := taggedUpstream("external-1", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
	})
	winner := winnerOf(cfg, analysis)

	require.NotNil(t, winner.Error, "one upstream cannot satisfy a grade requiring two")
	assert.Empty(t, winner.Policy)
}

func TestAcceptance_DuplicateUpstreamCannotSatisfyGradeAlone(t *testing.T) {
	// Hedge/retry can land twice on the same upstream. One node
	// corroborating itself must not fill a minAgreement of 2.
	cfg := cascadeConfig()
	ext1 := taggedUpstream("external-1", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext1, "0xaa", 1),
	})
	winner := winnerOf(cfg, analysis)

	require.NotNil(t, winner.Error, "self-corroboration must not satisfy the relaxed grade")
	assert.Empty(t, winner.Policy)
}

// caller authorization ---------------------------------------------------------

func requestForUser(t *testing.T, user *common.User) *common.NormalizedRequest {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[]}`))
	if user != nil {
		req.SetUser(user)
	}
	return req
}

func TestAcceptance_CallerRestrictedToStrictGradeIsNotServedRelaxed(t *testing.T) {
	// A settlement-grade caller: the round genuinely resolves at "degraded",
	// but this caller may not accept that, so it gets the dispute instead.
	cfg := cascadeConfig()
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xaa", 1),
	})

	strictOnly := &common.User{Id: "settlement", ConsensusPolicies: &[]string{"standard"}}
	winner := winnerOfFor(cfg, requestForUser(t, strictOnly), analysis)

	require.NotNil(t, winner.Error)
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusCompositionDispute),
		"got: %v", winner.Error)
	assert.Empty(t, winner.Policy)
}

func TestAcceptance_CallerAllowedRelaxedGradeIsServed(t *testing.T) {
	cfg := cascadeConfig()
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xaa", 1),
	})

	indexer := &common.User{Id: "indexer", ConsensusPolicies: &[]string{"standard", "degraded"}}
	winner := winnerOfFor(cfg, requestForUser(t, indexer), analysis)

	require.Nil(t, winner.Error, "got: %v", winner.Error)
	assert.Equal(t, "degraded", winner.Policy)
}

func TestAcceptance_RestrictedCallerStillServedGradeItAllows(t *testing.T) {
	// The restriction must not degrade the strict path: the same
	// strict-only caller is served normally when the round earns "standard".
	cfg := cascadeConfig()
	int1 := taggedUpstream("internal-1", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, int1, "0xaa", 0),
		resultFrom(t, ext1, "0xaa", 1),
	})

	strictOnly := &common.User{Id: "settlement", ConsensusPolicies: &[]string{"standard"}}
	winner := winnerOfFor(cfg, requestForUser(t, strictOnly), analysis)

	require.Nil(t, winner.Error, "got: %v", winner.Error)
	assert.Equal(t, "standard", winner.Policy)
}

func TestAcceptance_UnrestrictedCallerMayBeServedAnyGrade(t *testing.T) {
	// Unset allowlist (and unauthenticated deployments) must behave exactly
	// as before this feature existed.
	cfg := cascadeConfig()
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xaa", 1),
	})

	for name, user := range map[string]*common.User{
		"no user":         nil,
		"no restriction":  {Id: "u"},
		"explicit allows": {Id: "u", ConsensusPolicies: &[]string{"degraded"}},
	} {
		t.Run(name, func(t *testing.T) {
			winner := winnerOfFor(cfg, requestForUser(t, user), analysis)
			require.Nil(t, winner.Error, "got: %v", winner.Error)
			assert.Equal(t, "degraded", winner.Policy)
		})
	}
}

func TestAcceptance_EmptyAllowlistPermitsNothing(t *testing.T) {
	// An empty list is meaningful and distinct from unset.
	cfg := cascadeConfig()
	int1 := taggedUpstream("internal-1", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, int1, "0xaa", 0),
		resultFrom(t, ext1, "0xaa", 1),
	})

	denied := &common.User{Id: "denied", ConsensusPolicies: &[]string{}}
	winner := winnerOfFor(cfg, requestForUser(t, denied), analysis)

	require.NotNil(t, winner.Error)
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusCompositionDispute))
}

// ordering invariants -----------------------------------------------------------

func TestAcceptance_RelaxedWinDoesNotShortCircuitWhileStrictReachable(t *testing.T) {
	// Timing must not decide the grade. Two externals agree early while an
	// internal slot is still outstanding; cancelling here would serve
	// "degraded" for a round that was about to earn "standard".
	cfg := cascadeConfig()
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")
	int1 := taggedUpstream("internal-1", "type:internal")

	partial := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xaa", 1),
	})
	provisional := winnerOf(cfg, partial)
	require.Equal(t, "degraded", provisional.Policy)
	require.True(t, partial.hasRemaining(), "a slot must still be outstanding for this test to mean anything")

	e := newTestExecutor(cfg)
	reason, ok := e.shouldShortCircuit(provisional, partial)
	assert.False(t, ok, "must not cancel a strict-grade vote that can still arrive (reason=%s)", reason)

	// The late internal vote upgrades the same round to the strict grade.
	full := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xaa", 1),
		resultFrom(t, int1, "0xaa", 2),
	})
	final := winnerOf(cfg, full)
	require.Nil(t, final.Error)
	assert.Equal(t, "standard", final.Policy, "late internal agreement must upgrade the grade")
}

func TestAcceptance_StrictWinMayShortCircuit(t *testing.T) {
	// The converse of the deferral above: once the strictest grade is met
	// and its lead is unassailable, the round is final and the remaining
	// participant is cancelled as before. The relaxed-grade guard must not
	// disable short-circuit generally.
	cfg := &config{
		maxParticipants:         3, // 2 answered, 1 outstanding -> lead 2 > 0+1
		agreementThreshold:      2,
		acceptancePolicyConfigs: mixedThenExternalOnly(),
	}
	int1 := taggedUpstream("internal-1", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")

	partial := analyze(cfg, []*execResult{
		resultFrom(t, int1, "0xaa", 0),
		resultFrom(t, ext1, "0xaa", 1),
	})
	winner := winnerOf(cfg, partial)
	require.Equal(t, "standard", winner.Policy)
	require.True(t, partial.hasRemaining(), "a slot must still be outstanding for this test to mean anything")

	e := newTestExecutor(cfg)
	_, ok := e.shouldShortCircuit(winner, partial)
	assert.True(t, ok, "an unassailable strict-grade win should still short-circuit")
}

// compilation ---------------------------------------------------------------------

func TestAcceptance_ShorthandCompilesToSingleStandardGrade(t *testing.T) {
	// Configs written before named grades existed keep working, and report
	// the implicit grade name.
	cfg := &config{
		maxParticipants:      3,
		agreementThreshold:   2,
		requiredParticipants: mixedQuota(1),
	}
	int1 := taggedUpstream("internal-1", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, int1, "0xaa", 0),
		resultFrom(t, ext1, "0xaa", 1),
	})
	winner := winnerOf(cfg, analysis)

	require.Nil(t, winner.Error)
	assert.Equal(t, defaultAcceptancePolicyName, winner.Policy)
}

func TestAcceptance_NoPoliciesConfiguredLeavesRoundUngraded(t *testing.T) {
	// Opt-in: with no composition config at all the gate is a no-op and no
	// grade is reported, so the header/metric stay absent.
	cfg := &config{maxParticipants: 3, agreementThreshold: 2}
	u1 := taggedUpstream("upstream-1")
	u2 := taggedUpstream("upstream-2")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, u1, "0xaa", 0),
		resultFrom(t, u2, "0xaa", 1),
	})
	winner := winnerOf(cfg, analysis)

	require.Nil(t, winner.Error)
	assert.Empty(t, winner.Policy)
}

func TestAcceptance_CompileDerivesThresholdsAndLowersRulesBar(t *testing.T) {
	cfg := &config{
		maxParticipants:    5,
		agreementThreshold: 4,
		acceptancePolicyConfigs: []*common.ConsensusAcceptancePolicy{
			// threshold derived from quotas: 1 + 2 = 3
			{Name: "standard", RequiredAgreement: []*common.ConsensusAgreementQuota{
				{Tag: "type:internal", MinAgreement: 1},
				{Tag: "type:external", MinAgreement: 2},
			}},
			// explicit threshold wins over the derived sum
			{Name: "degraded", AgreementThreshold: 2, RequiredAgreement: []*common.ConsensusAgreementQuota{
				{Tag: "type:external", MinAgreement: 2},
			}},
		},
	}
	cfg.compile()

	require.Len(t, cfg.acceptancePolicies, 2)
	assert.Equal(t, 3, cfg.acceptancePolicies[0].threshold, "derived from the sum of quotas")
	assert.Equal(t, 2, cfg.acceptancePolicies[1].threshold, "explicit threshold")
	assert.Equal(t, 2, cfg.agreementThreshold,
		"rules engine must run at the lowest grade's bar so thin rounds still nominate a candidate")

	// Idempotent: a second compile must not lower the threshold again or
	// duplicate the grades.
	cfg.compile()
	assert.Len(t, cfg.acceptancePolicies, 2)
	assert.Equal(t, 2, cfg.agreementThreshold)
}

func TestAcceptance_GradeWithoutQuotasIsPureCountGrade(t *testing.T) {
	cfg := &config{
		maxParticipants:    5,
		agreementThreshold: 3,
		acceptancePolicyConfigs: []*common.ConsensusAcceptancePolicy{
			{Name: "strict", RequiredAgreement: []*common.ConsensusAgreementQuota{
				{Tag: "type:internal", MinAgreement: 1},
				{Tag: "type:external", MinAgreement: 1},
			}},
			{Name: "any-two", AgreementThreshold: 2},
		},
	}
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xaa", 0),
		resultFrom(t, ext2, "0xaa", 1),
	})
	winner := winnerOf(cfg, analysis)

	require.Nil(t, winner.Error)
	assert.Equal(t, "any-two", winner.Policy)
}

// exemptions ------------------------------------------------------------------------

func TestAcceptance_SendRawTransactionExempt(t *testing.T) {
	// A broadcast accepted by any node propagates network-wide, so winner
	// composition proves nothing — the exemption must survive the cascade.
	cfg := cascadeConfig()
	ext1 := taggedUpstream("external-1", "type:external")

	analysis := analyze(cfg, []*execResult{
		resultFrom(t, ext1, "0xhash", 0),
	})
	analysis.method = "eth_sendRawTransaction"
	winner := winnerOf(cfg, analysis)

	require.Nil(t, winner.Error, "sendRawTransaction must not be composition-gated, got: %v", winner.Error)
}

// config validation ------------------------------------------------------------------

func TestAcceptance_Validation(t *testing.T) {
	base := func(policies ...*common.ConsensusAcceptancePolicy) *common.ConsensusPolicyConfig {
		return &common.ConsensusPolicyConfig{
			MaxParticipants:    5,
			AgreementThreshold: 2,
			AcceptancePolicies: policies,
		}
	}
	quota := func(tag string, n int) *common.ConsensusAgreementQuota {
		return &common.ConsensusAgreementQuota{Tag: tag, MinAgreement: n}
	}

	t.Run("valid strict-then-relaxed cascade is accepted", func(t *testing.T) {
		cfg := base(mixedThenExternalOnly()...)
		require.NoError(t, cfg.Validate())
	})

	t.Run("combining with requiredParticipants minAgreement is rejected", func(t *testing.T) {
		// Two sources of winner composition would leave "which one wins" to
		// implementation order.
		cfg := base(mixedThenExternalOnly()...)
		cfg.RequiredParticipants = []*common.ConsensusRequiredParticipant{
			{Tag: "type:internal", MinParticipants: 1, MinAgreement: 1},
		}
		require.ErrorContains(t, cfg.Validate(), "cannot be combined")
	})

	t.Run("requiredParticipants without minAgreement still allowed alongside", func(t *testing.T) {
		// Participant SELECTION is a separate concern and must remain usable.
		cfg := base(mixedThenExternalOnly()...)
		cfg.RequiredParticipants = []*common.ConsensusRequiredParticipant{
			{Tag: "type:internal", MinParticipants: 1},
		}
		require.NoError(t, cfg.Validate())
	})

	t.Run("missing name is rejected", func(t *testing.T) {
		cfg := base(&common.ConsensusAcceptancePolicy{
			RequiredAgreement: []*common.ConsensusAgreementQuota{quota("type:external", 2)},
		})
		require.ErrorContains(t, cfg.Validate(), "name is required")
	})

	t.Run("duplicate names are rejected", func(t *testing.T) {
		// Names address grades in auth allowlists and metrics; duplicates
		// would make both ambiguous.
		cfg := base(
			&common.ConsensusAcceptancePolicy{Name: "same", RequiredAgreement: []*common.ConsensusAgreementQuota{quota("type:internal", 2)}},
			&common.ConsensusAcceptancePolicy{Name: "same", RequiredAgreement: []*common.ConsensusAgreementQuota{quota("type:external", 1)}},
		)
		require.ErrorContains(t, cfg.Validate(), "duplicates")
	})

	t.Run("empty quota tag is rejected", func(t *testing.T) {
		cfg := base(&common.ConsensusAcceptancePolicy{
			Name:              "standard",
			RequiredAgreement: []*common.ConsensusAgreementQuota{quota("", 1)},
		})
		require.ErrorContains(t, cfg.Validate(), "tag is required")
	})

	t.Run("zero minAgreement quota is rejected", func(t *testing.T) {
		cfg := base(&common.ConsensusAcceptancePolicy{
			Name:              "standard",
			RequiredAgreement: []*common.ConsensusAgreementQuota{quota("type:internal", 0)},
		})
		require.ErrorContains(t, cfg.Validate(), "must be greater than 0")
	})

	t.Run("grade requiring more than maxParticipants is rejected", func(t *testing.T) {
		cfg := base(&common.ConsensusAcceptancePolicy{
			Name:              "impossible",
			RequiredAgreement: []*common.ConsensusAgreementQuota{quota("type:internal", 6)},
		})
		require.ErrorContains(t, cfg.Validate(), "can never be satisfied")
	})

	t.Run("threshold below its own quotas is rejected", func(t *testing.T) {
		cfg := base(&common.ConsensusAcceptancePolicy{
			Name:               "contradictory",
			AgreementThreshold: 1,
			RequiredAgreement: []*common.ConsensusAgreementQuota{
				quota("type:internal", 1), quota("type:external", 1),
			},
		})
		require.ErrorContains(t, cfg.Validate(), "lower than the sum")
	})

	t.Run("relaxed grade listed before strict is rejected as unreachable", func(t *testing.T) {
		// Order IS the mechanism. An inverted list would quietly serve every
		// round at the relaxed grade, defeating the point of the strict one.
		cfg := base(
			&common.ConsensusAcceptancePolicy{Name: "degraded", RequiredAgreement: []*common.ConsensusAgreementQuota{quota("type:external", 1)}},
			&common.ConsensusAcceptancePolicy{Name: "standard", RequiredAgreement: []*common.ConsensusAgreementQuota{
				quota("type:internal", 1), quota("type:external", 1),
			}},
		)
		require.ErrorContains(t, cfg.Validate(), "unreachable")
	})

	t.Run("grades constraining different tags are both reachable", func(t *testing.T) {
		// Neither subsumes the other: an internal-only round satisfies the
		// first, an external-only round the second.
		cfg := base(
			&common.ConsensusAcceptancePolicy{Name: "internal-pair", RequiredAgreement: []*common.ConsensusAgreementQuota{quota("type:internal", 2)}},
			&common.ConsensusAcceptancePolicy{Name: "external-pair", RequiredAgreement: []*common.ConsensusAgreementQuota{quota("type:external", 2)}},
		)
		require.NoError(t, cfg.Validate())
	})
}
