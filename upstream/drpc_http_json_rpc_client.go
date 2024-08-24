package upstream

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/erpc/erpc/common"
)

var drpcNetworkNames = map[int64]string{
	1:           "ethereum",
	11155111:    "sepolia",
	17000:       "holesky",
	56:          "bsc",
	97:          "bsc-testnet",
	137:         "polygon",
	80002:       "polygon-amoy",
	42161:       "arbitrum",
	421614:      "arbitrum-sepolia",
	10:          "optimism",
	11155420:    "optimism-sepolia",
	324:         "zksync",
	300:         "zksync-sepolia",
	59144:       "linea",
	59141:       "linea-sepolia",
	8453:        "base",
	84532:       "base-sepolia",
	250:         "fantom",
	4002:        "fantom-testnet",
	43114:       "avalanche",
	43113:       "avalanche-fuji",
	100:         "gnosis",
	10200:       "gnosis-chiado",
	534352:      "scroll",
	534351:      "scroll-sepolia",
	5000:        "mantle",
	5003:        "mantle-sepolia",
	42170:       "arbitrum-nova",
	1313161554:  "aurora",
	1313161555:  "aurora-testnet",
	1101:        "polygon-zkevm",
	2442:        "polygon-zkevm-cardona",
	8217:        "klaytn",
	1001:        "klaytn-baobab",
	41455:       "alephzero",
	2039:        "alephzero-sepolia",
	88153591557: "arb-blueberry-testnet",
	80084:       "bartio",
	199:         "bittorrent",
	81457:       "blast",
	168587773:   "blast-sepolia",
	60808:       "bob",
	808813:      "bob-testnet",
	56288:       "boba-bnb",
	288:         "boba-eth",
	42220:       "celo",
	44787:       "celo-alfajores",
	1116:        "core",
	1115:        "core-testnet",
	25:          "cronos",
	338:         "cronos-testnet",
	1100:        "dymension",
	6398:        "everclear-sepolia",
	9001:        "evmos",
	9000:        "evmos-testnet",
	314:         "filecoin",
	314159:      "filecoin-calibration",
	252:         "fraxtal",
	2522:        "fraxtal-testnet",
	122:         "fuse",
	10888:       "gameswift-testnet",
	3456:        "goat-testnet",
	11235:       "haqq",
	54211:       "haqq-testnet",
	1666600000:  "harmony-0",
	1666600001:  "harmony-1",
	13371:       "immutable-zkevm",
	13473:       "immutable-zkevm-testnet",
	2222:        "kava",
	2221:        "kava-testnet",
	255:         "kroma",
	2358:        "kroma-sepolia",
	1135:        "lisk",
	4202:        "lisk-sepolia",
	169:         "manta-pacific",
	3441006:     "manta-pacific-sepolia",
	1750:        "metall2",
	1740:        "metall2-testnet",
	1088:        "metis",
	34443:       "mode",
	919:         "mode-testnet",
	1284:        "moonbeam",
	1287:        "moonbase-alpha",
	1285:        "moonriver",
	245022934:   "neon-evm",
	245022926:   "neon-evm-devnet",
	66:          "oktc",
	65:          "oktc-testnet",
	204:         "opbnb",
	5611:        "opbnb-testnet",
	123420111:   "opcelestia-raspberry-testnet",
	656476:      "open-campus-codex-sepolia",
	1829:        "playnance",
	94204209:    "polygon-blackberry-testnet",
	111188:      "real",
	2020:        "ronin",
	30:          "rootstock",
	31:          "rootstock-testnet",
	167000:      "taiko",
	167009:      "taiko-hekla",
	40:          "telos",
	41:          "telos-testnet",
	108:         "thundercore",
	18:          "thundercore-testnet",
	728126428:   "tron",
	2494104990:  "tron-shasta",
	1111:        "wemix",
	1112:        "wemix-testnet",
	7000:        "zeta-chain",
	7001:        "zeta-chain-testnet",
	48900:       "zircuit-mainnet",
	48899:       "zircuit-testnet",
	7777777:     "zora",
	999999999:   "zora-sepolia",
}

type DrpcHttpJsonRpcClient struct {
	upstream *Upstream
	apiKey   string
	clients  map[string]HttpJsonRpcClient
	mu       sync.RWMutex
}

func NewDrpcHttpJsonRpcClient(pu *Upstream, parsedUrl *url.URL) (HttpJsonRpcClient, error) {
	if !strings.HasSuffix(parsedUrl.Scheme, "drpc") {
		return nil, fmt.Errorf("invalid DRPC URL scheme: %s", parsedUrl.Scheme)
	}

	apiKey := parsedUrl.Host
	if apiKey == "" {
		return nil, fmt.Errorf("missing DRPC API key in URL")
	}

	return &DrpcHttpJsonRpcClient{
		upstream: pu,
		apiKey:   apiKey,
		clients:  make(map[string]HttpJsonRpcClient),
	}, nil
}

func (c *DrpcHttpJsonRpcClient) GetType() ClientType {
	return ClientTypeDrpcHttpJsonRpc
}

func (c *DrpcHttpJsonRpcClient) SupportsNetwork(networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainId, err := strconv.ParseInt(networkId[4:], 10, 64)
	if err != nil {
		return false, err
	}

	_, ok := drpcNetworkNames[chainId]
	return ok, nil
}

func (c *DrpcHttpJsonRpcClient) getOrCreateClient(network common.Network) (HttpJsonRpcClient, error) {
	networkID := network.Id()
	c.mu.RLock()
	client, exists := c.clients[networkID]
	c.mu.RUnlock()

	if exists {
		return client, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check to ensure another goroutine hasn't created the client
	if client, exists := c.clients[networkID]; exists {
		return client, nil
	}

	if network.Architecture() != common.ArchitectureEvm {
		return nil, fmt.Errorf("unsupported network architecture for DRPC client: %s", network.Architecture())
	}

	chainID, err := network.EvmChainId()
	if err != nil {
		return nil, err
	}

	netName, ok := drpcNetworkNames[chainID]
	if !ok {
		return nil, fmt.Errorf("unsupported network chain ID for DRPC: %d", chainID)
	}

	drpcURL := fmt.Sprintf("https://lb.drpc.org/ogrpc?network=%s&dkey=%s", netName, c.apiKey)
	parsedURL, err := url.Parse(drpcURL)
	if err != nil {
		return nil, err
	}

	client, err = NewGenericHttpJsonRpcClient(&c.upstream.Logger, c.upstream, parsedURL)
	if err != nil {
		return nil, err
	}

	c.clients[networkID] = client
	return client, nil
}

func (c *DrpcHttpJsonRpcClient) SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	network := req.Network()
	if network == nil {
		return nil, fmt.Errorf("network information is missing in the request")
	}

	client, err := c.getOrCreateClient(network)
	if err != nil {
		return nil, err
	}

	return client.SendRequest(ctx, req)
}
