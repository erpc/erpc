package upstream

import (
	"context"
	"fmt"
	"math/rand"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
)

var envioKnownSupportedChains = map[int64]struct{}{
	42161:      {}, // Arbitrum
	42170:      {}, // Arbitrum Nova
	421614:     {}, // Arbitrum Sepolia
	1313161554: {}, // Aurora
	43114:      {}, // Avalanche
	8453:       {}, // Base
	84532:      {}, // Base Sepolia
	81457:      {}, // Blast
	168587773:  {}, // Blast Sepolia
	288:        {}, // Boba
	56:         {}, // Bsc
	97:         {}, // Bsc Testnet
	2001:       {}, // C1 Milkomeda
	42220:      {}, // Celo
	44:         {}, // Crab
	7560:       {}, // Cyber
	46:         {}, // Darwinia
	1:          {}, // Ethereum Mainnet
	250:        {}, // Fantom
	14:         {}, // Flare
	43113:      {}, // Fuji
	100:        {}, // Gnosis
	10200:      {}, // Gnosis Chiado
	5:          {}, // Goerli
	1666600000: {}, // Harmony Shard 0
	17000:      {}, // Holesky
	9090:       {}, // Inco Gentry Testnet
	1802203764: {}, // Kakarot Sepolia
	255:        {}, // Kroma
	59144:      {}, // Linea
	42:         {}, // Lukso
	169:        {}, // Manta
	5000:       {}, // Mantle
	1088:       {}, // Metis
	17864:      {}, // Mev Commit
	1284:       {}, // Moonbeam
	245022934:  {}, // Neon Evm
	10:         {}, // Optimism
	11155420:   {}, // Optimism Sepolia
	137:        {}, // Polygon
	80002:      {}, // Polygon Amoy
	1101:       {}, // Polygon zkEVM
	424:        {}, // Publicgoods
	30:         {}, // Rsk
	534352:     {}, // Scroll
	11155111:   {}, // Sepolia
	148:        {}, // Shimmer Evm
	196:        {}, // X Layer
	195:        {}, // X Layer Testnet
	7000:       {}, // Zeta
	324:        {}, // ZKsync
	7777777:    {}, // Zora
}

type EnvioHttpJsonRpcClient struct {
	upstream   *Upstream
	rootDomain string
	clients    map[int64]HttpJsonRpcClient
	mu         sync.RWMutex
}

func NewEnvioHttpJsonRpcClient(pu *Upstream, parsedUrl *url.URL) (HttpJsonRpcClient, error) {
	if !strings.HasSuffix(parsedUrl.Scheme, "envio") {
		return nil, fmt.Errorf("invalid Envio URL scheme: %s", parsedUrl.Scheme)
	}

	rootDomain := parsedUrl.Host
	if rootDomain == "" {
		return nil, fmt.Errorf("missing Envio root domain in URL")
	}

	return &EnvioHttpJsonRpcClient{
		upstream:   pu,
		rootDomain: rootDomain,
		clients:    make(map[int64]HttpJsonRpcClient),
	}, nil
}

func (c *EnvioHttpJsonRpcClient) GetType() ClientType {
	return ClientTypeEnvioHttpJsonRpc
}

func (c *EnvioHttpJsonRpcClient) SupportsNetwork(networkId string) (bool, error) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rid := rand.Intn(100_000_000) // #nosec G404
	pr := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_chainId","params":[]}`, rid)))
	resp, err := client.SendRequest(ctx, pr)
	if err != nil {
		return false, err
	}

	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return false, err
	}

	cidh, err := common.NormalizeHex(jrr.Result)
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

	client, err = NewGenericHttpJsonRpcClient(&c.upstream.Logger, c.upstream, parsedURL)
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
