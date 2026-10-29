package consensus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	policyengine "github.com/erpc/erpc/internal/consensus/policy"
	"github.com/rs/zerolog"
)

// defaultPolicyName is the label reported (metric + X-ERPC-Consensus-Policy
// header) whenever the round runs under the inline/enclosing config — either
// because the operator eval explicitly returned null, or because the round
// fell closed to the strictest default (unknown name, eval failure, timeout,
// pool exhaustion). The inline config has no operator-given name of its own.
// Reserved: config validation rejects it as a consensus.policies key so the
// label stays unambiguous.
const defaultPolicyName = common.ConsensusDefaultPolicyName

// liveConsensuses tracks every Consensus built via NewConsensus so graceful
// shutdown can flush S3 batches and release file hubs (see CloseAll).
var liveConsensuses sync.Map // *Consensus -> struct{}

// inflightAnalyzers counts runAnalyzer goroutines. HTTP shutdown does not
// wait for them: fire-and-forget rounds detach from the request context, and
// the analyzer keeps exporting misbehavior after the handler returns.
// CloseAll waits for this before closing exporters.
var inflightAnalyzers sync.WaitGroup

// Consensus is the entry point to the consensus executor. The network
// executor calls (*Consensus).Run with a per-slot inner function; this
// struct orchestrates participant fan-out, voting, and dispute
// resolution.
//
// When customPolicy is configured, a pre-round selector picks one of the
// named policies to govern each round; otherwise the inline default policy
// governs every round with zero added per-request cost.
type Consensus struct {
	projectId   string                      // owning project; "" for test/bare consensus
	policy      *consensusPolicy            // inline default (strictest fallback)
	named       map[string]*consensusPolicy // named alternatives the selector can pick
	selector    *policyengine.Selector      // nil when customPolicy is unset
	evalTimeout time.Duration               // wall-clock cap for one selector eval
	logger      *zerolog.Logger
	// evalFailLogLimiter rate-limits the per-failure Warn so a failing eval
	// under load logs once per window instead of once per request.
	evalFailLogLimiter *policyLogLimiter
	closed             sync.Once
}

// HasSelector reports whether a pre-round customPolicy selector is configured.
// The request path uses it to skip building selector inputs (upstream
// snapshot, auth stamp) when no policy eval will consume them.
func (c *Consensus) HasSelector() bool {
	return c != nil && c.selector != nil
}

// NewConsensus constructs a Consensus from config. projectId scopes
// project-keyed recovery (admin uncordon sit-out reset) so same-ID upstreams
// in different projects are independent; pass "" where a project is unknown
// (tests).
func NewConsensus(cfg *common.ConsensusPolicyConfig, logger *zerolog.Logger, projectId string) (*Consensus, error) {
	if cfg == nil {
		return nil, errors.New("nil consensus config")
	}

	// Compile the operator selector before building any policy exporters so a
	// compile failure cannot leak open file handles from MisbehaviorsDestination.
	// Resolve omitted/zero EvalTimeout here so callers that skip SetDefaults
	// (tests, direct NewConsensus) cannot disable the wall-clock cap.
	var selector *policyengine.Selector
	var evalTimeout time.Duration
	if cfg.CustomPolicy != nil && strings.TrimSpace(cfg.CustomPolicy.EvalFunction) != "" {
		evalTimeout = time.Duration(cfg.CustomPolicy.EvalTimeout)
		if evalTimeout <= 0 {
			evalTimeout = common.ConsensusDefaultEvalTimeout
		}
		sel, err := policyengine.Compile(cfg.CustomPolicy.EvalFunction, evalTimeout)
		if err != nil {
			return nil, fmt.Errorf("consensus custom policy: %w", err)
		}
		selector = sel
	}

	// Share misbehavior limiter + sitout-timer maps across the inline default
	// and every named policy (feature §4.4). Per-policy maps would reset
	// accumulation on grade switch and let an earlier policy's sitout timer
	// uncordon while a later sitout is still active.
	//
	// File misbehavior exporters for the same destination path share one
	// sharedFileHub (mutex + handle map) in export.go so concurrent
	// named-policy writes cannot tear NDJSON. Each policy still gets its own
	// exporter wrapper so `{policy}` substitution stays correct; hubs are
	// refcounted and retained at refs==0 so Close cannot split the lock.
	sharedLimiter := &sync.Map{}
	sharedSitout := &sync.Map{}

	c := &Consensus{
		projectId:          projectId,
		policy:             buildPolicy(cfg, logger, defaultPolicyName),
		logger:             logger,
		evalFailLogLimiter: newPolicyLogLimiter(5 * time.Second),
	}
	c.policy.misbehavingUpstreamsLimiter = sharedLimiter
	c.policy.misbehavingUpstreamsSitoutTimer = sharedSitout
	// Bind every policy to the same PunishMisbehavior pointer so the shared
	// limiter's first-touch createRateLimiter cannot pick divergent settings
	// even if a named config somehow carried a distinct-but-equal copy.
	// Load-time validation already requires identical values across policies.
	sharedPunish := cfg.PunishMisbehavior
	c.policy.punishMisbehavior = sharedPunish

	// Pre-build every named policy up front so selection is a map lookup on
	// the request path (no per-request builder work).
	if len(cfg.Policies) > 0 {
		c.named = make(map[string]*consensusPolicy, len(cfg.Policies))
		for name, pc := range cfg.Policies {
			if pc == nil {
				continue
			}
			p := buildPolicy(pc, logger, name)
			p.misbehavingUpstreamsLimiter = sharedLimiter
			p.misbehavingUpstreamsSitoutTimer = sharedSitout
			p.punishMisbehavior = sharedPunish
			c.named[name] = p
		}
	}

	c.selector = selector
	c.evalTimeout = evalTimeout
	liveConsensuses.Store(c, struct{}{})
	return c, nil
}

// Close flushes and closes every misbehavior exporter owned by this Consensus
// (inline default + named policies). Safe to call more than once. Prefer
// CloseAll from process shutdown so every NewConsensus instance is covered.
func (c *Consensus) Close() error {
	if c == nil {
		return nil
	}
	var firstErr error
	c.closed.Do(func() {
		liveConsensuses.Delete(c)
		closeOne := func(p *consensusPolicy) {
			if p == nil || p.exporter == nil {
				return
			}
			if err := p.exporter.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		closeOne(c.policy)
		for _, p := range c.named {
			closeOne(p)
		}
	})
	return firstErr
}

// ResetSitout clears the shared sit-out timer entry for an upstream across
// the live Consensuses of one project, so a full admin uncordon lets the
// upstream be re-punished immediately (review finding): without it, an
// uncleared sit-out entry suppresses re-punish until the original penalty
// window ends. Scoped by project: upstream IDs are only unique per project,
// so a bare-ID global reset would stop a same-ID upstream's timer in another
// project while leaving its punishment bit set (stuck cordon).
func ResetSitout(projectId, upstreamId string) {
	liveConsensuses.Range(func(key, _ any) bool {
		c, ok := key.(*Consensus)
		if !ok || c.projectId != projectId {
			return true
		}
		clearPolicySitout(c.policy, upstreamId)
		for _, p := range c.named {
			clearPolicySitout(p, upstreamId)
		}
		return true
	})
}

func clearPolicySitout(p *consensusPolicy, upstreamId string) {
	if p == nil || p.misbehavingUpstreamsSitoutTimer == nil {
		return
	}
	if v, ok := p.misbehavingUpstreamsSitoutTimer.LoadAndDelete(upstreamId); ok {
		if t, ok := v.(*time.Timer); ok {
			t.Stop()
		}
	}
}

// CloseAll closes every Consensus registered by NewConsensus. Wired into
// graceful shutdown next to evm.CloseIntegrityExporters so S3 flush workers
// and shared file hubs release before the process exits.
//
// Fire-and-forget analyzers use context.WithoutCancel, and exporter Close may
// upload to S3 with no context of its own. ctx bounds the analyzer wait. If
// that wait expires, CloseAll returns without closing exporters: a close
// racing a still-running analyzer drops file appends and can lose an S3
// buffer. Process exit is the fallback for that hung analyzer.
func CloseAll(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !waitFinished(ctx, func() { inflightAnalyzers.Wait() }) {
		return
	}
	liveConsensuses.Range(func(key, _ any) bool {
		c, ok := key.(*Consensus)
		if !ok {
			return true
		}
		// Exporters close only after every analyzer has finished. A second
		// deadline here would close under an analyzer that is still inside
		// the wait we just completed; that wait already returned.
		_ = c.Close()
		return true
	})
}

// waitFinished runs fn and reports whether it finished before ctx ended.
// fn is not cancelled.
func waitFinished(ctx context.Context, fn func()) bool {
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// buildPolicy compiles one ConsensusPolicyConfig into a consensusPolicy. Used
// for both the inline default and each named policy. Nested CustomPolicy /
// Policies on a named config are intentionally ignored — selection happens
// once, at the top level.
func buildPolicy(cfg *common.ConsensusPolicyConfig, logger *zerolog.Logger, policyName string) *consensusPolicy {
	b := newBuilder().
		WithMaxParticipants(cfg.MaxParticipants).
		WithAgreementThreshold(cfg.AgreementThreshold).
		WithDisputeBehavior(cfg.DisputeBehavior).
		WithPunishMisbehavior(cfg.PunishMisbehavior).
		WithLowParticipantsBehavior(cfg.LowParticipantsBehavior).
		WithLogger(logger).
		WithFireAndForget(cfg.FireAndForget).
		WithMaxWaitOnResult(cfg.MaxWaitOnResult).
		WithMaxWaitOnEmpty(cfg.MaxWaitOnEmpty).
		WithRequiredParticipants(cfg.RequiredParticipants).
		WithPolicyName(policyName)

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
	if cfg.BlockEvidenceFields != nil {
		b = b.WithBlockEvidenceFields(cfg.BlockEvidenceFields)
	}
	if cfg.DisputeLogLevel != "" {
		level, err := zerolog.ParseLevel(cfg.DisputeLogLevel)
		if err == nil {
			b = b.WithDisputeLogLevel(level)
		}
	}

	return b.build()
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

	// Pre-round selection: pick the named policy that governs this round. When
	// no selector is configured, the inline default governs at zero added cost.
	// A nil request cannot build an eval context — keep the pre-selector
	// fallback and run under the inline default.
	pol := c.policy
	if c.selector != nil && req != nil {
		pol = c.resolvePolicy(req)
	}

	ex := &executor{consensusPolicy: pol}
	return ex.Run(ctx, req, inner)
}
