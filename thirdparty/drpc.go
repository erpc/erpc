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

// defaultDrpcNetworkNames is a built-in snapshot of chain ID → dRPC network
// name used as a cold-start fallback when the remote API is unreachable.
var defaultDrpcNetworkNames = map[int64]string{
	1:          "ethereum",
	10:         "optimism",
	25:         "cronos",
	30:         "rootstock",
	31:         "rootstock-testnet",
	40:         "telos",
	56:         "bsc",
	88:         "viction",
	89:         "viction-testnet",
	97:         "bsc-testnet",
	100:        "gnosis",
	109:        "shibarium",
	122:        "fuse",
	130:        "unichain",
	133:        "hashkey-testnet",
	137:        "polygon",
	143:        "monad-mainnet",
	146:        "sonic",
	169:        "manta-pacific",
	177:        "hashkey",
	196:        "xlayer",
	199:        "bittorrent",
	204:        "opbnb",
	232:        "lens",
	239:        "tac",
	240:        "cronos-zkevm-testnet",
	250:        "fantom",
	252:        "fraxtal",
	255:        "kroma",
	288:        "boba-eth",
	291:        "orderly",
	300:        "zksync-sepolia",
	314:        "filecoin",
	324:        "zksync",
	338:        "cronos-testnet",
	388:        "cronos-zkevm",
	480:        "worldchain",
	919:        "mode-testnet",
	945:        "bittensor-testnet",
	964:        "bittensor",
	998:        "hyperliquid-testnet",
	999:        "hyperliquid",
	1088:       "metis",
	1100:       "dymension",
	1101:       "polygon-zkevm",
	1111:       "wemix",
	1112:       "wemix-testnet",
	1114:       "core-testnet",
	1116:       "core",
	1135:       "lisk",
	1284:       "moonbeam",
	1285:       "moonriver",
	1301:       "unichain-sepolia",
	1315:       "story-aeneid-testnet",
	1328:       "sei-testnet",
	1329:       "sei",
	1750:       "metall2",
	1868:       "soneium",
	1923:       "swell",
	1924:       "swell-testnet",
	1946:       "soneium-minato",
	1952:       "xlayer-testnet",
	2020:       "ronin",
	2222:       "kava",
	2288:       "moca",
	2345:       "goat-mainnet-alpha",
	2442:       "polygon-zkevm-cardona",
	2523:       "fraxtal-testnet",
	2741:       "abstract",
	2818:       "morph",
	4002:       "fantom-testnet",
	4200:       "merlin",
	4202:       "lisk-sepolia",
	4217:       "tempo-mainnet",
	4326:       "megaeth",
	5000:       "mantle",
	5003:       "mantle-sepolia",
	5330:       "superseed",
	7000:       "zeta-chain",
	7001:       "zeta-chain-testnet",
	8217:       "klaytn",
	8453:       "base",
	9745:       "plasma",
	10143:      "monad-testnet",
	10200:      "gnosis-chiado",
	11124:      "abstract-sepolia",
	11235:      "haqq",
	13371:      "immutable-zkevm",
	13473:      "immutable-zkevm-testnet",
	14601:      "sonic-testnet-v2",
	16602:      "0g-galileo-testnet",
	16661:      "0g-mainnet",
	17000:      "holesky",
	25327:      "everclear",
	31611:      "mezo-testnet",
	31612:      "mezo",
	33111:      "apechain-curtis",
	33139:      "apechain",
	34443:      "mode",
	36888:      "abcore",
	37111:      "lens-testnet",
	42161:      "arbitrum",
	42170:      "arbitrum-nova",
	42220:      "celo",
	42431:      "tempo-moderato-testnet",
	43111:      "hemi",
	43113:      "avalanche-fuji",
	43114:      "avalanche",
	44787:      "celo-alfajores",
	46630:      "robinhood-testnet",
	48898:      "zircuit-garfield-testnet",
	48900:      "zircuit-mainnet",
	53302:      "superseed-sepolia",
	57073:      "ink",
	59141:      "linea-sepolia",
	59144:      "linea",
	60808:      "bob",
	80002:      "polygon-amoy",
	80069:      "berachain-bepolia",
	80094:      "berachain",
	81457:      "blast",
	84532:      "base-sepolia",
	97476:      "doma-testnet",
	97477:      "doma",
	98866:      "plume",
	102030:     "creditcoin",
	102031:     "creditcoin-testnet",
	167000:     "taiko",
	167013:     "taiko-hoodi",
	202601:     "ronin-saigon",
	314159:     "filecoin-calibration",
	421614:     "arbitrum-sepolia",
	534351:     "scroll-sepolia",
	534352:     "scroll",
	543210:     "zero",
	560048:     "hoodi",
	737373:     "katana-testnet",
	743111:     "hemi-testnet",
	747474:     "katana",
	763373:     "ink-sepolia",
	808813:     "bob-testnet",
	1440000:    "xrpl",
	5042002:    "arc-testnet",
	6281971:    "dogeos-testnet",
	7777777:    "zora",
	11142220:   "celo-sepolia",
	11155111:   "sepolia",
	11155420:   "optimism-sepolia",
	728126428:  "tron",
	1666600000: "harmony-0",
}

// drpcNetworksURL is a var (not const) so tests can point it at a mock server.
var drpcNetworksURL = "https://lb.drpc.org/networks"

const DefaultDrpcRecheckInterval = 24 * time.Hour

type drpcNetworksResponse []struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Chains []struct {
		Name           string `json:"name"`
		ChainID        string `json:"chain_id"`
		Priority       int    `json:"priority"`
		APIType        string `json:"api_type"`
		BlockchainType string `json:"blockchain_type"`
		HasPremium     bool   `json:"has_premium"`
	} `json:"chains"`
}

type DrpcVendor struct {
	common.Vendor

	remoteDataLock          sync.Mutex
	remoteData              map[string]map[int64]string
	remoteDataLastFetchedAt map[string]time.Time
}

func CreateDrpcVendor() common.Vendor {
	return &DrpcVendor{
		remoteData:              make(map[string]map[int64]string),
		remoteDataLastFetchedAt: make(map[string]time.Time),
	}
}

func (v *DrpcVendor) Name() string {
	return "drpc"
}

func (v *DrpcVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}

	chainsURL, ok := settings["chainsUrl"].(string)
	if !ok || chainsURL == "" {
		chainsURL = drpcNetworksURL
	}

	if err = validateChainsURL(chainsURL); err != nil {
		return false, err
	}

	recheckInterval, ok := settings["recheckInterval"].(time.Duration)
	if !ok {
		recheckInterval = DefaultDrpcRecheckInterval
	}

	if err = v.ensureRemoteData(ctx, logger, recheckInterval, chainsURL); err != nil {
		logger.Warn().Err(err).Msg("could not fetch dRPC networks data on cold start, falling back to built-in network map")
		_, exists := defaultDrpcNetworkNames[chainID]
		return exists, nil
	}

	networks, ok := v.remoteData[chainsURL]
	if !ok || networks == nil {
		return false, nil
	}

	_, exists := networks[chainID]
	return exists, nil
}

func (v *DrpcVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	// Intentionally not ignore missing method exceptions because dRPC sometimes routes to nodes that don't support the method
	// but it doesn't mean that method is actually not supported, i.e. on next retry to dRPC it might work.
	upstream.AutoIgnoreUnsupportedMethods = &common.FALSE

	if upstream.Endpoint == "" {
		apiKey, ok := settings["apiKey"].(string)
		if !ok || apiKey == "" {
			return nil, fmt.Errorf("apiKey is required in drpc settings")
		}

		if upstream.Evm == nil {
			return nil, fmt.Errorf("drpc vendor requires upstream.evm to be defined")
		}
		chainID := upstream.Evm.ChainId
		if chainID == 0 {
			return nil, fmt.Errorf("drpc vendor requires upstream.evm.chainId to be defined")
		}

		chainsURL, ok := settings["chainsUrl"].(string)
		if !ok || chainsURL == "" {
			chainsURL = drpcNetworksURL
		}

		if err := validateChainsURL(chainsURL); err != nil {
			return nil, err
		}

		recheckInterval, ok := settings["recheckInterval"].(time.Duration)
		if !ok {
			recheckInterval = DefaultDrpcRecheckInterval
		}

		var networks map[int64]string
		if err := v.ensureRemoteData(ctx, logger, recheckInterval, chainsURL); err != nil {
			logger.Warn().Err(err).Msg("could not fetch dRPC networks data on cold start, falling back to built-in network map")
			networks = defaultDrpcNetworkNames
		} else {
			networks = v.remoteData[chainsURL]
		}

		netName, ok := networks[chainID]
		if !ok {
			return nil, fmt.Errorf("unsupported network chain ID for DRPC: %d", chainID)
		}

		drpcURL := fmt.Sprintf("https://lb.drpc.org/ogrpc?network=%s&dkey=%s", netName, apiKey)
		parsedURL, err := url.Parse(drpcURL)
		if err != nil {
			return nil, err
		}
		upstream.Endpoint = parsedURL.String()
		upstream.Type = common.UpstreamTypeEvm
	}

	if v.OwnsUpstream(upstream) && upstream.Type == "" {
		upstream.Type = common.UpstreamTypeEvm
	}

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *DrpcVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
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

		if strings.Contains(msg, "token is invalid") {
			return common.NewErrEndpointUnauthorized(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorUnauthorized,
					msg,
					nil,
					details,
				),
			)
		}

		if strings.Contains(msg, "ChainException: Unexpected error (code=40000)") ||
			strings.Contains(msg, "invalid block range") {
			return common.NewErrEndpointMissingData(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorMissingData,
					err.Message,
					nil,
					details,
				),
				req.LastUpstream(),
			)
		}
	}

	// Other errors can be properly handled by generic error handling
	return nil
}

func (v *DrpcVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "drpc://") || strings.HasPrefix(ups.Endpoint, "evm+drpc://") {
		return true
	}

	return strings.Contains(ups.Endpoint, ".drpc.org")
}

func (v *DrpcVendor) ensureRemoteData(ctx context.Context, logger *zerolog.Logger, recheckInterval time.Duration, chainsURL string) error {
	v.remoteDataLock.Lock()
	defer v.remoteDataLock.Unlock()

	if ltm, ok := v.remoteDataLastFetchedAt[chainsURL]; ok && time.Since(ltm) < recheckInterval {
		return nil
	}

	newData, err := v.fetchDrpcNetworks(ctx, logger, chainsURL)
	if err != nil {
		if _, ok := v.remoteData[chainsURL]; ok {
			logger.Warn().Err(err).Msg("could not refresh dRPC networks data; will use stale data")
			return nil
		}
		return err
	}

	v.remoteData[chainsURL] = newData
	v.remoteDataLastFetchedAt[chainsURL] = time.Now()
	return nil
}

func (v *DrpcVendor) fetchDrpcNetworks(ctx context.Context, logger *zerolog.Logger, chainsURL string) (map[int64]string, error) {
	var httpClient = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
		},
	}

	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, "GET", chainsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("dRPC networks API returned non-200 code: %d", resp.StatusCode)
	}

	var networks drpcNetworksResponse
	if err := json.NewDecoder(resp.Body).Decode(&networks); err != nil {
		return nil, fmt.Errorf("failed to parse dRPC networks response: %w", err)
	}

	// Only include EVM JSON-RPC chains with premium access; prefer highest priority when a chain ID appears multiple times.
	type candidate struct {
		name     string
		priority int
	}
	best := make(map[int64]candidate)
	for _, network := range networks {
		for _, chain := range network.Chains {
			if chain.BlockchainType != "eth" || chain.APIType != "jsonrpc" || chain.ChainID == "" || !chain.HasPremium {
				continue
			}
			chainIDStr := strings.TrimPrefix(chain.ChainID, "0x")
			chainID, err := strconv.ParseInt(chainIDStr, 16, 64)
			if err != nil {
				logger.Debug().Str("chain_id", chain.ChainID).Err(err).Msg("failed to parse dRPC chain ID")
				continue
			}
			if cur, ok := best[chainID]; !ok || chain.Priority > cur.priority {
				best[chainID] = candidate{name: chain.Name, priority: chain.Priority}
			}
		}
	}

	networkNames := make(map[int64]string, len(best))
	for cid, c := range best {
		networkNames[cid] = c.name
		logger.Trace().Int64("chain_id", cid).Str("name", c.name).Int("priority", c.priority).Msg("selected dRPC network name")
	}

	logger.Info().Int("count", len(networkNames)).Str("url", chainsURL).Msg("successfully fetched dRPC network names")

	return networkNames, nil
}
