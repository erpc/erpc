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
type ConsensusPolicy interface {
	failsafe.Policy[*common.NormalizedResponse]
}

// R is the execution result type. This type is not concurrency safe.
type ConsensusPolicyBuilder interface {
	WithLogger(logger *zerolog.Logger) ConsensusPolicyBuilder
	WithMaxParticipants(maxParticipants int) ConsensusPolicyBuilder
	WithAgreementThreshold(agreementThreshold int) ConsensusPolicyBuilder
	WithDisputeBehavior(disputeBehavior common.ConsensusDisputeBehavior) ConsensusPolicyBuilder
	WithPunishMisbehavior(cfg *common.PunishMisbehaviorConfig) ConsensusPolicyBuilder
	WithLowParticipantsBehavior(lowParticipantsBehavior common.ConsensusLowParticipantsBehavior) ConsensusPolicyBuilder
	WithMisbehaviorsDestination(cfg *common.MisbehaviorsDestinationConfig) ConsensusPolicyBuilder
	OnAgreement(listener func(failsafe.ExecutionEvent[*common.NormalizedResponse])) ConsensusPolicyBuilder
	OnDispute(listener func(failsafe.ExecutionEvent[*common.NormalizedResponse])) ConsensusPolicyBuilder
	OnLowParticipants(listener func(failsafe.ExecutionEvent[*common.NormalizedResponse])) ConsensusPolicyBuilder
	WithDisputeLogLevel(level zerolog.Level) ConsensusPolicyBuilder
	WithIgnoreFields(ignoreFields map[string][]string) ConsensusPolicyBuilder
	WithPreferNonEmpty(preferNonEmpty bool) ConsensusPolicyBuilder
	WithPreferLargerResponses(preferLargerResponses bool) ConsensusPolicyBuilder
	WithPreferHighestValueFor(preferHighestValueFor map[string][]string) ConsensusPolicyBuilder

	// Build returns a new ConsensusPolicy using the builder's configuration.
	Build() ConsensusPolicy
}

type config struct {
	*policy.BaseAbortablePolicy[*common.NormalizedResponse]

	maxParticipants         int
	agreementThreshold      int
	disputeBehavior         common.ConsensusDisputeBehavior
	lowParticipantsBehavior common.ConsensusLowParticipantsBehavior
	punishMisbehavior       *common.PunishMisbehaviorConfig
	misbehaviorsDestination *common.MisbehaviorsDestinationConfig
	timeout                 time.Duration
	logger                  *zerolog.Logger
	disputeLogLevel         zerolog.Level
	ignoreFields            map[string][]string
	preferNonEmpty          bool
	preferLargerResponses   bool
	preferHighestValueFor   map[string][]string

	onAgreement       func(event failsafe.ExecutionEvent[*common.NormalizedResponse])
	onDispute         func(event failsafe.ExecutionEvent[*common.NormalizedResponse])
	onLowParticipants func(event failsafe.ExecutionEvent[*common.NormalizedResponse])
}

var _ ConsensusPolicyBuilder = &config{}

func NewConsensusPolicyBuilder() ConsensusPolicyBuilder {
	return &config{
		BaseAbortablePolicy: &policy.BaseAbortablePolicy[*common.NormalizedResponse]{},
	}
}

type consensusPolicy struct {
	*config
	logger                          *zerolog.Logger
	misbehavingUpstreamsLimiter     *sync.Map // [string, *ratelimiter.Limiter]
	misbehavingUpstreamsSitoutTimer *sync.Map // [string, *time.Timer]
	disputeLogLevel                 zerolog.Level
	exporter                        misbehaviorExporter
}

var _ ConsensusPolicy = &consensusPolicy{}

func (c *config) WithMaxParticipants(maxParticipants int) ConsensusPolicyBuilder {
	c.maxParticipants = maxParticipants
	return c
}

func (c *config) WithAgreementThreshold(agreementThreshold int) ConsensusPolicyBuilder {
	c.agreementThreshold = agreementThreshold
	return c
}

func (c *config) WithDisputeBehavior(disputeBehavior common.ConsensusDisputeBehavior) ConsensusPolicyBuilder {
	c.disputeBehavior = disputeBehavior
	return c
}

func (c *config) WithPunishMisbehavior(cfg *common.PunishMisbehaviorConfig) ConsensusPolicyBuilder {
	c.punishMisbehavior = cfg
	return c
}

func (c *config) WithLowParticipantsBehavior(lowParticipantsBehavior common.ConsensusLowParticipantsBehavior) ConsensusPolicyBuilder {
	c.lowParticipantsBehavior = lowParticipantsBehavior
	return c
}

func (c *config) WithMisbehaviorsDestination(cfg *common.MisbehaviorsDestinationConfig) ConsensusPolicyBuilder {
	c.misbehaviorsDestination = cfg
	return c
}

func (c *config) WithLogger(logger *zerolog.Logger) ConsensusPolicyBuilder {
	c.logger = logger
	return c
}

func (c *config) OnAgreement(listener func(failsafe.ExecutionEvent[*common.NormalizedResponse])) ConsensusPolicyBuilder {
	c.onAgreement = listener
	return c
}

func (c *config) OnDispute(listener func(failsafe.ExecutionEvent[*common.NormalizedResponse])) ConsensusPolicyBuilder {
	c.onDispute = listener
	return c
}

func (c *config) OnLowParticipants(listener func(failsafe.ExecutionEvent[*common.NormalizedResponse])) ConsensusPolicyBuilder {
	c.onLowParticipants = listener
	return c
}

func (c *config) WithDisputeLogLevel(level zerolog.Level) ConsensusPolicyBuilder {
	c.disputeLogLevel = level
	return c
}

func (c *config) WithIgnoreFields(ignoreFields map[string][]string) ConsensusPolicyBuilder {
	c.ignoreFields = ignoreFields
	return c
}

func (c *config) WithPreferNonEmpty(preferNonEmpty bool) ConsensusPolicyBuilder {
	c.preferNonEmpty = preferNonEmpty
	return c
}

func (c *config) WithPreferLargerResponses(preferLargerResponses bool) ConsensusPolicyBuilder {
	c.preferLargerResponses = preferLargerResponses
	return c
}

func (c *config) WithPreferHighestValueFor(preferHighestValueFor map[string][]string) ConsensusPolicyBuilder {
	c.preferHighestValueFor = preferHighestValueFor
	return c
}

func (c *config) Build() ConsensusPolicy {
	hCopy := *c
	if !c.BaseAbortablePolicy.IsConfigured() {
		c.AbortIf(func(exec failsafe.ExecutionAttempt[*common.NormalizedResponse], r *common.NormalizedResponse, err error) bool {
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

	var exp misbehaviorExporter
	if c.misbehaviorsDestination != nil {
		exp = createMisbehaviorExporter(c.misbehaviorsDestination, &log)
	}

	return &consensusPolicy{
		config:                          &hCopy,
		logger:                          &log,
		misbehavingUpstreamsLimiter:     &sync.Map{},
		misbehavingUpstreamsSitoutTimer: &sync.Map{},
		disputeLogLevel:                 disputeLevel,
		exporter:                        exp,
	}
}

func (p *consensusPolicy) WithMaxParticipants(required int) ConsensusPolicy {
	pCopy := *p
	pCopy.maxParticipants = required
	return &pCopy
}

func (p *consensusPolicy) WithAgreementThreshold(threshold int) ConsensusPolicy {
	pCopy := *p
	pCopy.agreementThreshold = threshold
	return &pCopy
}

func (p *consensusPolicy) WithTimeout(timeout time.Duration) ConsensusPolicy {
	pCopy := *p
	pCopy.timeout = timeout
	return &pCopy
}

func (p *consensusPolicy) WithLogger(logger *zerolog.Logger) ConsensusPolicy {
	pCopy := *p
	lg := logger.With().Str("component", "consensus").Logger()
	pCopy.logger = &lg
	return &pCopy
}

func (p *consensusPolicy) WithDisputeLogLevel(level zerolog.Level) ConsensusPolicy {
	pCopy := *p
	pCopy.disputeLogLevel = level
	return &pCopy
}

func (p *consensusPolicy) ToExecutor(_ *common.NormalizedResponse) any {
	return p.Build()
}

func (p *consensusPolicy) Build() policy.Executor[*common.NormalizedResponse] {
	e := &executor{
		BaseExecutor:    &policy.BaseExecutor[*common.NormalizedResponse]{},
		consensusPolicy: p,
	}
	return e
}

// createMisbehaviorExporter creates the appropriate exporter based on configuration
func createMisbehaviorExporter(cfg *common.MisbehaviorsDestinationConfig, log *zerolog.Logger) misbehaviorExporter {
	if cfg == nil || cfg.Path == "" {
		return nil
	}

	switch cfg.Type {
	case common.MisbehaviorsDestinationTypeS3:
		exp, err := newS3MisbehaviorExporter(cfg, log)
		if err != nil {
			log.Error().
				Str("path", cfg.Path).
				Err(err).
				Msg("failed to initialize S3 misbehavior exporter; export disabled")
			return nil
		}
		return exp

	case common.MisbehaviorsDestinationTypeFile:
		fallthrough
	default:
		exp, err := newFileMisbehaviorExporter(cfg, log)
		if err != nil {
			log.Warn().
				Str("path", cfg.Path).
				Err(err).
				Msg("failed to initialize file misbehavior exporter; export disabled")
			return nil
		}
		return exp
	}
}
