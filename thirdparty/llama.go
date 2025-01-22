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

var llamaNetworkNames = map[int64]string{
	1:     "eth",
	42161: "arbitrum",
	8453:  "base",
	56:    "binance",
	10:    "optimism",
	137:   "polygon",
}

type LlamaVendor struct {
	common.Vendor
}

func CreateLlamaVendor() common.Vendor {
	return &LlamaVendor{}
}

func (v *LlamaVendor) Name() string {
	return "llama"
}

func (v *LlamaVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainId, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}
	_, ok := llamaNetworkNames[chainId]
	return ok, nil
}

func (v *LlamaVendor) PrepareConfig(upstream *common.UpstreamConfig, settings common.VendorSettings) error {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}
	if upstream.Endpoint == "" {
		if apiKey, ok := settings["apiKey"].(string); ok && apiKey != "" {
			chainID := upstream.Evm.ChainId
			if chainID == 0 {
				return fmt.Errorf("llama vendor requires upstream.evm.chainId to be defined")
			}
			netName, ok := llamaNetworkNames[chainID]
			if !ok {
				return fmt.Errorf("unsupported network chain ID for Llama: %d", chainID)
			}
			upstream.Endpoint = fmt.Sprintf("https://%s.llamarpc.com/%s", netName, apiKey)
			upstream.Type = common.UpstreamTypeEvm
		} else {
			return fmt.Errorf("apiKey is required in llama settings")
		}
	}

	return nil
}

func (v *LlamaVendor) GetVendorSpecificErrorIfAny(resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	bodyMap, ok := jrr.(*common.JsonRpcResponse)
	if !ok {
		return nil
	}

	err := bodyMap.Error
	code := err.Code
	msg := err.Message
	if err.Data != "" {
		details["data"] = err.Data
	}

	if strings.Contains(msg, "code: 1015") {
		return common.NewErrEndpointCapacityExceeded(
			common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorCapacityExceeded, msg, nil, details),
		)
	}

	// Other errors can be properly handled by generic error handling
	return nil
}

func (v *LlamaVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	return strings.Contains(ups.Endpoint, ".llamarpc.com")
}
