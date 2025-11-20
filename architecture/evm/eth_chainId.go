package evm

import (
	"context"
	"fmt"
	"strconv"
	"strings"

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

// networkPreForward_eth_chainId short-circuits eth_chainId at network level using the configured chainId
// when skip-cache-read is not requested. This applies after upstream selection but before sending.
func networkPreForward_eth_chainId(ctx context.Context, n common.Network, _ []common.Upstream, r *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	if r == nil || n == nil {
		return false, nil, nil
	}
	if r.Directives().SkipCacheRead {
		return false, nil, nil
	}

	ctx, span := common.StartDetailSpan(ctx, "Network.PreForwardHook.eth_chainId", trace.WithAttributes(
		attribute.String("request.id", fmt.Sprintf("%v", r.ID())),
		attribute.String("network.id", n.Id()),
	))
	defer span.End()

	netCfg := n.Config()
	if netCfg == nil || netCfg.Evm == nil || netCfg.Evm.ChainId == 0 {
		span.SetAttributes(attribute.Bool("skipped.no_chain_id_config", true))
		return false, nil, nil
	}

	hexChainId, err := common.NormalizeHex(netCfg.Evm.ChainId)
	if err != nil {
		span.SetAttributes(attribute.String("error.normalize_hex", err.Error()))
		return true, nil, fmt.Errorf("failed to normalize chainId %d to hex: %w", netCfg.Evm.ChainId, err)
	}

	jrqID := r.ID()
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

// upstreamPreForward_eth_chainId short-circuits eth_chainId at upstream level using upstream or network config.
// Respects skip-cache-read directive.
func upstreamPreForward_eth_chainId(ctx context.Context, n common.Network, u common.Upstream, r *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	if r == nil || n == nil || u == nil {
		return false, nil, nil
	}
	if r.Directives().SkipCacheRead {
		return false, nil, nil
	}

	ctx, span := common.StartDetailSpan(ctx, "Upstream.PreForwardHook.eth_chainId", trace.WithAttributes(
		attribute.String("request.id", fmt.Sprintf("%v", r.ID())),
		attribute.String("network.id", n.Id()),
		attribute.String("upstream.id", u.Id()),
	))
	defer span.End()

	var chainId int64
	// Prefer upstream's resolved config (detectFeatures fills this when possible)
	if uc := u.Config(); uc != nil && uc.Evm != nil && uc.Evm.ChainId > 0 {
		chainId = uc.Evm.ChainId
	}
	// If still empty, try parsing from upstream NetworkId, e.g. "evm:123"
	if chainId == 0 {
		if nid := u.NetworkId(); strings.HasPrefix(nid, "evm:") {
			if p := strings.TrimPrefix(nid, "evm:"); p != "" {
				if v, perr := strconv.ParseInt(p, 10, 64); perr == nil && v > 0 {
					chainId = v
				}
			}
		}
	}
	// Finally, fallback to network-level config
	if chainId == 0 {
		if nc := n.Config(); nc != nil && nc.Evm != nil && nc.Evm.ChainId > 0 {
			chainId = nc.Evm.ChainId
		}
	}
	if chainId == 0 {
		span.SetAttributes(attribute.Bool("skipped.no_chain_id_config", true))
		return false, nil, nil
	}

	hexChainId, err := common.NormalizeHex(chainId)
	if err != nil {
		span.SetAttributes(attribute.String("error.normalize_hex", err.Error()))
		return true, nil, fmt.Errorf("failed to normalize chainId %d to hex: %w", chainId, err)
	}

	jrqID := r.ID()
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
