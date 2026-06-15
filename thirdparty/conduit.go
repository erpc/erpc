package thirdparty

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

const DefaultConduitNetworksUrl = "https://api.conduit.xyz/public/network/all"
const DefaultConduitRecheckInterval = 24 * time.Hour

type ConduitNetwork struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ChainID      string `json:"chainId"`
	HttpEndpoint string `json:"httpEndpoint"`
	WsEndpoint   string `json:"wsEndpoint"`
}

type ConduitResponse struct {
	Endpoints []*ConduitNetwork `json:"endpoints"`
}

// ConduitVendor uses RemoteDataCache for lock-free, async-refresh access
// to the network list. See remote_cache.go for the request-path safety rule.
type ConduitVendor struct {
	common.Vendor
	cache *RemoteDataCache[map[int64]*ConduitNetwork]
}

func CreateConduitVendor() common.Vendor {
	return &ConduitVendor{
		cache: NewRemoteDataCache[map[int64]*ConduitNetwork]("conduit"),
	}
}

func (v *ConduitVendor) Name() string {
	return "conduit"
}

func (v *ConduitVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}

	networksUrl, ok := settings["networksUrl"].(string)
	if !ok || networksUrl == "" {
		networksUrl = DefaultConduitNetworksUrl
	}

	recheckInterval, ok := settings["recheckInterval"].(time.Duration)
	if !ok {
		recheckInterval = DefaultConduitRecheckInterval
	}

	networks, ok := v.resolveNetworks(logger, networksUrl, recheckInterval)
	if !ok {
		// Cold start: surface a retryable error so the bootstrap auto-retry
		// loop reschedules; the async refresh kicked off above will
		// populate the cache for the next attempt. NEVER blocks here.
		return false, ErrRemoteCacheCold
	}

	network, exists := networks[chainID]
	return exists && network != nil && network.HttpEndpoint != "", nil
}

// resolveNetworks does a lock-free Lookup, kicks off an async refresh on
// staleness, and returns (data, true) on hit or (nil, false) on cold
// start. Conduit has no built-in fallback map, so cold start surfaces as
// a retryable error to callers. See remote_cache.go for the safety rule.
func (v *ConduitVendor) resolveNetworks(logger *zerolog.Logger, networksUrl string, recheckInterval time.Duration) (map[int64]*ConduitNetwork, bool) {
	networks, fresh := v.cache.Lookup(networksUrl, recheckInterval)
	if !fresh {
		v.cache.TriggerAsyncRefresh(logger, networksUrl, func(ctx context.Context) (map[int64]*ConduitNetwork, error) {
			return v.fetchConduitNetworks(ctx, logger, networksUrl)
		})
	}
	if networks == nil {
		return nil, false
	}
	return networks, true
}

func (v *ConduitVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	// If a specific endpoint URL is already provided, use it directly.
	// This allows users to bypass the provider logic if needed, assuming they've included credentials.
	if upstream.Endpoint != "" {
		return []*common.UpstreamConfig{upstream}, nil
	}

	if upstream.Evm == nil {
		return nil, fmt.Errorf("conduit vendor requires upstream.evm to be defined")
	}

	if upstream.Evm.ChainId == 0 {
		return nil, fmt.Errorf("conduit vendor requires upstream.evm.chainId to be defined")
	}
	chainID := upstream.Evm.ChainId

	if settings == nil {
		settings = make(common.VendorSettings)
	}

	apiKey, ok := settings["apiKey"].(string)
	if !ok || apiKey == "" {
		return nil, fmt.Errorf("apiKey is required in conduit settings")
	}

	networksUrl, ok := settings["networksUrl"].(string)
	if !ok || networksUrl == "" {
		networksUrl = DefaultConduitNetworksUrl
	}

	recheckInterval, ok := settings["recheckInterval"].(time.Duration)
	if !ok {
		recheckInterval = DefaultConduitRecheckInterval
	}

	networks, ok := v.resolveNetworks(logger, networksUrl, recheckInterval)
	if !ok {
		return nil, ErrRemoteCacheCold
	}

	network, ok := networks[chainID]
	if !ok || network == nil || network.HttpEndpoint == "" {
		return nil, fmt.Errorf("chain ID %d not found in remote data or has no endpoint", chainID)
	}

	endpointURL := fmt.Sprintf("%s/%s", network.HttpEndpoint, apiKey)

	upsCfg := upstream.Copy()
	upsCfg.Type = common.UpstreamTypeEvm
	upsCfg.Endpoint = endpointURL
	upsCfg.VendorName = v.Name()

	logger.Debug().Int64("chainId", chainID).Interface("upstream", upsCfg).Interface("settings", map[string]interface{}{
		"networksUrl":     networksUrl,
		"recheckInterval": recheckInterval,
	}).Msg("generated upstream from conduit provider")

	return []*common.UpstreamConfig{upsCfg}, nil
}

func (v *ConduitVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	bodyMap, ok := jrr.(*common.JsonRpcResponse)
	if !ok {
		return nil
	}

	err := bodyMap.Error
	if code := err.Code; code != 0 {
		msg := err.Message
		if err.Data != "" {
			details["data"] = err.Data
		}

		if code == -32600 && (strings.Contains(msg, "be authenticated") || strings.Contains(msg, "access key") || strings.Contains(msg, "api key")) {
			return common.NewErrEndpointUnauthorized(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorUnauthorized,
					msg,
					nil,
					details,
				),
			)
		} else if strings.Contains(msg, "limit exceeded") || strings.Contains(msg, "capacity limit") {
			return common.NewErrEndpointCapacityExceeded(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorCapacityExceeded,
					msg,
					nil,
					details,
				),
			)
		} else if code >= -32000 && code <= -32099 {
			return common.NewErrEndpointServerSideException(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorServerSideException,
					msg,
					nil,
					details,
				),
				nil,
				resp.StatusCode,
			)
		}
	}

	return nil
}

func (v *ConduitVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "conduit://") || strings.HasPrefix(ups.Endpoint, "evm+conduit://") {
		return true
	}

	if ups.VendorName == v.Name() {
		return true
	}

	return false
}

func (v *ConduitVendor) fetchConduitNetworks(ctx context.Context, logger *zerolog.Logger, networksUrl string) (map[int64]*ConduitNetwork, error) {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, "GET", networksUrl, nil)
	if err != nil {
		return nil, err
	}
	var httpClient = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
		},
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("Conduit API returned non-200 code: %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var response ConduitResponse
	if err := common.SonicCfg.Unmarshal(bodyBytes, &response); err != nil {
		return nil, fmt.Errorf("failed to parse Conduit API data: %w", err)
	}

	newData := make(map[int64]*ConduitNetwork)
	for _, network := range response.Endpoints {
		chainID, err := strconv.ParseInt(network.ChainID, 10, 64)
		if err != nil {
			logger.Debug().Str("chainId", network.ChainID).Msg("skipping network with invalid chainId")
			continue
		}
		if chainID > 0 && network.HttpEndpoint != "" {
			newData[chainID] = network
		}
	}

	return newData, nil
}
