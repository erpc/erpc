package thirdparty

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
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

type ConduitVendor struct {
	common.Vendor

	remoteDataLock          sync.Mutex
	remoteData              map[string]map[int64]*ConduitNetwork
	remoteDataLastFetchedAt map[string]time.Time
}

func CreateConduitVendor() common.Vendor {
	return &ConduitVendor{
		remoteData:              make(map[string]map[int64]*ConduitNetwork),
		remoteDataLastFetchedAt: make(map[string]time.Time),
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

	err = v.ensureRemoteData(ctx, recheckInterval, networksUrl)
	if err != nil {
		return false, fmt.Errorf("unable to load remote data: %w", err)
	}

	networks, ok := v.remoteData[networksUrl]
	if !ok || networks == nil {
		return false, nil
	}

	network, exists := networks[chainID]
	return exists && network != nil && network.HttpEndpoint != "", nil
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

	if err := v.ensureRemoteData(context.Background(), recheckInterval, networksUrl); err != nil {
		return nil, fmt.Errorf("unable to load remote data: %w", err)
	}

	networks, ok := v.remoteData[networksUrl]
	if !ok || networks == nil {
		return nil, fmt.Errorf("network data not available")
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

	log.Debug().Int64("chainId", chainID).Interface("upstream", upsCfg).Interface("settings", map[string]interface{}{
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

func (v *ConduitVendor) ensureRemoteData(ctx context.Context, recheckInterval time.Duration, networksUrl string) error {
	v.remoteDataLock.Lock()
	defer v.remoteDataLock.Unlock()

	if ltm, ok := v.remoteDataLastFetchedAt[networksUrl]; ok && time.Since(ltm) < recheckInterval {
		return nil
	}

	newData, err := v.fetchConduitNetworks(ctx, networksUrl)
	if err != nil {
		if _, ok := v.remoteData[networksUrl]; ok {
			log.Warn().Err(err).Msg("could not refresh Conduit API data; will use stale data")
			return nil
		}
		return err
	}

	v.remoteData[networksUrl] = newData
	v.remoteDataLastFetchedAt[networksUrl] = time.Now()
	return nil
}

func (v *ConduitVendor) fetchConduitNetworks(ctx context.Context, networksUrl string) (map[int64]*ConduitNetwork, error) {
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
			log.Debug().Str("chainId", network.ChainID).Msg("skipping network with invalid chainId")
			continue
		}
		if chainID > 0 && network.HttpEndpoint != "" {
			newData[chainID] = network
		}
	}

	return newData, nil
}
