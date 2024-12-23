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

var etherspotMainnets = map[int64]string{
	1:         "ethereum",
	137:       "polygon",
	10:        "optimism",
	42161:     "arbitrum",
	122:       "fuse",
	5000:      "mantle",
	100:       "gnosis",
	8453:      "base",
	43114:     "avalanche",
	56:        "bsc",
	59144:     "linea",
	14:        "flare",
	534352:    "scroll",
	30:        "rootstock",
	888888888: "ancient8",
}

var etherspotTestnets = map[int64]struct{}{
	80002:    {}, // Amoy
	11155111: {}, // Sepolia
	84532:    {}, // Base Sepolia
	421614:   {}, // Arbitrum Sepolia
	11155420: {}, // OP Sepolia
	534351:   {}, // Scroll Sepolia
	5003:     {}, // Mantle Sepolia
	114:      {}, // Flare Testnet Coston2
	123:      {}, // Fuse Sparknet
	28122024: {}, // Ancient8 Testnet
	31:       {}, // Rootstock Testnet
}

type EtherspotHttpJsonRpcClient struct {
	appCtx   context.Context
	upstream *Upstream
	apiKey   string
	clients  map[int64]HttpJsonRpcClient
	mu       sync.RWMutex
}

func NewEtherspotHttpJsonRpcClient(appCtx context.Context, pu *Upstream, parsedUrl *url.URL) (HttpJsonRpcClient, error) {
	if !strings.HasSuffix(parsedUrl.Scheme, "etherspot") {
		return nil, fmt.Errorf("invalid Etherspot URL scheme: %s", parsedUrl.Scheme)
	}

	apiKey := parsedUrl.Host
	if apiKey == "" {
		return nil, fmt.Errorf("missing Etherspot API key in URL")
	}

	return &EtherspotHttpJsonRpcClient{
		appCtx:   appCtx,
		upstream: pu,
		apiKey:   apiKey,
		clients:  make(map[int64]HttpJsonRpcClient),
	}, nil
}

func (c *EtherspotHttpJsonRpcClient) GetType() ClientType {
	return ClientTypeEtherspotHttpJsonRpc
}

func (c *EtherspotHttpJsonRpcClient) SupportsNetwork(ctx context.Context, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainId, err := strconv.ParseInt(networkId[4:], 10, 64)
	if err != nil {
		return false, err
	}

	if _, ok := etherspotMainnets[chainId]; ok {
		return true, nil
	}

	if _, ok := etherspotTestnets[chainId]; ok {
		return true, nil
	}

	return false, nil
}

func (c *EtherspotHttpJsonRpcClient) createClient(chainID int64) (HttpJsonRpcClient, error) {
	c.mu.RLock()
	client, exists := c.clients[chainID]
	c.mu.RUnlock()

	if exists {
		return client, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check to ensure another goroutine hasn't created the client
	if client, exists := c.clients[chainID]; exists {
		return client, nil
	}

	var etherspotURL string
	if networkName, ok := etherspotMainnets[chainID]; ok {
		// Mainnet URL structure using network name
		etherspotURL = fmt.Sprintf("https://%s-bundler.etherspot.io/", networkName)
	} else if _, ok := etherspotTestnets[chainID]; ok {
		// Testnet URL structure
		etherspotURL = fmt.Sprintf("https://testnet-rpc.etherspot.io/v1/%d", chainID)
	}

	if c.apiKey != "public" {
		etherspotURL = fmt.Sprintf("%s?apikey=%s", etherspotURL, c.apiKey)
	}

	parsedURL, err := url.Parse(etherspotURL)
	if err != nil {
		return nil, err
	}

	client, err = NewGenericHttpJsonRpcClient(c.appCtx, &c.upstream.Logger, c.upstream, parsedURL)
	if err != nil {
		return nil, err
	}

	c.clients[chainID] = client
	return client, nil
}

func (c *EtherspotHttpJsonRpcClient) getOrCreateClient(network common.Network) (HttpJsonRpcClient, error) {
	if network.Architecture() != common.ArchitectureEvm {
		return nil, fmt.Errorf("unsupported network architecture for Etherspot client: %s", network.Architecture())
	}

	chainID, err := network.EvmChainId()
	if err != nil {
		return nil, err
	}

	return c.createClient(chainID)
}

func (c *EtherspotHttpJsonRpcClient) SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
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
