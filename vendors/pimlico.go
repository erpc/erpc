package vendors

import (
	"net/http"
	"strings"

	"github.com/erpc/erpc/common"
)

type PimlicoVendor struct {
	common.Vendor
}

func CreatePimlicoVendor() common.Vendor {
	return &PimlicoVendor{}
}

func (v *PimlicoVendor) Name() string {
	return "pimlico"
}

func (v *PimlicoVendor) OverrideConfig(upstream *common.UpstreamConfig) error {
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
			"eth_sendUserOperation",
			"eth_estimateUserOperationGas",
			"eth_getUserOperationReceipt",
			"eth_getUserOperationByHash",
			"eth_supportedEntryPoints",
			"pimlico_sendCompressedUserOperation",
			"pimlico_getUserOperationGasPrice",
			"pimlico_getUserOperationStatus",
			"pm_sponsorUserOperation",
			"pm_getPaymasterData",
			"pm_getPaymasterStubData",
		}
	}

	return nil
}

func (v *PimlicoVendor) GetVendorSpecificErrorIfAny(resp *http.Response, jrr interface{}) error {
	return nil
}

func (v *PimlicoVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	return strings.HasPrefix(ups.Endpoint, "pimlico") ||
		strings.HasPrefix(ups.Endpoint, "evm+pimlico") ||
		strings.Contains(ups.Endpoint, "pimlico.io")
}
