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

type SqdVendor struct {
	common.Vendor
}

func CreateSqdVendor() common.Vendor {
	return &SqdVendor{}
}

func (v *SqdVendor) Name() string {
	return "sqd"
}

func (v *SqdVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	return ups.VendorName == v.Name()
}

func (v *SqdVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}

	if dataset, ok := sqdDatasetFromSettings(settings, chainID); ok && dataset != "" {
		return true, nil
	}

	if !sqdUseDefaultDatasets(settings) {
		return false, nil
	}

	_, exists := sqdChainToDataset[chainID]
	return exists, nil
}

func (v *SqdVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Evm == nil {
		return nil, fmt.Errorf("sqd vendor requires upstream.evm to be defined")
	}

	if upstream.Evm.ChainId == 0 {
		return nil, fmt.Errorf("sqd vendor requires upstream.evm.chainId to be defined")
	}

	if upstream.Endpoint == "" {
		return nil, fmt.Errorf("sqd vendor requires upstream.endpoint to be defined (portal wrapper)")
	}

	upstream.Type = common.UpstreamTypeEvm
	upstream.VendorName = v.Name()

	if upstream.IgnoreMethods == nil {
		upstream.IgnoreMethods = []string{"*"}
	}
	if upstream.AllowMethods == nil {
		upstream.AllowMethods = []string{
			"eth_chainId",
			"eth_blockNumber",
			"eth_getBlockByNumber",
			"eth_getTransactionByBlockNumberAndIndex",
			"eth_getLogs",
			"trace_block",
		}
	}

	if settings != nil {
		if apiKey, ok := settings["wrapperApiKey"].(string); ok && apiKey != "" {
			header := "X-API-Key"
			if headerOverride, ok := settings["wrapperApiKeyHeader"].(string); ok && headerOverride != "" {
				header = headerOverride
			}
			if upstream.JsonRpc == nil {
				upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
			}
			if upstream.JsonRpc.Headers == nil {
				upstream.JsonRpc.Headers = make(map[string]string)
			}
			if _, exists := upstream.JsonRpc.Headers[header]; !exists {
				upstream.JsonRpc.Headers[header] = apiKey
			}
		}
	}

	if logger != nil {
		logger.Debug().Int64("chainId", upstream.Evm.ChainId).Interface("upstream", upstream).Msg("generated upstream from sqd provider")
	}

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *SqdVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	// Wrapper returns standard JSON-RPC errors; let generic normalization handle them.
	return nil
}
