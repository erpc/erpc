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

var alchemyNetworkSubdomains = map[uint64]string{
	// Ethereum
	1:        "eth-mainnet",
	5:        "eth-goerli",
	11155111: "eth-sepolia",

	// Polygon
	137:   "polygon-mainnet",
	80001: "polygon-mumbai",

	// Optimism
	10:       "opt-mainnet",
	420:      "opt-goerli",
	11155420: "opt-sepolia",

	// Arbitrum
	42161:  "arb-mainnet",
	421613: "arb-goerli",
	421614: "arb-sepolia",

	// Astar
	592: "astar-mainnet",

	// Polygon zkEVM
	1101: "polygonzk-mainnet",
	1442: "polygonzk-testnet",

	// Base
	8453:  "base-mainnet",
	84531: "base-goerli",
	84532: "base-sepolia",

	// zkSync
	324: "zksync-mainnet",
	300: "zksync-sepolia",

	// Fantom Opera
	250:  "fantom-mainnet",
	4002: "fantom-testnet",

	// BSC (Binance Smart Chain)
	56: "bsc-mainnet",
	97: "bsc-testnet",

	// Avalanche
	43114: "avalanche-mainnet",
	43113: "avalanche-fuji",

	// Blast
	81457:     "blast-mainnet",
	168587773: "blast-sepolia",

	// Zeta
	7000: "zeta-mainnet",
	7001: "zeta-testnet",
}

type AlchemyHttpJsonRpcClient struct {
	upstream *Upstream
	apiKey   string
	clients  map[string]HttpJsonRpcClient
	mu       sync.RWMutex
}

func NewAlchemyHttpJsonRpcClient(pu *Upstream, parsedUrl *url.URL) (HttpJsonRpcClient, error) {
	if !strings.HasSuffix(parsedUrl.Scheme, "alchemy") {
		return nil, fmt.Errorf("invalid Alchemy URL scheme: %s", parsedUrl.Scheme)
	}

	apiKey := parsedUrl.Host
	if apiKey == "" {
		return nil, fmt.Errorf("missing Alchemy API key in URL")
	}

	return &AlchemyHttpJsonRpcClient{
		upstream: pu,
		apiKey:   apiKey,
		clients:  make(map[string]HttpJsonRpcClient),
	}, nil
}

func (c *AlchemyHttpJsonRpcClient) GetType() ClientType {
	return ClientTypeAlchemyHttpJsonRpc
}

func (c *AlchemyHttpJsonRpcClient) SupportsNetwork(networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainId, err := strconv.ParseUint(networkId[4:], 10, 64)
	if err != nil {
		return false, err
	}

	_, ok := alchemyNetworkSubdomains[chainId]
	return ok, nil
}

func (c *AlchemyHttpJsonRpcClient) getOrCreateClient(network common.Network) (HttpJsonRpcClient, error) {
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
		return nil, fmt.Errorf("unsupported network architecture for Alchemy client: %s", network.Architecture())
	}

	chainID, err := network.EvmChainId()
	if err != nil {
		return nil, err
	}

	subdomain, ok := alchemyNetworkSubdomains[chainID]
	if !ok {
		return nil, fmt.Errorf("unsupported network chain ID for Alchemy: %d", chainID)
	}

	alchemyURL := fmt.Sprintf("https://%s.g.alchemy.com/v2/%s", subdomain, c.apiKey)
	parsedURL, err := url.Parse(alchemyURL)
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

func (c *AlchemyHttpJsonRpcClient) SendRequest(ctx context.Context, req *NormalizedRequest) (*NormalizedResponse, error) {
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
