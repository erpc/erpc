package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// validConsensusBase returns a minimal ConsensusPolicyConfig that passes the
// pre-existing validation, so each test can bolt on only the field under test.
func validConsensusBase() *ConsensusPolicyConfig {
	return &ConsensusPolicyConfig{
		MaxParticipants:    5,
		AgreementThreshold: 3,
	}
}

func s3Dest(path, filePattern string) *MisbehaviorsDestinationConfig {
	return &MisbehaviorsDestinationConfig{
		Type:        MisbehaviorsDestinationTypeS3,
		Path:        path,
		FilePattern: filePattern,
		S3:          &S3FlushConfig{},
	}
}

func TestConsensusPolicyValidate_MissingDataWaiverFloor_R9(t *testing.T) {
	t.Run("waiver-free config needs no minAgreement floor", func(t *testing.T) {
		c := validConsensusBase()
		// No requiredParticipants at all — the vast majority of existing configs.
		require.NoError(t, c.Validate())

		c = validConsensusBase()
		c.RequiredParticipants = []*ConsensusRequiredParticipant{
			{Tag: "type:external", MinParticipants: 2}, // minAgreement 0, no waiver
		}
		require.NoError(t, c.Validate())
	})

	t.Run("waiver requires a never-waivable minAgreement>0 floor", func(t *testing.T) {
		c := validConsensusBase()
		c.RequiredParticipants = []*ConsensusRequiredParticipant{
			{Tag: "type:internal", MinParticipants: 1, MinAgreement: 1, WaiveAgreementOnMissingData: true},
		}
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "never-waivable")
	})

	t.Run("a never-waivable entry with minAgreement=0 is not a floor", func(t *testing.T) {
		c := validConsensusBase()
		c.RequiredParticipants = []*ConsensusRequiredParticipant{
			{Tag: "type:internal", MinParticipants: 1, MinAgreement: 1, WaiveAgreementOnMissingData: true},
			{Tag: "type:external", MinParticipants: 2, MinAgreement: 0}, // enforces nothing
		}
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "never-waivable")
	})

	t.Run("accepts waiver alongside a never-waivable minAgreement>0 floor", func(t *testing.T) {
		c := validConsensusBase()
		c.RequiredParticipants = []*ConsensusRequiredParticipant{
			{Tag: "type:internal", MinParticipants: 1, MinAgreement: 1, WaiveAgreementOnMissingData: true},
			{Tag: "type:external", MinParticipants: 2, MinAgreement: 2}, // never waived
		}
		require.NoError(t, c.Validate())
	})
}

func TestConsensusPolicyValidate_EmptyWaiver_v11(t *testing.T) {
	// A valid empty-waiver config: an opted-in internal quota with a retention
	// boundary, a never-waivable external floor, and the block-evidence field
	// the waiver's proof reads.
	validEmptyWaiver := func() *ConsensusPolicyConfig {
		c := validConsensusBase()
		c.BlockEvidenceFields = map[string][]string{"eth_getTransactionReceipt": {"blockNumber"}}
		c.RequiredParticipants = []*ConsensusRequiredParticipant{
			{Tag: "type:internal", MinParticipants: 1, MinAgreement: 1, WaiveAgreementOnEmptyOutsideRetention: true, RetentionBlocks: 128},
			{Tag: "type:external", MinParticipants: 2, MinAgreement: 2},
		}
		return c
	}

	t.Run("accepts a well-formed empty-waiver config", func(t *testing.T) {
		require.NoError(t, validEmptyWaiver().Validate())
	})

	t.Run("opting in without retentionBlocks is rejected (R-retention)", func(t *testing.T) {
		c := validEmptyWaiver()
		c.RequiredParticipants[0].RetentionBlocks = 0
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "retentionBlocks")
	})

	t.Run("negative retentionBlocks is rejected", func(t *testing.T) {
		c := validEmptyWaiver()
		c.RequiredParticipants[0].RetentionBlocks = -1
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "retentionBlocks must not be negative")
	})

	t.Run("opting in without blockEvidenceFields is rejected (R11)", func(t *testing.T) {
		c := validEmptyWaiver()
		c.BlockEvidenceFields = nil
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "blockEvidenceFields")
	})

	t.Run("blockEvidenceFields path overlapping ignoreFields is rejected", func(t *testing.T) {
		c := validEmptyWaiver()
		c.IgnoreFields = map[string][]string{
			"eth_getTransactionReceipt": {"blockNumber", "logs"},
		}
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "also listed in ignoreFields")
	})

	t.Run("blockEvidenceFields overlap with ignoreFields is case-insensitive on method", func(t *testing.T) {
		c := validEmptyWaiver()
		c.IgnoreFields = map[string][]string{
			"ETH_GETTRANSACTIONRECEIPT": {"blockNumber"},
		}
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "also listed in ignoreFields")
	})

	t.Run("blockEvidenceFields nested under an ignored ancestor is rejected", func(t *testing.T) {
		c := validEmptyWaiver()
		c.BlockEvidenceFields = map[string][]string{"eth_getTransactionReceipt": {"receipt.blockNumber"}}
		c.IgnoreFields = map[string][]string{
			"eth_getTransactionReceipt": {"receipt"},
		}
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "also listed in ignoreFields")
	})

	t.Run("the empty-waiver also needs a never-waivable floor (R9)", func(t *testing.T) {
		c := validEmptyWaiver()
		// Drop the external floor; only the waivable internal quota remains.
		c.RequiredParticipants = c.RequiredParticipants[:1]
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "never-waivable")
	})
}

func TestConsensusPolicyValidate_S3PathCollision_R10(t *testing.T) {
	t.Run("two policies sharing an S3 path without {timestampMs} is rejected", func(t *testing.T) {
		c := validConsensusBase()
		c.MisbehaviorsDestination = s3Dest("s3://bucket/misbehaviors", "{method}-{networkId}")
		named := validConsensusBase()
		named.MisbehaviorsDestination = s3Dest("s3://bucket/misbehaviors", "{method}-{networkId}")
		c.Policies = map[string]*ConsensusPolicyConfig{"historical": named}

		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "{timestampMs}")
	})

	t.Run("shared S3 path with {timestampMs} in every filePattern is allowed", func(t *testing.T) {
		c := validConsensusBase()
		c.MisbehaviorsDestination = s3Dest("s3://bucket/misbehaviors", "{timestampMs}-{policy}-{method}")
		named := validConsensusBase()
		named.MisbehaviorsDestination = s3Dest("s3://bucket/misbehaviors", "{timestampMs}-{policy}-{networkId}")
		c.Policies = map[string]*ConsensusPolicyConfig{"historical": named}

		require.NoError(t, c.Validate())
	})

	t.Run("shared S3 path with {timestampMs} but no {policy} is rejected", func(t *testing.T) {
		c := validConsensusBase()
		c.MisbehaviorsDestination = s3Dest("s3://bucket/misbehaviors", "{timestampMs}-{method}")
		named := validConsensusBase()
		named.MisbehaviorsDestination = s3Dest("s3://bucket/misbehaviors", "{timestampMs}-{networkId}")
		c.Policies = map[string]*ConsensusPolicyConfig{"historical": named}

		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "{policy}")
	})

	t.Run("trailing-slash variants of the same S3 prefix collide", func(t *testing.T) {
		c := validConsensusBase()
		c.MisbehaviorsDestination = s3Dest("s3://bucket/misbehaviors", "{timestampMs}-{method}")
		named := validConsensusBase()
		named.MisbehaviorsDestination = s3Dest("s3://bucket/misbehaviors/", "{timestampMs}-{networkId}")
		c.Policies = map[string]*ConsensusPolicyConfig{"historical": named}

		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "{policy}")
	})

	t.Run("distinct S3 paths never collide", func(t *testing.T) {
		c := validConsensusBase()
		c.MisbehaviorsDestination = s3Dest("s3://bucket/a", "{method}")
		named := validConsensusBase()
		named.MisbehaviorsDestination = s3Dest("s3://bucket/b", "{method}")
		c.Policies = map[string]*ConsensusPolicyConfig{"historical": named}

		require.NoError(t, c.Validate())
	})

	t.Run("case-distinct S3 key prefixes do not collide", func(t *testing.T) {
		c := validConsensusBase()
		c.MisbehaviorsDestination = s3Dest("s3://bucket/Logs", "{method}")
		named := validConsensusBase()
		named.MisbehaviorsDestination = s3Dest("s3://bucket/logs", "{method}")
		c.Policies = map[string]*ConsensusPolicyConfig{"historical": named}

		require.NoError(t, c.Validate())
	})
}

func TestConsensusPolicyValidate_EmptyPolicyNameRejected(t *testing.T) {
	c := validConsensusBase()
	c.Policies = map[string]*ConsensusPolicyConfig{
		"": validConsensusBase(),
	}
	err := c.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-empty")
}

func TestConsensusPolicyValidate_CustomPolicy(t *testing.T) {
	t.Run("empty evalFunction is rejected", func(t *testing.T) {
		c := validConsensusBase()
		c.CustomPolicy = &ConsensusCustomPolicyConfig{EvalFunction: "   "}
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "evalFunction is required")
	})

	t.Run("syntactically broken evalFunction fails at load", func(t *testing.T) {
		c := validConsensusBase()
		c.CustomPolicy = &ConsensusCustomPolicyConfig{EvalFunction: "(ctx) => { return "}
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to compile")
	})

	t.Run("valid evalFunction compiles", func(t *testing.T) {
		c := validConsensusBase()
		c.CustomPolicy = &ConsensusCustomPolicyConfig{
			EvalFunction: `(ctx) => ctx.user?.auth === "consensus-waive" ? "historical" : null`,
		}
		require.NoError(t, c.Validate())
	})

	t.Run("negative evalTimeout is rejected", func(t *testing.T) {
		c := validConsensusBase()
		c.CustomPolicy = &ConsensusCustomPolicyConfig{
			EvalFunction: `(ctx) => null`,
			EvalTimeout:  Duration(-1 * time.Millisecond),
		}
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "evalTimeout must be >= 0")
	})
}

func TestConsensusPolicyValidate_NamedPolicyNestingRejected(t *testing.T) {
	c := validConsensusBase()
	nested := validConsensusBase()
	nested.CustomPolicy = &ConsensusCustomPolicyConfig{EvalFunction: `(ctx) => null`}
	c.Policies = map[string]*ConsensusPolicyConfig{"historical": nested}

	err := c.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not itself define customPolicy or policies")
}

func TestConsensusPolicyValidate_ReservedDefaultName(t *testing.T) {
	c := validConsensusBase()
	c.Policies = map[string]*ConsensusPolicyConfig{ConsensusDefaultPolicyName: validConsensusBase()}

	err := c.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved name")
}

func TestConsensusPolicyValidate_NamedPolicyValidated(t *testing.T) {
	c := validConsensusBase()
	bad := &ConsensusPolicyConfig{MaxParticipants: 0} // invalid: MaxParticipants must be > 0
	c.Policies = map[string]*ConsensusPolicyConfig{"historical": bad}

	err := c.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `policies["historical"]`)
}

func TestConsensusPolicyValidate_SharedPunishMisbehaviorAcrossPolicies(t *testing.T) {
	matching := &PunishMisbehaviorConfig{
		DisputeThreshold: 2,
		DisputeWindow:    Duration(10 * time.Minute),
		SitOutPenalty:    Duration(5 * time.Minute),
	}
	t.Run("identical punishMisbehavior across policies is ok", func(t *testing.T) {
		c := validConsensusBase()
		c.PunishMisbehavior = matching
		named := validConsensusBase()
		named.PunishMisbehavior = matching.Copy()
		c.Policies = map[string]*ConsensusPolicyConfig{"fallback": named}
		require.NoError(t, c.Validate())
	})
	t.Run("all-nil punishMisbehavior across policies is ok", func(t *testing.T) {
		c := validConsensusBase()
		c.Policies = map[string]*ConsensusPolicyConfig{"fallback": validConsensusBase()}
		require.NoError(t, c.Validate())
	})
	t.Run("mismatched disputeThreshold is rejected", func(t *testing.T) {
		c := validConsensusBase()
		c.PunishMisbehavior = matching
		named := validConsensusBase()
		named.PunishMisbehavior = matching.Copy()
		named.PunishMisbehavior.DisputeThreshold = 9
		c.Policies = map[string]*ConsensusPolicyConfig{"fallback": named}
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "punishMisbehavior must match")
	})
}

func TestFailsafeConfigValidate_InvokesConsensusValidate(t *testing.T) {
	// Consensus.Validate must run on the production FailsafeConfig path —
	// otherwise reserved "default" / never-waivable floor never fire at load.
	f := &FailsafeConfig{
		MatchMethod: "*",
		Consensus: &ConsensusPolicyConfig{
			MaxParticipants:    3,
			AgreementThreshold: 2,
			Policies: map[string]*ConsensusPolicyConfig{
				ConsensusDefaultPolicyName: validConsensusBase(),
			},
		},
	}
	err := f.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved name")
}

func TestConsensusPolicyConfig_CopyIncludesNewFields(t *testing.T) {
	c := validConsensusBase()
	c.CustomPolicy = &ConsensusCustomPolicyConfig{EvalFunction: `(ctx) => null`, EvalTimeout: Duration(50)}
	c.Policies = map[string]*ConsensusPolicyConfig{
		"historical": {MaxParticipants: 3, AgreementThreshold: 2},
	}
	c.RequiredParticipants = []*ConsensusRequiredParticipant{
		{Tag: "type:internal", MinParticipants: 1, MinAgreement: 1, WaiveAgreementOnMissingData: true,
			WaiveAgreementOnEmptyOutsideRetention: true, RetentionBlocks: 128},
	}
	c.BlockEvidenceFields = map[string][]string{"eth_getTransactionReceipt": {"blockNumber"}}

	cp := c.Copy()
	require.NotNil(t, cp.CustomPolicy)
	assert.Equal(t, c.CustomPolicy.EvalFunction, cp.CustomPolicy.EvalFunction)
	// Deep copy: mutating the copy must not affect the original.
	cp.CustomPolicy.EvalFunction = "changed"
	assert.NotEqual(t, c.CustomPolicy.EvalFunction, cp.CustomPolicy.EvalFunction)

	require.Contains(t, cp.Policies, "historical")
	cp.Policies["historical"].MaxParticipants = 99
	assert.Equal(t, 3, c.Policies["historical"].MaxParticipants)

	require.Len(t, cp.RequiredParticipants, 1)
	assert.True(t, cp.RequiredParticipants[0].WaiveAgreementOnMissingData)
	assert.True(t, cp.RequiredParticipants[0].WaiveAgreementOnEmptyOutsideRetention)
	assert.Equal(t, int64(128), cp.RequiredParticipants[0].RetentionBlocks)

	// BlockEvidenceFields is deep-copied: mutating the copy must not leak back.
	require.Contains(t, cp.BlockEvidenceFields, "eth_getTransactionReceipt")
	cp.BlockEvidenceFields["eth_getTransactionReceipt"][0] = "changed"
	assert.Equal(t, "blockNumber", c.BlockEvidenceFields["eth_getTransactionReceipt"][0])
}

func TestConsensusPolicy_ExtendsExplicitFireAndForgetFalse(t *testing.T) {
	// Composite literal keeps compiling: the public field is a bool.
	_ = ConsensusPolicyConfig{FireAndForget: true}

	const src = `
maxParticipants: 3
agreementThreshold: 2
fireAndForget: true
policies:
  strict:
    extends: default
    fireAndForget: false
  inherit:
    extends: default
`
	var c ConsensusPolicyConfig
	require.NoError(t, yaml.Unmarshal([]byte(src), &c))
	require.NoError(t, c.SetDefaults())
	assert.True(t, c.FireAndForget)
	assert.False(t, c.Policies["strict"].FireAndForget, "explicit false overlays a true base")
	assert.True(t, c.Policies["inherit"].FireAndForget, "omitted field keeps the base")
}

func TestConsensusPolicy_UnknownFieldRejected(t *testing.T) {
	var c ConsensusPolicyConfig
	err := yaml.Unmarshal([]byte("maxParticipants: 3\nnotAField: true\n"), &c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "notAField")
	assert.NotContains(t, err.Error(), "common.plain")
}

func TestConsensusPolicy_ExtendsDefault(t *testing.T) {
	c := validConsensusBase()
	c.MaxParticipants = 5
	c.AgreementThreshold = 3
	c.RequiredParticipants = []*ConsensusRequiredParticipant{
		{Tag: "type:internal", MinParticipants: 1, MinAgreement: 1},
		{Tag: "type:external", MinParticipants: 2, MinAgreement: 2},
	}
	c.Policies = map[string]*ConsensusPolicyConfig{
		"fallback": {
			Extends:              ConsensusDefaultPolicyName,
			RequiredParticipants: []*ConsensusRequiredParticipant{}, // explicit clear
		},
		"waive-missing": {
			Extends: ConsensusDefaultPolicyName,
			RequiredParticipants: []*ConsensusRequiredParticipant{
				{Tag: "type:internal", MinParticipants: 1, MinAgreement: 1, WaiveAgreementOnMissingData: true},
				{Tag: "type:external", MinParticipants: 2, MinAgreement: 2},
			},
		},
	}
	require.NoError(t, c.SetDefaults())
	require.NoError(t, c.Validate())

	fb := c.Policies["fallback"]
	require.NotNil(t, fb)
	assert.Equal(t, 5, fb.MaxParticipants, "extends: default should inherit maxParticipants")
	assert.Equal(t, 3, fb.AgreementThreshold)
	assert.Empty(t, fb.RequiredParticipants, "explicit [] clears quotas")
	assert.Empty(t, fb.Extends, "extends cleared after resolve")

	wm := c.Policies["waive-missing"]
	require.NotNil(t, wm)
	assert.Equal(t, 5, wm.MaxParticipants)
	require.Len(t, wm.RequiredParticipants, 2)
	assert.True(t, wm.RequiredParticipants[0].WaiveAgreementOnMissingData)
}

func TestConsensusPolicy_ExtendsUnknownBase(t *testing.T) {
	c := validConsensusBase()
	c.Policies = map[string]*ConsensusPolicyConfig{
		"fallback": {Extends: "nope", MaxParticipants: 3, AgreementThreshold: 2},
	}
	err := c.SetDefaults()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown base")
}

func TestConsensusPolicy_ExtendsNestedRejected(t *testing.T) {
	c := validConsensusBase()
	c.Policies = map[string]*ConsensusPolicyConfig{
		"base":  {Extends: ConsensusDefaultPolicyName, MaxParticipants: 3, AgreementThreshold: 2},
		"child": {Extends: "base", MaxParticipants: 3, AgreementThreshold: 2},
	}
	// Resolve order is map iteration — either SetDefaults fails on nested, or
	// Validate catches unresolved nested extends.
	err := c.SetDefaults()
	if err == nil {
		err = c.Validate()
	}
	// After SetDefaults, "base" is resolved (Extends cleared). "child" extending
	// "base" is allowed only if base had no Extends at resolve time — but base
	// started with Extends, so resolveNamedPolicyExtends must reject child.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "one-level")
}

func TestConsensusPolicy_ExtendsWithNestedSelectorRejected(t *testing.T) {
	// SetDefaults used to strip customPolicy during extends resolve before
	// Validate ran, so extends+customPolicy silently loaded. Reject at resolve.
	c := validConsensusBase()
	c.Policies = map[string]*ConsensusPolicyConfig{
		"fallback": {
			Extends:      ConsensusDefaultPolicyName,
			CustomPolicy: &ConsensusCustomPolicyConfig{EvalFunction: `(ctx) => null`},
		},
	}
	err := c.SetDefaults()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not itself define customPolicy or policies")
}
