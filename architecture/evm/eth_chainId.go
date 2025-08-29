package evm

import (
	"context"
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func BuildEthChainIdRequest() (*common.JsonRpcRequest, error) {
	jrq := common.NewJsonRpcRequest("eth_chainId", []interface{}{})
	err := jrq.SetID(util.RandomID())
	if err != nil {
		return nil, err
	}

	return jrq, nil
}

func projectPreForward_eth_chainId(ctx context.Context, n common.Network, r *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	if r.Directives().SkipCacheRead {
		return false, nil, nil // Proceed to actual upstream call
	}

	ctx, span := common.StartDetailSpan(ctx, "Project.PreForwardHook.eth_chainId", trace.WithAttributes(
		attribute.String("request.id", fmt.Sprintf("%v", r.ID())),
		attribute.String("network.id", n.Id()),
	))
	defer span.End()

	netCfg := n.Config()
	if netCfg == nil || netCfg.Evm == nil || netCfg.Evm.ChainId == 0 {
		span.SetAttributes(attribute.Bool("skipped.no_chain_id_config", true))
		return false, nil, nil
	}

	chainId := netCfg.Evm.ChainId
	hexChainId, err := common.NormalizeHex(chainId)
	if err != nil {
		span.SetAttributes(attribute.String("error.normalize_hex", err.Error()))
		return true, nil, fmt.Errorf("failed to normalize chainId %d to hex: %w", chainId, err)
	}

	jrqID := r.ID()
	// If original request ID is nil (e.g. for some internal requests), generate one.
	if jrqID == nil {
		jrqID = util.RandomID()
	}

	jrr, err := common.NewJsonRpcResponse(jrqID, hexChainId, nil)
	if err != nil {
		span.SetAttributes(attribute.String("error.new_jsonrpc_response", err.Error()))
		return true, nil, fmt.Errorf("failed to create JsonRpcResponse: %w", err)
	}

	nr := common.NewNormalizedResponse().
		WithRequest(r).
		WithJsonRpcResponse(jrr)

	span.SetAttributes(attribute.Bool("handled_by_hook", true), attribute.String("chain_id_from_config", hexChainId))
	return true, nr, nil
}
