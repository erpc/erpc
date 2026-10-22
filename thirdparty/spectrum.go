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
	path    string // "{network}/{mn|tn}/{chain}", e.g. "base/tn/84532"
	rpc     string // path after the node type; "rpc/" when empty
	archive bool   // archive nodes offered
	pruned  bool   // pruned nodes offered
}

// spectrumNetworks maps EVM chain IDs to their Spectrum Nodes routes, taken
// from the public catalog at https://spectrumnodes.com/networks. Keys are the
// chain IDs the endpoints actually serve: Spectrum's "Core Testnet" (1115)
// serves 1114 and its "Merlin Testnet" (4203) serves 686868.
// URL: https://{host}/{apiKey}/{path}/{plan}/{nodeType}/{rpc}
var spectrumNetworks = map[int64]spectrumNetwork{
	1:         {path: "ethereum/mn/1", archive: true, pruned: true},                                // Ethereum Mainnet
	10:        {path: "optimism/mn/10", archive: true, pruned: true},                               // Optimism Mainnet
	14:        {path: "flare/mn/14", archive: false, pruned: true},                                 // Flare Mainnet
	25:        {path: "cronos/mn/cronosmainnet_25-1", archive: true, pruned: false},                // Cronos Mainnet
	30:        {path: "rsk/mn/30", archive: true, pruned: true},                                    // RSK Mainnet
	40:        {path: "telos/mn/40", archive: false, pruned: true},                                 // Telos Mainnet
	50:        {path: "xdc/mn/50", archive: false, pruned: true},                                   // XDC Mainnet
	51:        {path: "xdc/tn/51", archive: false, pruned: true},                                   // XDC Testnet
	56:        {path: "bsc/mn/56", archive: true, pruned: true},                                    // BSC Mainnet
	97:        {path: "bsc/tn/97", archive: true, pruned: true},                                    // BSC Testnet
	100:       {path: "gnosis/mn/100", archive: true, pruned: true},                                // Gnosis Mainnet
	109:       {path: "shibarium/mn/109", archive: true, pruned: false},                            // Shibarium Mainnet
	114:       {path: "flare/tn/114", archive: false, pruned: true},                                // Flare Testnet
	130:       {path: "unichain/mn/130", archive: true, pruned: false},                             // UniChain Mainnet
	137:       {path: "polygon/mn/137", archive: true, pruned: true},                               // Polygon Mainnet
	143:       {path: "monad/mn/143", archive: true, pruned: true},                                 // Monad Mainnet
	146:       {path: "sonic/mn/146", archive: true, pruned: false},                                // Sonic Mainnet
	177:       {path: "hashkey/mn/177", archive: true, pruned: false},                              // Hashkey Mainnet
	185:       {path: "mint/mn/185", archive: false, pruned: true},                                 // Mint Mainnet
	196:       {path: "xlayer/mn/196", archive: true, pruned: false},                               // Xlayer Mainnet
	204:       {path: "opbnb/mn/204", archive: true, pruned: true},                                 // opBNB Mainnet
	223:       {path: "b2/mn/223", archive: true, pruned: false},                                   // B2 Mainnet
	228:       {path: "mind/mn/228", archive: false, pruned: true},                                 // Mind Mainnet
	232:       {path: "lens/mn/232", archive: false, pruned: true},                                 // Lens Mainnet
	239:       {path: "tac/mn/239", archive: false, pruned: true},                                  // Tac Mainnet
	240:       {path: "cronoszkevm/tn/240", archive: true, pruned: false},                          // Cronos zkevm Testnet
	250:       {path: "fantom/mn/250", archive: false, pruned: true},                               // Fantom Mainnet
	252:       {path: "fraxtal/mn/252", archive: true, pruned: true},                               // Fraxtal Mainnet
	295:       {path: "hedera/mn/295", archive: false, pruned: true},                               // Hedera Mainnet
	296:       {path: "hedera/tn/296", archive: false, pruned: true},                               // Hedera Testnet
	300:       {path: "zksync/tn/300", archive: true, pruned: false},                               // ZKsync Testnet
	314:       {path: "filecoin/mn/314", archive: false, pruned: true},                             // Filecoin Mainnet
	324:       {path: "zksync/mn/324", archive: true, pruned: false},                               // ZKsync Mainnet
	338:       {path: "cronos/tn/cronostestnet_338", archive: true, pruned: false},                 // Cronos Testnet
	388:       {path: "cronoszkevm/mn/388", archive: true, pruned: true},                           // Cronos zkevm Mainnet
	480:       {path: "worldchain/mn/480", archive: true, pruned: false},                           // Worldchain Mainnet
	592:       {path: "astar/mn/592", archive: true, pruned: true},                                 // Astar Mainnet
	919:       {path: "mode/tn/919", archive: true, pruned: false},                                 // Mode Testnet
	964:       {path: "bittensor/mn/964", archive: false, pruned: true},                            // Bittensor Mainnet
	998:       {path: "hyperliquid/tn/998", archive: true, pruned: true},                           // HyperEVM Testnet
	999:       {path: "hyperliquid/mn/999", archive: true, pruned: true},                           // HyperEVM Mainnet
	1075:      {path: "iota/tn/1075", archive: false, pruned: true},                                // Iota Testnet
	1088:      {path: "metis/mn/1088", archive: true, pruned: false},                               // Metis Mainnet
	1101:      {path: "polygonzkevm/mn/1101", archive: true, pruned: false},                        // Polygon zkEVM Mainnet
	1111:      {path: "wemix/mn/1111", archive: true, pruned: false},                               // Wemix Mainnet
	1114:      {path: "core/tn/1115", archive: true, pruned: false},                                // Core Testnet
	1116:      {path: "core/mn/1116", archive: true, pruned: true},                                 // Core Mainnet
	1123:      {path: "b2/tn/1123", archive: true, pruned: false},                                  // B2 Testnet
	1135:      {path: "lisk/mn/1135", archive: false, pruned: true},                                // Lisk Mainnet
	1284:      {path: "moonbeam/mn/1284", archive: false, pruned: true},                            // Moonbeam Mainnet
	1285:      {path: "moonriver/mn/1285", archive: true, pruned: true},                            // Moonriver Mainnet
	1287:      {path: "moonbeam/tn/1287", archive: true, pruned: false},                            // Moonbeam Testnet
	1301:      {path: "unichain/tn/1301", archive: true, pruned: false},                            // UniChain Testnet
	1328:      {path: "sei/tn/1328", archive: false, pruned: true},                                 // Sei Testnet
	1329:      {path: "sei/mn/1329", archive: false, pruned: true},                                 // Sei Mainnet
	1514:      {path: "story/mn/1514", archive: false, pruned: true},                               // Story Mainnet
	1750:      {path: "metal/mn/1750", archive: false, pruned: true},                               // Metal Mainnet
	1868:      {path: "soneium/mn/1868", archive: true, pruned: false},                             // Soneium Mainnet
	1946:      {path: "soneium/tn/1946", archive: true, pruned: false},                             // Soneium Testnet
	1952:      {path: "xlayer/tn/1952", archive: true, pruned: false},                              // X Layer Testnet
	2020:      {path: "ronin/mn/2020", archive: true, pruned: false},                               // Ronin Mainnet
	2031:      {path: "centrifuge/mn/2031", archive: true, pruned: false},                          // Centrifuge Mainnet
	2368:      {path: "kite/tn/2368", archive: false, pruned: true},                                // Kite Testnet
	2442:      {path: "polygonzkevm/tn/2442", archive: true, pruned: false},                        // Polygon zkEVM Testnet
	2741:      {path: "abstract/mn/2741", archive: false, pruned: true},                            // Abstract Mainnet
	2810:      {path: "morph/tn/2810", archive: false, pruned: true},                               // Morph Testnet
	2818:      {path: "morph/mn/2818", archive: true, pruned: false},                               // Morph Mainnet
	3637:      {path: "botanix/mn/3637", archive: false, pruned: true},                             // Botanix Mainnet
	4002:      {path: "fantom/tn/4002", archive: true, pruned: false},                              // Fantom Testnet
	4200:      {path: "merlin/mn/4200", archive: true, pruned: false},                              // Merlin Mainnet
	4217:      {path: "tempo/mn/4217", archive: true, pruned: true},                                // Tempo Mainnet
	4326:      {path: "megaeth/mn/4326", archive: true, pruned: true},                              // MegaETH Mainnet
	4663:      {path: "robinhood/mn/4663", archive: true, pruned: true},                            // Robinhood Mainnet
	5000:      {path: "mantle/mn/5000", archive: true, pruned: true},                               // Mantle Mainnet
	5003:      {path: "mantle/tn/5003", archive: true, pruned: false},                              // Mantle Testnet
	5042:      {path: "arc/mn/5042", archive: true, pruned: false},                                 // Arc Mainnet
	5330:      {path: "superseed/mn/5330", archive: false, pruned: true},                           // Superseed Mainnet
	5611:      {path: "opbnb/tn/5611", archive: true, pruned: false},                               // opBNB Testnet
	7000:      {path: "zetachain/mn/7000", archive: false, pruned: true},                           // ZetaChain Mainnet
	8217:      {path: "kaia/mn/8217", archive: false, pruned: true},                                // Kaia Mainnet
	8453:      {path: "base/mn/8453", archive: true, pruned: true},                                 // Base Mainnet
	8822:      {path: "iota/mn/8822", archive: false, pruned: true},                                // Iota Mainnet
	9745:      {path: "plasma/mn/9745", archive: false, pruned: true},                              // Plasma Mainnet
	10143:     {path: "monad/tn/10143", archive: false, pruned: true},                              // Monad Testnet
	10200:     {path: "gnosis/tn/10200", archive: true, pruned: false},                             // Gnosis Testnet
	14601:     {path: "sonic/tn/14601", archive: true, pruned: false},                              // Sonic Testnet
	16661:     {path: "0g/mn/16661", archive: false, pruned: true},                                 // 0G Mainnet
	17000:     {path: "ethereum/tn/17000", archive: false, pruned: true},                           // Ethereum Testnet
	26888:     {path: "abcore/tn/26888", archive: true, pruned: false},                             // AB Core Testnet
	31611:     {path: "mezo/tn/31611", archive: false, pruned: true},                               // Mezo Testnet
	31612:     {path: "mezo/mn/31612", archive: false, pruned: true},                               // Mezo Mainnet
	33139:     {path: "apechain/mn/33139", archive: false, pruned: true},                           // Apechain Mainnet
	34443:     {path: "mode/mn/34443", archive: true, pruned: false},                               // Mode Mainnet
	36888:     {path: "abcore/mn/36888", archive: true, pruned: false},                             // AB Core Mainnet
	36900:     {path: "adi/mn/36900", archive: false, pruned: true},                                // ADI Mainnet
	42161:     {path: "arbitrum/mn/42161", archive: true, pruned: true},                            // Arbitrum Mainnet
	42220:     {path: "celo/mn/42220", archive: true, pruned: false},                               // Celo Mainnet
	42431:     {path: "tempo/tn/42431", archive: false, pruned: true},                              // Tempo Testnet
	42793:     {path: "etherlink/mn/42793", archive: false, pruned: true},                          // Etherlink Mainnet
	43111:     {path: "hemi/mn/43111", archive: true, pruned: true},                                // Hemi Mainnet
	43113:     {path: "avalanche/tn/43113", rpc: "rpc/ext/bc/C/rpc", archive: true, pruned: false}, // Avalanche Testnet
	43114:     {path: "avalanche/mn/43114", rpc: "rpc/ext/bc/C/rpc", archive: true, pruned: true},  // Avalanche Mainnet
	47763:     {path: "neox/mn/47763", archive: false, pruned: true},                               // NeoX Mainnet
	48900:     {path: "zircuit/mn/48900", archive: false, pruned: true},                            // Zircuit Mainnet
	53302:     {path: "superseed/tn/53302", archive: true, pruned: false},                          // Superseed Testnet
	57073:     {path: "ink/mn/57073", archive: true, pruned: false},                                // Ink Mainnet
	59141:     {path: "linea/tn/59141", archive: true, pruned: false},                              // Linea Testnet
	59144:     {path: "linea/mn/59144", archive: true, pruned: true},                               // Linea Mainnet
	60808:     {path: "bob/mn/60808", archive: true, pruned: false},                                // BOB Mainnet
	80002:     {path: "polygon/tn/80002", archive: true, pruned: false},                            // Polygon Testnet
	80069:     {path: "berachain/tn/80069", archive: true, pruned: false},                          // Berachain Testnet
	80094:     {path: "berachain/mn/80094", archive: true, pruned: false},                          // Berachain Mainnet
	81457:     {path: "blast/mn/81457", archive: true, pruned: false},                              // Blast Mainnet
	84532:     {path: "base/tn/84532", archive: true, pruned: false},                               // Base Testnet
	91342:     {path: "giwa/tn/91342", archive: true, pruned: false},                               // Giwa Testnet
	98866:     {path: "plume/mn/98866", archive: true, pruned: true},                               // Plume Mainnet
	102030:    {path: "creditcoin/mn/102030", archive: true, pruned: false},                        // Creditcoin Mainnet
	127001:    {path: "gravity/mn/127001", archive: false, pruned: true},                           // Gravity Mainnet
	128123:    {path: "etherlink/tn/128123", archive: false, pruned: true},                         // Etherlink Testnet
	129399:    {path: "polygonkatana/tn/129399", archive: false, pruned: true},                     // Polygon Katana Testnet
	167000:    {path: "taiko/mn/167000", archive: false, pruned: true},                             // Taiko Mainnet
	200810:    {path: "bitlayer/tn/200810", archive: false, pruned: true},                          // Bitlayer Testnet
	200901:    {path: "bitlayer/mn/200901", archive: false, pruned: true},                          // Bitlayer Mainnet
	202601:    {path: "ronin/tn/202601", archive: true, pruned: false},                             // Ronin Testnet
	421614:    {path: "arbitrum/tn/421614", archive: true, pruned: true},                           // Arbitrum Testnet
	534352:    {path: "scroll/mn/534352", archive: true, pruned: false},                            // Scroll Mainnet
	560048:    {path: "ethereum/tn/560048", archive: false, pruned: true},                          // Ethereum Testnet
	686868:    {path: "merlin/tn/4203", archive: true, pruned: false},                              // Merlin Testnet
	688689:    {path: "pharos/tn/688689", archive: false, pruned: true},                            // Pharos Testnet
	743111:    {path: "hemi/tn/743111", archive: false, pruned: true},                              // Hemi Testnet
	747474:    {path: "polygonkatana/mn/747474", archive: true, pruned: true},                      // Polygon Katana Mainnet
	808813:    {path: "bob/tn/808813", archive: true, pruned: false},                               // BOB Testnet
	1440000:   {path: "xrplevm/mn/1440000", archive: false, pruned: true},                          // XRPL EVM Mainnet
	1440004:   {path: "xrplevm/tn/1440004", archive: false, pruned: true},                          // XRPL EVM Testnet
	2019775:   {path: "jovay/tn/2019775", archive: false, pruned: true},                            // Jovay Testnet
	5734951:   {path: "jovay/mn/5734951", archive: false, pruned: true},                            // Jovay Mainnet
	7777777:   {path: "zora/mn/7777777", archive: false, pruned: true},                             // Zora Mainnet
	11155111:  {path: "ethereum/tn/11155111", archive: true, pruned: true},                         // Ethereum Testnet
	11155420:  {path: "optimism/tn/11155420", archive: true, pruned: false},                        // Optimism Testnet
	12227332:  {path: "neox/tn/12227332", archive: false, pruned: true},                            // NeoX Testnet
	21000000:  {path: "corn/mn/21000000", archive: true, pruned: false},                            // Corn Mainnet
	168587773: {path: "blast/tn/168587773", archive: true, pruned: false},                          // Blast Testnet
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

	rpc := network.rpc
	if rpc == "" {
		rpc = "rpc/"
	}
	endpoint := url.URL{
		Scheme: "https",
		Host:   spectrumSetting(settings, "host", spectrumDefaultHost),
		Path: fmt.Sprintf("/%s/%s/%s/%s/%s",
			apiKey,
			network.path,
			spectrumSetting(settings, "plan", spectrumDefaultPlan),
			network.nodeType(spectrumSetting(settings, "nodeType", "archive")),
			rpc,
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
