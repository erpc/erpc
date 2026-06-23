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
}

func newIntegrityResolver(n common.Network, u common.Upstream) integrity.Resolver {
	if n == nil {
		return nil
	}
	return &integrityResolver{network: n, upstream: u}
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
	// A 32-byte hash reference (0x + 64 hex) resolves by hash; otherwise treat
	// blockRef as a block number/tag.
	method := "eth_getBlockByNumber"
	if len(blockRef) == 66 {
		method = "eth_getBlockByHash"
	}
	req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"%s","params":["%s",false]}`, method, blockRef)))
	req.SetDirectives(&common.RequestDirectives{IsInternal: true})
	req.SetNetwork(r.network)

	resp, err := r.network.Forward(ctx, req)
	r.emitAux("canonical_header", err == nil && resp != nil)
	if err != nil || resp == nil {
		return nil, false
	}
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return nil, false
	}
	var h integrity.Header
	if err := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &h); err != nil {
		return nil, false
	}
	return &h, true
}
