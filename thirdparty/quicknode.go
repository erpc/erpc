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
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"golang.org/x/sync/semaphore"
)

type QuicknodeVendor struct {
	common.Vendor

	// snapshot is the immutable read-only view of vendor state. Hot-path
	// callers (SupportsNetwork, GenerateConfigs) do an atomic.Load and
	// never wait for a mutex or HTTP call. Refreshes build a new snapshot
	// off-thread and CAS it in place.
	snapshot atomic.Pointer[quicknodeSnapshot]

	// refreshMu serializes the inflight tracker only. It is NEVER held
	// during an HTTP call. Holding it for any reason longer than a few
	// nanoseconds violates the request-path safety rule documented above.
	refreshMu sync.Mutex
	inflight  map[string]struct{} // key: apiKey + filter hash; presence = refresh running
}

// quicknodeSnapshot is immutable once published. Building a refreshed
// snapshot is copy-on-write — refresh goroutines clone the maps, mutate
// the copy, then CAS the new pointer into place.
type quicknodeSnapshot struct {
	endpoints map[string][]*QuicknodeEndpoint
	fetchedAt map[string]time.Time
}

// errQuicknodeCacheCold is returned by SupportsNetwork when no snapshot
// has been populated yet for the requested apiKey. The bootstrap initializer
// classifies this as retryable (not fatal), so the auto-retry loop will
// call SupportsNetwork again later — by which point the async refresh
// kicked off in this same call should have populated the cache.
var errQuicknodeCacheCold = fmt.Errorf("quicknode endpoint cache not yet populated; retry shortly")

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
		inflight: make(map[string]struct{}),
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

// SupportsNetwork answers the routing-time question "does this vendor handle
// this network?" — and is on the request hot path. It MUST NOT block on a
// mutex or an HTTP call. See the request-path safety rule at the top of this
// file.
//
// Behaviour:
//   - Lock-free atomic snapshot read.
//   - Triggers an async refresh (fire-and-forget) when the snapshot is stale
//     or missing. The refresh runs in its own goroutine and never holds any
//     mutex during the HTTP call.
//   - Cold-start (no snapshot yet) returns errQuicknodeCacheCold so the
//     bootstrap auto-retry loop schedules another attempt; by then the
//     async refresh kicked off here will have populated the snapshot.
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
	filterParams := v.extractFilterParams(settings)

	endpoints, fresh := v.lookupSnapshot(apiKey, recheckInterval)
	if !fresh {
		// Off-thread refresh; this call returns immediately.
		v.triggerAsyncRefresh(logger, apiKey, filterParams)
	}
	if endpoints == nil {
		// Cold start: no data yet. Surface a retryable error so the
		// bootstrap auto-retry loop tries again after the async refresh
		// populates the snapshot.
		return false, errQuicknodeCacheCold
	}
	for _, endpoint := range endpoints {
		if endpoint.ChainID == chainID && endpoint.HttpUrl != "" {
			return true, nil
		}
	}
	return false, nil
}

// GenerateConfigs builds upstream configurations for this vendor for a given
// network. When the upstream has a static Endpoint, this is purely
// in-memory. When using dynamic endpoint discovery, it relies on the same
// lock-free snapshot as SupportsNetwork — and triggers an async refresh on
// staleness rather than blocking. Cold start returns an error so the
// bootstrap auto-retry loop reschedules.
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
		filterParams := v.extractFilterParams(settings)

		endpoints, fresh := v.lookupSnapshot(apiKey, recheckInterval)
		if !fresh {
			v.triggerAsyncRefresh(logger, apiKey, filterParams)
		}
		if endpoints == nil {
			return nil, errQuicknodeCacheCold
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

// lookupSnapshot returns (endpoints-for-key, fresh) for the current snapshot.
// Reading the snapshot is lock-free; this is the hot path called from
// SupportsNetwork on every routing decision.
//
//   - endpoints == nil means the cache has never been populated for this key.
//   - fresh == true means the cached fetchedAt is within recheckInterval.
//
// A return of (non-nil, false) means stale data — caller should still use
// the data and trigger an async refresh in the background.
func (v *QuicknodeVendor) lookupSnapshot(apiKey string, recheckInterval time.Duration) ([]*QuicknodeEndpoint, bool) {
	snap := v.snapshot.Load()
	if snap == nil {
		return nil, false
	}
	endpoints, ok := snap.endpoints[apiKey]
	if !ok {
		return nil, false
	}
	fetchedAt := snap.fetchedAt[apiKey]
	return endpoints, time.Since(fetchedAt) < recheckInterval
}

// triggerAsyncRefresh starts a background refresh for the given key if and
// only if no other refresh is currently in flight for the same key. NEVER
// holds a mutex while calling out to the QuickNode API. Single-flight: if a
// refresh is already running, this call is a no-op.
//
// The HTTP work is done with a budgeted background context so it cannot leak
// past the calling process's lifetime. The refresh either succeeds and CAS-es
// a new snapshot in place, or fails silently — readers continue to use
// whatever snapshot was previously published. There is no return path that
// can block a request goroutine.
func (v *QuicknodeVendor) triggerAsyncRefresh(logger *zerolog.Logger, apiKey string, filterParams *QuicknodeFilterParams) {
	v.refreshMu.Lock()
	if _, busy := v.inflight[apiKey]; busy {
		v.refreshMu.Unlock()
		return
	}
	v.inflight[apiKey] = struct{}{}
	v.refreshMu.Unlock()

	go func() {
		defer func() {
			v.refreshMu.Lock()
			delete(v.inflight, apiKey)
			v.refreshMu.Unlock()
			if rec := recover(); rec != nil {
				logger.Error().Interface("panic", rec).Msg("panic recovered during QuickNode async refresh")
			}
		}()

		// Self-contained context: the original request that triggered this
		// refresh may have already returned; we should not be cancelled by it.
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		endpoints, err := v.fetchEndpoints(ctx, apiKey, filterParams)
		if err != nil {
			logger.Warn().Err(err).Msg("QuickNode endpoint refresh failed; keeping previous snapshot")
			return
		}
		if err := v.fetchChainIDs(ctx, logger, endpoints); err != nil {
			logger.Warn().Err(err).Msg("some QuickNode chain ID fetches failed; continuing with available data")
		}

		// Copy-on-write snapshot publish.
		old := v.snapshot.Load()
		newSnap := &quicknodeSnapshot{
			endpoints: make(map[string][]*QuicknodeEndpoint),
			fetchedAt: make(map[string]time.Time),
		}
		if old != nil {
			for k, v := range old.endpoints {
				newSnap.endpoints[k] = v
			}
			for k, v := range old.fetchedAt {
				newSnap.fetchedAt[k] = v
			}
		}
		newSnap.endpoints[apiKey] = endpoints
		newSnap.fetchedAt[apiKey] = time.Now()
		v.snapshot.Store(newSnap)
	}()
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
