package evm

import (
	"context"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
)

// integrityResolver implements integrity.Resolver over the real network and the
// serving upstream. Finality is the request's Head.Finalized (spec §6.4);
// canonical block data comes from the group's chain tracker, which fetches a
// miss through the scheduler (deduped, reorg-aware). held: the response under
// check is eRPC's own data (a tracker fetch, a source header), so the
// resolver reads only what the tracker holds and never fetches (spec §7).
type integrityResolver struct {
	network  common.Network
	upstream common.Upstream
	view     *ChainTracker
	held     bool
}

func newIntegrityResolver(n common.Network, u common.Upstream, view *ChainTracker, held bool) integrity.Resolver {
	if n == nil {
		return nil
	}
	return &integrityResolver{network: n, upstream: u, view: view, held: held}
}

func (r *integrityResolver) IsFinalized(ctx context.Context, blockNumber int64) (bool, bool) {
	if blockNumber < 0 {
		return false, false
	}
	fin := common.EvmHeadOf(r.network, ctx, nil).Finalized
	if fin <= 0 {
		return false, false // finality unknown — caller treats as unfinalized
	}
	return blockNumber <= fin, true
}

// CanonicalReceipts serves a block's receipts from the tracker: a hit returns
// the held receipts with no fetch; a miss fetches them once (by hash,
// deduped), unless held. Callers pass a block HASH so the corroboration is
// race-free.
func (r *integrityResolver) CanonicalReceipts(ctx context.Context, blockHash string, blockNumber int64) ([]integrity.Receipt, bool) {
	if r.view == nil {
		return nil, false
	}
	if r.held {
		return r.view.CachedReceipts(blockHash)
	}
	return r.view.receiptsByHash(ctx, blockHash, blockNumber)
}

// CanonicalHeader serves a header from the tracker. A 32-byte hash (0x + 64
// hex) resolves by hash; a hex number resolves through the pin; a tag
// resolves directly. Held: only held headers, and no tag.
func (r *integrityResolver) CanonicalHeader(ctx context.Context, blockRef string) (*integrity.Header, bool) {
	if r.view == nil {
		return nil, false
	}
	if len(blockRef) == 66 {
		if r.held {
			return r.view.HeaderByHash(blockRef)
		}
		return r.view.headerByHash(ctx, blockRef)
	}
	if n, err := common.HexToInt64(blockRef); err == nil {
		if r.held {
			return r.view.HeaderAt(n)
		}
		return r.view.headerByNumber(ctx, n, blockRef)
	}
	if r.held {
		return nil, false
	}
	return r.view.resolveTag(ctx, blockRef)
}
