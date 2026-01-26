package thirdparty

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

type TenderlyVendor struct {
	common.Vendor
	remoteDataLock          sync.Mutex
	remoteData              map[string]map[int64]string
	remoteDataLastFetchedAt map[string]time.Time
}

func CreateTenderlyVendor() common.Vendor {
	return &TenderlyVendor{
		remoteData:              make(map[string]map[int64]string),
		remoteDataLastFetchedAt: make(map[string]time.Time),
	}
}

func (v *TenderlyVendor) Name() string {
	return "tenderly"
}

const DefaultTenderlyRecheckInterval = 24 * time.Hour
const tenderlyApiUrl = "https://api.tenderly.co/api/v1/supported-networks"

type tenderlySupportedNetwork struct {
	ChainID      string `json:"chain_id"`
	NetworkSlugs struct {
		ExplorerSlug string `json:"explorer_slug"`
		NodeRPCSlug  string `json:"node_rpc_slug"`
		VnetRPCSlug  string `json:"vnet_rpc_slug"`
	} `json:"network_slugs"`
}

func (v *TenderlyVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}

	recheckInterval, ok := settings["recheckInterval"].(time.Duration)
	if !ok {
		recheckInterval = DefaultTenderlyRecheckInterval
	}

	if err := v.ensureRemoteData(ctx, logger, recheckInterval); err != nil {
		return false, fmt.Errorf("unable to load remote data: %w", err)
	}

	networks, ok := v.remoteData[tenderlyApiUrl]
	if !ok || networks == nil {
		return false, nil
	}

	_, exists := networks[chainID]
	return exists, nil
}

func (v *TenderlyVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint == "" {
		apiKey, ok := settings["apiKey"].(string)
		if !ok || apiKey == "" {
			return nil, fmt.Errorf("apiKey is required in tenderly settings")
		}

		if upstream.Evm == nil {
			return nil, fmt.Errorf("tenderly vendor requires upstream.evm to be defined")
		}

		chainID := upstream.Evm.ChainId
		if chainID == 0 {
			return nil, fmt.Errorf("tenderly vendor requires upstream.evm.chainId to be defined")
		}

		recheckInterval, ok := settings["recheckInterval"].(time.Duration)
		if !ok {
			recheckInterval = DefaultTenderlyRecheckInterval
		}

		if err := v.ensureRemoteData(ctx, logger, recheckInterval); err != nil {
			return nil, fmt.Errorf("unable to load remote data: %w", err)
		}

		networks, ok := v.remoteData[tenderlyApiUrl]
		if !ok || networks == nil {
			return nil, fmt.Errorf("network data not available")
		}

		subdomain, ok := networks[chainID]
		if !ok {
			return nil, fmt.Errorf("unsupported network chain ID for Tenderly: %d", chainID)
		}

		tenderlyURL := fmt.Sprintf("https://%s.gateway.tenderly.co/%s", subdomain, apiKey)
		parsedURL, err := url.Parse(tenderlyURL)
		if err != nil {
			return nil, err
		}

		upstream.Endpoint = parsedURL.String()
		upstream.Type = common.UpstreamTypeEvm
	}

	upstream.VendorName = v.Name()

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *TenderlyVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	bodyMap, ok := jrr.(*common.JsonRpcResponse)
	if !ok {
		return nil
	}

	err := bodyMap.Error
	if err.Data != "" {
		details["data"] = err.Data
	}

	return nil
}

func (v *TenderlyVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "tenderly://") || strings.HasPrefix(ups.Endpoint, "evm+tenderly://") {
		return true
	}

	if ups.VendorName == v.Name() {
		return true
	}

	return strings.Contains(ups.Endpoint, ".gateway.tenderly.co")
}

func (v *TenderlyVendor) ensureRemoteData(ctx context.Context, logger *zerolog.Logger, recheckInterval time.Duration) error {
	v.remoteDataLock.Lock()
	defer v.remoteDataLock.Unlock()

	if ltm, ok := v.remoteDataLastFetchedAt[tenderlyApiUrl]; ok && time.Since(ltm) < recheckInterval {
		return nil
	}

	newData, err := v.fetchTenderlyNetworks(ctx)
	if err != nil {
		if _, ok := v.remoteData[tenderlyApiUrl]; ok {
			logger.Warn().Err(err).Msg("could not refresh Tenderly API data; will use stale data")
			return nil
		}
		return err
	}

	v.remoteData[tenderlyApiUrl] = newData
	v.remoteDataLastFetchedAt[tenderlyApiUrl] = time.Now()
	return nil
}

func (v *TenderlyVendor) fetchTenderlyNetworks(ctx context.Context) (map[int64]string, error) {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, "GET", tenderlyApiUrl, nil)
	if err != nil {
		return nil, err
	}

	httpClient := &http.Client{
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
		return nil, fmt.Errorf("Tenderly API returned non-200 code: %d", resp.StatusCode)
	}

	var apiResp []tenderlySupportedNetwork
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse Tenderly API data: %w", err)
	}

	newData := make(map[int64]string)
	for _, n := range apiResp {
		if n.ChainID == "" {
			continue
		}
		cid, err := strconv.ParseInt(n.ChainID, 10, 64)
		if err != nil {
			continue
		}
		var slug string
		if n.NetworkSlugs.NodeRPCSlug != "" {
			slug = n.NetworkSlugs.NodeRPCSlug
		} else if n.NetworkSlugs.VnetRPCSlug != "" {
			slug = n.NetworkSlugs.VnetRPCSlug
		} else {
			slug = n.NetworkSlugs.ExplorerSlug
		}
		if slug != "" {
			newData[cid] = slug
		}
	}

	return newData, nil
}
