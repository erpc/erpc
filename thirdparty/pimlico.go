package thirdparty

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
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

type PimlicoVendor struct {
	common.Vendor
}

type PimlicoSettings struct {
	ApiKey string `json:"apiKey"`
}

func (s *PimlicoSettings) IsObjectNull() bool {
	return s == nil || s.ApiKey == ""
}

func (s *PimlicoSettings) Validate() error {
	if s.ApiKey == "" {
		return fmt.Errorf("apiKey is required")
	}
	return nil
}

func (s *PimlicoSettings) SetDefaults() {}

func CreatePimlicoVendor() common.Vendor {
	return &PimlicoVendor{}
}

func (v *PimlicoVendor) Name() string {
	return "pimlico"
}

func (v *PimlicoVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	chainId, err := strconv.ParseInt(networkId, 10, 64)
	if err != nil {
		return false, err
	}
	_, ok := pimlicoSupportedChains[chainId]
	if ok {
		return true, nil
	}

	client, err := v.createClient(ctx, logger, nil)
	if err != nil {
		return false, err
	}

	ctx, cancel := context.WithTimeoutCause(ctx, 10*time.Second, errors.New("pimlico client timeout during eth_chainId"))
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

func (v *PimlicoVendor) OverrideConfig(upstream *common.UpstreamConfig, settings common.VendorSettings) error {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint == "" && settings != nil && !settings.IsObjectNull() {
		if stg, ok := settings.(*PimlicoSettings); ok {
			if stg.ApiKey != "" {
				chainID := upstream.Evm.ChainId
				if chainID == 0 {
					return fmt.Errorf("pimlico vendor requires upstream.evm.chainId to be defined")
				}
				var pimlicoURL string
				if stg.ApiKey == "public" {
					pimlicoURL = fmt.Sprintf("https://public.pimlico.io/v2/%d/rpc", chainID)
				} else {
					pimlicoURL = fmt.Sprintf("https://api.pimlico.io/v2/%d/rpc?apikey=%s", chainID, stg.ApiKey)
				}
				parsedURL, err := url.Parse(pimlicoURL)
				if err != nil {
					return err
				}

				upstream.Endpoint = parsedURL.String()
			}
		} else {
			return fmt.Errorf("provided settings is not of type *PimlicoSettings it is of type %T", settings)
		}
	}

	if upstream.IgnoreMethods == nil {
		upstream.IgnoreMethods = []string{"*"}
	}
	if upstream.AllowMethods == nil {
		upstream.AllowMethods = []string{
			"eth_sendUserOperation",
			"eth_estimateUserOperationGas",
			"eth_getUserOperationReceipt",
			"eth_getUserOperationByHash",
			"eth_supportedEntryPoints",
			"pimlico_sendCompressedUserOperation",
			"pimlico_getUserOperationGasPrice",
			"pimlico_getUserOperationStatus",
			"pm_sponsorUserOperation",
			"pm_getPaymasterData",
			"pm_getPaymasterStubData",
		}
	}

	return nil
}

func (v *PimlicoVendor) GetVendorSpecificErrorIfAny(resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	return nil
}

func (v *PimlicoVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	return strings.HasPrefix(ups.Endpoint, "pimlico") ||
		strings.HasPrefix(ups.Endpoint, "evm+pimlico") ||
		strings.Contains(ups.Endpoint, "pimlico.io")
}

func (v *PimlicoVendor) createClient(ctx context.Context, logger *zerolog.Logger, parsedURL *url.URL) (clients.HttpJsonRpcClient, error) {
	client, err := clients.NewGenericHttpJsonRpcClient(ctx, logger, "n/a", "n/a", parsedURL, nil)
	if err != nil {
		return nil, err
	}
	return client, nil
}
