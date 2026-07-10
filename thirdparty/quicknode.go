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

// quicknodeCreditUnits is QuickNode's published API-credit model
// (https://www.quicknode.com/api-credits, 2026-07-10): a base cost per
// method on EVM chains (20 credits on the Ethereum tier), 2x for Advanced
// APIs (debug/trace family) and 4x for Large Calls (trace replays). Values
// are QuickNode credits, not money.
var quicknodeCreditUnits = map[string]int64{
	"*":                             20,
	"debug_traceBlockByHash":        40,
	"debug_traceBlockByNumber":      40,
	"debug_traceCall":               40,
	"debug_traceTransaction":        40,
	"trace_block":                   40,
	"trace_call":                    40,
	"trace_filter":                  40,
	"trace_transaction":             40,
	"trace_replayBlockTransactions": 80,
	"trace_replayTransaction":       80,
}

// CreditUnits implements common.CreditUnitsProvider: QuickNode's published
// credit model, overridable per method via `providers[].settings.creditUnits`.
func (v *QuicknodeVendor) CreditUnits(req *common.NormalizedRequest, upstream *common.UpstreamConfig) int64 {
	method, _ := req.Method()
	var override map[string]int64
	if upstream != nil {
		override = upstream.CreditUnits
	}
	return common.ResolveCreditUnits(quicknodeCreditUnits, override, method)
}

type QuicknodeEndpoint struct {
	ID      string `json:"id"`
	HttpUrl string `json:"http_url"`
	ChainID int64  `json:"-"`
}

type QuicknodeEndpointsResponse struct {
	Data  []*QuicknodeEndpoint `json:"data"`
	Error string               `json:"error,omitempty"`
}

type QuicknodeFilterParams struct {
	TagIDs    []int
	TagLabels []string
}

const DefaultQuicknodeRecheckInterval = 1 * time.Hour

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
				if upstream.Id != "" {
					upsCopy.Id = fmt.Sprintf("%s-%s", upstream.Id, endpoint.ID)
				} else {
					upsCopy.Id = fmt.Sprintf("quicknode-%d-%s", chainID, endpoint.ID)
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
			return fetched, nil
		})
	}
	if endpoints == nil {
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
		if endpoint.HttpUrl == "" {
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

			// Make eth_chainId call
			reqBody := []byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`)
			req, err := http.NewRequestWithContext(ctx, "POST", e.HttpUrl, bytes.NewReader(reqBody))
			if err != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("failed to create request for endpoint %s: %w", e.ID, err))
				mu.Unlock()
				return
			}

			req.Header.Set("Content-Type", "application/json")

			resp, err := httpClient.Do(req)
			if err != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("failed to fetch chain ID for endpoint %s: %w", e.ID, err))
				mu.Unlock()
				return
			}
			defer resp.Body.Close()

			var result struct {
				Result string `json:"result"`
				Error  *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}

			if err := common.SonicCfg.NewDecoder(resp.Body).Decode(&result); err != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("failed to decode chain ID response for endpoint %s: %w", e.ID, err))
				mu.Unlock()
				return
			}

			if result.Error != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("RPC error for endpoint %s: %s", e.ID, result.Error.Message))
				mu.Unlock()
				return
			}

			// Parse hex chain ID
			chainIDStr := strings.TrimPrefix(result.Result, "0x")
			chainID, err := strconv.ParseInt(chainIDStr, 16, 64)
			if err != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("failed to parse chain ID for endpoint %s: %w", e.ID, err))
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
