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

var blastapiNetworkNames = map[int64]string{
	1:           "eth-mainnet",
	10:          "optimism-mainnet",
	100:         "gnosis-mainnet",
	10000:       "crynux-testnet",
	10200:       "gnosis-chiado",
	1075:        "iota-testnet-evm",
	1088:        "metis-mainnet",
	1100:        "dymension-mainnet",
	1101:        "polygon-zkevm-mainnet",
	111:         "dymension-testnet",
	11124:       "abstract-testnet",
	11155111:    "eth-sepolia",
	11155420:    "optimism-sepolia",
	1122:        "nim-mainnet",
	11297108099: "palm-testnet",
	11297108109: "palm-mainnet",
	1231:        "rivalz-testnet",
	1284:        "moonbeam",
	1285:        "moonriver",
	1287:        "moonbase-alpha",
	137:         "polygon-mainnet",
	168587773:   "blastl2-sepolia",
	17000:       "eth-holesky",
	18071918:    "mande-mainnet",
	204:         "opbnb-mainnet",
	2442:        "polygon-zkevm-cardona",
	250:         "fantom-mainnet",
	300:         "zksync-sepolia",
	324:         "zksync-mainnet",
	336:         "shiden",
	34443:       "mode-mainnet",
	4002:        "fantom-testnet",
	4157:        "crossfi-testnet",
	420:         "optimism-goerli",
	42161:       "arbitrum-one",
	421614:      "arbitrum-sepolia",
	42170:       "arbitrum-nova",
	43113:       "ava-testnet",
	43114:       "ava-mainnet",
	5:           "eth-goerli",
	5000:        "mantle-mainnet",
	5003:        "mantle-sepolia",
	534351:      "scroll-sepolia",
	534352:      "scroll-mainnet",
	56:          "bsc-mainnet",
	5611:        "opbnb-testnet",
	59140:       "linea-goerli",
	59141:       "linea-sepolia",
	59144:       "linea-mainnet",
	592:         "astar",
	60808:       "bob-mainnet",
	62298:       "citrea-signet",
	66:          "oktc-mainnet",
	7000:        "zetachain-mainnet",
	7001:        "zetachain-testnet",
	80001:       "polygon-testnet",
	80002:       "polygon-amoy",
	80084:       "berachain-bartio",
	808813:      "bob-sepolia",
	81:          "shibuya",
	81457:       "blastl2-mainnet",
	8453:        "base-mainnet",
	84531:       "base-goerli",
	84532:       "base-sepolia",
	8822:        "iota-mainnet-evm",
	9001:        "evmos-mainnet",
	919:         "mode-sepolia",
	97:          "bsc-testnet",
}

type BlastapiHttpJsonRpcClient struct {
	upstream *Upstream
	apiKey   string
	clients  map[string]HttpJsonRpcClient
	mu       sync.RWMutex
}

func NewBlastapiHttpJsonRpcClient(pu *Upstream, parsedUrl *url.URL) (HttpJsonRpcClient, error) {
	if !strings.HasSuffix(parsedUrl.Scheme, "blastapi") {
		return nil, fmt.Errorf("invalid BlastAPI URL scheme: %s", parsedUrl.Scheme)
	}

	apiKey := parsedUrl.Host
	if apiKey == "" {
		return nil, fmt.Errorf("missing BlastAPI API key in URL")
	}

	return &BlastapiHttpJsonRpcClient{
		upstream: pu,
		apiKey:   apiKey,
		clients:  make(map[string]HttpJsonRpcClient),
	}, nil
}

func (c *BlastapiHttpJsonRpcClient) GetType() ClientType {
	return ClientTypeBlastapiHttpJsonRpc
}

func (c *BlastapiHttpJsonRpcClient) SupportsNetwork(networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainId, err := strconv.ParseInt(networkId[4:], 10, 64)
	if err != nil {
		return false, err
	}

	_, ok := blastapiNetworkNames[chainId]
	return ok, nil
}

func (c *BlastapiHttpJsonRpcClient) getOrCreateClient(network common.Network) (HttpJsonRpcClient, error) {
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
		return nil, fmt.Errorf("unsupported network architecture for BlastAPI client: %s", network.Architecture())
	}

	chainID, err := network.EvmChainId()
	if err != nil {
		return nil, err
	}

	netName, ok := blastapiNetworkNames[chainID]
	if !ok {
		return nil, fmt.Errorf("unsupported network chain ID for BlastAPI: %d", chainID)
	}
	blastapiURL := fmt.Sprintf("https://%s.blastapi.io/%s", netName, c.apiKey)
	if netName == "ava-mainnet" || netName == "ava-testnet" {
		// Avalanche endpoints need an extra path `/ext/bc/C/rpc`
		blastapiURL = fmt.Sprintf("%s/ext/bc/C/rpc", blastapiURL)
	}
	parsedURL, err := url.Parse(blastapiURL)
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

func (c *BlastapiHttpJsonRpcClient) SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
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
