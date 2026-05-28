package consensus

import (
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// config carries the consensus-policy configuration through the
// builder-style API.
type config struct {
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
	fireAndForget           bool
	maxWaitOnResult         *common.AdaptiveDuration
	maxWaitOnEmpty          *common.AdaptiveDuration
	requiredParticipants    []*common.ConsensusRequiredParticipant
}

// builder is the internal builder used by NewConsensus.
// It's not exposed externally — callers construct a *Consensus via
// NewConsensus(cfg, logger) and call Run().
type builder struct {
	cfg config
}

func newBuilder() *builder { return &builder{} }

// NewConsensusPolicyBuilder is a test-friendly alias for newBuilder
// — callers in the same package use newBuilder, tests use this name to
// keep their fluent-builder DSL readable.
func NewConsensusPolicyBuilder() *builder { return &builder{} }

// Build constructs the *Consensus runtime entry point. Used by both
// production code (via NewConsensus) and tests (fluent-builder DSL).
func (b *builder) Build() *Consensus {
	pol := b.build()
	return &Consensus{policy: pol, logger: pol.logger}
}

func (b *builder) WithMaxParticipants(n int) *builder {
	b.cfg.maxParticipants = n
	return b
}
func (b *builder) WithAgreementThreshold(n int) *builder {
	b.cfg.agreementThreshold = n
	return b
}
func (b *builder) WithDisputeBehavior(v common.ConsensusDisputeBehavior) *builder {
	b.cfg.disputeBehavior = v
	return b
}
func (b *builder) WithLowParticipantsBehavior(v common.ConsensusLowParticipantsBehavior) *builder {
	b.cfg.lowParticipantsBehavior = v
	return b
}
func (b *builder) WithPunishMisbehavior(v *common.PunishMisbehaviorConfig) *builder {
	b.cfg.punishMisbehavior = v
	return b
}
func (b *builder) WithMisbehaviorsDestination(v *common.MisbehaviorsDestinationConfig) *builder {
	b.cfg.misbehaviorsDestination = v
	return b
}
func (b *builder) WithLogger(lg *zerolog.Logger) *builder { b.cfg.logger = lg; return b }
func (b *builder) WithDisputeLogLevel(l zerolog.Level) *builder {
	b.cfg.disputeLogLevel = l
	return b
}
func (b *builder) WithIgnoreFields(m map[string][]string) *builder {
	b.cfg.ignoreFields = m
	return b
}
func (b *builder) WithPreferNonEmpty(v bool) *builder { b.cfg.preferNonEmpty = v; return b }
func (b *builder) WithPreferLargerResponses(v bool) *builder {
	b.cfg.preferLargerResponses = v
	return b
}
func (b *builder) WithPreferHighestValueFor(m map[string][]string) *builder {
	b.cfg.preferHighestValueFor = m
	return b
}
func (b *builder) WithFireAndForget(v bool) *builder { b.cfg.fireAndForget = v; return b }
func (b *builder) WithMaxWaitOnResult(d *common.AdaptiveDuration) *builder {
	b.cfg.maxWaitOnResult = d
	return b
}
func (b *builder) WithMaxWaitOnEmpty(d *common.AdaptiveDuration) *builder {
	b.cfg.maxWaitOnEmpty = d
	return b
}
func (b *builder) WithRequiredParticipants(v []*common.ConsensusRequiredParticipant) *builder {
	b.cfg.requiredParticipants = v
	return b
}

// build snapshots the config and constructs the runtime consensus policy.
func (b *builder) build() *consensusPolicy {
	hCopy := b.cfg
	log := hCopy.logger.With().Str("component", "consensus").Logger()

	disputeLevel := hCopy.disputeLogLevel
	if disputeLevel == 0 {
		disputeLevel = zerolog.WarnLevel
	}

	var exp misbehaviorExporter
	if hCopy.misbehaviorsDestination != nil {
		exp = createMisbehaviorExporter(hCopy.misbehaviorsDestination, &log)
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

// consensusPolicy is the runtime consensus state. It owns per-upstream
// misbehavior rate limiters and the misbehavior exporter. Construction
// goes through the package-private builder; callers receive a
// *Consensus from NewConsensus().
type consensusPolicy struct {
	*config
	logger                          *zerolog.Logger
	misbehavingUpstreamsLimiter     *sync.Map // map[string]*rate.Limiter
	misbehavingUpstreamsSitoutTimer *sync.Map // map[string]*time.Timer
	disputeLogLevel                 zerolog.Level
	exporter                        misbehaviorExporter
}

// createMisbehaviorExporter selects the configured exporter
// implementation. Errors are logged and the exporter is disabled —
// misbehavior export is best-effort.
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
