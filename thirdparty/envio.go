package thirdparty

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

const DefaultEnvioRootDomain = "rpc.hypersync.xyz"

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
	50:         {}, // Xdc
	2741:       {}, // Abstract
	5115:       {}, // Citrea Testnet
	7560:       {}, // Cyber
	80084:      {}, // Berachain Bartio
	80094:      {}, // Berachain
	8888:       {}, // Chiliz
	645749:     {}, // Hyperliquid
	1750:       {}, // Metall2
	1287:       {}, // Moonbase Alpha
	50104:      {}, // Sophon
}

type EnvioVendor struct {
	common.Vendor
	headlessClients sync.Map
}

func CreateEnvioVendor() common.Vendor {
	return &EnvioVendor{
		headlessClients: sync.Map{},
	}
}

func (v *EnvioVendor) Name() string {
	return "envio"
}

func (v *EnvioVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainId, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}

	if _, ok := envioKnownSupportedChains[chainId]; ok {
		return true, nil
	}

	rootDomain, ok := settings["rootDomain"].(string)
	if !ok || rootDomain == "" {
		rootDomain = DefaultEnvioRootDomain
	}

	apiKey, _ := settings["apiKey"].(string)
	parsedURL, err := v.generateUrl(chainId, rootDomain, apiKey)
	if err != nil {
		return false, err
	}

	// Check against endpoint to see if eth_chainId responds successfully
	client, err := v.getOrCreateClient(ctx, logger, chainId, parsedURL)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pr := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_chainId","params":[]}`, util.RandomID())))
	resp, err := client.SendRequest(ctx, pr)
	if err != nil {
		// Consider "failed to verify certificate" as unsupported due to how Envios load-balancing in their K8S works
		if strings.Contains(err.Error(), "failed to verify certificate") {
			return false, nil
		}
		return false, err
	}

	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return false, err
	}

	cids, err := jrr.PeekStringByPath(ctx)
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

func (v *EnvioVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.IgnoreMethods == nil {
		upstream.IgnoreMethods = []string{"*"}
	}
	if upstream.AllowMethods == nil {
		upstream.AllowMethods = []string{
			"eth_chainId",
			"eth_blockNumber",
			"eth_getBlockByNumber",
			"eth_getBlockByHash",
			"eth_getTransactionByHash",
			"eth_getTransactionByBlockHashAndIndex",
			"eth_getTransactionByBlockNumberAndIndex",
			"eth_getTransactionReceipt",
			"eth_getBlockReceipts",
			"eth_getLogs",
			"eth_getFilterLogs",
			"eth_getFilterChanges",
			"eth_uninstallFilter",
			"eth_newFilter",
		}
	}

	if upstream.Endpoint == "" {
		rootDomain, ok := settings["rootDomain"].(string)
		if !ok || rootDomain == "" {
			rootDomain = DefaultEnvioRootDomain
		}
		apiKey, _ := settings["apiKey"].(string)
		chainID := upstream.Evm.ChainId
		if chainID == 0 {
			return nil, fmt.Errorf("envio vendor requires upstream.evm.chainId to be defined")
		}
		parsedURL, err := v.generateUrl(chainID, rootDomain, apiKey)
		if err != nil {
			return nil, err
		}
		upstream.Endpoint = parsedURL.String()
		upstream.Type = common.UpstreamTypeEvm
	}

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *EnvioVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	return nil
}

func (v *EnvioVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	return strings.HasPrefix(ups.Endpoint, "envio") ||
		strings.HasPrefix(ups.Endpoint, "evm+envio") ||
		strings.Contains(ups.Endpoint, "envio.dev") ||
		strings.Contains(ups.Endpoint, "hypersync.xyz")
}

func (v *EnvioVendor) generateUrl(chainId int64, rootDomain string, apiKey string) (*url.URL, error) {
	var envioURL string
	if apiKey != "" {
		envioURL = fmt.Sprintf("https://%d.%s/%s", chainId, rootDomain, apiKey)
	} else {
		envioURL = fmt.Sprintf("https://%d.%s", chainId, rootDomain)
	}
	parsedURL, err := url.Parse(envioURL)
	if err != nil {
		return nil, err
	}
	return parsedURL, nil
}

func (v *EnvioVendor) getOrCreateClient(ctx context.Context, logger *zerolog.Logger, chainId int64, parsedURL *url.URL) (clients.HttpJsonRpcClient, error) {
	// Check if we already have a client for this chain ID
	if client, ok := v.headlessClients.Load(chainId); ok {
		return client.(clients.HttpJsonRpcClient), nil
	}

	// Create a new client for this chain ID
	client, err := clients.NewGenericHttpJsonRpcClient(ctx, logger, "n/a", nil, parsedURL, nil, nil)
	if err != nil {
		return nil, err
	}

	// Store the client for this chain ID
	v.headlessClients.Store(chainId, client)
	return client, nil
}
