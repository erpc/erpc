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

var thirdwebNetworkSubdomains = map[int64]struct{}{
	1:           {}, // Ethereum Mainnet
	10:          {}, // OP Mainnet
	56:          {}, // BNB Smart Chain Mainnet
	100:         {}, // Gnosis
	137:         {}, // Polygon Mainnet
	204:         {}, // opBNB Mainnet
	252:         {}, // Fraxtal
	324:         {}, // zkSync Mainnet
	5000:        {}, // Mantle
	7560:        {}, // Cyber Mainnet
	8333:        {}, // B3
	8453:        {}, // Base
	34443:       {}, // Mode
	42026:       {}, // Donatuz
	42161:       {}, // Arbitrum One
	42170:       {}, // Arbitrum Nova
	43114:       {}, // Avalanche C-Chain
	59144:       {}, // Linea
	84532:       {}, // Base Sepolia Testnet
	421614:      {}, // Arbitrum Sepolia
	534352:      {}, // Scroll
	660279:      {}, // Xai Mainnet
	7777777:     {}, // Zora
	11155111:    {}, // Sepolia
	11155420:    {}, // OP Sepolia Testnet
	666666666:   {}, // Degen Chain
	888888888:   {}, // Ancient8
	30:          {}, // Rootstock Mainnet
	31:          {}, // Rootstock Testnet
	47:          {}, // Xpla Testnet
	97:          {}, // BNB Smart Chain Testnet
	122:         {}, // Fuse Mainnet
	123:         {}, // Fuse Sparknet
	250:         {}, // Fantom Opera
	288:         {}, // Boba Network
	300:         {}, // zkSync Sepolia Testnet
	302:         {}, // zkCandy Sepolia Testnet
	335:         {}, // DFK Chain Test
	690:         {}, // Redstone
	919:         {}, // Mode Testnet
	957:         {}, // Lyra Chain
	1001:        {}, // Kaia Testnet Kairos
	1088:        {}, // Metis Andromeda Mainnet
	1101:        {}, // Polygon zkEVM
	1135:        {}, // Lisk
	1284:        {}, // Moonbeam
	1993:        {}, // B3 Sepolia Testnet
	2039:        {}, // Aleph Zero Testnet
	2040:        {}, // Vanar Mainnet
	2442:        {}, // Polygon zkEVM Cardona Testnet
	2522:        {}, // Fraxtal Testnet
	2662:        {}, // APEX
	4202:        {}, // Lisk Sepolia Testnet
	5003:        {}, // Mantle Sepolia Testnet
	7887:        {}, // Kinto Mainnet
	8217:        {}, // Kaia Mainnet
	10200:       {}, // Gnosis Chiado Testnet
	11124:       {}, // Abstract Testnet
	17069:       {}, // Garnet Holesky
	22222:       {}, // Nautilus Mainnet
	41455:       {}, // Aleph Zero EVM
	42220:       {}, // Celo Mainnet
	43113:       {}, // Avalanche Fuji Testnet
	44787:       {}, // Celo Dango Testnet
	53935:       {}, // DFK Chain
	59141:       {}, // Linea Sepolia
	60808:       {}, // BOB
	78600:       {}, // Vanguard
	80002:       {}, // Amoy
	81457:       {}, // Blast
	98985:       {}, // Superposition Testnet
	132902:      {}, // Form Testnet
	167000:      {}, // Taiko Mainnet
	167009:      {}, // Taiko Hekla L2
	325000:      {}, // Camp Network Testnet V2
	534351:      {}, // Scroll Sepolia Testnet
	978657:      {}, // Treasure Ruby
	28122024:    {}, // Ancient8 Testnet
	37084624:    {}, // SKALE Nebula Hub Testnet
	111557560:   {}, // Cyber Testnet
	161221135:   {}, // Plume Testnet
	168587773:   {}, // Blast Sepolia Testnet
	994873017:   {}, // Lumia Prism
	999999999:   {}, // Zora Sepolia Testnet
	1380012617:  {}, // RARI Chain Mainnet
	1903648807:  {}, // Gemuchain Testnet
	1952959480:  {}, // Lumia Testnet
	37714555429: {}, // Xai Testnet v2
	2:           {}, // Expanse Network
	7:           {}, // ThaiChain
	8:           {}, // Ubiq
	9:           {}, // Ubiq Network Testnet
	11:          {}, // Metadium Mainnet
	12:          {}, // Metadium Testnet
	13:          {}, // Diode Testnet Staging
	14:          {}, // Flare Mainnet
	15:          {}, // Diode Prenet
	16:          {}, // Songbird Testnet Coston
	17:          {}, // ThaiChain 2.0 ThaiFi
	18:          {}, // ThunderCore Testnet
	19:          {}, // Songbird Canary-Network
	20:          {}, // Elastos Smart Chain
	21:          {}, // Elastos Smart Chain Testnet
	22:          {}, // ELA-DID-Sidechain Mainnet
	23:          {}, // ELA-DID-Sidechain Testnet
	24:          {}, // KardiaChain Mainnet
	25:          {}, // Cronos Mainnet
	26:          {}, // Genesis L1 testnet
	27:          {}, // ShibaChain
	29:          {}, // Genesis L1
	32:          {}, // GoodData Testnet
	33:          {}, // GoodData Mainnet
	34:          {}, // SecureChain Mainnet
	35:          {}, // TBWG Chain
	36:          {}, // Dxchain Mainnet
	37:          {}, // Xpla Mainnet
	38:          {}, // Valorbit
	39:          {}, // U2U Solaris Mainnet
	40:          {}, // Telos EVM Mainnet
	41:          {}, // Telos EVM Testnet
}

type ThirdwebHttpJsonRpcClient struct {
	upstream *Upstream
	clientId string
	clients  map[string]HttpJsonRpcClient
	mu       sync.RWMutex
}

func NewThirdwebHttpJsonRpcClient(pu *Upstream, parsedUrl *url.URL) (HttpJsonRpcClient, error) {
	if !strings.HasSuffix(parsedUrl.Scheme, "thirdweb") {
		return nil, fmt.Errorf("invalid Thirdweb URL scheme: %s", parsedUrl.Scheme)
	}

	clientId := parsedUrl.Host
	if clientId == "" {
		return nil, fmt.Errorf("missing Thirdweb clientId in URL")
	}

	return &ThirdwebHttpJsonRpcClient{
		upstream: pu,
		clientId: clientId,
		clients:  make(map[string]HttpJsonRpcClient),
	}, nil
}

func (c *ThirdwebHttpJsonRpcClient) GetType() ClientType {
	return ClientTypeThirdwebHttpJsonRpc
}

func (c *ThirdwebHttpJsonRpcClient) SupportsNetwork(networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainId, err := strconv.ParseInt(networkId[4:], 10, 64)
	if err != nil {
		return false, err
	}

	_, ok := thirdwebNetworkSubdomains[chainId]
	return ok, nil
}

func (c *ThirdwebHttpJsonRpcClient) getOrCreateClient(network common.Network) (HttpJsonRpcClient, error) {
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
		return nil, fmt.Errorf("unsupported network architecture for Thirdweb client: %s", network.Architecture())
	}

	chainID, err := network.EvmChainId()
	if err != nil {
		return nil, err
	}

	if _, ok := thirdwebNetworkSubdomains[chainID]; !ok {
		return nil, fmt.Errorf("unsupported network chain ID for Thirdweb: %d", chainID)
	}

	thirdwebURL := fmt.Sprintf("https://%d.rpc.thirdweb.com/%s", chainID, c.clientId)
	parsedURL, err := url.Parse(thirdwebURL)
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

func (c *ThirdwebHttpJsonRpcClient) SendRequest(ctx context.Context, req *NormalizedRequest) (*NormalizedResponse, error) {
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
