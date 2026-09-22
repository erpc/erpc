package thirdparty

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	archEvm "github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

var defaultAlchemyNetworkSubdomains = map[int64]string{
	1:          "eth-mainnet",
	11155111:   "eth-sepolia",
	560048:     "eth-hoodi",
	10:         "opt-mainnet",
	11155420:   "opt-sepolia",
	137:        "polygon-mainnet",
	80002:      "polygon-amoy",
	42161:      "arb-mainnet",
	421614:     "arb-sepolia",
	8453:       "base-mainnet",
	84532:      "base-sepolia",
	324:        "zksync-mainnet",
	300:        "zksync-sepolia",
	7777777:    "zora-mainnet",
	999999999:  "zora-sepolia",
	81457:      "blast-mainnet",
	168587773:  "blast-sepolia",
	534352:     "scroll-mainnet",
	534351:     "scroll-sepolia",
	59144:      "linea-mainnet",
	59141:      "linea-sepolia",
	100:        "gnosis-mainnet",
	10200:      "gnosis-chiado",
	5000:       "mantle-mainnet",
	5003:       "mantle-sepolia",
	42220:      "celo-mainnet",
	11142220:   "celo-sepolia",
	56:         "bnb-mainnet",
	97:         "bnb-testnet",
	43114:      "avax-mainnet",
	43113:      "avax-fuji",
	1088:       "metis-mainnet",
	204:        "opbnb-mainnet",
	5611:       "opbnb-testnet",
	592:        "astar-mainnet",
	1101:       "polygonzkevm-mainnet",
	2442:       "polygonzkevm-cardona",
	7000:       "zetachain-mainnet",
	7001:       "zetachain-testnet",
	747:        "flow-mainnet",
	545:        "flow-testnet",
	480:        "worldchain-mainnet",
	4801:       "worldchain-sepolia",
	252:        "frax-mainnet",
	2523:       "frax-sepolia",
	288:        "boba-mainnet",
	28882:      "boba-sepolia",
	1329:       "sei-mainnet",
	1328:       "sei-testnet",
	80094:      "berachain-mainnet",
	80069:      "berachain-bepolia",
	360:        "shape-mainnet",
	11011:      "shape-sepolia",
	60808:      "bob-mainnet",
	808813:     "bob-sepolia",
	34443:      "mode-mainnet",
	919:        "mode-sepolia",
	69000:      "anime-mainnet",
	6900:       "anime-sepolia",
	33139:      "apechain-mainnet",
	33111:      "apechain-curtis",
	232:        "lens-mainnet",
	37111:      "lens-sepolia",
	1868:       "soneium-mainnet",
	1946:       "soneium-minato",
	30:         "rootstock-mainnet",
	31:         "rootstock-testnet",
	130:        "unichain-mainnet",
	1301:       "unichain-sepolia",
	146:        "sonic-mainnet",
	14601:      "sonic-testnet",
	57054:      "sonic-blaze",
	2741:       "abstract-mainnet",
	11124:      "abstract-testnet",
	143:        "monad-mainnet",
	10143:      "monad-testnet",
	5330:       "superseed-mainnet",
	53302:      "superseed-sepolia",
	57073:      "ink-mainnet",
	763373:     "ink-sepolia",
	2020:       "ronin-mainnet",
	202601:     "ronin-saigon",
	6985385:    "humanity-mainnet",
	7080969:    "humanity-testnet",
	1514:       "story-mainnet",
	1315:       "story-aeneid",
	999:        "hyperliquid-mainnet",
	998:        "hyperliquid-testnet",
	9745:       "plasma-mainnet",
	9746:       "plasma-testnet",
	4157:       "crossfi-testnet",
	4158:       "crossfi-mainnet",
	5371:       "settlus-mainnet",
	5373:       "settlus-septestnet",
	3637:       "botanix-mainnet",
	3636:       "botanix-testnet",
	613419:     "galactica-mainnet",
	843843:     "galactica-cassiopeia",
	510:        "synd-mainnet",
	36900:      "adi-mainnet",
	99999:      "adi-testnet",
	988:        "stable-mainnet",
	2201:       "stable-testnet",
	510525:     "clankermon-mainnet",
	4114:       "citrea-mainnet",
	5115:       "citrea-testnet",
	5042002:    "arc-testnet",
	1284:       "moonbeam-mainnet",
	685685:     "gensyn-testnet",
	11155931:   "rise-testnet",
	6343:       "megaeth-testnet",
	323432:     "worldmobile-testnet",
	869:        "worldmobilechain-mainnet",
	666666666:  "degen-mainnet",
	196:        "xlayer-mainnet",
	1952:       "xlayer-testnet",
	1776:       "injective-mainnet",
	1439:       "injective-testnet",
	42018:      "mythos-mainnet",
	4326:       "megaeth-mainnet",
	4153:       "rise-mainnet",
	4217:       "tempo-mainnet",
	42431:      "tempo-moderato",
	46630:      "robinhood-testnet",
	685689:     "gensyn-mainnet",
	1672:       "pharos-mainnet",
	688689:     "pharos-atlantic",
	747474:     "katana-mainnet",
	737373:     "katana-bokuto",
	5734951:    "jovay-mainnet",
	2019775:    "jovay-testnet",
	351243127:  "xmtp-ropsten",
	728126428:  "tron-mainnet",
	3448148188: "tron-testnet",
}

const DefaultAlchemyRecheckInterval = 24 * time.Hour

// alchemyApiUrl is the tRPC endpoint used to discover Alchemy networks.
// Declared as var (not const) so tests can point it at a mock server.
var alchemyApiUrl = "https://app-api.alchemy.com/trpc/config.getNetworkConfig"

type alchemyNetworkConfigResponse struct {
	Result struct {
		Data []struct {
			NetworkChainID int64  `json:"networkChainId"`
			KebabCaseID    string `json:"kebabCaseId"`
		} `json:"data"`
	} `json:"result"`
}

// AlchemyVendor uses RemoteDataCache for lock-free, async-refresh access to
// the network list AND the per-method credit-unit table. See remote_cache.go
// for the request-path safety rule.
type AlchemyVendor struct {
	common.Vendor
	cache   *RemoteDataCache[map[int64]string]
	cuCache *RemoteDataCache[map[string]int64]
}

func CreateAlchemyVendor() common.Vendor {
	return &AlchemyVendor{
		cache:   NewRemoteDataCache[map[int64]string]("alchemy"),
		cuCache: NewRemoteDataCache[map[string]int64]("alchemy-cu"),
	}
}

func (v *AlchemyVendor) Name() string {
	return "alchemy"
}

// alchemyCreditUnits is the built-in FALLBACK per-method compute-unit (CU)
// cost table (https://www.alchemy.com/docs/reference/compute-unit-costs,
// 2026-07-10). At runtime the vendor prefers the live table fetched from
// Alchemy's docs (see creditUnitsTable); this map is used on cold start and
// whenever that fetch fails. Only commonly relayed EVM methods are listed;
// "*" approximates the modal standard cost for anything unlisted. Values
// are Alchemy CUs, not money.
var alchemyCreditUnits = map[string]int64{
	"*":                         20,
	"eth_blockNumber":           10,
	"eth_call":                  26,
	"eth_chainId":               0,
	"eth_estimateGas":           20,
	"eth_feeHistory":            10,
	"eth_gasPrice":              20,
	"eth_getBalance":            20,
	"eth_getBlockByHash":        20,
	"eth_getBlockByNumber":      20,
	"eth_getBlockReceipts":      20,
	"eth_getCode":               20,
	"eth_getLogs":               60,
	"eth_getStorageAt":          20,
	"eth_getTransactionByHash":  20,
	"eth_getTransactionCount":   20,
	"eth_getTransactionReceipt": 20,
	"eth_maxPriorityFeePerGas":  10,
	"eth_sendRawTransaction":    40,
	"net_version":               0,
	"debug_traceTransaction":    40,
	"trace_block":               20,
}

// DefaultAlchemyCreditUnitsRecheckInterval is how long a fetched CU table is
// treated as fresh before an async refresh is triggered. Alchemy's published
// costs change rarely, so a long interval keeps the docs endpoint essentially
// untouched while still tracking pricing changes without a redeploy.
const DefaultAlchemyCreditUnitsRecheckInterval = 7 * 24 * time.Hour

// alchemyCreditUnitsURL is Alchemy's compute-unit-costs docs page served as
// Markdown (the docs platform returns text/markdown for the `.md` suffix): a
// stable, unauthenticated artifact of the per-method CU table. It is the
// closest thing Alchemy offers to a machine-readable CU API — there is no
// JSON endpoint. Declared as a var so tests can point it at a mock server.
var alchemyCreditUnitsURL = "https://www.alchemy.com/docs/reference/compute-unit-costs.md" // #nosec G101 -- public docs URL, not a credential (gosec matches "Credit" in the name)

// alchemyCreditUnitsSections is the allowlist of Markdown H1 sections in
// compute-unit-costs.md whose pipe tables carry the generic EVM JSON-RPC
// method costs eRPC relays. Chain- and product-specific sections (Solana,
// NFT/Token/Prices APIs, per-chain overrides) are ignored so their method
// names can't shadow the standard EVM costs.
var alchemyCreditUnitsSections = map[string]bool{
	"EVM: Standard JSON-RPC Methods": true,
	"Debug API":                      true,
	"Trace API":                      true,
}

// alchemyMethodRe matches a JSON-RPC method name (the first cell of a CU
// table row), rejecting the "Method" header and "---" separator rows.
var alchemyMethodRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]+$`)

// CreditUnits implements common.CreditUnitsProvider: Alchemy's CU table
// (live-fetched from the docs, built-in fallback), overridable per method
// via `providers[].settings.creditUnits`.
func (v *AlchemyVendor) CreditUnits(req *common.NormalizedRequest, upstream *common.UpstreamConfig) int64 {
	method, _ := req.Method()
	var override map[string]int64
	if upstream != nil {
		override = upstream.CreditUnits
	}
	return common.ResolveCreditUnits(v.creditUnitsTable(), override, method)
}

// creditUnitsTable returns Alchemy's effective per-method CU table: the table
// fetched from compute-unit-costs.md (refreshed roughly weekly) when
// available, otherwise the built-in alchemyCreditUnits fallback. Hot-path
// safe — a lock-free Lookup plus a non-blocking, single-flight async refresh
// on staleness (see remote_cache.go). Cold start and every fetch failure
// transparently use the built-in map.
func (v *AlchemyVendor) creditUnitsTable() map[string]int64 {
	if v == nil || v.cuCache == nil {
		return alchemyCreditUnits
	}
	// Pure, lock-free read — the request hot path never triggers I/O. Any
	// snapshot (fresh or stale) is preferred; the refresh is kicked off from
	// the vendor lifecycle (refreshCreditUnitsAsync). Cold cache → built-in.
	table, _ := v.cuCache.Lookup(alchemyCreditUnitsURL, DefaultAlchemyCreditUnitsRecheckInterval)
	if table == nil {
		return alchemyCreditUnits
	}
	return table
}

// refreshCreditUnitsAsync kicks off a non-blocking, single-flight refresh of
// the CU table when the cached snapshot is missing or older than the recheck
// interval. Called from SupportsNetwork / GenerateConfigs — the same
// hot-path-safe lifecycle points that refresh the chain list — so the table
// tracks Alchemy's pricing without a redeploy and without ever fetching from
// the request hot path (see remote_cache.go for the safety rule).
func (v *AlchemyVendor) refreshCreditUnitsAsync(logger *zerolog.Logger) {
	if v == nil || v.cuCache == nil {
		return
	}
	if _, fresh := v.cuCache.Lookup(alchemyCreditUnitsURL, DefaultAlchemyCreditUnitsRecheckInterval); fresh {
		return
	}
	v.cuCache.TriggerAsyncRefresh(logger, alchemyCreditUnitsURL, fetchAlchemyCreditUnits)
}

// fetchAlchemyCreditUnits fetches and parses the CU table from
// alchemyCreditUnitsURL, merged over the built-in table so a partial parse
// (or a docs format change) never loses coverage or the "*" fallback. Run in
// the RemoteDataCache refresh goroutine with a self-contained timeout ctx.
func fetchAlchemyCreditUnits(ctx context.Context) (map[string]int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, alchemyCreditUnitsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/markdown, text/plain")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("alchemy compute-unit-costs returned status %d", resp.StatusCode)
	}

	// The doc is ~70 KB; cap the read so a misbehaving endpoint can't stream
	// unbounded data into memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}

	parsed := parseAlchemyCreditUnits(string(body))
	if len(parsed) == 0 {
		return nil, fmt.Errorf("alchemy compute-unit-costs: no methods parsed (docs format may have changed)")
	}

	// Built-in as base (keeps "*" and any method the parse missed); the
	// fetched values win on conflict.
	merged := make(map[string]int64, len(alchemyCreditUnits)+len(parsed))
	for k, cu := range alchemyCreditUnits {
		merged[k] = cu
	}
	for k, cu := range parsed {
		merged[k] = cu
	}
	return merged, nil
}

// parseAlchemyCreditUnits extracts method→CU from the allowlisted EVM
// sections of the compute-unit-costs Markdown. Each section is a
// GitHub-flavored pipe table `| Method | CU | Throughput CU |`; rows whose CU
// cell is blank or non-numeric (e.g. throughput-only rows) are skipped.
func parseAlchemyCreditUnits(md string) map[string]int64 {
	out := map[string]int64{}
	section := ""
	for _, line := range strings.Split(md, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			section = strings.TrimSpace(strings.TrimLeft(line, "# "))
			continue
		}
		if !alchemyCreditUnitsSections[section] || !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 2 {
			continue
		}
		method := strings.Trim(strings.TrimSpace(cells[0]), "`*")
		if !alchemyMethodRe.MatchString(method) {
			continue // header ("Method") / separator ("---") / junk
		}
		cu, err := strconv.ParseInt(strings.TrimSpace(cells[1]), 10, 64)
		if err != nil {
			continue // blank / non-numeric CU cell
		}
		out[method] = cu
	}
	return out
}

func (v *AlchemyVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}
	// Refresh the CU table off the hot path, alongside chain-list discovery.
	v.refreshCreditUnitsAsync(logger)

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}

	apiUrl, ok := settings["chainsUrl"].(string)
	if !ok || apiUrl == "" {
		apiUrl = alchemyApiUrl
	}

	if err = validateChainsURL(apiUrl); err != nil {
		return false, err
	}

	recheckInterval, ok := settings["recheckInterval"].(time.Duration)
	if !ok {
		recheckInterval = DefaultAlchemyRecheckInterval
	}

	networks := v.resolveNetworks(logger, apiUrl, recheckInterval)
	_, exists := networks[chainID]
	return exists, nil
}

func (v *AlchemyVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}
	// Fetch the live CU table once per upstream at bootstrap (off the hot
	// path); CreditUnits then reads it lock-free with a built-in fallback.
	v.refreshCreditUnitsAsync(logger)

	if upstream.Endpoint == "" {
		apiKey, ok := settings["apiKey"].(string)
		if !ok || apiKey == "" {
			return nil, fmt.Errorf("apiKey is required in alchemy settings")
		}

		if upstream.Evm == nil {
			return nil, fmt.Errorf("alchemy vendor requires upstream.evm to be defined")
		}

		chainID := upstream.Evm.ChainId
		if chainID == 0 {
			return nil, fmt.Errorf("alchemy vendor requires upstream.evm.chainId to be defined")
		}

		apiUrl, ok := settings["chainsUrl"].(string)
		if !ok || apiUrl == "" {
			apiUrl = alchemyApiUrl
		}

		if err := validateChainsURL(apiUrl); err != nil {
			return nil, err
		}

		recheckInterval, ok := settings["recheckInterval"].(time.Duration)
		if !ok {
			recheckInterval = DefaultAlchemyRecheckInterval
		}

		networks := v.resolveNetworks(logger, apiUrl, recheckInterval)

		subdomain, ok := networks[chainID]
		if !ok {
			return nil, fmt.Errorf("unsupported network chain ID for Alchemy: %d", chainID)
		}

		alchemyURL := fmt.Sprintf("https://%s.g.alchemy.com/v2/%s", subdomain, apiKey)
		parsedURL, err := url.Parse(alchemyURL)
		if err != nil {
			return nil, err
		}

		upstream.Endpoint = parsedURL.String()
		upstream.Type = common.UpstreamTypeEvm
	}

	upstream.VendorName = v.Name()

	// upstream.Evm is nil for a non-EVM upstream. Alchemy sells Solana endpoints,
	// so a user-supplied `https://solana-mainnet.g.alchemy.com/v2/KEY` with
	// `type: svm` reaches here having skipped the endpoint-derivation branch
	// above (which is the only thing that requires Evm). zerolog evaluates its
	// arguments eagerly, so an unguarded deref panicked at ANY log level.
	logEvt := logger.Debug()
	if upstream.Evm != nil {
		logEvt = logEvt.Int64("chainId", upstream.Evm.ChainId)
	}
	logEvt.Interface("upstream", upstream).Interface("settings", map[string]interface{}{
		"recheckInterval": settings["recheckInterval"],
	}).Msg("generated upstream from alchemy provider")

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *AlchemyVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	bodyMap, ok := jrr.(*common.JsonRpcResponse)
	if !ok {
		return nil
	}

	err := bodyMap.Error
	if code := err.Code; code != 0 {
		msg := err.Message
		if err.Data != "" {
			details["data"] = err.Data
		}

		if code == -32600 && (strings.Contains(msg, "be authenticated") || strings.Contains(msg, "access key")) {
			return common.NewErrEndpointUnauthorized(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorUnauthorized,
					msg,
					nil,
					details,
				),
			)
		} else if code >= -32099 && code <= -32599 || code >= -32603 && code <= -32699 || code >= -32701 && code <= -32768 {
			// For invalid request errors (codes above), there is a high chance that the error is due to a mistake that the user
			// has done, and retrying another upstream would not help.
			// Ref: https://docs.alchemy.com/reference/error-reference#json-rpc-error-codes
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorClientSideException,
					msg,
					nil,
					details,
				),
			).WithRetryableTowardNetwork(false)
		} else if code == 3 {
			// Alchemy uses code 3 both for EVM execution errors (reverts) and for
			// data-availability errors such as "Unknown block" on near-tip reads.
			// The latter is missing data that another upstream may have, so it must
			// remain retryable toward the network instead of being classified as a
			// deterministic revert.
			if archEvm.IsMissingDataError(err) {
				return common.NewErrEndpointMissingData(
					common.NewErrJsonRpcExceptionInternal(
						code,
						common.JsonRpcErrorMissingData,
						msg,
						nil,
						details,
					),
					req.LastUpstream(),
				)
			}
			return common.NewErrEndpointExecutionException(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorEvmReverted,
					msg,
					nil,
					details,
				),
			)
		}
	}

	// Other errors can be properly handled by generic error handling
	return nil
}

func (v *AlchemyVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "alchemy://") || strings.HasPrefix(ups.Endpoint, "evm+alchemy://") {
		return true
	}

	if ups.VendorName == v.Name() {
		return true
	}

	return strings.Contains(ups.Endpoint, ".alchemy.com") || strings.Contains(ups.Endpoint, ".alchemyapi.io")
}

// resolveNetworks returns the cached network map for apiUrl, or the
// built-in static map if no remote data has been fetched yet. Always
// non-blocking: lock-free Lookup, async refresh on staleness, never holds
// a mutex during HTTP. See remote_cache.go for the safety rule.
func (v *AlchemyVendor) resolveNetworks(logger *zerolog.Logger, apiUrl string, recheckInterval time.Duration) map[int64]string {
	networks, fresh := v.cache.Lookup(apiUrl, recheckInterval)
	if !fresh {
		v.cache.TriggerAsyncRefresh(logger, apiUrl, func(ctx context.Context) (map[int64]string, error) {
			return v.fetchAlchemyNetworks(ctx, apiUrl)
		})
	}
	if networks == nil {
		// Cold start: built-in fallback while async refresh is in flight.
		return defaultAlchemyNetworkSubdomains
	}
	return networks
}

func (v *AlchemyVendor) fetchAlchemyNetworks(ctx context.Context, apiUrl string) (map[int64]string, error) {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, "GET", apiUrl, nil)
	if err != nil {
		return nil, err
	}

	var httpClient = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
		},
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("Alchemy API returned non-200 code: %d", resp.StatusCode)
	}

	var apiResp alchemyNetworkConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse Alchemy API data: %w", err)
	}

	newData := make(map[int64]string)
	for _, network := range apiResp.Result.Data {
		if network.KebabCaseID != "" && network.NetworkChainID != 0 {
			newData[network.NetworkChainID] = network.KebabCaseID
		}
	}

	// Merge with defaults, API data takes precedence
	for chainID, subdomain := range defaultAlchemyNetworkSubdomains {
		if _, exists := newData[chainID]; !exists {
			newData[chainID] = subdomain
		}
	}

	return newData, nil
}
