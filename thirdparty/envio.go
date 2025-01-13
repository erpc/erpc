package thirdparty

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/erpc/erpc/clients"
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

type EnvioVendor struct {
	common.Vendor
}

type EnvioSettings struct {
	RootDomain string `yaml:"rootDomain" json:"rootDomain"`
}

func (s *EnvioSettings) IsObjectNull() bool {
	return s == nil || s.RootDomain == ""
}

func (s *EnvioSettings) Validate() error {
	return nil
}

func CreateEnvioVendor() common.Vendor {
	return &EnvioVendor{}
}

func (v *EnvioVendor) Name() string {
	return "envio"
}

func (v *EnvioVendor) SupportsNetwork(ctx context.Context, networkId string) (bool, error) {
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
	client, err := v.createClient(ctx, chainId)
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

func (v *EnvioVendor) OverrideConfig(upstream *common.UpstreamConfig) error {
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

	return nil
}

func (v *EnvioVendor) GetVendorSpecificErrorIfAny(resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	return nil
}

func (v *EnvioVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	return strings.HasPrefix(ups.Endpoint, "envio") ||
		strings.HasPrefix(ups.Endpoint, "evm+envio") ||
		strings.Contains(ups.Endpoint, "envio.dev") ||
		strings.Contains(ups.Endpoint, "hypersync.xyz")
}

func (v *EnvioVendor) createClient(ctx context.Context, logger *zerolog.Logger, chainID int64) (clients.HttpJsonRpcClient, error) {
	client, err := clients.NewGenericHttpJsonRpcClient(ctx, logger, c.upstream, parsedURL)
	if err != nil {
		return nil, err
	}

	c.clients[chainID] = client
	return client, nil
}
