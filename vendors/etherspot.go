package vendors

import (
	"net/http"
	"strings"

	"github.com/erpc/erpc/common"
)

type EtherSpotVendor struct {
	common.Vendor
}

func CreateEtherSpotVendor() common.Vendor {
	return &EtherSpotVendor{}
}

func (v *EtherSpotVendor) Name() string {
	return "etherspot"
}

func (v *EtherSpotVendor) OverrideConfig(upstream *common.UpstreamConfig) error {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.JsonRpc.SupportsBatch == nil {
		upstream.JsonRpc.SupportsBatch = &TRUE
		upstream.JsonRpc.BatchMaxWait = "100ms"
		upstream.JsonRpc.BatchMaxSize = 100
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

func (v *EtherSpotVendor) GetVendorSpecificErrorIfAny(resp *http.Response, jrr interface{}) error {
	return nil
}

func (v *EtherSpotVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	return strings.HasPrefix(ups.Endpoint, "etherspot") ||
		strings.HasPrefix(ups.Endpoint, "evm+etherspot") ||
		strings.Contains(ups.Endpoint, "etherspot.io")
}
