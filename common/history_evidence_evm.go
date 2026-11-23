package common

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/erpc/erpc/util"
)

// Registered here, not in architecture/evm: every EVM input lives in this
// package, and the consensus package (which architecture/evm imports) gets
// EVM evidence without an architecture import.
func init() {
	RegisterHistoryEvidence(ArchitectureEvm, evmHistoryEvidence{})
}

type evmHistoryEvidence struct{}

func (evmHistoryEvidence) RequestedBlock(ctx context.Context, req *NormalizedRequest) (int64, bool) {
	return EvmRequestedBlockNumber(ctx, req)
}

// CorroboratedHead trusts the request's Head only under servedTip.quorum
// "majority": the default quorum (2) can still be one voter's report on a
// small pool.
func (evmHistoryEvidence) CorroboratedHead(ctx context.Context, req *NormalizedRequest) int64 {
	net := req.Network()
	if net == nil {
		return 0
	}
	cfg := net.Config()
	if cfg == nil || cfg.Evm == nil || cfg.Evm.ServedTip == nil || cfg.Evm.ServedTip.Quorum != ServedTipQuorumMajority {
		return 0
	}
	return EvmHeadOf(net, ctx, req).Latest
}

func (evmHistoryEvidence) UpstreamLatestBlock(u Upstream) int64 {
	eu, ok := u.(EvmUpstream)
	if !ok {
		return 0
	}
	return eu.EvmEffectiveLatestBlock()
}

func (evmHistoryEvidence) UpstreamLowerBound(u Upstream) int64 {
	eu, ok := u.(EvmUpstream)
	if !ok {
		return math.MinInt64
	}
	lower, _ := eu.EvmBlockAvailabilityBounds()
	return lower
}

// LookupSubject is the 32-byte hash the request names as its first param,
// lowercased, when the method's respRefs name the block of the answer.
func (evmHistoryEvidence) LookupSubject(ctx context.Context, req *NormalizedRequest) string {
	method, err := req.Method()
	if err != nil {
		return ""
	}
	if mc := ResolveMethodConfig(method, req.Network()); mc == nil || len(mc.RespRefs) == 0 {
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
	if !ok || !isEvmHash(s) {
		return ""
	}
	return strings.ToLower(s)
}

// AnswerBlock reads the block number and hash at the method's respRefs:
// blockNumber/blockHash for a tx or receipt, number/hash for a block. It needs
// both; a malformed value returns nothing.
func (evmHistoryEvidence) AnswerBlock(ctx context.Context, req *NormalizedRequest, resp *NormalizedResponse) (int64, string) {
	if resp == nil {
		return 0, ""
	}
	method, err := req.Method()
	if err != nil {
		return 0, ""
	}
	mc := ResolveMethodConfig(method, req.Network())
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
		ref, bn, err := ParseEvmBlockParam(val)
		if err != nil {
			return 0, ""
		}
		if bn > 0 && number == 0 {
			number = bn
		} else if isEvmHash(ref) && hash == "" {
			hash = strings.ToLower(ref)
		}
	}
	if number <= 0 || hash == "" {
		return 0, ""
	}
	return number, hash
}

// ProbeBlock fetches block n with its tx hashes the way the state poller
// fetches heads. u's configured range says where clients are routed, not what
// u still stores; a node without the block answers null.
func (evmHistoryEvidence) ProbeBlock(ctx context.Context, u Upstream, n int64) (string, []string, bool) {
	pr := NewProbeRequest([]byte(fmt.Sprintf(
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
	if err := SonicCfg.Unmarshal(jrr.GetResultBytes(), &blk); err != nil {
		return "", nil, false
	}
	if bn, err := HexToInt64(blk.Number); err != nil || bn != n || !isEvmHash(blk.Hash) {
		return "", nil, false
	}
	for i, tx := range blk.Transactions {
		blk.Transactions[i] = strings.ToLower(tx)
	}
	return strings.ToLower(blk.Hash), blk.Transactions, true
}

func isEvmHash(s string) bool {
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
