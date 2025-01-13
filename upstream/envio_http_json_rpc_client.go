package upstream

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

type EnvioHttpJsonRpcClient struct {
	appCtx     context.Context
	upstream   *Upstream
	rootDomain string
	clients    map[int64]HttpJsonRpcClient
	mu         sync.RWMutex
}

func NewEnvioHttpJsonRpcClient(appCtx context.Context, pu *Upstream, parsedUrl *url.URL) (HttpJsonRpcClient, error) {
	if !strings.HasSuffix(parsedUrl.Scheme, "envio") {
		return nil, fmt.Errorf("invalid Envio URL scheme: %s", parsedUrl.Scheme)
	}

	rootDomain := parsedUrl.Host
	if rootDomain == "" {
		return nil, fmt.Errorf("missing Envio root domain in URL")
	}

	return &EnvioHttpJsonRpcClient{
		appCtx:     appCtx,
		upstream:   pu,
		rootDomain: rootDomain,
		clients:    make(map[int64]HttpJsonRpcClient),
	}, nil
}

func (c *EnvioHttpJsonRpcClient) GetType() ClientType {
	return ClientTypeEnvioHttpJsonRpc
}

func (c *EnvioHttpJsonRpcClient) SupportsNetwork(ctx context.Context, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainId, err := strconv.ParseInt(networkId[4:], 10, 64)
	if err != nil {
		return false, err
	}

	if _, ok := envioKnownSupportedChains[chainId]; ok {
		return true, nil
	}

	// Check against endpoint to see if eth_chainId responds successfully
	client, err := c.createClient(chainId)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pr := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_chainId","params":[]}`, util.RandomID())))
	resp, err := client.SendRequest(ctx, pr)
	if err != nil {
		return false, err
	}

	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return false, err
	}

	cids, err := jrr.PeekStringByPath()
	if err != nil {
		return false, err
	}

	cidh, err := common.NormalizeHex(cids)
	if err != nil {
		return false, err
	}

	cid, err := common.HexToInt64(cidh)
	if err != nil {
		return false, err
	}

	return cid == chainId, nil
}

func (c *EnvioHttpJsonRpcClient) createClient(chainID int64) (HttpJsonRpcClient, error) {
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

	envioURL := fmt.Sprintf("https://%d.%s", chainID, c.rootDomain)
	parsedURL, err := url.Parse(envioURL)
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

func (c *EnvioHttpJsonRpcClient) getOrCreateClient(network common.Network) (HttpJsonRpcClient, error) {
	if network.Architecture() != common.ArchitectureEvm {
		return nil, fmt.Errorf("unsupported network architecture for Envio client: %s", network.Architecture())
	}

	chainID, err := network.EvmChainId()
	if err != nil {
		return nil, err
	}

	return c.createClient(chainID)
}

func (c *EnvioHttpJsonRpcClient) SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
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
