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

var alchemyNetworkSubdomains = map[int64]string{
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

	chainId, err := strconv.ParseInt(networkId[4:], 10, 64)
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

func (c *AlchemyHttpJsonRpcClient) SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
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
