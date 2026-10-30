package thirdparty

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// defaultWillowChains is a built-in snapshot of the chains Willow serves
// (https://docs.willow.tech/reference/chains/), used until a fetch of the list
// at chainsUrl (default willowChainsURL) succeeds; after that, a failed fetch
// keeps the last list. It answers SupportsNetwork only: GenerateConfigs builds
// the URL for any chain id, so onlyNetworks can add a chain before either list
// does.
var defaultWillowChains = map[int64]struct{}{
	1:     {}, // Ethereum
	10:    {}, // OP Mainnet
	130:   {}, // Unichain
	4663:  {}, // Robinhood Chain
	8453:  {}, // Base
	42161: {}, // Arbitrum One
	57073: {}, // Ink
}

const willowHost = "api.willow.tech"

// willowChainsURL is a var (not const) so tests can point it at a mock server.
var willowChainsURL = "https://" + willowHost + "/v1/rpc/chains"

const DefaultWillowRecheckInterval = 24 * time.Hour

// willowChainsResponse is GET /v1/rpc/chains: {"chains":[{"id":1,"name":"Ethereum"},...]}.
// Entries are decoded one by one so an entry this vendor can't read is skipped, not fatal.
type willowChainsResponse struct {
	Chains []json.RawMessage `json:"chains"`
}

// WillowVendor uses RemoteDataCache for lock-free, async-refresh access to
// the chain list. See remote_cache.go for the request-path safety rule.
type WillowVendor struct {
	common.Vendor
	cache *RemoteDataCache[map[int64]struct{}]
}

func CreateWillowVendor() common.Vendor {
	return &WillowVendor{
		cache: NewRemoteDataCache[map[int64]struct{}]("willow"),
	}
}

func (v *WillowVendor) Name() string {
	return "willow"
}

func (v *WillowVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}
	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}

	chainsURL, ok := settings["chainsUrl"].(string)
	if !ok || chainsURL == "" {
		chainsURL = willowChainsURL
	}

	if err = validateChainsURL(chainsURL); err != nil {
		return false, err
	}

	recheckInterval, ok := settings["recheckInterval"].(time.Duration)
	if !ok {
		recheckInterval = DefaultWillowRecheckInterval
	}

	_, exists := v.resolveChains(logger, chainsURL, recheckInterval)[chainID]
	return exists, nil
}

// resolveChains does a lock-free Lookup, kicks off an async refresh on
// staleness, and falls back to the built-in chain map on cold start.
// Never blocks the request hot path on an HTTP call. See remote_cache.go.
func (v *WillowVendor) resolveChains(logger *zerolog.Logger, chainsURL string, recheckInterval time.Duration) map[int64]struct{} {
	chains, fresh := v.cache.Lookup(chainsURL, recheckInterval)
	if !fresh {
		v.cache.TriggerAsyncRefresh(logger, chainsURL, func(ctx context.Context) (map[int64]struct{}, error) {
			return v.fetchWillowChains(ctx, logger, chainsURL)
		})
	}
	if chains == nil {
		return defaultWillowChains
	}
	return chains
}

func (v *WillowVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}
	if upstream.Endpoint != "" {
		return []*common.UpstreamConfig{upstream}, nil
	}

	apiKey, _ := settings["apiKey"].(string)
	if apiKey == "" {
		return nil, fmt.Errorf("apiKey is required in willow settings")
	}
	if upstream.Evm == nil || upstream.Evm.ChainId == 0 {
		return nil, fmt.Errorf("willow vendor requires upstream.evm.chainId to be defined")
	}

	// The key is the last path segment: https://api.willow.tech/v1/rpc/{chainId}/{apiKey}
	upstream.Endpoint = fmt.Sprintf("https://%s/v1/rpc/%d/%s", willowHost, upstream.Evm.ChainId, url.PathEscape(apiKey))
	upstream.Type = common.UpstreamTypeEvm

	return []*common.UpstreamConfig{upstream}, nil
}

// GetVendorSpecificErrorIfAny adds nothing: the generic EVM normalizer already
// maps Willow's errors, as willow_test.go pins.
func (v *WillowVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	return nil
}

func (v *WillowVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "willow://") || strings.HasPrefix(ups.Endpoint, "evm+willow://") {
		return true
	}
	return strings.Contains(ups.Endpoint, "://"+willowHost+"/")
}

func (v *WillowVendor) fetchWillowChains(ctx context.Context, logger *zerolog.Logger, chainsURL string) (map[int64]struct{}, error) {
	var httpClient = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
		},
	}

	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, "GET", chainsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("willow chains API returned non-2xx code: %d", resp.StatusCode)
	}

	var body willowChainsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("failed to parse willow chains response: %w", err)
	}

	chains := make(map[int64]struct{}, len(body.Chains))
	for _, raw := range body.Chains {
		var chain struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(raw, &chain); err != nil || chain.ID <= 0 {
			logger.Debug().RawJSON("chain", raw).Err(err).Msg("skipping unreadable Willow chain entry")
			continue
		}
		chains[chain.ID] = struct{}{}
	}
	// No readable entry (an empty list, or another body shape) keeps the previous snapshot or the fallback.
	if len(chains) == 0 {
		return nil, fmt.Errorf("willow chains response lists no chains")
	}

	logger.Info().Int("count", len(chains)).Str("url", chainsURL).Msg("successfully fetched Willow chains")

	return chains, nil
}
