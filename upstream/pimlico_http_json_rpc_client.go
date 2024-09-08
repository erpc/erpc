package upstream

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
)

var pimlicoSupportedChains = map[int64]struct{}{
	1:           {}, // ethereum
	11155111:    {}, // sepolia
	42161:       {}, // arbitrum
	421614:      {}, // arbitrum-sepolia
	137:         {}, // polygon
	80002:       {}, // polygon-amoy
	10:          {}, // optimism
	11155420:    {}, // optimism-sepolia
	7777777:     {}, // zora
	999999999:   {}, // zora-sepolia
	100:         {}, // gnosis
	10200:       {}, // chiado-testnet
	59144:       {}, // linea
	59141:       {}, // linea-sepolia
	8453:        {}, // base
	84532:       {}, // base-sepolia
	690:         {}, // redstone
	17069:       {}, // garnet-holesky
	43114:       {}, // avalanche
	43113:       {}, // avalanche-fuji
	534352:      {}, // scroll
	534351:      {}, // scroll-sepolia-testnet
	42220:       {}, // celo
	44787:       {}, // celo-alfajores-testnet
	56:          {}, // binance
	97:          {}, // binance-testnet
	7560:        {}, // cyber-mainnet
	111557560:   {}, // cyber-testnet
	53935:       {}, // dfk-chain
	335:         {}, // dfk-chain-test
	8217:        {}, // klaytn-cypress
	1001:        {}, // klaytn-baobab
	34443:       {}, // mode
	919:         {}, // mode-sepolia
	660279:      {}, // xai
	37714555429: {}, // xai-sepolia-orbit
	81457:       {}, // blast
	168587773:   {}, // blast-sepolia
	888888888:   {}, // ancient8
	28122024:    {}, // ancient8-testnet
	41455:       {}, // alephzero
	2039:        {}, // alephzero-testnet
	122:         {}, // fuse
	123:         {}, // fuse-sparknet
	60808:       {}, // bob
	111:         {}, // bob-sepolia
	7979:        {}, // dos-mainnet
	3939:        {}, // dos-testnet
	204:         {}, // opbnb
	42170:       {}, // arbitrum-nova
	978657:      {}, // treasure-ruby
	22222:       {}, // nautilus
	252:         {}, // fraxtal
	7887:        {}, // kinto
	957:         {}, // lyra
	5000:        {}, // mantle
	132902:      {}, // form-testnet
	167008:      {}, // taiko-katla-l2
	1513:        {}, // story-testnet
	90354:       {}, // camp-sepolia
	1993:        {}, // b3-sepolia
	161221135:   {}, // plume-testnet
	98985:       {}, // superposition-testnet
	3397901:     {}, // funki-testnet
	78600:       {}, // vanguard-testnet
	4202:        {}, // lisk-sepolia
	31:          {}, // rootstock-testnet
}

type PimlicoHttpJsonRpcClient struct {
	upstream *Upstream
	apiKey   string
	clients  map[int64]HttpJsonRpcClient
	mu       sync.RWMutex
}

func NewPimlicoHttpJsonRpcClient(pu *Upstream, parsedUrl *url.URL) (HttpJsonRpcClient, error) {
	if !strings.HasSuffix(parsedUrl.Scheme, "pimlico") {
		return nil, fmt.Errorf("invalid Pimlico URL scheme: %s", parsedUrl.Scheme)
	}

	apiKey := parsedUrl.Host
	if apiKey == "" {
		return nil, fmt.Errorf("missing Pimlico API key in URL")
	}

	return &PimlicoHttpJsonRpcClient{
		upstream: pu,
		apiKey:   apiKey,
		clients:  make(map[int64]HttpJsonRpcClient),
	}, nil
}

func (c *PimlicoHttpJsonRpcClient) GetType() ClientType {
	return ClientTypePimlicoHttpJsonRpc
}

func (c *PimlicoHttpJsonRpcClient) SupportsNetwork(networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainId, err := strconv.ParseInt(networkId[4:], 10, 64)
	if err != nil {
		return false, err
	}

	if _, ok := pimlicoSupportedChains[chainId]; ok {
		return true, nil
	}

	// Check against endpoint to see if eth_chainId responds successfully
	client, err := c.createClient(chainId)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeoutCause(context.Background(), 10*time.Second, errors.New("pimlico client timeout during eth_chainId"))
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

func (c *PimlicoHttpJsonRpcClient) getOrCreateClient(network common.Network) (HttpJsonRpcClient, error) {
	if network.Architecture() != common.ArchitectureEvm {
		return nil, fmt.Errorf("unsupported network architecture for Pimlico client: %s", network.Architecture())
	}

	chainID, err := network.EvmChainId()
	if err != nil {
		return nil, err
	}

	return c.createClient(chainID)
}

func (c *PimlicoHttpJsonRpcClient) createClient(chainID int64) (HttpJsonRpcClient, error) {
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

	var pimlicoURL string
	if c.apiKey == "public" {
		pimlicoURL = fmt.Sprintf("https://public.pimlico.io/v2/%d/rpc", chainID)
	} else {
		pimlicoURL = fmt.Sprintf("https://api.pimlico.io/v2/%d/rpc?apikey=%s", chainID, c.apiKey)
	}
	parsedURL, err := url.Parse(pimlicoURL)
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

func (c *PimlicoHttpJsonRpcClient) SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
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
