package consensus

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
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
	number, _ := answerBlock(ctx, analysis.originalRequest, g.LargestResult)
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
		proves, contradicts := availabilityBoundProofState(empties, claimed)
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
// carry the answer's block hash, and the request's hash must be that block or
// one of its transactions. Abstainers go first: the quota the waiver releases
// trusts them, and a node that pruned its tx index still serves the block.
// The first block returned decides. One that disagrees fails closed instead of
// asking the next upstream, so a liar cannot shop for a confirmer. A null or
// an error moves on to the next.
func (e *executor) confirmAnswerBlock(ctx context.Context, analysis *consensusAnalysis, g *responseGroup) waiverBlockProof {
	subject := requestHash(ctx, analysis.originalRequest)
	number, hash := answerBlock(ctx, analysis.originalRequest, g.LargestResult)
	if subject == "" || number <= 0 || hash == "" {
		return waiverBlockProof{reason: waiverUnprovenReasonNoRequestBlock}
	}
	for _, u := range confirmers(analysis, e.agreeingResults(analysis, g), e.config.requiredParticipants) {
		blockHash, txs, ok := probeBlock(ctx, u, number)
		if !ok {
			continue
		}
		if !strings.EqualFold(blockHash, hash) || (subject != hash && !containsFold(txs, subject)) {
			return waiverBlockProof{reason: waiverUnprovenReasonBlockContradicted}
		}
		return waiverBlockProof{block: big.NewInt(number)}
	}
	return waiverBlockProof{reason: waiverUnprovenReasonBlockUnconfirmed}
}

// emptyWaiverHasBlock reports whether the empty-outside-retention waiver can
// get a block for req: the request names one, the policy opts the method into
// blockEvidenceFields, or it names a hash and the method's answer names a
// block (respRefs) that confirmAnswerBlock can check.
func (e *executor) emptyWaiverHasBlock(ctx context.Context, req *common.NormalizedRequest) bool {
	if _, ok := common.EvmRequestedBlockNumber(ctx, req); ok {
		return true
	}
	method, err := req.Method()
	if err != nil {
		return false
	}
	if fields, ok := methodFields(e.config.blockEvidenceFields, method); ok && len(fields) > 0 {
		return true
	}
	if requestHash(ctx, req) == "" {
		return false
	}
	mc := common.ResolveMethodConfig(method, req.Network())
	return mc != nil && len(mc.RespRefs) > 0
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

// requestHash is the 32-byte hash (a tx or block hash) a request names as its
// first param, lowercased, or "" when it names none.
func requestHash(ctx context.Context, req *common.NormalizedRequest) string {
	if req == nil {
		return ""
	}
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil || jrq == nil {
		return ""
	}
	jrq.RLock()
	defer jrq.RUnlock()
	if len(jrq.Params) == 0 {
		return ""
	}
	s, ok := jrq.Params[0].(string)
	if !ok || !isHash(s) {
		return ""
	}
	return strings.ToLower(s)
}

// answerBlock reads the block number and hash an answer names at the method's
// respRefs: blockNumber/blockHash for a tx or receipt, number/hash for a block.
// A malformed value returns nothing, which fails the waiver closed.
func answerBlock(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) (int64, string) {
	if req == nil || resp == nil {
		return 0, ""
	}
	method, err := req.Method()
	if err != nil {
		return 0, ""
	}
	mc := common.ResolveMethodConfig(method, req.Network())
	if mc == nil || len(mc.RespRefs) == 0 {
		return 0, ""
	}
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return 0, ""
	}
	var number int64
	var hash string
	for _, path := range mc.RespRefs {
		val, err := jrr.PeekStringByPath(ctx, path...)
		if err != nil {
			continue
		}
		ref, bn, err := common.ParseEvmBlockParam(val)
		if err != nil {
			return 0, ""
		}
		if bn > 0 && number == 0 {
			number = bn
		} else if isHash(ref) && hash == "" {
			hash = ref
		}
	}
	return number, hash
}

// confirmers lists the round's upstreams outside the agreeing set that
// answered without an error, once each, in attempt order: abstainers of an
// opted-in quota first, then the rest.
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
	return out
}

// probeBlock fetches block n with its tx hashes from u the way the state
// poller fetches heads: straight to u under the timeout policy alone, so it
// never re-enters consensus or the network's availability gate. u's configured
// range says where clients are routed, not what u still stores; a node without
// the block answers null and the caller moves on.
func probeBlock(ctx context.Context, u common.Upstream, n int64) (hash string, txs []string, ok bool) {
	pr := common.NewProbeRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%d,"method":"eth_getBlockByNumber","params":["0x%x",false]}`, util.RandomID(), n,
	)))
	resp, err := u.Forward(ctx, pr, true, false)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil || resp == nil {
		return "", nil, false
	}
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil || jrr == nil || jrr.Error != nil || jrr.IsResultEmptyish(ctx) {
		return "", nil, false
	}
	var blk struct {
		Number       string   `json:"number"`
		Hash         string   `json:"hash"`
		Transactions []string `json:"transactions"`
	}
	if err := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &blk); err != nil {
		return "", nil, false
	}
	if bn, err := common.HexToInt64(blk.Number); err != nil || bn != n || !isHash(blk.Hash) {
		return "", nil, false
	}
	return blk.Hash, blk.Transactions, true
}

func isHash(s string) bool {
	if len(s) != 66 || s[0] != '0' || (s[1] != 'x' && s[1] != 'X') {
		return false
	}
	for i := 2; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
