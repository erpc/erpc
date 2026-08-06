package thirdparty

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

var solidrpcChainIDs = map[int64]struct{}{
	1: {}, 10: {}, 50: {}, 56: {}, 88: {}, 97: {}, 100: {}, 130: {}, 137: {}, 143: {}, 146: {},
	169: {}, 204: {}, 288: {}, 369: {}, 480: {}, 545: {}, 747: {}, 943: {}, 988: {}, 1135: {},
	1301: {}, 2020: {}, 4202: {}, 4217: {}, 4326: {}, 4663: {}, 4689: {}, 5000: {}, 5003: {}, 5611: {},
	7000: {}, 8453: {}, 10200: {}, 10143: {}, 13371: {}, 42161: {}, 42220: {}, 43114: {}, 57073: {}, 59144: {},
	80002: {}, 80069: {}, 80094: {}, 81457: {}, 84532: {}, 88882: {}, 88888: {}, 11155111: {}, 11155420: {},
	202601: {}, 421614: {}, 534352: {}, 7777777: {}, 999999999: {},
}

type SolidrpcVendor struct {
	common.Vendor
}

func CreateSolidrpcVendor() common.Vendor {
	return &SolidrpcVendor{}
}

func (v *SolidrpcVendor) Name() string {
	return "solidrpc"
}

func (v *SolidrpcVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}
	_, ok := solidrpcChainIDs[chainID]
	return ok, nil
}

func (v *SolidrpcVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint != "" {
		return []*common.UpstreamConfig{upstream}, nil
	}

	apiKey, ok := settings["apiKey"].(string)
	if !ok || apiKey == "" {
		return nil, fmt.Errorf("apiKey is required in solidrpc provider settings")
	}
	if upstream.Evm == nil {
		return nil, fmt.Errorf("solidrpc vendor requires upstream.evm to be defined")
	}
	chainID := upstream.Evm.ChainId
	if chainID == 0 {
		return nil, fmt.Errorf("solidrpc vendor requires upstream.evm.chainId to be defined")
	}
	if _, ok := solidrpcChainIDs[chainID]; !ok {
		return nil, fmt.Errorf("unsupported network chain ID for SolidRPC: %d", chainID)
	}

	endpointURL := &url.URL{
		Scheme: "https",
		Host:   "rpc.solidrpc.io",
		Path:   fmt.Sprintf("/%s/evm/%d", url.PathEscape(apiKey), chainID),
	}
	upstream.Endpoint = endpointURL.String()
	upstream.Type = common.UpstreamTypeEvm

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *SolidrpcVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	body, ok := jrr.(*common.JsonRpcResponse)
	if !ok || body.Error == nil || body.Error.Code != -32005 {
		return nil
	}
	if body.Error.Data != "" {
		details["data"] = body.Error.Data
	}
	return common.NewErrEndpointCapacityExceeded(
		common.NewErrJsonRpcExceptionInternal(
			body.Error.Code,
			common.JsonRpcErrorCapacityExceeded,
			body.Error.Message,
			nil,
			details,
		),
	)
}

func (v *SolidrpcVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "solidrpc://") || strings.HasPrefix(ups.Endpoint, "evm+solidrpc://") {
		return true
	}
	return strings.Contains(ups.Endpoint, "rpc.solidrpc.io")
}
