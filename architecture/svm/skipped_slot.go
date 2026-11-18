package svm

import (
	"context"
	"errors"
	"fmt"

	"github.com/erpc/erpc/common"
	"go.opentelemetry.io/otel/attribute"
)

// upstreamPostForward_provenSkippedSlot ends the sweep when the upstream that
// answered a slot as permanently missing can show, from its own finalized
// ledger, that the slot was skipped.
//
// A permanent missing-data verdict for a slot (-32007, -32009) reads "skipped,
// OR not in this node's storage". Only the first half is chain truth, so the
// verdict sweeps every upstream. The second half cannot hold when the chain
// itself says the slot was skipped: every block names its parent slot, so the
// first finalized block after the slot, with a parent below it, proves no
// block exists at that slot on the finalized fork. Every upstream must then
// answer the same verdict, and the error is marked non-retryable toward the
// network: the sweep stops and the fallback tier is not escalated to.
//
// The proof is two reads on the same upstream: getBlocksWithLimit(slot+1, 1)
// and that block's header (getBlock without transactions). Anything short of
// a proof — no next block yet, a parent at or above the slot, a failed read,
// a node with a ledger gap that cannot name the next block — returns the
// verdict untouched, which is the sweep without this hook.
//
// It runs only with svm.probeSkippedSlots on (the reads cost requests), when
// the request is not a consensus participant, the method names one slot
// positionally (RequestLowerBoundSlot, ranges excluded), and the upstream's
// poller has finalized past that slot: below its finalized slot is the only
// place the next block can already be finalized.
func upstreamPostForward_provenSkippedSlot(ctx context.Context, n common.Network, u common.Upstream, r *common.NormalizedRequest, err error) error {
	if err == nil || common.IsConsensusSlot(ctx) || !common.IsPermanentlyMissingData(err) {
		return err
	}
	if n == nil || n.Config() == nil || !n.Config().Svm.ProbeSkippedSlotsEnabled() {
		return err
	}
	if method, merr := r.Method(); merr != nil {
		return err
	} else if _, isRange := rangeSlotMethods[method]; isRange {
		return err
	}
	slot, ok := RequestLowerBoundSlot(ctx, r)
	if !ok {
		return err
	}
	sup, ok := u.(common.SvmUpstream)
	if !ok {
		return err
	}
	poller := sup.SvmStatePoller()
	if poller == nil || poller.IsObjectNull() || poller.FinalizedSlot() <= slot {
		return err
	}
	var me *common.ErrEndpointMissingData
	if !errors.As(err, &me) {
		return err
	}

	ctx, span := common.StartDetailSpan(ctx, "Upstream.PostForward.provenSkippedSlot")
	defer span.End()
	span.SetAttributes(attribute.Int64("slot", slot))

	next, parent, perr := nextFinalizedBlockParent(ctx, u, slot)
	if perr != nil {
		span.SetAttributes(attribute.String("proof_error", perr.Error()))
		return err
	}
	span.SetAttributes(attribute.Int64("next_block", next), attribute.Int64("next_block_parent", parent))
	if parent >= slot {
		return err
	}
	span.SetAttributes(attribute.Bool("skipped_proven", true))
	me.WithRetryableTowardNetwork(false)
	return err
}

// nextFinalizedBlockParent returns the first finalized block after slot on u
// and that block's parent slot.
func nextFinalizedBlockParent(ctx context.Context, u common.Upstream, slot int64) (int64, int64, error) {
	var blocks []int64
	if err := forwardForResult(ctx, u, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"getBlocksWithLimit","params":[%d,1,{"commitment":"finalized"}]}`, slot+1,
	), &blocks); err != nil {
		return 0, 0, err
	}
	if len(blocks) == 0 || blocks[0] <= slot {
		return 0, 0, fmt.Errorf("no finalized block after slot %d", slot)
	}
	var header struct {
		ParentSlot *int64 `json:"parentSlot"`
	}
	if err := forwardForResult(ctx, u, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[%d,{"commitment":"finalized","encoding":"json","transactionDetails":"none","rewards":false,"maxSupportedTransactionVersion":0}]}`, blocks[0],
	), &header); err != nil {
		return 0, 0, err
	}
	if header.ParentSlot == nil || *header.ParentSlot >= blocks[0] {
		return 0, 0, fmt.Errorf("block %d has no usable parentSlot", blocks[0])
	}
	return blocks[0], *header.ParentSlot, nil
}

// forwardForResult sends body to u and decodes the JSON-RPC result into out.
func forwardForResult(ctx context.Context, u common.Upstream, body string, out interface{}) error {
	resp, err := u.Forward(ctx, common.NewNormalizedRequest([]byte(body)), true, false)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil {
		return err
	}
	if resp == nil {
		return errors.New("no response")
	}
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil {
		return err
	}
	if jrr == nil {
		return errors.New("no JSON-RPC response")
	}
	if jrr.Error != nil {
		return jrr.Error
	}
	return common.SonicCfg.Unmarshal(jrr.GetResultBytes(), out)
}
