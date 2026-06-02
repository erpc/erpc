package evm

import (
	"context"
	"strings"

	"github.com/erpc/erpc/common"
)

// EmptyResultTruthfulForFutureBlock reports whether an empty/null result for the
// given request should be accepted as the truthful answer rather than treated as
// missing data worth retrying across upstreams.
//
// It returns true only when all of the following hold:
//
//   - the request method is one where an empty result is treated as missing data
//     (i.e. listed in EvmNetworkConfig.MarkEmptyAsErrorMethods),
//   - the request targets a concrete numeric block (tags and block hashes are excluded),
//   - the network's highest known latest block is available, and
//   - the requested block lies further than the configured distance beyond that head.
//
// The distance bound is EvmNetworkConfig.MaxFutureBlockRetryDistance; when unset it
// falls back to common.DefaultMaxFutureBlockRetryDistance, so the behavior is on for
// all EVM networks by default. A negative bound disables the check; a sufficiently
// large bound effectively disables it too (no real request reaches that far ahead).
//
// A block beyond the head by more than the bound has almost certainly not been
// produced yet, so an empty response is the correct answer (e.g. a null block) and
// retrying it only burns latency until the block lands or the request times out.
//
// It fails open (returns false) for non-eligible methods, non-numeric block
// references, and when the head is unknown (e.g. a cold state poller), so a
// temporarily-missing head never causes a real block to be reported as empty.
func EmptyResultTruthfulForFutureBlock(ctx context.Context, n common.Network, rq *common.NormalizedRequest) bool {
	if n == nil || rq == nil {
		return false
	}
	cfg := n.Config()
	if cfg == nil || cfg.Evm == nil {
		return false
	}

	// Only methods where an empty result means "missing data" are eligible; this
	// keeps the hook and the retry decision scoped to the same set of methods.
	method, err := rq.Method()
	if err != nil || !isMethodInMarkEmptyList(n, strings.ToLower(method)) {
		return false
	}

	distance := common.DefaultMaxFutureBlockRetryDistance
	if cfg.Evm.MaxFutureBlockRetryDistance != nil {
		distance = *cfg.Evm.MaxFutureBlockRetryDistance
	}
	if distance < 0 {
		// Negative bound explicitly disables the check.
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
