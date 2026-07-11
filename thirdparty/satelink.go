package thirdparty

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

var satelinkNetworkNames = map[int64]string{
	137:   "polygon",
	80002: "amoy",
}

type SatelinkVendor struct {
	common.Vendor
}

func CreateSatelinkVendor() common.Vendor {
	return &SatelinkVendor{}
}

func (v *SatelinkVendor) Name() string {
	return "satelink"
}

func (v *SatelinkVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}
	_, ok := satelinkNetworkNames[chainID]
	return ok, nil
}

func (v *SatelinkVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint == "" {
		apiKey, ok := settings["apiKey"].(string)
		if !ok || apiKey == "" {
			// Self-service keys: POST https://rpc.satelink.network/v1/machine/register
			// with {"mode":"instant"} returns a key in one call (no signup).
			return nil, fmt.Errorf("apiKey is required in satelink settings")
		}
		if upstream.Evm == nil {
			return nil, fmt.Errorf("satelink vendor requires upstream.evm to be defined")
		}
		chainID := upstream.Evm.ChainId
		if chainID == 0 {
			return nil, fmt.Errorf("satelink vendor requires upstream.evm.chainId to be defined")
		}
		netName, ok := satelinkNetworkNames[chainID]
		if !ok {
			return nil, fmt.Errorf("unsupported network chain ID for Satelink: %d", chainID)
		}

		upstream.Endpoint = fmt.Sprintf("https://rpc.satelink.network/rpc/%s", netName)
		upstream.Type = common.UpstreamTypeEvm

		// Satelink authenticates via the X-API-Key header (same pattern as
		// the blockdaemon vendor's Authorization header).
		if upstream.JsonRpc.Headers == nil {
			upstream.JsonRpc.Headers = make(map[string]string)
		}
		if upstream.JsonRpc.Headers["X-API-Key"] == "" && upstream.JsonRpc.Headers["x-api-key"] == "" {
			upstream.JsonRpc.Headers["X-API-Key"] = apiKey
		}
	}

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *SatelinkVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	bodyMap, ok := jrr.(*common.JsonRpcResponse)
	if !ok {
		return nil
	}

	err := bodyMap.Error
	if err.Data != "" {
		details["data"] = err.Data
	}

	return nil
}

func (v *SatelinkVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "satelink://") || strings.HasPrefix(ups.Endpoint, "evm+satelink://") {
		return true
	}

	return strings.Contains(ups.Endpoint, "rpc.satelink.network")
}
