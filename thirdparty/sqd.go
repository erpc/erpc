package thirdparty

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	// Match .sqd.dev or ://sqd.dev to avoid false positives like "notsqd.dev"
	return strings.Contains(ups.Endpoint, ".sqd.dev") || strings.Contains(ups.Endpoint, "://sqd.dev")
}

func (v *SqdVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	_, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}

	// Any EVM chain is supported - endpoint uses {chainId} placeholder
	return true, nil
}

func (v *SqdVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	// Set batch defaults - SQD portal supports efficient batching
	if upstream.JsonRpc.SupportsBatch == nil {
		supportsBatch := true
		upstream.JsonRpc.SupportsBatch = &supportsBatch
	}
	if upstream.JsonRpc.BatchMaxWait == 0 {
		upstream.JsonRpc.BatchMaxWait = common.Duration(time.Nanosecond)
	}
	if upstream.JsonRpc.BatchMaxSize == 0 {
		upstream.JsonRpc.BatchMaxSize = 1000
	}

	if upstream.Evm == nil {
		return nil, fmt.Errorf("sqd vendor requires upstream.evm to be defined")
	}

	if upstream.Evm.ChainId == 0 {
		return nil, fmt.Errorf("sqd vendor requires upstream.evm.chainId to be defined")
	}

	// Always prefer endpoint from settings if it contains {chainId} placeholder
	// This ensures the provider's configured endpoint takes precedence over any
	// empty or invalid endpoint that might be set on the upstream from overrides
	if settingsEndpoint, ok := settings["endpoint"].(string); ok && settingsEndpoint != "" && strings.Contains(settingsEndpoint, "{chainId}") {
		upstream.Endpoint = settingsEndpoint
	} else if upstream.Endpoint == "" {
		return nil, fmt.Errorf("sqd vendor requires endpoint in settings with {chainId} placeholder")
	}

	upstream.Type = common.UpstreamTypeEvm
	upstream.VendorName = v.Name()

	// Substitute {chainId} placeholder if present.
	// When called from NewUpstream (second call for provider-generated configs),
	// the endpoint is already substituted and settings are nil, so we skip substitution.
	if strings.Contains(upstream.Endpoint, "{chainId}") {
		upstream.Endpoint = strings.ReplaceAll(upstream.Endpoint, "{chainId}", strconv.FormatInt(upstream.Evm.ChainId, 10))
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

	// Set X-Api-Key header if provided
	if apiKey, ok := settings["apiKey"].(string); ok && apiKey != "" {
		if upstream.JsonRpc.Headers == nil {
			upstream.JsonRpc.Headers = make(map[string]string)
		}
		if _, exists := upstream.JsonRpc.Headers["X-Api-Key"]; !exists {
			upstream.JsonRpc.Headers["X-Api-Key"] = apiKey
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
