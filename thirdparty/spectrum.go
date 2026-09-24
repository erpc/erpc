package thirdparty

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// spectrumNetwork is the chain-specific part of a Spectrum Nodes RPC URL.
type spectrumNetwork struct {
	slug string // chain slug, e.g. "ethereum"
	net  string // "mn" (mainnet) or "tn" (testnet)
}

// spectrumNetworks maps EVM chain IDs to their Spectrum Nodes path segments.
// URL: https://{host}/{apiKey}/{slug}/{net}/{chainId}/{plan}/{nodeType}/rpc/
var spectrumNetworks = map[int64]spectrumNetwork{
	1:         {"ethereum", "mn"},
	10:        {"optimism", "mn"},
	30:        {"rsk", "mn"},
	56:        {"bsc", "mn"},
	97:        {"bsc", "tn"},
	100:       {"gnosis", "mn"},
	109:       {"shibarium", "mn"},
	130:       {"unichain", "mn"},
	137:       {"polygon", "mn"},
	146:       {"sonic", "mn"},
	177:       {"hashkey", "mn"},
	196:       {"xlayer", "mn"},
	204:       {"opbnb", "mn"},
	223:       {"b2", "mn"},
	240:       {"cronos_zkevm", "tn"},
	300:       {"zksync", "tn"},
	324:       {"zksync", "mn"},
	388:       {"cronos_zkevm", "mn"},
	480:       {"worldchain", "mn"},
	592:       {"astar", "mn"},
	919:       {"mode", "tn"},
	999:       {"hyperliquid", "mn"},
	1088:      {"metis", "mn"},
	1101:      {"polygon_zkevm", "mn"},
	1111:      {"wemix", "mn"},
	1115:      {"core", "tn"},
	1116:      {"core", "mn"},
	1123:      {"b2", "tn"},
	1287:      {"moonbeam", "tn"},
	1301:      {"unichain", "tn"},
	1868:      {"soneium", "mn"},
	1946:      {"soneium", "tn"},
	1952:      {"xlayer", "tn"},
	2020:      {"ronin", "mn"},
	2442:      {"polygon_zkevm", "tn"},
	2818:      {"morph", "mn"},
	4002:      {"fantom", "tn"},
	4200:      {"merlin", "mn"},
	4203:      {"merlin", "tn"},
	4217:      {"tempo", "mn"},
	4326:      {"megaeth", "mn"},
	4663:      {"robinhood", "mn"},
	5000:      {"mantle", "mn"},
	5003:      {"mantle", "tn"},
	5042:      {"arc", "mn"},
	5611:      {"opbnb", "tn"},
	8453:      {"base", "mn"},
	10200:     {"gnosis", "tn"},
	14601:     {"sonic", "tn"},
	26888:     {"abcore", "tn"},
	34443:     {"mode", "mn"},
	36888:     {"abcore", "mn"},
	42161:     {"arbitrum", "mn"},
	42220:     {"celo", "mn"},
	43113:     {"avalanche", "tn"},
	43114:     {"avalanche", "mn"},
	53302:     {"superseed", "tn"},
	57073:     {"ink", "mn"},
	59141:     {"linea", "tn"},
	59144:     {"linea", "mn"},
	60808:     {"bob", "mn"},
	80002:     {"polygon", "tn"},
	80069:     {"berachain", "tn"},
	80094:     {"berachain", "mn"},
	81457:     {"blast", "mn"},
	84532:     {"base", "tn"},
	91342:     {"giwa", "tn"},
	102030:    {"creditcoin", "mn"},
	202601:    {"ronin", "tn"},
	421614:    {"arbitrum", "tn"},
	534352:    {"scroll", "mn"},
	747474:    {"polygonkatana", "mn"},
	808813:    {"bob", "tn"},
	11155111:  {"ethereum", "tn"},
	11155420:  {"optimism", "tn"},
	21000000:  {"corn", "mn"},
	168587773: {"blast", "tn"},
}

const (
	spectrumDefaultHost     = "spectrum-03.simplystaking.xyz"
	spectrumDefaultPlan     = "shared"
	spectrumDefaultNodeType = "archive"
	spectrumDomain          = "simplystaking.xyz"
)

type SpectrumVendor struct {
	common.Vendor
}

func CreateSpectrumVendor() common.Vendor {
	return &SpectrumVendor{}
}

func (v *SpectrumVendor) Name() string {
	return "spectrum"
}

func (v *SpectrumVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}
	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, nil
	}
	_, ok := spectrumNetworks[chainID]
	return ok, nil
}

func (v *SpectrumVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}
	if upstream.Endpoint != "" {
		return []*common.UpstreamConfig{upstream}, nil
	}

	apiKey, _ := settings["apiKey"].(string)
	if apiKey == "" {
		return nil, fmt.Errorf("apiKey is required in spectrum provider settings")
	}
	if upstream.Evm == nil || upstream.Evm.ChainId == 0 {
		return nil, fmt.Errorf("spectrum vendor requires upstream.evm.chainId to be defined")
	}
	chainID := upstream.Evm.ChainId
	network, ok := spectrumNetworks[chainID]
	if !ok {
		return nil, fmt.Errorf("unsupported network chain ID for Spectrum: %d", chainID)
	}

	endpoint := url.URL{
		Scheme: "https",
		Host:   spectrumSetting(settings, "host", spectrumDefaultHost),
		Path: fmt.Sprintf("/%s/%s/%s/%d/%s/%s/rpc/",
			apiKey,
			network.slug,
			network.net,
			chainID,
			spectrumSetting(settings, "plan", spectrumDefaultPlan),
			spectrumSetting(settings, "nodeType", spectrumDefaultNodeType),
		),
	}
	upstream.Endpoint = endpoint.String()
	upstream.Type = common.UpstreamTypeEvm

	return []*common.UpstreamConfig{upstream}, nil
}

func spectrumSetting(settings common.VendorSettings, key, fallback string) string {
	if s, ok := settings[key].(string); ok && s != "" {
		return s
	}
	return fallback
}

func (v *SpectrumVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	return nil
}

func (v *SpectrumVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "spectrum://") || strings.HasPrefix(ups.Endpoint, "evm+spectrum://") {
		return true
	}
	return strings.Contains(ups.Endpoint, "."+spectrumDomain+"/")
}
