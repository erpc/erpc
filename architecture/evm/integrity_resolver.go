package evm

import (
	"context"
	"fmt"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
)

// integrityResolver implements integrity.Resolver over the real network and the
// serving upstream. Finality comes from the upstream's effective finalized
// block; canonical block data is force-fetched through the network (cache-backed,
// inheriting whatever failsafe — e.g. consensus — the network is configured
// with) and marked internal so it does not recurse back into the engine.
type integrityResolver struct {
	network  common.Network
	upstream common.Upstream
	view     *chainView
}

func newIntegrityResolver(n common.Network, u common.Upstream) integrity.Resolver {
	if n == nil {
		return nil
	}
	return &integrityResolver{network: n, upstream: u, view: networkChainView(n)}
}

// emitAux records an auxiliary (force-fetch) request the integrity engine made
// while validating r.upstream's response — these are NOT part of the user's
// request. ok reflects whether the force-fetch returned a usable response.
func (r *integrityResolver) emitAux(kind string, ok bool) {
	outcome := "error"
	if ok {
		outcome = "ok"
	}
	var vendor, ups string
	if r.upstream != nil {
		vendor, ups = r.upstream.VendorName(), r.upstream.Id()
	}
	telemetry.MetricIntegrityAuxRequest.WithLabelValues(
		r.network.ProjectId(), vendor, r.network.Label(), ups, kind, outcome,
	).Inc()
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

func (r *integrityResolver) CanonicalReceipts(ctx context.Context, blockRef string) ([]integrity.Receipt, bool) {
	req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["%s"]}`, blockRef)))
	req.SetDirectives(&common.RequestDirectives{IsInternal: true})
	req.SetNetwork(r.network)

	resp, err := r.network.Forward(ctx, req)
	r.emitAux("canonical_receipts", err == nil && resp != nil)
	if err != nil || resp == nil {
		return nil, false
	}
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return nil, false
	}
	var receipts []integrity.Receipt
	if err := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &receipts); err != nil {
		return nil, false
	}
	return receipts, true
}

func (r *integrityResolver) CanonicalHeader(ctx context.Context, blockRef string) (*integrity.Header, bool) {
	// Served from the per-network ChainView: a hit returns the committed header
	// with no fetch; a miss fetches once (deduped) and pins it. A 32-byte hash
	// (0x + 64 hex) resolves by hash; a hex number resolves through the pin; a tag
	// resolves directly.
	if r.view == nil {
		return nil, false
	}
	if len(blockRef) == 66 {
		return r.view.headerByHash(ctx, blockRef)
	}
	if n, err := common.HexToInt64(blockRef); err == nil {
		return r.view.headerByNumber(ctx, n, blockRef)
	}
	return r.view.resolve(ctx, "eth_getBlockByNumber", blockRef)
}
