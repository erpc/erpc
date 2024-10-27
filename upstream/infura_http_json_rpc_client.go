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

var infuraNetworkNames = map[int64]string{
	42161: "arbitrum-mainnet",
	421614: "arbitrum-sepolia",
	43114: "avalanche-mainnet",
	43113: "avalanche-fuji",
    8453: "base-mainnet",
	84532: "base-sepolia",
	56: "bsc-mainnet",
	97: "bsc-testnet",
	81457: "blast-mainnet",
	168587773: "blast-sepolia",
	42220: "celo-mainnet",
	44787: "celo-alfajores",
	1: "mainnet",
	17000: "holesky",
	11155111: "sepolia",
	59144: "linea-mainnet",
	59141: "linea-sepolia",
	5000: "mantle-mainnet",
	5003: "mantle-sepolia",
	204: "opbnb-mainnet",
	5611: "opbnb-testnet",
	10: "optimism-mainnet",
	11155420: "optimism-sepolia",
	11297108109: "palm-mainnet",
	11297108099: "palm-testnet",
	137: "polygon-mainnet",
	80002: "polygon-amoy",
	534352: "scroll-mainnet",
	534351: "scroll-sepolia",
	324: "zksync-mainnet",
	300: "zksync-sepolia",

}

type InfuraHttpJsonRpcClient struct {
	upstream *Upstream
	apiKey   string
	clients  map[string]HttpJsonRpcClient
	mu       sync.RWMutex
}

func NewInfuraHttpJsonRpcClient(pu *Upstream, parsedUrl *url.URL) (HttpJsonRpcClient, error) {
	if !strings.HasSuffix(parsedUrl.Scheme, "infura") {
		return nil, fmt.Errorf("invalid Infura URL scheme: %s", parsedUrl.Scheme)
	}

	apiKey := parsedUrl.Host
	if apiKey == "" {
		return nil, fmt.Errorf("missing Infura API key in URL")
	}

	return &InfuraHttpJsonRpcClient{
		upstream: pu,
		apiKey:   apiKey,
		clients:  make(map[string]HttpJsonRpcClient),
	}, nil
}

func (c *InfuraHttpJsonRpcClient) GetType() ClientType {
	return ClientTypeInfuraHttpJsonRpc
}

func (c *InfuraHttpJsonRpcClient) SupportsNetwork(networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainId, err := strconv.ParseInt(networkId[4:], 10, 64)
	if err != nil {
		return false, err
	}

	_, ok := infuraNetworkNames[chainId]
	return ok, nil
}

func (c *InfuraHttpJsonRpcClient) getOrCreateClient(network common.Network) (HttpJsonRpcClient, error) {
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
		return nil, fmt.Errorf("unsupported network architecture for Infura client: %s", network.Architecture())
	}

	chainID, err := network.EvmChainId()
	if err != nil {
		return nil, err
	}

	netName, ok := infuraNetworkNames[chainID]
	if !ok {
		return nil, fmt.Errorf("unsupported network chain ID for Infura: %d", chainID)
	} infuraURL
	infuraURL := fmt.Sprintf("https://%s.infura.io/v3/%s", netName, c.apiKey)
	if netName == "ava-mainnet" || netName == "ava-testnet" {
		// Avalanche endpoints need an extra path `/ext/bc/C/rpc`
		infuraURL = fmt.Sprintf("%s/ext/bc/C/rpc", infuraURL)
	}
	parsedURL, err := url.Parse(infuraURL)
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

func (c *InfuraHttpJsonRpcClient) SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
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
