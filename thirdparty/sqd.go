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
	if ups.VendorName == v.Name() {
		return true
	}
	return strings.Contains(ups.Endpoint, "sqd.dev")
}

func (v *SqdVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}

	// If endpoint uses {chainId} placeholder, any EVM chain is supported
	if endpoint, ok := settings["endpoint"].(string); ok && strings.Contains(endpoint, "{chainId}") {
		return true, nil
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

	// Support {chainId} placeholder for direct chain ID substitution (preferred)
	if strings.Contains(upstream.Endpoint, "{chainId}") {
		upstream.Endpoint = strings.ReplaceAll(upstream.Endpoint, "{chainId}", strconv.FormatInt(upstream.Evm.ChainId, 10))
	} else if dataset, ok := sqdDatasetForChain(settings, upstream.Evm.ChainId); ok {
		// Fall back to {dataset} placeholder for backward compatibility
		if endpoint, updated := sqdApplyDatasetToEndpoint(upstream.Endpoint, dataset); updated {
			upstream.Endpoint = endpoint
		} else if logger != nil {
			logger.Warn().
				Int64("chainId", upstream.Evm.ChainId).
				Str("dataset", dataset).
				Str("endpoint", upstream.Endpoint).
				Msg("sqd dataset resolved but could not be applied to endpoint format")
		}
	} else if strings.Contains(upstream.Endpoint, "{dataset}") {
		return nil, fmt.Errorf("sqd endpoint contains {dataset} placeholder but no dataset found for chainId %d", upstream.Evm.ChainId)
	}

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

	if apiKey, ok := settings["wrapperApiKey"].(string); ok && apiKey != "" {
		header := "X-API-Key"
		if headerOverride, ok := settings["wrapperApiKeyHeader"].(string); ok && headerOverride != "" {
			header = headerOverride
		}
		if upstream.JsonRpc.Headers == nil {
			upstream.JsonRpc.Headers = make(map[string]string)
		}
		if _, exists := upstream.JsonRpc.Headers[header]; !exists {
			upstream.JsonRpc.Headers[header] = apiKey
		}
	}

	if logger != nil {
		logger.Debug().Int64("chainId", upstream.Evm.ChainId).Interface("upstream", upstream).Msg("generated upstream from sqd provider")
	}

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *SqdVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	if resp == nil {
		return nil
	}

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return common.NewErrEndpointUnauthorized(fmt.Errorf("sqd portal wrapper unauthorized: %d", resp.StatusCode))
	case http.StatusTooManyRequests:
		return common.NewErrEndpointCapacityExceeded(fmt.Errorf("sqd portal wrapper rate limited: %d", resp.StatusCode))
	case http.StatusPaymentRequired:
		return common.NewErrEndpointBillingIssue(fmt.Errorf("sqd portal wrapper billing issue: %d", resp.StatusCode))
	}

	// Wrapper returns standard JSON-RPC errors; let generic normalization handle them.
	return nil
}

func sqdDatasetForChain(settings common.VendorSettings, chainId int64) (string, bool) {
	if dataset, ok := sqdDatasetFromSettings(settings, chainId); ok {
		return dataset, true
	}
	if !sqdUseDefaultDatasets(settings) {
		return "", false
	}
	if dataset, ok := sqdChainToDataset[chainId]; ok {
		return dataset, true
	}
	return "", false
}

func sqdApplyDatasetToEndpoint(endpoint string, dataset string) (string, bool) {
	if dataset == "" {
		return endpoint, false
	}
	if strings.Contains(endpoint, "{dataset}") {
		return strings.ReplaceAll(endpoint, "{dataset}", dataset), true
	}
	trimmed := strings.TrimRight(endpoint, "/")
	if strings.HasSuffix(strings.ToLower(trimmed), "/datasets") {
		return trimmed + "/" + dataset, true
	}
	return endpoint, false
}
