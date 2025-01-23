package consensus

import (
	// "time"

	"github.com/erpc/erpc/common"
	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/policy"
)

// If the execution is configured with a Context, a child context will be created for each attempt and outstanding
// contexts are canceled when the ConsensusPolicy is finished.
//
// R is the execution result type. This type is concurrency safe.
type ConsensusPolicy[R any] interface {
	failsafe.Policy[R]
}

// R is the execution result type. This type is not concurrency safe.
type ConsensusPolicyBuilder[R any] interface {
	WithRequiredParticipants(requiredParticipants int) ConsensusPolicyBuilder[R]
	WithAgreementThreshold(agreementThreshold int) ConsensusPolicyBuilder[R]
	WithDisputeBehavior(disputeBehavior common.ConsensusDisputeBehavior) ConsensusPolicyBuilder[R]
	WithPunishMisbehavior(cfg *common.PunishMisbehaviorConfig) ConsensusPolicyBuilder[R]
	WithFailureBehavior(failureBehavior common.ConsensusFailureBehavior) ConsensusPolicyBuilder[R]
	WithLowParticipantsBehavior(lowParticipantsBehavior common.ConsensusLowParticipantsBehavior) ConsensusPolicyBuilder[R]
	OnAgreement(listener func(failsafe.ExecutionEvent[R])) ConsensusPolicyBuilder[R]
	OnDispute(listener func(failsafe.ExecutionEvent[R])) ConsensusPolicyBuilder[R]
	OnFailure(listener func(failsafe.ExecutionEvent[R])) ConsensusPolicyBuilder[R]
	OnLowParticipants(listener func(failsafe.ExecutionEvent[R])) ConsensusPolicyBuilder[R]

	// Build returns a new ConsensusPolicy using the builder's configuration.
	Build() ConsensusPolicy[R]
}

type config[R any] struct {
	*policy.BaseAbortablePolicy[R]

	requiredParticipants    int
	agreementThreshold      int
	disputeBehavior         common.ConsensusDisputeBehavior
	failureBehavior         common.ConsensusFailureBehavior
	lowParticipantsBehavior common.ConsensusLowParticipantsBehavior
	punishMisbehavior       *common.PunishMisbehaviorConfig

	onAgreement       func(event failsafe.ExecutionEvent[R])
	onDispute         func(event failsafe.ExecutionEvent[R])
	onFailure         func(event failsafe.ExecutionEvent[R])
	onLowParticipants func(event failsafe.ExecutionEvent[R])
}

var _ ConsensusPolicyBuilder[any] = &config[any]{}

func NewConsensusPolicyBuilder[R any]() ConsensusPolicyBuilder[R] {
	return &config[R]{
		BaseAbortablePolicy: &policy.BaseAbortablePolicy[R]{},
	}
}

type consensusPolicy[R any] struct {
	*config[R]
}

var _ ConsensusPolicy[any] = &consensusPolicy[any]{}

func (c *config[R]) WithRequiredParticipants(requiredParticipants int) ConsensusPolicyBuilder[R] {
	c.requiredParticipants = requiredParticipants
	return c
}

func (c *config[R]) WithAgreementThreshold(agreementThreshold int) ConsensusPolicyBuilder[R] {
	c.agreementThreshold = agreementThreshold
	return c
}

func (c *config[R]) WithDisputeBehavior(disputeBehavior common.ConsensusDisputeBehavior) ConsensusPolicyBuilder[R] {
	c.disputeBehavior = disputeBehavior
	return c
}

func (c *config[R]) WithPunishMisbehavior(cfg *common.PunishMisbehaviorConfig) ConsensusPolicyBuilder[R] {
	c.punishMisbehavior = cfg
	return c
}

func (c *config[R]) WithFailureBehavior(failureBehavior common.ConsensusFailureBehavior) ConsensusPolicyBuilder[R] {
	c.failureBehavior = failureBehavior
	return c
}

func (c *config[R]) WithLowParticipantsBehavior(lowParticipantsBehavior common.ConsensusLowParticipantsBehavior) ConsensusPolicyBuilder[R] {
	c.lowParticipantsBehavior = lowParticipantsBehavior
	return c
}

func (c *config[R]) OnAgreement(listener func(failsafe.ExecutionEvent[R])) ConsensusPolicyBuilder[R] {
	c.onAgreement = listener
	return c
}

func (c *config[R]) OnDispute(listener func(failsafe.ExecutionEvent[R])) ConsensusPolicyBuilder[R] {
	c.onDispute = listener
	return c
}

func (c *config[R]) OnFailure(listener func(failsafe.ExecutionEvent[R])) ConsensusPolicyBuilder[R] {
	c.onFailure = listener
	return c
}

func (c *config[R]) OnLowParticipants(listener func(failsafe.ExecutionEvent[R])) ConsensusPolicyBuilder[R] {
	c.onLowParticipants = listener
	return c
}

func (c *config[R]) Build() ConsensusPolicy[R] {
	hCopy := *c
	if !c.BaseAbortablePolicy.IsConfigured() {
		c.AbortIf(func(r R, err error) bool {
			// TODO abort if consensus is reached
			return true
		})
	}
	return &consensusPolicy[R]{
		config: &hCopy, // TODO copy base fields
	}
}

func (h *consensusPolicy[R]) ToExecutor(_ R) any {
	he := &executor[R]{
		BaseExecutor:    &policy.BaseExecutor[R]{},
		consensusPolicy: h,
	}
	he.Executor = he
	return he
}
