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
	path    string // "{network}/{service}", e.g. "base/tn_sepolia"
	archive bool   // archive nodes offered
	pruned  bool   // pruned nodes offered
}

// spectrumNetworks maps EVM chain IDs to their Spectrum Nodes path segments,
// taken from the public catalog at https://spectrumnodes.com/networks.
// URL: https://{host}/{apiKey}/{path}/{chainId}/{plan}/{nodeType}/rpc/
var spectrumNetworks = map[int64]spectrumNetwork{
	1:         {"ethereum/mn", true, true},                     // Ethereum Mainnet
	10:        {"optimism/mn", true, true},                     // Optimism Mainnet
	14:        {"flare/mn", false, true},                       // Flare Mainnet
	25:        {"cronos/mn", true, false},                      // Cronos Mainnet
	30:        {"rsk/mn", true, true},                          // RSK Mainnet
	40:        {"telos/mn", false, true},                       // Telos Mainnet
	50:        {"xdc/mn", false, true},                         // XDC Mainnet
	51:        {"xdc/tn", false, true},                         // XDC Testnet
	56:        {"bsc/mn", true, true},                          // BSC Mainnet
	97:        {"bsc/tn", true, true},                          // BSC Testnet
	100:       {"gnosis/mn", true, true},                       // Gnosis Mainnet
	109:       {"shibarium/mn", true, false},                   // Shibarium Mainnet
	114:       {"flare/tn_coston2", false, true},               // Flare Testnet
	130:       {"unichain/mn", true, false},                    // UniChain Mainnet
	137:       {"polygon/mn", true, true},                      // Polygon Mainnet
	143:       {"monad/mn", true, true},                        // Monad Mainnet
	146:       {"sonic/mn", true, false},                       // Sonic Mainnet
	177:       {"hashkey/mn", true, false},                     // Hashkey Mainnet
	185:       {"mint/mn", false, true},                        // Mint Mainnet
	196:       {"xlayer/mn", true, false},                      // Xlayer Mainnet
	204:       {"opbnb/mn", true, true},                        // opBNB Mainnet
	223:       {"b2/mn", true, false},                          // B2 Mainnet
	228:       {"mind/mn", false, true},                        // Mind Mainnet
	232:       {"lens/mn", false, true},                        // Lens Mainnet
	239:       {"tac/mn", false, true},                         // Tac Mainnet
	240:       {"cronos_zkevm/tn", true, false},                // Cronos zkevm Testnet
	250:       {"fantom/mn", false, true},                      // Fantom Mainnet
	252:       {"fraxtal/mn", true, true},                      // Fraxtal Mainnet
	295:       {"hedera/mn", false, true},                      // Hedera Mainnet
	296:       {"hedera/tn", false, true},                      // Hedera Testnet
	300:       {"zksync/tn", true, false},                      // ZKsync Testnet
	314:       {"filecoin/mn", false, true},                    // Filecoin Mainnet
	324:       {"zksync/mn", true, false},                      // ZKsync Mainnet
	338:       {"cronos/tn_cronos", true, false},               // Cronos Testnet
	388:       {"cronos_zkevm/mn", true, true},                 // Cronos zkevm Mainnet
	480:       {"worldchain/mn", true, false},                  // Worldchain Mainnet
	545:       {"flow/tn_evm", false, true},                    // Flow Testnet
	592:       {"astar/mn", true, true},                        // Astar Mainnet
	747:       {"flow/mn_evm", false, true},                    // Flow Mainnet
	919:       {"mode/tn", true, false},                        // Mode Testnet
	964:       {"bittensor/mn", false, true},                   // Bittensor Mainnet
	998:       {"hyperliquid/tn", true, true},                  // HyperEVM Testnet
	999:       {"hyperliquid/mn", true, true},                  // HyperEVM Mainnet
	1030:      {"conflux/mn_espace", false, true},              // Conflux Mainnet
	1075:      {"iota/tn", false, true},                        // Iota Testnet
	1088:      {"metis/mn", true, false},                       // Metis Mainnet
	1101:      {"polygon_zkevm/mn", true, false},               // Polygon zkEVM Mainnet
	1111:      {"wemix/mn", true, false},                       // Wemix Mainnet
	1115:      {"core/tn", true, false},                        // Core Testnet
	1116:      {"core/mn", true, true},                         // Core Mainnet
	1123:      {"b2/tn", true, false},                          // B2 Testnet
	1135:      {"lisk/mn", false, true},                        // Lisk Mainnet
	1284:      {"moonbeam/mn", false, true},                    // Moonbeam Mainnet
	1285:      {"moonriver/mn", true, true},                    // Moonriver Mainnet
	1287:      {"moonbeam/tn", true, false},                    // Moonbeam Testnet
	1301:      {"unichain/tn", true, false},                    // UniChain Testnet
	1328:      {"sei/tn_evm", false, true},                     // Sei Testnet
	1329:      {"sei/mn", false, true},                         // Sei Mainnet
	1514:      {"story/mn", false, true},                       // Story Mainnet
	1750:      {"metal/mn", false, true},                       // Metal Mainnet
	1868:      {"soneium/mn", true, false},                     // Soneium Mainnet
	1946:      {"soneium/tn", true, false},                     // Soneium Testnet
	1952:      {"xlayer/tn", true, false},                      // X Layer Testnet
	2020:      {"ronin/mn", true, false},                       // Ronin Mainnet
	2031:      {"centrifuge/mn", true, false},                  // Centrifuge Mainnet
	2368:      {"kite/tn", false, true},                        // Kite Testnet
	2442:      {"polygon_zkevm/tn_cardona", true, false},       // Polygon zkEVM Testnet
	2741:      {"abstract/mn", false, true},                    // Abstract Mainnet
	2810:      {"morph/tn", false, true},                       // Morph Testnet
	2818:      {"morph/mn", true, false},                       // Morph Mainnet
	3637:      {"botanix/mn", false, true},                     // Botanix Mainnet
	4002:      {"fantom/tn", true, false},                      // Fantom Testnet
	4200:      {"merlin/mn", true, false},                      // Merlin Mainnet
	4203:      {"merlin/tn", true, false},                      // Merlin Testnet
	4217:      {"tempo/mn", true, true},                        // Tempo Mainnet
	4326:      {"megaeth/mn", true, true},                      // MegaETH Mainnet
	4663:      {"robinhood/mn", true, true},                    // Robinhood Mainnet
	5000:      {"mantle/mn", true, true},                       // Mantle Mainnet
	5003:      {"mantle/tn", true, false},                      // Mantle Testnet
	5042:      {"arc/mn", true, false},                         // Arc Mainnet
	5330:      {"superseed/mn", false, true},                   // Superseed Mainnet
	5611:      {"opbnb/tn", true, false},                       // opBNB Testnet
	7000:      {"zetachain/mn", false, true},                   // ZetaChain Mainnet
	8217:      {"kaia/mn", false, true},                        // Kaia Mainnet
	8453:      {"base/mn", true, true},                         // Base Mainnet
	8822:      {"iota/mn", false, true},                        // Iota Mainnet
	9745:      {"plasma/mn", false, true},                      // Plasma Mainnet
	10143:     {"monad/tn", false, true},                       // Monad Testnet
	10200:     {"gnosis/tn_chiado", true, false},               // Gnosis Testnet
	14601:     {"sonic/tn", true, false},                       // Sonic Testnet
	16661:     {"0g/mn", false, true},                          // 0G Mainnet
	17000:     {"ethereum/tn_holesky", false, true},            // Ethereum Testnet
	23294:     {"oasis/mn_sapphire", true, true},               // Oasis Mainnet
	23295:     {"oasis/tn_sapphire", false, true},              // Oasis Testnet
	26888:     {"abcore/tn", true, false},                      // AB Core Testnet
	31611:     {"mezo/tn_evm", false, true},                    // Mezo Testnet
	31612:     {"mezo/mn_evm", false, true},                    // Mezo Mainnet
	33139:     {"apechain/mn", false, true},                    // Apechain Mainnet
	34443:     {"mode/mn", true, false},                        // Mode Mainnet
	36888:     {"abcore/mn", true, false},                      // AB Core Mainnet
	36900:     {"adi/mn", false, true},                         // ADI Mainnet
	42161:     {"arbitrum/mn", true, true},                     // Arbitrum Mainnet
	42220:     {"celo/mn", true, false},                        // Celo Mainnet
	42431:     {"tempo/tn_moderato", false, true},              // Tempo Testnet
	42793:     {"etherlink/mn", false, true},                   // Etherlink Mainnet
	43111:     {"hemi/mn", true, true},                         // Hemi Mainnet
	43113:     {"avalanche/tn_fuji", true, false},              // Avalanche Testnet
	43114:     {"avalanche/mn", true, true},                    // Avalanche Mainnet
	47763:     {"neox/mn", false, true},                        // NeoX Mainnet
	48900:     {"zircuit/mn", false, true},                     // Zircuit Mainnet
	53302:     {"superseed/tn", true, false},                   // Superseed Testnet
	57073:     {"ink/mn", true, false},                         // Ink Mainnet
	59141:     {"linea/tn", true, false},                       // Linea Testnet
	59144:     {"linea/mn", true, true},                        // Linea Mainnet
	60808:     {"bob/mn", true, false},                         // BOB Mainnet
	80002:     {"polygon/tn_amoy", true, false},                // Polygon Testnet
	80069:     {"berachain/tn", true, false},                   // Berachain Testnet
	80094:     {"berachain/mn", true, false},                   // Berachain Mainnet
	81457:     {"blast/mn", true, false},                       // Blast Mainnet
	84532:     {"base/tn_sepolia", true, false},                // Base Testnet
	91342:     {"giwa/tn", true, false},                        // Giwa Testnet
	98866:     {"plume/mn", true, true},                        // Plume Mainnet
	102030:    {"creditcoin/mn", true, false},                  // Creditcoin Mainnet
	127001:    {"gravity/mn", false, true},                     // Gravity Mainnet
	128123:    {"etherlink/tn", false, true},                   // Etherlink Testnet
	129399:    {"polygonkatana/tn_polygonkatana", false, true}, // Polygon Katana Testnet
	167000:    {"taiko/mn", false, true},                       // Taiko Mainnet
	200810:    {"bitlayer/tn", false, true},                    // Bitlayer Testnet
	200901:    {"bitlayer/mn", false, true},                    // Bitlayer Mainnet
	202601:    {"ronin/tn", true, false},                       // Ronin Testnet
	421614:    {"arbitrum/tn", true, true},                     // Arbitrum Testnet
	534352:    {"scroll/mn", true, false},                      // Scroll Mainnet
	560048:    {"ethereum/tn_hoodi", false, true},              // Ethereum Testnet
	688689:    {"pharos/tn", false, true},                      // Pharos Testnet
	743111:    {"hemi/tn", false, true},                        // Hemi Testnet
	747474:    {"polygonkatana/mn_polygonkatana", true, true},  // Polygon Katana Mainnet
	808813:    {"bob/tn", true, false},                         // BOB Testnet
	1440000:   {"xrpl_evm/mn", false, true},                    // XRPL EVM Mainnet
	1440004:   {"xrpl_evm/tn", false, true},                    // XRPL EVM Testnet
	2019775:   {"jovay/tn", false, true},                       // Jovay Testnet
	5734951:   {"jovay/mn", false, true},                       // Jovay Mainnet
	7777777:   {"zora/mn", false, true},                        // Zora Mainnet
	11155111:  {"ethereum/tn_sepolia", true, true},             // Ethereum Testnet
	11155420:  {"optimism/tn_sepolia", true, false},            // Optimism Testnet
	12227332:  {"neox/tn", false, true},                        // NeoX Testnet
	21000000:  {"corn/mn", true, false},                        // Corn Mainnet
	168587773: {"blast/tn_blast", true, false},                 // Blast Testnet
}

const (
	spectrumDefaultHost = "spectrum-03.simplystaking.xyz"
	spectrumDefaultPlan = "shared"
	spectrumDomain      = "simplystaking.xyz"
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
		Path: fmt.Sprintf("/%s/%s/%d/%s/%s/rpc/",
			apiKey,
			network.path,
			chainID,
			spectrumSetting(settings, "plan", spectrumDefaultPlan),
			network.nodeType(spectrumSetting(settings, "nodeType", "archive")),
		),
	}
	upstream.Endpoint = endpoint.String()
	upstream.Type = common.UpstreamTypeEvm

	return []*common.UpstreamConfig{upstream}, nil
}

// nodeType returns the preferred node type when the chain offers it, and the
// type the chain does offer otherwise: no single type is served on every chain.
func (n spectrumNetwork) nodeType(preferred string) string {
	switch {
	case preferred == "pruned" && n.pruned, !n.archive:
		return "pruned"
	default:
		return "archive"
	}
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
