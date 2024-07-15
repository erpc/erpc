package vendors

import (
	"net/http"
	"strings"

	"github.com/flair-sdk/erpc/common"
)

type LlamaVendor struct {
	common.Vendor
}

func CreateLlamaVendor() common.Vendor {
	return &DrpcVendor{}
}

func (v *LlamaVendor) Name() string {
	return "llama"
}

func (v *LlamaVendor) GetVendorSpecificErrorIfAny(resp *http.Response, jrr interface{}) error {
	bodyMap, ok := jrr.(*common.JsonRpcResponse)
	if !ok {
		return nil
	}

	err := bodyMap.Error
	if code := err.OriginalCode(); code != 0 {
		msg := err.Message

		if code == 1015 {
			return common.NewErrEndpointUnauthorized(
				common.NewErrJsonRpcException(code, common.JsonRpcErrorCapacityExceeded, msg, nil),
			)
		}
	}

	// Other errors can be properly handled by generic error handling
	return nil
}

func (v *LlamaVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	return strings.Contains(ups.Endpoint, ".llamarpc.com")
}
