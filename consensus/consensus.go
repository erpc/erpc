package consensus

import (
	"context"
	"errors"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// Consensus is the entry point to the consensus executor. The network
// executor calls (*Consensus).Run with a per-slot inner function; this
// struct orchestrates participant fan-out, voting, and dispute
// resolution.
type Consensus struct {
	policy *consensusPolicy
	logger *zerolog.Logger
}

// NewConsensus constructs a Consensus from config.
func NewConsensus(cfg *common.ConsensusPolicyConfig, logger *zerolog.Logger) (*Consensus, error) {
	if cfg == nil {
		return nil, errors.New("nil consensus config")
	}

	b := newBuilder().
		WithMaxParticipants(cfg.MaxParticipants).
		WithAgreementThreshold(cfg.AgreementThreshold).
		WithDisputeBehavior(cfg.DisputeBehavior).
		WithPunishMisbehavior(cfg.PunishMisbehavior).
		WithLowParticipantsBehavior(cfg.LowParticipantsBehavior).
		WithLogger(logger).
		WithFireAndForget(cfg.FireAndForget).
		WithMaxWaitOnResult(cfg.MaxWaitOnResult).
		WithMaxWaitOnEmpty(cfg.MaxWaitOnEmpty)

	if cfg.MisbehaviorsDestination != nil {
		b = b.WithMisbehaviorsDestination(cfg.MisbehaviorsDestination)
	}
	if cfg.IgnoreFields != nil {
		b = b.WithIgnoreFields(cfg.IgnoreFields)
	}
	if cfg.PreferNonEmpty != nil {
		b = b.WithPreferNonEmpty(*cfg.PreferNonEmpty)
	}
	if cfg.PreferLargerResponses != nil {
		b = b.WithPreferLargerResponses(*cfg.PreferLargerResponses)
	}
	if cfg.PreferHighestValueFor != nil {
		b = b.WithPreferHighestValueFor(cfg.PreferHighestValueFor)
	}
	if cfg.DisputeLogLevel != "" {
		level, err := zerolog.ParseLevel(cfg.DisputeLogLevel)
		if err == nil {
			b = b.WithDisputeLogLevel(level)
		}
	}

	pol := b.build()
	return &Consensus{policy: pol, logger: logger}, nil
}

// Run executes the consensus policy with the given inner function as
// the per-slot worker. Returns the winning (response, error) pair or
// the appropriate dispute / low-participant outcome.
func (c *Consensus) Run(
	ctx context.Context,
	req *common.NormalizedRequest,
	inner func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error),
) (*common.NormalizedResponse, error) {
	if c == nil {
		return inner(ctx, req)
	}

	// Bind the request to context so internal helpers (analysis,
	// classify-and-hash) can read it via common.RequestContextKey.
	if req != nil {
		ctx = context.WithValue(ctx, common.RequestContextKey, req)
	}

	ex := &executor{consensusPolicy: c.policy}
	return ex.Run(ctx, req, inner)
}
