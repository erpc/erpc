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
	"github.com/rs/zerolog/log"
)

var defaultAlchemyNetworkSubdomains = map[int64]string{
	1:         "eth-mainnet",
	10:        "opt-mainnet",
	100:       "gnosis-mainnet",
	10200:     "gnosis-chiado",
	1088:      "metis-mainnet",
	1101:      "polygonzkevm-mainnet",
	11011:     "shape-sepolia",
	11155111:  "eth-sepolia",
	11155420:  "opt-sepolia",
	137:       "polygon-mainnet",
	168587773: "blast-sepolia",
	17000:     "eth-holesky",
	204:       "opbnb-mainnet",
	2442:      "polygonzkevm-cardona",
	250:       "fantom-mainnet",
	300:       "zksync-sepolia",
	324:       "zksync-mainnet",
	4002:      "fantom-testnet",
	42161:     "arb-mainnet",
	421614:    "arb-sepolia",
	42170:     "arbnova-mainnet",
	43113:     "avax-fuji",
	43114:     "avax-mainnet",
	5000:      "mantle-mainnet",
	56:        "bnb-mainnet",
	5611:      "opbnb-testnet",
	59141:     "linea-sepolia",
	59144:     "linea-mainnet",
	592:       "astar-mainnet",
	7000:      "zetachain-mainnet",
	7001:      "zetachain-testnet",
	7777777:   "zora-mainnet",
	80002:     "polygon-amoy",
	81457:     "blast-mainnet",
	8453:      "base-mainnet",
	84532:     "base-sepolia",
	97:        "bnb-testnet",
	999999999: "zora-sepolia",
	534351:    "scroll-sepolia",
	534352:    "scroll-mainnet",
}

const DefaultAlchemyRecheckInterval = 24 * time.Hour
const alchemyApiUrl = "https://app-api.alchemy.com/trpc/config.getNetworkConfig"

type alchemyNetworkConfigResponse struct {
	Result struct {
		Data []struct {
			NetworkChainID int64  `json:"networkChainId"`
			KebabCaseID    string `json:"kebabCaseId"`
		} `json:"data"`
	} `json:"result"`
}

type AlchemyVendor struct {
	common.Vendor

	remoteDataLock          sync.Mutex
	remoteData              map[string]map[int64]string
	remoteDataLastFetchedAt map[string]time.Time
}

func CreateAlchemyVendor() common.Vendor {
	return &AlchemyVendor{
		remoteData:              make(map[string]map[int64]string),
		remoteDataLastFetchedAt: make(map[string]time.Time),
	}
}

func (v *AlchemyVendor) Name() string {
	return "alchemy"
}

func (v *AlchemyVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}

	recheckInterval, ok := settings["recheckInterval"].(time.Duration)
	if !ok {
		recheckInterval = DefaultAlchemyRecheckInterval
	}

	err = v.ensureRemoteData(ctx, recheckInterval)
	if err != nil {
		return false, fmt.Errorf("unable to load remote data: %w", err)
	}

	networks, ok := v.remoteData[alchemyApiUrl]
	if !ok || networks == nil {
		return false, nil
	}

	_, exists := networks[chainID]
	return exists, nil
}

func (v *AlchemyVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint == "" {
		apiKey, ok := settings["apiKey"].(string)
		if !ok || apiKey == "" {
			return nil, fmt.Errorf("apiKey is required in alchemy settings")
		}

		if upstream.Evm == nil {
			return nil, fmt.Errorf("alchemy vendor requires upstream.evm to be defined")
		}

		chainID := upstream.Evm.ChainId
		if chainID == 0 {
			return nil, fmt.Errorf("alchemy vendor requires upstream.evm.chainId to be defined")
		}

		recheckInterval, ok := settings["recheckInterval"].(time.Duration)
		if !ok {
			recheckInterval = DefaultAlchemyRecheckInterval
		}

		if err := v.ensureRemoteData(ctx, recheckInterval); err != nil {
			return nil, fmt.Errorf("unable to load remote data: %w", err)
		}

		networks, ok := v.remoteData[alchemyApiUrl]
		if !ok || networks == nil {
			return nil, fmt.Errorf("network data not available")
		}

		subdomain, ok := networks[chainID]
		if !ok {
			return nil, fmt.Errorf("unsupported network chain ID for Alchemy: %d", chainID)
		}

		alchemyURL := fmt.Sprintf("https://%s.g.alchemy.com/v2/%s", subdomain, apiKey)
		parsedURL, err := url.Parse(alchemyURL)
		if err != nil {
			return nil, err
		}

		upstream.Endpoint = parsedURL.String()
		upstream.Type = common.UpstreamTypeEvm
	}

	upstream.VendorName = v.Name()

	log.Debug().Int64("chainId", upstream.Evm.ChainId).Interface("upstream", upstream).Interface("settings", map[string]interface{}{
		"recheckInterval": settings["recheckInterval"],
	}).Msg("generated upstream from alchemy provider")

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *AlchemyVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
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

		if code == -32600 && (strings.Contains(msg, "be authenticated") || strings.Contains(msg, "access key")) {
			return common.NewErrEndpointUnauthorized(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorUnauthorized,
					msg,
					nil,
					details,
				),
			)
		} else if code >= -32099 && code <= -32599 || code >= -32603 && code <= -32699 || code >= -32701 && code <= -32768 {
			// For invalid request errors (codes above), there is a high chance that the error is due to a mistake that the user
			// has done, and retrying another upstream would not help.
			// Ref: https://docs.alchemy.com/reference/error-reference#json-rpc-error-codes
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorClientSideException,
					msg,
					nil,
					details,
				),
			).WithRetryableTowardNetwork(false)
		} else if code == 3 {
			return common.NewErrEndpointExecutionException(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorEvmReverted,
					msg,
					nil,
					details,
				),
			)
		}
	}

	// Other errors can be properly handled by generic error handling
	return nil
}

func (v *AlchemyVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "alchemy://") || strings.HasPrefix(ups.Endpoint, "evm+alchemy://") {
		return true
	}

	if ups.VendorName == v.Name() {
		return true
	}

	return strings.Contains(ups.Endpoint, ".alchemy.com") || strings.Contains(ups.Endpoint, ".alchemyapi.io")
}

func (v *AlchemyVendor) ensureRemoteData(ctx context.Context, recheckInterval time.Duration) error {
	v.remoteDataLock.Lock()
	defer v.remoteDataLock.Unlock()

	if ltm, ok := v.remoteDataLastFetchedAt[alchemyApiUrl]; ok && time.Since(ltm) < recheckInterval {
		return nil
	}

	newData, err := v.fetchAlchemyNetworks(ctx)
	if err != nil {
		if _, ok := v.remoteData[alchemyApiUrl]; ok {
			log.Warn().Err(err).Msg("could not refresh Alchemy API data; will use stale data")
			return nil
		}
		return err
	}

	v.remoteData[alchemyApiUrl] = newData
	v.remoteDataLastFetchedAt[alchemyApiUrl] = time.Now()
	return nil
}

func (v *AlchemyVendor) fetchAlchemyNetworks(ctx context.Context) (map[int64]string, error) {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, "GET", alchemyApiUrl, nil)
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
		return nil, fmt.Errorf("Alchemy API returned non-200 code: %d", resp.StatusCode)
	}

	var apiResp alchemyNetworkConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse Alchemy API data: %w", err)
	}

	newData := make(map[int64]string)
	for _, network := range apiResp.Result.Data {
		if network.KebabCaseID != "" && network.NetworkChainID != 0 {
			newData[network.NetworkChainID] = network.KebabCaseID
		}
	}

	// Merge with defaults, API data takes precedence
	for chainID, subdomain := range defaultAlchemyNetworkSubdomains {
		if _, exists := newData[chainID]; !exists {
			newData[chainID] = subdomain
		}
	}

	return newData, nil
}
