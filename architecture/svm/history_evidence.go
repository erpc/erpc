package svm

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

func init() {
	common.RegisterHistoryEvidence(common.ArchitectureSvm, svmHistoryEvidence{})
}

// svmHistoryEvidence is the SVM side of common.HistoryEvidence. Its block is
// the slot.
type svmHistoryEvidence struct{}

// RequestedBlock is the slot a positional read names (getBlock, getBlockTime)
// or the end slot getBlocks names. getBlocksWithLimit and a getBlocks without
// an end run to the tip, so they name no highest slot.
func (svmHistoryEvidence) RequestedBlock(ctx context.Context, req *common.NormalizedRequest) (int64, bool) {
	start, ok := RequestLowerBoundSlot(ctx, req)
	if !ok {
		return 0, false
	}
	method, _ := req.Method()
	if _, isRange := rangeSlotMethods[method]; !isRange {
		return start, true
	}
	if method != "getBlocks" {
		return 0, false
	}
	rpcReq, err := req.JsonRpcRequest(ctx)
	if err != nil || rpcReq == nil {
		return 0, false
	}
	rpcReq.RLock()
	defer rpcReq.RUnlock()
	end, present, valid := getBlocksEndParam(rpcReq.Params)
	if !present || !valid {
		return 0, false
	}
	return end, true
}

// CorroboratedHead is the network's majority latest slot: SvmHighestLatestSlot
// always picks with a strict-majority quorum.
func (svmHistoryEvidence) CorroboratedHead(ctx context.Context, req *common.NormalizedRequest) int64 {
	sn, ok := req.Network().(common.SvmNetwork)
	if !ok {
		return 0
	}
	return sn.SvmHighestLatestSlot(ctx)
}

func (svmHistoryEvidence) UpstreamLatestBlock(u common.Upstream) int64 {
	su, ok := u.(common.SvmUpstream)
	if !ok {
		return 0
	}
	poller := su.SvmStatePoller()
	if poller == nil || poller.IsObjectNull() {
		return 0
	}
	latest := poller.LatestSlot()
	if latest <= 0 {
		return 0
	}
	if _, upper := ResolveDeclaredSlotBounds(u); upper < latest {
		return upper
	}
	return latest
}

func (svmHistoryEvidence) UpstreamLowerBound(u common.Upstream) int64 {
	lower, _ := ResolveDeclaredSlotBounds(u)
	return lower
}

// LookupSubject is the signature getTransaction names. Its answer names the
// slot that holds it.
func (svmHistoryEvidence) LookupSubject(ctx context.Context, req *common.NormalizedRequest) string {
	method, err := req.Method()
	if err != nil || method != "getTransaction" {
		return ""
	}
	rpcReq, err := req.JsonRpcRequest(ctx)
	if err != nil || rpcReq == nil {
		return ""
	}
	rpcReq.RLock()
	defer rpcReq.RUnlock()
	if len(rpcReq.Params) == 0 {
		return ""
	}
	sig, ok := rpcReq.Params[0].(string)
	if !ok || base58Len(sig) != 64 {
		return ""
	}
	return sig
}

// AnswerBlock is the slot a getTransaction answer names. The answer carries
// no id of that block (its recentBlockhash is an older block's), so the
// confirmer checks the signature against the block alone.
func (svmHistoryEvidence) AnswerBlock(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) (int64, string) {
	if resp == nil {
		return 0, ""
	}
	if method, err := req.Method(); err != nil || method != "getTransaction" {
		return 0, ""
	}
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil || jrr == nil || jrr.Error != nil {
		return 0, ""
	}
	raw, err := jrr.PeekBytesByPath(ctx, "slot")
	if err != nil {
		return 0, ""
	}
	slot, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || slot <= 0 {
		return 0, ""
	}
	return slot, ""
}

// ProbeBlock fetches slot n's block with its signatures only, at the node's
// default finalized commitment. A pruned node answers -32001 / -32009 and a
// skipped slot null; both leave ok false.
func (svmHistoryEvidence) ProbeBlock(ctx context.Context, u common.Upstream, n int64) (string, []string, bool) {
	pr := common.NewProbeRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%d,"method":"getBlock","params":[%d,{"encoding":"json","transactionDetails":"signatures","rewards":false,"maxSupportedTransactionVersion":0}]}`,
		util.RandomID(), n,
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
		Blockhash  string   `json:"blockhash"`
		Signatures []string `json:"signatures"`
	}
	if err := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &blk); err != nil || base58Len(blk.Blockhash) != 32 {
		return "", nil, false
	}
	return blk.Blockhash, blk.Signatures, true
}

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// base58Len is the byte length s decodes to, or -1 when s is not base58.
// Bounded at 90 characters: more than any 64-byte value encodes to, and
// 58^90 fits the 66-byte buffer.
func base58Len(s string) int {
	if s == "" || len(s) > 90 {
		return -1
	}
	var b [66]byte // little-endian magnitude
	size, zeros := 0, 0
	leading := true
	for i := range len(s) {
		d := strings.IndexByte(base58Alphabet, s[i])
		if d < 0 {
			return -1
		}
		if leading && d == 0 {
			zeros++
			continue
		}
		leading = false
		carry := d
		for k := range size {
			carry += int(b[k]) * 58
			b[k] = byte(carry)
			carry >>= 8
		}
		for ; carry > 0; carry >>= 8 {
			b[size] = byte(carry)
			size++
		}
	}
	return zeros + size
}
