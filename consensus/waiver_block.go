package consensus

import (
	"context"
	"math/big"
	"slices"
	"sort"

	"github.com/erpc/erpc/common"
)

// waiverBlockProof is the block the empty-outside-retention waiver judges
// retention against or, when there is none, the waiverUnprovenReason* that
// says why.
type waiverBlockProof struct {
	block  *big.Int
	reason string
}

// waiverBlock resolves the block the empty-outside-retention waiver judges for
// winning group g. A request that names a block gives it. A hash lookup
// (eth_getTransactionByHash, eth_getTransactionReceipt, eth_getBlockByHash)
// names none. g's answer names one, but the agreeing upstreams cannot vouch
// for their own claim (#1363), so it counts only once an upstream outside the
// agreeing set confirms it (confirmAnswerBlock). A policy that opts into
// blockEvidenceFields trusts g's own block when no upstream confirms it, but
// never when one contradicts it. That is network I/O, so it runs only on a
// complete round where an opted-in quota has abstainers to release, and once
// per group. When the answer's claimed number cannot prove outside retention
// for any opted-in quota (head depth and availability bounds), the confirmer
// probe is skipped — the common just-mined / inside-retention hash dispute
// must not pay for eth_getBlockByNumber. That skip also skips the
// blockEvidenceFields fallback: the gate reads RespRefs, and a divergent
// evidence path must not waive an older number without a confirmer (which
// previously could still contradict and block the fallback).
func (e *executor) waiverBlock(ctx context.Context, analysis *consensusAnalysis, g *responseGroup) waiverBlockProof {
	if b := requestBlock(ctx, analysis); b != nil {
		return waiverBlockProof{block: b}
	}
	if analysis == nil || g == nil || analysis.hasRemaining() || !anyEmptyOutsideRetentionAbstention(analysis, e.config.requiredParticipants) {
		return waiverBlockProof{reason: waiverUnprovenReasonNoRequestBlock}
	}
	if p, ok := analysis.waiverBlocks[g]; ok {
		return p
	}
	var p waiverBlockProof
	if e.answerCannotProveOutsideRetention(ctx, analysis, g) {
		p = waiverBlockProof{reason: waiverUnprovenReasonBlockInsideRetention}
	} else {
		p = e.confirmAnswerBlock(ctx, analysis, g)
		if p.block == nil && p.reason != waiverUnprovenReasonBlockContradicted {
			if b := e.winnerBlockEvidence(analysis, g); b != nil {
				p = waiverBlockProof{block: b}
			}
		}
	}
	if analysis.waiverBlocks == nil {
		analysis.waiverBlocks = make(map[*responseGroup]waiverBlockProof, 1)
	}
	analysis.waiverBlocks[g] = p
	return p
}

// answerCannotProveOutsideRetention reports whether g's answer names a block
// number that, even if trusted, cannot release any empty-outside-retention
// quota this round — every opted-in abstaining quota fails both the
// availability-bound proof and the chain-head depth proof. Used only to skip
// the confirmer probe; it never feeds the claimed number into the waiver.
func (e *executor) answerCannotProveOutsideRetention(ctx context.Context, analysis *consensusAnalysis, g *responseGroup) bool {
	if analysis == nil || g == nil || analysis.originalRequest == nil {
		return false
	}
	number, _ := answerBlock(ctx, analysis, g)
	if number <= 0 {
		return false
	}
	claimed := big.NewInt(number)
	head := e.chainHead(ctx, analysis, g)
	any := false
	for _, req := range e.config.requiredParticipants {
		empties := abstainingEmpties(analysis, req)
		if empties == nil {
			continue
		}
		any = true
		proves, contradicts := availabilityBoundProofState(analysis.historyEvidence(), empties, claimed)
		if proves {
			return false
		}
		if !contradicts && chainHeadProvesOutsideRetention(claimed, head, req.RetentionBlocks) {
			return false
		}
	}
	return any
}

// confirmAnswerBlock checks the block g's answer names against an upstream
// outside the agreeing set. The confirmer's block at the answer's number must
// carry the answer's block id when the answer names one, and the request's
// subject must be that block or one of its lookup ids (a tx hash, a
// signature). Abstainers go first: the quota the waiver releases trusts them,
// and a node that pruned its tx index still serves the block. Upstreams
// outside the round come last. The first block returned decides. One that
// disagrees fails closed instead of asking the next upstream, so a liar
// cannot shop for a confirmer. A null or an error moves on to the next.
func (e *executor) confirmAnswerBlock(ctx context.Context, analysis *consensusAnalysis, g *responseGroup) waiverBlockProof {
	subject := lookupSubject(ctx, analysis)
	number, blockId := answerBlock(ctx, analysis, g)
	if subject == "" || number <= 0 {
		return waiverBlockProof{reason: waiverUnprovenReasonNoRequestBlock}
	}
	ev := analysis.historyEvidence()
	for _, u := range confirmers(analysis, e.agreeingResults(analysis, g), e.config.requiredParticipants) {
		// A node declared not to serve the block cannot confirm it.
		if ev.UpstreamLowerBound(u) > number {
			continue
		}
		probedId, subjects, ok := ev.ProbeBlock(ctx, u, number)
		if !ok {
			continue
		}
		if (blockId != "" && probedId != blockId) || (subject != probedId && !slices.Contains(subjects, subject)) {
			return waiverBlockProof{reason: waiverUnprovenReasonBlockContradicted}
		}
		return waiverBlockProof{block: big.NewInt(number)}
	}
	return waiverBlockProof{reason: waiverUnprovenReasonBlockUnconfirmed}
}

// emptyWaiverHasBlock reports whether the empty-outside-retention waiver can
// get a block for req under ev: the request names one, the policy opts the
// method into blockEvidenceFields, or it names a lookup subject whose answer
// names a block that confirmAnswerBlock can check.
func (e *executor) emptyWaiverHasBlock(ctx context.Context, req *common.NormalizedRequest, ev common.HistoryEvidence) bool {
	method, err := req.Method()
	if err != nil {
		return false
	}
	if fields, ok := methodFields(e.config.blockEvidenceFields, method); ok && len(fields) > 0 {
		return true
	}
	if ev == nil {
		return false
	}
	if _, ok := ev.RequestedBlock(ctx, req); ok {
		return true
	}
	return ev.LookupSubject(ctx, req) != ""
}

// winnerBlockEvidence is the block g's answer names at the policy's
// blockEvidenceFields paths for the round's method, the first that resolves.
// It is the winners' own claim, trusted only by that opt-in. A negative value
// fails closed rather than falling through to a later path.
func (e *executor) winnerBlockEvidence(analysis *consensusAnalysis, g *responseGroup) *big.Int {
	if analysis == nil || g == nil || g.LargestResult == nil {
		return nil
	}
	fields, ok := methodFields(e.config.blockEvidenceFields, analysis.method)
	if !ok {
		return nil
	}
	for _, field := range fields {
		values := extractFieldValues(g.LargestResult, []string{field})
		if len(values) == 0 || values[0] == nil {
			continue
		}
		if values[0].Sign() < 0 {
			return nil
		}
		return values[0]
	}
	return nil
}

// lookupSubject is the id the round's lookup request names (a tx or block
// hash, a tx signature) when its answer names the block that holds it, or "".
func lookupSubject(ctx context.Context, analysis *consensusAnalysis) string {
	ev := analysis.historyEvidence()
	if ev == nil || analysis.originalRequest == nil {
		return ""
	}
	return ev.LookupSubject(ctx, analysis.originalRequest)
}

// answerBlock is the block g's answer names and that block's own id, when
// the chain's answer carries one. A malformed value returns nothing, which
// fails the waiver closed.
func answerBlock(ctx context.Context, analysis *consensusAnalysis, g *responseGroup) (int64, string) {
	ev := analysis.historyEvidence()
	if ev == nil || analysis.originalRequest == nil || g == nil {
		return 0, ""
	}
	return ev.AnswerBlock(ctx, analysis.originalRequest, g.LargestResult)
}

// confirmers lists the upstreams outside the agreeing set that may confirm
// the answer's block, once each: first the round's upstreams that answered
// without an error, in attempt order with abstainers of an opted-in quota
// first, then the request's upstreams that took no part in the round, in
// routing order. The round's own abstainers often cannot help: a node that
// answered empty because it pruned the data also lacks the block that holds
// it, so only a node outside the round can confirm.
func confirmers(analysis *consensusAnalysis, agreeing []*execResult, reqs []*common.ConsensusRequiredParticipant) []common.Upstream {
	seen := make(map[string]struct{}, len(agreeing))
	for _, r := range agreeing {
		if r != nil && r.Upstream != nil {
			seen[r.Upstream.Id()] = struct{}{}
		}
	}
	var candidates []*execResult
	for _, g := range analysis.groups {
		for _, r := range g.Results {
			if r == nil || r.Upstream == nil || r.Err != nil {
				continue
			}
			if _, dup := seen[r.Upstream.Id()]; dup {
				continue
			}
			seen[r.Upstream.Id()] = struct{}{}
			candidates = append(candidates, r)
		}
	}
	abstains := func(r *execResult) bool {
		if r.CachedResponseType != ResponseTypeEmpty {
			return false
		}
		for _, req := range reqs {
			if req != nil && req.WaiveAgreementOnEmptyOutsideRetention && upstreamMatchesTag(r.Upstream, req.Tag) {
				return true
			}
		}
		return false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		ai, aj := abstains(candidates[i]), abstains(candidates[j])
		if ai != aj {
			return ai
		}
		return candidates[i].Index < candidates[j].Index
	})
	out := make([]common.Upstream, len(candidates))
	for i, r := range candidates {
		out[i] = r.Upstream
	}
	req := analysis.originalRequest
	if req == nil {
		return out
	}
	// A participant that failed this round is not asked again.
	for _, g := range analysis.groups {
		for _, r := range g.Results {
			if r != nil && r.Upstream != nil {
				seen[r.Upstream.Id()] = struct{}{}
			}
		}
	}
	for _, u := range req.Upstreams() {
		if u == nil || !req.MatchesUseUpstream(u) {
			continue
		}
		if _, dup := seen[u.Id()]; dup {
			continue
		}
		seen[u.Id()] = struct{}{}
		out = append(out, u)
	}
	return out
}
