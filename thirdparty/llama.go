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

func (v *LlamaVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}
	if upstream.Endpoint == "" {
		if apiKey, ok := settings["apiKey"].(string); ok && apiKey != "" {
			chainID := upstream.Evm.ChainId
			if chainID == 0 {
				return nil, fmt.Errorf("llama vendor requires upstream.evm.chainId to be defined")
			}
			netName, ok := llamaNetworkNames[chainID]
			if !ok {
				return nil, fmt.Errorf("unsupported network chain ID for Llama: %d", chainID)
			}
			upstream.Endpoint = fmt.Sprintf("https://%s.llamarpc.com/%s", netName, apiKey)
			upstream.Type = common.UpstreamTypeEvm
		} else {
			return nil, fmt.Errorf("apiKey is required in llama settings")
		}
	}

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *LlamaVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
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

	if strings.Contains(msg, "code: 1015") || isCloudflare1015Body(code, err.Data) {
		return common.NewErrEndpointCapacityExceeded(
			common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorCapacityExceeded, msg, nil, details),
		)
	}

	// Other errors can be properly handled by generic error handling
	return nil
}

// isCloudflare1015Body reports a non-JSON Cloudflare rate-limit page
// ("error code: 1015"). Such bodies fail JSON parsing, so the raw body is only
// available as the parse exception's data. Any other unparseable body keeps its
// generic classification.
func isCloudflare1015Body(code int, data interface{}) bool {
	if code != int(common.JsonRpcErrorParseException) {
		return false
	}
	s, ok := data.(string)
	return ok && strings.HasPrefix(strings.TrimSpace(s), "error code: 1015")
}

func (v *LlamaVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	return strings.Contains(ups.Endpoint, ".llamarpc.com")
}
