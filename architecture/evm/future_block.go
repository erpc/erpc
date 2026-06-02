package evm

import (
	"context"

	"github.com/erpc/erpc/common"
)

// EmptyResultTruthfulForFutureBlock reports whether an empty/null result for the
// given request should be accepted as the truthful answer rather than treated as
// missing data worth retrying across upstreams.
//
// It returns true only when all of the following hold:
//
//   - the network configures a future-block distance bound (opt-in; nil disables it),
//   - the request targets a concrete numeric block (tags and block hashes are excluded),
//   - the network's highest known latest block is available, and
//   - the requested block lies further than the configured distance beyond that head.
//
// A block beyond the head by more than the bound has almost certainly not been
// produced yet, so an empty response is the correct answer (e.g. a null block) and
// retrying it only burns latency until the block lands or the request times out.
//
// In every other case it returns false so the caller keeps the existing
// retry-on-empty behavior. Notably it fails open when the head is unknown (e.g. a
// cold state poller), so a temporarily-missing head never causes a real block to be
// reported as empty.
func EmptyResultTruthfulForFutureBlock(ctx context.Context, n common.Network, rq *common.NormalizedRequest) bool {
	if n == nil || rq == nil {
		return false
	}
	cfg := n.Config()
	if cfg == nil || cfg.Evm == nil || cfg.Evm.MaxFutureBlockRetryDistance == nil {
		return false
	}
	distance := *cfg.Evm.MaxFutureBlockRetryDistance
	if distance < 0 {
		return false
	}

	bn := requestedBlockNumber(ctx, rq)
	if bn <= 0 {
		return false
	}

	head := n.EvmHighestLatestBlockNumber(ctx)
	if head <= 0 {
		// Head unknown — fail open to existing behavior.
		return false
	}

	return bn > head+distance
}

// requestedBlockNumber resolves the concrete numeric block a request targets, or 0
// when the request uses a tag (latest/pending/finalized/safe/earliest), a block
// hash, or carries no block parameter. It prefers the value cached during request
// normalization and falls back to extracting it from the raw params.
func requestedBlockNumber(ctx context.Context, rq *common.NormalizedRequest) int64 {
	if v := rq.EvmBlockNumber(); v != nil {
		if bn, ok := v.(int64); ok && bn > 0 {
			return bn
		}
	}
	if _, bn, err := ExtractBlockReferenceFromRequest(ctx, rq); err == nil && bn > 0 {
		return bn
	}
	return 0
}
