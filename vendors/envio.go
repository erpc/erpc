package vendors

import (
	"net/http"
	"strings"

	"github.com/erpc/erpc/common"
)

type EnvioVendor struct {
	common.Vendor
}

func CreateEnvioVendor() common.Vendor {
	return &EnvioVendor{}
}

func (v *EnvioVendor) Name() string {
	return "envio"
}

func (v *EnvioVendor) OverrideConfig(upstream *common.UpstreamConfig) error {
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
			"eth_chainId",
			"eth_blockNumber",
			"eth_getBlockByNumber",
			"eth_getBlockByHash",
			"eth_getTransactionByHash",
			"eth_getTransactionByBlockHashAndIndex",
			"eth_getTransactionByBlockNumberAndIndex",
			"eth_getTransactionReceipt",
			"eth_getBlockReceipts",
			"eth_getLogs",
			"eth_getFilterLogs",
			"eth_getFilterChanges",
			"eth_uninstallFilter",
			"eth_newFilter",
		}
	}

	return nil
}

func (v *EnvioVendor) GetVendorSpecificErrorIfAny(resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	return nil
}

func (v *EnvioVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	return strings.HasPrefix(ups.Endpoint, "envio") ||
		strings.HasPrefix(ups.Endpoint, "evm+envio") ||
		strings.Contains(ups.Endpoint, "envio.dev") ||
		strings.Contains(ups.Endpoint, "hypersync.xyz")
}
