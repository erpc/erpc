package consensus

import (
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/policy"
	"github.com/rs/zerolog"
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
	WithLogger(logger *zerolog.Logger) ConsensusPolicyBuilder[R]
	WithRequiredParticipants(requiredParticipants int) ConsensusPolicyBuilder[R]
	WithAgreementThreshold(agreementThreshold int) ConsensusPolicyBuilder[R]
	WithDisputeBehavior(disputeBehavior common.ConsensusDisputeBehavior) ConsensusPolicyBuilder[R]
	WithPunishMisbehavior(cfg *common.PunishMisbehaviorConfig) ConsensusPolicyBuilder[R]
	WithLowParticipantsBehavior(lowParticipantsBehavior common.ConsensusLowParticipantsBehavior) ConsensusPolicyBuilder[R]
	OnAgreement(listener func(failsafe.ExecutionEvent[R])) ConsensusPolicyBuilder[R]
	OnDispute(listener func(failsafe.ExecutionEvent[R])) ConsensusPolicyBuilder[R]
	OnLowParticipants(listener func(failsafe.ExecutionEvent[R])) ConsensusPolicyBuilder[R]
	WithDisputeLogLevel(level zerolog.Level) ConsensusPolicyBuilder[R]
	WithIgnoreFields(ignoreFields map[string][]string) ConsensusPolicyBuilder[R]

	// Build returns a new ConsensusPolicy using the builder's configuration.
	Build() ConsensusPolicy[R]
}

type config[R any] struct {
	*policy.BaseAbortablePolicy[R]

	requiredParticipants    int
	agreementThreshold      int
	disputeBehavior         common.ConsensusDisputeBehavior
	lowParticipantsBehavior common.ConsensusLowParticipantsBehavior
	punishMisbehavior       *common.PunishMisbehaviorConfig
	timeout                 time.Duration
	logger                  *zerolog.Logger
	disputeLogLevel         zerolog.Level
	ignoreFields            map[string][]string

	onAgreement       func(event failsafe.ExecutionEvent[R])
	onDispute         func(event failsafe.ExecutionEvent[R])
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
	logger                          *zerolog.Logger
	misbehavingUpstreamsLimiter     *sync.Map // [string, *ratelimiter.Limiter]
	misbehavingUpstreamsSitoutTimer *sync.Map // [string, *time.Timer]
	disputeLogLevel                 zerolog.Level
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

func (c *config[R]) WithLowParticipantsBehavior(lowParticipantsBehavior common.ConsensusLowParticipantsBehavior) ConsensusPolicyBuilder[R] {
	c.lowParticipantsBehavior = lowParticipantsBehavior
	return c
}

func (c *config[R]) WithLogger(logger *zerolog.Logger) ConsensusPolicyBuilder[R] {
	c.logger = logger
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

func (c *config[R]) OnLowParticipants(listener func(failsafe.ExecutionEvent[R])) ConsensusPolicyBuilder[R] {
	c.onLowParticipants = listener
	return c
}

func (c *config[R]) WithDisputeLogLevel(level zerolog.Level) ConsensusPolicyBuilder[R] {
	c.disputeLogLevel = level
	return c
}

func (c *config[R]) WithIgnoreFields(ignoreFields map[string][]string) ConsensusPolicyBuilder[R] {
	c.ignoreFields = ignoreFields
	return c
}

func (c *config[R]) Build() ConsensusPolicy[R] {
	hCopy := *c
	if !c.BaseAbortablePolicy.IsConfigured() {
		c.AbortIf(func(exec failsafe.ExecutionAttempt[R], r R, err error) bool {
			// We'll let the executor handle the actual consensus check
			return false
		})
	}

	log := c.logger.With().Str("component", "consensus").Logger()

	// Set default dispute log level if not specified
	disputeLevel := c.disputeLogLevel
	if disputeLevel == 0 {
		disputeLevel = zerolog.WarnLevel
	}

	return &consensusPolicy[R]{
		config:                          &hCopy,
		logger:                          &log,
		misbehavingUpstreamsLimiter:     &sync.Map{},
		misbehavingUpstreamsSitoutTimer: &sync.Map{},
		disputeLogLevel:                 disputeLevel,
	}
}

func (p *consensusPolicy[R]) WithRequiredParticipants(required int) ConsensusPolicy[R] {
	pCopy := *p
	pCopy.requiredParticipants = required
	return &pCopy
}

func (p *consensusPolicy[R]) WithAgreementThreshold(threshold int) ConsensusPolicy[R] {
	pCopy := *p
	pCopy.agreementThreshold = threshold
	return &pCopy
}

func (p *consensusPolicy[R]) WithTimeout(timeout time.Duration) ConsensusPolicy[R] {
	pCopy := *p
	pCopy.timeout = timeout
	return &pCopy
}

func (p *consensusPolicy[R]) WithLogger(logger *zerolog.Logger) ConsensusPolicy[R] {
	pCopy := *p
	lg := logger.With().Str("component", "consensus").Logger()
	pCopy.logger = &lg
	return &pCopy
}

func (p *consensusPolicy[R]) WithDisputeLogLevel(level zerolog.Level) ConsensusPolicy[R] {
	pCopy := *p
	pCopy.disputeLogLevel = level
	return &pCopy
}

func (p *consensusPolicy[R]) ToExecutor(_ R) any {
	return p.Build()
}

func (p *consensusPolicy[R]) Build() policy.Executor[R] {
	return &executor[R]{
		BaseExecutor:    &policy.BaseExecutor[R]{},
		consensusPolicy: p,
	}
}
