package thirdparty

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"golang.org/x/sync/semaphore"
)

// QuicknodeVendor uses RemoteDataCache for lock-free, async-refresh access
// to the per-apiKey endpoint list. See remote_cache.go for the
// request-path safety rule.
type QuicknodeVendor struct {
	common.Vendor
	cache *RemoteDataCache[[]*QuicknodeEndpoint]
}

type QuicknodeEndpoint struct {
	ID         string `json:"id"`
	HttpUrl    string `json:"http_url"`
	Multichain bool   `json:"is_multichain"`
	ChainID    int64  `json:"-"`
	// Slug is populated for derived Chain Prism entries (e.g. "arbitrum-mainnet").
	// Empty for the original endpoint returned by /v0/endpoints.
	Slug string `json:"-"`
}

type QuicknodeEndpointsResponse struct {
	Data  []*QuicknodeEndpoint `json:"data"`
	Error string               `json:"error,omitempty"`
}

// QuicknodeUrlsResponse is the wire shape of GET /v0/endpoints/:id/urls.
// Data is nil-safe: QuickNode returns {"data": null, "error": "..."} for
// invalid ids, and {"data": {..., "multichain_urls": null}} for single-chain
// endpoints.
type QuicknodeUrlsResponse struct {
	Data  *QuicknodeUrlsData `json:"data"`
	Error string             `json:"error,omitempty"`
}

type QuicknodeUrlsData struct {
	HttpUrl        string                       `json:"http_url"`
	WssUrl         string                       `json:"wss_url"`
	MultichainUrls map[string]QuicknodeSlugUrls `json:"multichain_urls"`
}

type QuicknodeSlugUrls struct {
	HttpUrl string `json:"http_url"`
	WssUrl  string `json:"wss_url"`
}

type QuicknodeFilterParams struct {
	TagIDs           []int
	TagLabels        []string
	EnableMultiChain bool
}

const (
	DefaultQuicknodeRecheckInterval = 1 * time.Hour
)

func CreateQuicknodeVendor() common.Vendor {
	return &QuicknodeVendor{
		cache: NewRemoteDataCache[[]*QuicknodeEndpoint]("quicknode"),
	}
}

func (v *QuicknodeVendor) Name() string {
	return "quicknode"
}

func (v *QuicknodeVendor) extractFilterParams(settings common.VendorSettings) *QuicknodeFilterParams {
	params := &QuicknodeFilterParams{}

	// Extract tagIds - can be a single integer or array of integers
	if tagIds, ok := settings["tagIds"]; ok && tagIds != nil {
		switch val := tagIds.(type) {
		case int:
			params.TagIDs = []int{val}
		case []int:
			params.TagIDs = val
		case []interface{}:
			for _, id := range val {
				if intVal, ok := id.(int); ok {
					params.TagIDs = append(params.TagIDs, intVal)
				}
			}
		}
	}

	// Extract tagLabels - can be a single string or array of strings
	if tagLabels, ok := settings["tagLabels"]; ok && tagLabels != nil {
		switch val := tagLabels.(type) {
		case string:
			params.TagLabels = []string{val}
		case []string:
			params.TagLabels = val
		case []interface{}:
			for _, label := range val {
				if strLabel, ok := label.(string); ok {
					params.TagLabels = append(params.TagLabels, strLabel)
				}
			}
		}
	}

	// Opt-in flag for QuickNode Chain Prism (multi-chain) endpoint expansion.
	if enable, ok := settings["enableMultiChain"].(bool); ok {
		params.EnableMultiChain = enable
	}

	return params
}

// SupportsNetwork answers the routing-time question "does this vendor
// handle this network?" — on the request hot path. It MUST NOT block on a
// mutex or an HTTP call. Reads are lock-free via RemoteDataCache; staleness
// triggers an async refresh; cold start returns ErrRemoteCacheCold so the
// bootstrap auto-retry loop reschedules.
func (v *QuicknodeVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}

	apiKey, ok := settings["apiKey"].(string)
	if !ok || apiKey == "" {
		return false, nil
	}

	recheckInterval := DefaultQuicknodeRecheckInterval
	if interval, ok := settings["recheckInterval"].(time.Duration); ok {
		recheckInterval = interval
	}

	endpoints, ok := v.resolveEndpoints(logger, apiKey, recheckInterval, settings)
	if !ok {
		return false, ErrRemoteCacheCold
	}
	for _, endpoint := range endpoints {
		if endpoint.ChainID == chainID && endpoint.HttpUrl != "" {
			return true, nil
		}
	}
	return false, nil
}

// GenerateConfigs builds upstream configurations for the given network.
// Static Endpoint is in-memory only; dynamic discovery uses the same
// lock-free snapshot as SupportsNetwork.
func (v *QuicknodeVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint == "" {
		apiKey, ok := settings["apiKey"].(string)
		if !ok || apiKey == "" {
			return nil, fmt.Errorf("apiKey is required in quicknode settings")
		}
		if upstream.Evm == nil {
			return nil, fmt.Errorf("quicknode vendor requires upstream.evm to be defined")
		}
		chainID := upstream.Evm.ChainId
		if chainID == 0 {
			return nil, fmt.Errorf("quicknode vendor requires upstream.evm.chainId to be defined")
		}

		recheckInterval := DefaultQuicknodeRecheckInterval
		if interval, ok := settings["recheckInterval"].(time.Duration); ok {
			recheckInterval = interval
		}

		endpoints, ok := v.resolveEndpoints(logger, apiKey, recheckInterval, settings)
		if !ok {
			return nil, ErrRemoteCacheCold
		}

		var upstreams []*common.UpstreamConfig
		for _, endpoint := range endpoints {
			if endpoint.ChainID == chainID && endpoint.HttpUrl != "" {
				upsCopy := upstream.Copy()
				suffix := endpoint.ID
				if endpoint.Slug != "" {
					suffix = fmt.Sprintf("%s-%s", endpoint.ID, endpoint.Slug)
				}
				if upstream.Id != "" {
					upsCopy.Id = fmt.Sprintf("%s-%s", upstream.Id, suffix)
				} else {
					upsCopy.Id = fmt.Sprintf("quicknode-%d-%s", chainID, suffix)
				}
				upsCopy.Endpoint = endpoint.HttpUrl
				upsCopy.Type = common.UpstreamTypeEvm
				upstreams = append(upstreams, upsCopy)
			}
		}
		return upstreams, nil
	}
	return []*common.UpstreamConfig{upstream}, nil
}

// resolveEndpoints does a lock-free Lookup, kicks off an async refresh on
// staleness, and returns (endpoints, true) on hit or (nil, false) on cold
// start. See remote_cache.go for the request-path safety rule.
func (v *QuicknodeVendor) resolveEndpoints(logger *zerolog.Logger, apiKey string, recheckInterval time.Duration, settings common.VendorSettings) ([]*QuicknodeEndpoint, bool) {
	endpoints, fresh := v.cache.Lookup(apiKey, recheckInterval)
	if !fresh {
		filterParams := v.extractFilterParams(settings)
		v.cache.TriggerAsyncRefresh(logger, apiKey, func(ctx context.Context) ([]*QuicknodeEndpoint, error) {
			fetched, err := v.fetchEndpoints(ctx, apiKey, filterParams)
			if err != nil {
				return nil, err
			}
			if err := v.fetchChainIDs(ctx, logger, fetched); err != nil {
				// Partial success: chain ID fetches may individually fail
				// without invalidating the rest of the data.
				logger.Warn().Err(err).Msg("some quicknode chain ID fetches failed; continuing with available data")
			}
			// If Chain Prism support is opted-in, fetch per-endpoint multichain URLs
			// from /v0/endpoints/:id/urls and probe each one. The probe is still the
			// activation gate (Select Access Chains the customer hasn't paid for are
			// listed in /urls but return 401 at JSON-RPC time).
			if filterParams != nil && filterParams.EnableMultiChain {
				extra := v.probeMultiChainExpansions(ctx, logger, fetched, apiKey)
				fetched = append(fetched, extra...)
			}
			return fetched, nil
		})
	}
	if !v.cache.Has(apiKey) {
		return nil, false
	}
	return endpoints, true
}

func (v *QuicknodeVendor) fetchEndpoints(ctx context.Context, apiKey string, filterParams *QuicknodeFilterParams) ([]*QuicknodeEndpoint, error) {
	var allEndpoints []*QuicknodeEndpoint

	// Build URL with pagination
	baseURL := "https://api.quicknode.com/v0/endpoints"
	limit := 100
	offset := 0

	httpClient := &http.Client{
		Timeout: 30 * time.Second,
	}

	for {
		// Build URL with query parameters
		params := url.Values{}
		params.Set("limit", strconv.Itoa(limit))
		params.Set("offset", strconv.Itoa(offset))

		// Add tag_ids filter if provided (comma-separated list)
		if filterParams != nil && len(filterParams.TagIDs) > 0 {
			tagIDStrs := make([]string, len(filterParams.TagIDs))
			for i, id := range filterParams.TagIDs {
				tagIDStrs[i] = strconv.Itoa(id)
			}
			params.Set("tag_ids", strings.Join(tagIDStrs, ","))
		}

		// Add tag_labels filter if provided (comma-separated list)
		if filterParams != nil && len(filterParams.TagLabels) > 0 {
			params.Set("tag_labels", strings.Join(filterParams.TagLabels, ","))
		}

		requestURL := baseURL + "?" + params.Encode()

		req, err := http.NewRequestWithContext(ctx, "GET", requestURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("accept", "application/json")
		req.Header.Set("x-api-key", apiKey)

		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("quicknode API returned status %d: %s", resp.StatusCode, string(body))
		}

		var endpointsResp QuicknodeEndpointsResponse
		if err := common.SonicCfg.NewDecoder(resp.Body).Decode(&endpointsResp); err != nil {
			return nil, fmt.Errorf("failed to decode QuickNode endpoints response: %w", err)
		}

		if endpointsResp.Error != "" {
			return nil, fmt.Errorf("quicknode API error: %s", endpointsResp.Error)
		}

		// Filter out endpoints without HTTP URLs
		for _, endpoint := range endpointsResp.Data {
			if endpoint.HttpUrl != "" {
				allEndpoints = append(allEndpoints, endpoint)
			}
		}

		// Check if we got fewer results than the limit, indicating we've reached the end
		if len(endpointsResp.Data) < limit {
			break
		}

		offset += limit
	}

	return allEndpoints, nil
}

func (v *QuicknodeVendor) fetchChainIDs(ctx context.Context, logger *zerolog.Logger, endpoints []*QuicknodeEndpoint) error {
	// Use semaphore to limit concurrent requests
	sem := semaphore.NewWeighted(10)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errors []error

	httpClient := &http.Client{
		Timeout: 10 * time.Second,
	}

	for _, endpoint := range endpoints {
		if endpoint.HttpUrl == "" || endpoint.ChainID != 0 {
			continue
		}

		wg.Add(1)
		go func(e *QuicknodeEndpoint) {
			defer wg.Done()

			if err := sem.Acquire(ctx, 1); err != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("failed to acquire semaphore for endpoint %s: %w", e.ID, err))
				mu.Unlock()
				return
			}
			defer sem.Release(1)

			chainID, err := probeChainID(ctx, httpClient, e.HttpUrl)
			if err != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("endpoint %s: %w", e.ID, err))
				mu.Unlock()
				return
			}
			e.ChainID = chainID
		}(endpoint)
	}

	wg.Wait()

	if len(errors) > 0 {
		logger.Warn().Errs("errors", errors).Msg("failed to fetch chain IDs for some QuickNode endpoints")
	}

	return nil
}

// probeChainID sends a JSON-RPC eth_chainId call to httpUrl and returns the
// resolved chain ID, or an error describing why the probe failed. Used both
// for root endpoints and for Chain Prism derived URLs.
func probeChainID(ctx context.Context, httpClient *http.Client, httpUrl string) (int64, error) {
	reqBody := []byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`)
	req, err := http.NewRequestWithContext(ctx, "POST", httpUrl, bytes.NewReader(reqBody))
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch chain ID: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("non-200 status %d", resp.StatusCode)
	}

	var result struct {
		Result string `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := common.SonicCfg.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("failed to decode chain ID response: %w", err)
	}

	if result.Error != nil {
		return 0, fmt.Errorf("RPC error: %s", result.Error.Message)
	}

	chainIDStr := strings.TrimPrefix(result.Result, "0x")
	chainID, err := strconv.ParseInt(chainIDStr, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse chain ID %q: %w", result.Result, err)
	}
	return chainID, nil
}

// fetchEndpointUrls calls GET /v0/endpoints/:id/urls and returns the
// slug -> http_url map of multichain URLs for the endpoint. Returns
// (nil, nil) when the endpoint is single-chain (multichain_urls is null)
// or the endpoint id is no longer valid (HTTP 404).
func fetchEndpointUrls(ctx context.Context, apiKey, endpointID string) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.quicknode.com/v0/endpoints/"+endpointID+"/urls", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("x-api-key", apiKey)

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("quicknode urls API returned status %d: %s", resp.StatusCode, string(body))
	}

	var out QuicknodeUrlsResponse
	if err := common.SonicCfg.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("failed to decode QuickNode urls response: %w", err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("quicknode urls API error: %s", out.Error)
	}
	if out.Data == nil || out.Data.MultichainUrls == nil {
		return nil, nil
	}

	urls := make(map[string]string, len(out.Data.MultichainUrls))
	for slug, entry := range out.Data.MultichainUrls {
		if entry.HttpUrl != "" {
			urls[slug] = entry.HttpUrl
		}
	}
	return urls, nil
}

// probeMultiChainExpansions fetches /v0/endpoints/:id/urls per multichain
// endpoint, then probes each listed URL with eth_chainId. Successful probes
// yield a derived endpoint carrying the probed chain_id; failures (non-EVM
// slugs, Select Access Chains the customer hasn't paid for, etc.) are
// dropped at debug level. Single-chain endpoints are skipped entirely.
func (v *QuicknodeVendor) probeMultiChainExpansions(ctx context.Context, logger *zerolog.Logger, endpoints []*QuicknodeEndpoint, apiKey string) []*QuicknodeEndpoint {
	// Chain Prism discovery probes ~137 URLs per multichain endpoint and can
	// take 60-90s end-to-end on a cold account. The caller's context is
	// typically bound to the triggering HTTP request (default 30s read
	// timeout), which would cancel probes mid-sweep and leave chains
	// unregistered. Detach via context.WithoutCancel + our own budget so the
	// sweep runs to completion; subsequent requests then see a stable set.
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()

	type candidate struct {
		source *QuicknodeEndpoint
		slug   string
		url    string
	}

	var candidates []candidate
	for _, e := range endpoints {
		if !e.Multichain {
			continue
		}
		urls, err := fetchEndpointUrls(probeCtx, apiKey, e.ID)
		if err != nil {
			logger.Warn().Str("endpoint_id", e.ID).Err(err).Msg("failed to fetch QuickNode endpoint urls; skipping multi-chain expansion for this endpoint")
			continue
		}
		for slug, httpURL := range urls {
			if httpURL == "" || httpURL == e.HttpUrl {
				// Skip the endpoint's own root URL to avoid duplicate
				// registrations; it's already in the endpoints list.
				continue
			}
			candidates = append(candidates, candidate{source: e, slug: slug, url: httpURL})
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	// /v0/endpoints/:id/urls returns every slug QuickNode supports (~137
	// including non-EVM like btc/solana), and cold-start latency on
	// never-touched EVM subdomains routinely reaches 15-25s. Size the
	// semaphore large enough that all candidates can dispatch in a single
	// batch even when tens of them run their full 30s HTTP timeout, so slow
	// probes never starve the queue and block legitimate EVM chains.
	sem := semaphore.NewWeighted(60)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var successes []*QuicknodeEndpoint

	httpClient := &http.Client{Timeout: 30 * time.Second}

	for _, c := range candidates {
		wg.Add(1)
		go func(c candidate) {
			defer wg.Done()
			if err := sem.Acquire(probeCtx, 1); err != nil {
				return
			}
			defer sem.Release(1)

			probed, err := probeChainID(probeCtx, httpClient, c.url)
			if err != nil {
				// Expected: non-EVM slugs and un-activated Select chains
				// return 401/404 or non-JSON-RPC bodies.
				logger.Debug().Str("endpoint_id", c.source.ID).Str("slug", c.slug).Err(err).Msg("quicknode multi-chain probe failed")
				return
			}
			mu.Lock()
			successes = append(successes, &QuicknodeEndpoint{
				ID:      c.source.ID,
				HttpUrl: c.url,
				ChainID: probed,
				Slug:    c.slug,
			})
			mu.Unlock()
		}(c)
	}
	wg.Wait()

	return successes
}

func (v *QuicknodeVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	bodyMap, ok := jrr.(*common.JsonRpcResponse)
	if !ok {
		return nil
	}

	err := bodyMap.Error
	if code := err.Code; code != 0 {
		msg := err.Message
		var details map[string]interface{} = make(map[string]interface{})
		if err.Data != "" {
			details["data"] = err.Data
		}

		method, _ := req.Method()

		if code == -32614 || (method == "eth_getLogs" && strings.Contains(msg, "limited to")) {
			return common.NewErrEndpointRequestTooLarge(
				common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorEvmLargeRange, msg, nil, details),
				common.EvmBlockRangeTooLarge,
			)
		} else if code == -32009 || code == -32007 {
			return common.NewErrEndpointCapacityExceeded(
				common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorCapacityExceeded, msg, nil, details),
			)
		} else if code == -32612 || code == -32613 {
			return common.NewErrEndpointUnsupported(
				common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorCapacityExceeded, msg, nil, details),
			)
		} else if strings.Contains(msg, "failed to parse") {
			// We do not retry on parse errors, as retrying another upstream would not help.
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorParseException, msg, nil, details),
			).WithRetryableTowardNetwork(false)
		} else if code == -32010 { // Transaction cost exceeds current gas limit
			// retrying on gas limit exceeded errors toward other upstreams would be helpful, as max gas limit
			// can be defined per client (reth, geth, parity, etc.) (still needs to be lower than overall block gas limit)
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorClientSideException, msg, nil, details),
			)
		} else if code == -32602 && strings.Contains(msg, "cannot unmarshal hex string") {
			// we do not retry on invalid argument errors, as retrying another upstream would not help.
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorInvalidArgument, msg, nil, details),
			).WithRetryableTowardNetwork(false)
		} else if strings.Contains(msg, "UNAUTHORIZED") {
			return common.NewErrEndpointUnauthorized(
				common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorUnauthorized, msg, nil, details),
			)
		} else if code == 3 {
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

func (v *QuicknodeVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "quicknode://") || strings.HasPrefix(ups.Endpoint, "evm+quicknode://") {
		return true
	}

	return strings.Contains(ups.Endpoint, ".quiknode.pro")
}
