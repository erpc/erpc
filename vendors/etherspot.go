package vendors

import (
	"net/http"
	"strings"

	"github.com/erpc/erpc/common"
)

type EtherspotVendor struct {
	common.Vendor
}

func CreateEtherspotVendor() common.Vendor {
	return &EtherspotVendor{}
}

func (v *EtherspotVendor) Name() string {
	return "etherspot"
}

func (v *EtherspotVendor) OverrideConfig(upstream *common.UpstreamConfig) error {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.AutoIgnoreUnsupportedMethods == nil {
		upstream.AutoIgnoreUnsupportedMethods = &TRUE
	}

	if upstream.IgnoreMethods == nil {
		upstream.IgnoreMethods = []string{"*"}
	}
	if upstream.AllowMethods == nil {
		upstream.AllowMethods = []string{
			"skandha_config",
			"skandha_feeHistory",
			"skandha_getGasPrice",
			"eth_getUserOperationReceipt",
			"eth_getUserOperationByHash",
			"eth_sendUserOperation",
		}
	}

	return nil
}

func (v *EtherspotVendor) GetVendorSpecificErrorIfAny(resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	return nil
}

func (v *EtherspotVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	return strings.HasPrefix(ups.Endpoint, "etherspot") ||
		strings.HasPrefix(ups.Endpoint, "evm+etherspot") ||
		strings.Contains(ups.Endpoint, "etherspot.io")
}
