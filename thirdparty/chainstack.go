package thirdparty

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/sync/semaphore"
)

type ChainstackVendor struct {
	common.Vendor

	// local cache of nodes data
	nodesDataLock          sync.RWMutex
	nodesData              map[string][]*ChainstackNode // key is apiKey
	nodesDataLastFetchedAt map[string]time.Time
}

type ChainstackNode struct {
	ID            string                `json:"id"`
	Status        string                `json:"status"`
	Configuration ChainstackNodeConfig  `json:"configuration"`
	Details       ChainstackNodeDetails `json:"details"`
	ChainID       int64                 `json:"-"` // populated by eth_chainId call
}

type ChainstackNodeConfig struct {
	Archive bool `json:"archive,omitempty"`
}

type ChainstackNodeDetails struct {
	HTTPSEndpoint string `json:"https_endpoint"`
	AuthKey       string `json:"auth_key,omitempty"`
}

type ChainstackNodesResponse struct {
	Next    *string           `json:"next"`
	Results []json.RawMessage `json:"results"`
}

const DefaultChainstackRecheckInterval = 1 * time.Hour

func CreateChainstackVendor() common.Vendor {
	return &ChainstackVendor{
		nodesData:              make(map[string][]*ChainstackNode),
		nodesDataLastFetchedAt: make(map[string]time.Time),
	}
}

func (v *ChainstackVendor) Name() string {
	return "chainstack"
}

func (v *ChainstackVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}

	apiKey, ok := settings["apiKey"].(string)
	if !ok {
		return ok, nil
	}

	// If we have an API key, check if we have nodes for this chain ID
	recheckInterval := DefaultChainstackRecheckInterval
	if interval, ok := settings["recheckInterval"].(time.Duration); ok {
		recheckInterval = interval
	}

	err = v.ensureRefreshNodes(ctx, logger, apiKey, recheckInterval)
	if err != nil {
		logger.Warn().Err(err).Msg("failed to refresh Chainstack nodes, falling back to static network names")
		return false, err
	}

	v.nodesDataLock.RLock()
	nodes := v.nodesData[apiKey]
	v.nodesDataLock.RUnlock()

	for _, node := range nodes {
		if node.ChainID == chainID && node.Status == "running" {
			return true, nil
		}
	}

	return false, nil
}

func (v *ChainstackVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint == "" {
		apiKey, ok := settings["apiKey"].(string)
		if !ok || apiKey == "" {
			return nil, fmt.Errorf("apiKey is required in chainstack settings")
		}

		if upstream.Evm == nil {
			return nil, fmt.Errorf("chainstack vendor requires upstream.evm to be defined")
		}
		chainID := upstream.Evm.ChainId
		if chainID == 0 {
			return nil, fmt.Errorf("chainstack vendor requires upstream.evm.chainId to be defined")
		}

		// Try to use dynamic node discovery first
		recheckInterval := DefaultChainstackRecheckInterval
		if interval, ok := settings["recheckInterval"].(time.Duration); ok {
			recheckInterval = interval
		}

		err := v.ensureRefreshNodes(ctx, &log.Logger, apiKey, recheckInterval)
		if err != nil {
			log.Warn().Err(err).Msg("failed to refresh Chainstack nodes, falling back to static endpoint generation")
			return nil, err
		}

		v.nodesDataLock.RLock()
		nodes := v.nodesData[apiKey]
		v.nodesDataLock.RUnlock()

		var upstreams []*common.UpstreamConfig
		for _, node := range nodes {
			if node.ChainID == chainID && node.Status == "running" && node.Details.HTTPSEndpoint != "" {
				// Create a copy of the upstream config for each node
				upsCopy := upstream.Copy()
				upsCopy.Id = fmt.Sprintf("chainstack-%s", node.ID)
				upsCopy.Endpoint = node.Details.HTTPSEndpoint + "/" + node.Details.AuthKey
				upsCopy.Type = common.UpstreamTypeEvm

				// Add authentication if available
				if upsCopy.JsonRpc == nil {
					upsCopy.JsonRpc = &common.JsonRpcUpstreamConfig{}
				}
				if upsCopy.JsonRpc.Headers == nil {
					upsCopy.JsonRpc.Headers = make(map[string]string)
				}

				upstreams = append(upstreams, upsCopy)
			}
		}
		return upstreams, nil
	} else {
		return []*common.UpstreamConfig{upstream}, nil
	}
}

func (v *ChainstackVendor) ensureRefreshNodes(ctx context.Context, logger *zerolog.Logger, apiKey string, recheckInterval time.Duration) error {
	v.nodesDataLock.Lock()
	defer v.nodesDataLock.Unlock()

	// Check if we've fetched recently
	if lastFetch, ok := v.nodesDataLastFetchedAt[apiKey]; ok && time.Since(lastFetch) < recheckInterval {
		return nil
	}

	// Fetch nodes from API
	nodes, err := v.fetchNodes(ctx, apiKey)
	if err != nil {
		// Keep stale data if fetch fails
		if _, hasData := v.nodesData[apiKey]; hasData {
			log.Warn().Err(err).Msg("could not refresh Chainstack nodes data; will use stale data")
			return nil
		}
		return err
	}

	// Fetch chain IDs in parallel with semaphore
	err = v.fetchChainIDs(ctx, nodes)
	if err != nil {
		log.Warn().Err(err).Msg("some chain ID fetches failed, but continuing with available data")
	}

	// Update cache
	v.nodesData[apiKey] = nodes
	v.nodesDataLastFetchedAt[apiKey] = time.Now()

	return nil
}

func (v *ChainstackVendor) fetchNodes(ctx context.Context, apiKey string) ([]*ChainstackNode, error) {
	var allNodes []*ChainstackNode
	nextURL := "https://api.chainstack.com/v1/nodes/"

	httpClient := &http.Client{
		Timeout: 30 * time.Second,
	}

	for nextURL != "" {
		req, err := http.NewRequestWithContext(ctx, "GET", nextURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", apiKey))

		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("chainstack API returned status %d: %s", resp.StatusCode, string(body))
		}

		var nodesResp ChainstackNodesResponse
		if err := common.SonicCfg.NewDecoder(resp.Body).Decode(&nodesResp); err != nil {
			return nil, fmt.Errorf("failed to decode Chainstack nodes response: %w", err)
		}

		// Decode each node individually, ignoring nodes that fail to decode
		for _, rawNode := range nodesResp.Results {
			var node ChainstackNode
			if err := json.Unmarshal(rawNode, &node); err != nil {
				// Log and skip nodes that fail to decode
				log.Debug().Err(err).Msg("failed to decode individual node, skipping")
				continue
			}

			// Only include nodes with valid data
			if node.ID != "" && node.Details.HTTPSEndpoint != "" {
				allNodes = append(allNodes, &node)
			}
		}

		if nodesResp.Next != nil && *nodesResp.Next != "" {
			nextURL = *nodesResp.Next
		} else {
			nextURL = ""
		}
	}

	return allNodes, nil
}

func (v *ChainstackVendor) fetchChainIDs(ctx context.Context, nodes []*ChainstackNode) error {
	// Use semaphore to limit concurrent requests
	sem := semaphore.NewWeighted(10)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errors []error

	httpClient := &http.Client{
		Timeout: 10 * time.Second,
	}

	for _, node := range nodes {
		if node.Status != "running" || node.Details.HTTPSEndpoint == "" {
			continue
		}

		wg.Add(1)
		go func(n *ChainstackNode) {
			defer wg.Done()

			if err := sem.Acquire(ctx, 1); err != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("failed to acquire semaphore for node %s: %w", n.ID, err))
				mu.Unlock()
				return
			}
			defer sem.Release(1)

			// Make eth_chainId call
			reqBody := []byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`)
			req, err := http.NewRequestWithContext(ctx, "POST", n.Details.HTTPSEndpoint+"/"+n.Details.AuthKey, bytes.NewReader(reqBody))
			if err != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("failed to create request for node %s: %w", n.ID, err))
				mu.Unlock()
				return
			}

			req.Header.Set("Content-Type", "application/json")

			resp, err := httpClient.Do(req)
			if err != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("failed to fetch chain ID for node %s: %w", n.ID, err))
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
				errors = append(errors, fmt.Errorf("failed to decode chain ID response for node %s: %w", n.ID, err))
				mu.Unlock()
				return
			}

			if result.Error != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("RPC error for node %s: %s", n.ID, result.Error.Message))
				mu.Unlock()
				return
			}

			// Parse hex chain ID
			chainIDStr := strings.TrimPrefix(result.Result, "0x")
			chainID, err := strconv.ParseInt(chainIDStr, 16, 64)
			if err != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("failed to parse chain ID for node %s: %w", n.ID, err))
				mu.Unlock()
				return
			}

			n.ChainID = chainID
		}(node)
	}

	wg.Wait()

	if len(errors) > 0 {
		log.Warn().Errs("errors", errors).Msg("failed to fetch chain IDs for some Chainstack nodes")
	}

	return nil
}

func (v *ChainstackVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	bodyMap, ok := jrr.(*common.JsonRpcResponse)
	if !ok {
		return nil
	}

	err := bodyMap.Error
	if err.Data != "" {
		details["data"] = err.Data
	}

	// Other errors can be properly handled by generic error handling
	return nil
}

func (v *ChainstackVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "chainstack://") || strings.HasPrefix(ups.Endpoint, "evm+chainstack://") {
		return true
	}

	return strings.Contains(ups.Endpoint, ".core.chainstack.com")
}
