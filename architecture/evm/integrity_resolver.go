package evm

import (
	"context"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
)

// integrityResolver implements integrity.Resolver over the real network and the
// serving upstream. Finality comes from the upstream's effective finalized block;
// canonical block data is served through the per-network ChainView (fetch-once,
// deduped, reorg-aware), which itself goes through the trusted network path
// (cache-backed, inheriting whatever failsafe — e.g. consensus — the network is
// configured with) and marks fetches internal so they don't recurse into the engine.
type integrityResolver struct {
	network  common.Network
	upstream common.Upstream
	view     *chainView
}

func newIntegrityResolver(ctx context.Context, n common.Network, u common.Upstream, selector string) integrity.Resolver {
	if n == nil {
		return nil
	}
	return &integrityResolver{network: n, upstream: u, view: groupChainView(ctx, n, selector)}
}

func (r *integrityResolver) IsFinalized(ctx context.Context, blockNumber int64) (bool, bool) {
	if blockNumber < 0 {
		return false, false
	}
	eu, ok := r.upstream.(common.EvmUpstream)
	if !ok {
		return false, false
	}
	fin := eu.EvmEffectiveFinalizedBlock()
	if fin <= 0 {
		return false, false // finality unknown — caller treats as unfinalized
	}
	return blockNumber <= fin, true
}

// CanonicalReceipts serves a block's receipts from the ChainView: a hit returns the
// cached receipts with no fetch; a miss fetches them once (by hash, deduped) and
// caches. Callers pass a block HASH so the corroboration is race-free.
func (r *integrityResolver) CanonicalReceipts(ctx context.Context, blockRef string) ([]integrity.Receipt, bool) {
	if r.view == nil {
		return nil, false
	}
	return r.view.receiptsByHash(ctx, blockRef)
}

// CanonicalHeader serves a header from the ChainView (deduped + pinned). A 32-byte
// hash (0x + 64 hex) resolves by hash; a hex number resolves through the pin; a tag
// resolves directly.
func (r *integrityResolver) CanonicalHeader(ctx context.Context, blockRef string) (*integrity.Header, bool) {
	if r.view == nil {
		return nil, false
	}
	if len(blockRef) == 66 {
		return r.view.headerByHash(ctx, blockRef)
	}
	if n, err := common.HexToInt64(blockRef); err == nil {
		return r.view.headerByNumber(ctx, n, blockRef)
	}
	return r.view.resolveHeader(ctx, "eth_getBlockByNumber", blockRef)
}
