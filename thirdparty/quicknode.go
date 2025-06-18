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
	"github.com/rs/zerolog/log"
	"golang.org/x/sync/semaphore"
)

type QuicknodeVendor struct {
	common.Vendor

	remoteDataLock          sync.RWMutex
	remoteData              map[string][]*QuicknodeEndpoint
	remoteDataLastFetchedAt map[string]time.Time
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

const DefaultQuicknodeRecheckInterval = 1 * time.Hour

func CreateQuicknodeVendor() common.Vendor {
	return &QuicknodeVendor{
		remoteData:              make(map[string][]*QuicknodeEndpoint),
		remoteDataLastFetchedAt: make(map[string]time.Time),
	}
}

func (v *QuicknodeVendor) Name() string {
	return "quicknode"
}

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

	// Check if we have endpoints for this chain ID
	recheckInterval := DefaultQuicknodeRecheckInterval
	if interval, ok := settings["recheckInterval"].(time.Duration); ok {
		recheckInterval = interval
	}

	err = v.ensureRefreshEndpoints(ctx, logger, apiKey, recheckInterval)
	if err != nil {
		logger.Warn().Err(err).Msg("failed to refresh QuickNode endpoints")
		return false, err
	}

	v.remoteDataLock.RLock()
	endpoints := v.remoteData[apiKey]
	v.remoteDataLock.RUnlock()

	for _, endpoint := range endpoints {
		if endpoint.ChainID == chainID && endpoint.HttpUrl != "" {
			return true, nil
		}
	}

	return false, nil
}

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

		// Try to use dynamic endpoint discovery
		recheckInterval := DefaultQuicknodeRecheckInterval
		if interval, ok := settings["recheckInterval"].(time.Duration); ok {
			recheckInterval = interval
		}

		err := v.ensureRefreshEndpoints(ctx, &log.Logger, apiKey, recheckInterval)
		if err != nil {
			log.Warn().Err(err).Msg("failed to refresh QuickNode endpoints, falling back to static endpoint generation")
			return nil, err
		}

		v.remoteDataLock.RLock()
		endpoints := v.remoteData[apiKey]
		v.remoteDataLock.RUnlock()

		var upstreams []*common.UpstreamConfig
		for _, endpoint := range endpoints {
			if endpoint.ChainID == chainID && endpoint.HttpUrl != "" {
				// Create a copy of the upstream config for each endpoint
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
	} else {
		return []*common.UpstreamConfig{upstream}, nil
	}
}

func (v *QuicknodeVendor) ensureRefreshEndpoints(ctx context.Context, logger *zerolog.Logger, apiKey string, recheckInterval time.Duration) error {
	v.remoteDataLock.Lock()
	defer v.remoteDataLock.Unlock()

	// Check if we've fetched recently
	if lastFetch, ok := v.remoteDataLastFetchedAt[apiKey]; ok && time.Since(lastFetch) < recheckInterval {
		return nil
	}

	// Fetch endpoints from API
	endpoints, err := v.fetchEndpoints(ctx, apiKey)
	if err != nil {
		// Keep stale data if fetch fails
		if _, hasData := v.remoteData[apiKey]; hasData {
			logger.Warn().Err(err).Msg("could not refresh QuickNode endpoints data; will use stale data")
			return nil
		}
		return err
	}

	// Fetch chain IDs in parallel
	err = v.fetchChainIDs(ctx, endpoints)
	if err != nil {
		log.Warn().Err(err).Msg("some chain ID fetches failed, but continuing with available data")
	}

	// Update cache
	v.remoteData[apiKey] = endpoints
	v.remoteDataLastFetchedAt[apiKey] = time.Now()

	return nil
}

func (v *QuicknodeVendor) fetchEndpoints(ctx context.Context, apiKey string) ([]*QuicknodeEndpoint, error) {
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

func (v *QuicknodeVendor) fetchChainIDs(ctx context.Context, endpoints []*QuicknodeEndpoint) error {
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
		log.Warn().Errs("errors", errors).Msg("failed to fetch chain IDs for some QuickNode endpoints")
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
