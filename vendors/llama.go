package vendors

import (
	"net/http"
	"strings"

	"github.com/erpc/erpc/common"
)

type LlamaVendor struct {
	common.Vendor
}

func CreateLlamaVendor() common.Vendor {
	return &LlamaVendor{}
}

func (v *LlamaVendor) Name() string {
	return "llama"
}

func (v *LlamaVendor) OverrideConfig(upstream *common.UpstreamConfig) error {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.JsonRpc.SupportsBatch == nil {
		upstream.JsonRpc.SupportsBatch = &TRUE
		upstream.JsonRpc.BatchMaxWait = "100ms"
		upstream.JsonRpc.BatchMaxSize = 100
	}
	if upstream.AutoIgnoreUnsupportedMethods == nil {
		upstream.AutoIgnoreUnsupportedMethods = &TRUE
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
