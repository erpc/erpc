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

var defaultAlchemyNetworkSubdomains = map[int64]string{
	1:          "eth-mainnet",
	11155111:   "eth-sepolia",
	17000:      "eth-holesky",
	560048:     "eth-hoodi",
	10:         "opt-mainnet",
	11155420:   "opt-sepolia",
	137:        "polygon-mainnet",
	80002:      "polygon-amoy",
	42161:      "arb-mainnet",
	421614:     "arb-sepolia",
	42170:      "arbnova-mainnet",
	8453:       "base-mainnet",
	84532:      "base-sepolia",
	324:        "zksync-mainnet",
	300:        "zksync-sepolia",
	7777777:    "zora-mainnet",
	999999999:  "zora-sepolia",
	81457:      "blast-mainnet",
	168587773:  "blast-sepolia",
	534352:     "scroll-mainnet",
	534351:     "scroll-sepolia",
	59144:      "linea-mainnet",
	59141:      "linea-sepolia",
	100:        "gnosis-mainnet",
	10200:      "gnosis-chiado",
	5000:       "mantle-mainnet",
	5003:       "mantle-sepolia",
	42220:      "celo-mainnet",
	44787:      "celo-alfajores",
	11142220:   "celo-sepolia",
	56:         "bnb-mainnet",
	97:         "bnb-testnet",
	43114:      "avax-mainnet",
	43113:      "avax-fuji",
	1088:       "metis-mainnet",
	204:        "opbnb-mainnet",
	5611:       "opbnb-testnet",
	592:        "astar-mainnet",
	1101:       "polygonzkevm-mainnet",
	2442:       "polygonzkevm-cardona",
	7000:       "zetachain-mainnet",
	7001:       "zetachain-testnet",
	747:        "flow-mainnet",
	545:        "flow-testnet",
	480:        "worldchain-mainnet",
	4801:       "worldchain-sepolia",
	252:        "frax-mainnet",
	2523:       "frax-sepolia",
	288:        "boba-mainnet",
	28882:      "boba-sepolia",
	1329:       "sei-mainnet",
	1328:       "sei-testnet",
	80094:      "berachain-mainnet",
	80069:      "berachain-bepolia",
	360:        "shape-mainnet",
	11011:      "shape-sepolia",
	8008:       "polynomial-mainnet",
	80008:      "polynomial-sepolia",
	60808:      "bob-mainnet",
	808813:     "bob-sepolia",
	34443:      "mode-mainnet",
	919:        "mode-sepolia",
	69000:      "anime-mainnet",
	6900:       "anime-sepolia",
	33139:      "apechain-mainnet",
	33111:      "apechain-curtis",
	232:        "lens-mainnet",
	37111:      "lens-sepolia",
	1868:       "soneium-mainnet",
	1946:       "soneium-minato",
	30:         "rootstock-mainnet",
	31:         "rootstock-testnet",
	994873017:  "lumia-prism",
	2030232745: "lumia-beam",
	130:        "unichain-mainnet",
	1301:       "unichain-sepolia",
	146:        "sonic-mainnet",
	14601:      "sonic-testnet",
	57054:      "sonic-blaze",
	2741:       "abstract-mainnet",
	11124:      "abstract-testnet",
	143:        "monad-mainnet",
	10143:      "monad-testnet",
	5330:       "superseed-mainnet",
	53302:      "superseed-sepolia",
	57073:      "ink-mainnet",
	763373:     "ink-sepolia",
	2020:       "ronin-mainnet",
	2021:       "ronin-saigon",
	6985385:    "humanity-mainnet",
	7080969:    "humanity-testnet",
	1514:       "story-mainnet",
	1315:       "story-aeneid",
	999:        "hyperliquid-mainnet",
	998:        "hyperliquid-testnet",
	9745:       "plasma-mainnet",
	9746:       "plasma-testnet",
	4157:       "crossfi-testnet",
	4158:       "crossfi-mainnet",
	5371:       "settlus-mainnet",
	5373:       "settlus-septestnet",
	3637:       "botanix-mainnet",
	3636:       "botanix-testnet",
	613419:     "galactica-mainnet",
	843843:     "galactica-cassiopeia",
	510:        "synd-mainnet",
	36900:      "adi-testnet",
	988:        "stable-mainnet",
	2201:       "stable-testnet",
	510525:     "clankermon-mainnet",
	4114:       "citrea-mainnet",
	5115:       "citrea-testnet",
	5042002:    "arc-testnet",
	1284:       "moonbeam-mainnet",
	10218:      "tea-sepolia",
	685685:     "gensyn-testnet",
	11155931:   "rise-testnet",
	6343:       "megaeth-testnet",
	323432:     "worldmobile-testnet",
	869:        "worldmobilechain-mainnet",
	666666666:  "degen-mainnet",
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

	err = v.ensureRemoteData(ctx, logger, recheckInterval)
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

		if err := v.ensureRemoteData(ctx, logger, recheckInterval); err != nil {
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

	logger.Debug().Int64("chainId", upstream.Evm.ChainId).Interface("upstream", upstream).Interface("settings", map[string]interface{}{
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

func (v *AlchemyVendor) ensureRemoteData(ctx context.Context, logger *zerolog.Logger, recheckInterval time.Duration) error {
	v.remoteDataLock.Lock()
	defer v.remoteDataLock.Unlock()

	if ltm, ok := v.remoteDataLastFetchedAt[alchemyApiUrl]; ok && time.Since(ltm) < recheckInterval {
		return nil
	}

	newData, err := v.fetchAlchemyNetworks(ctx)
	if err != nil {
		if _, ok := v.remoteData[alchemyApiUrl]; ok {
			logger.Warn().Err(err).Msg("could not refresh Alchemy API data, will use stale data")
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
