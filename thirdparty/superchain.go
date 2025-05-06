package thirdparty

import (
	"context"
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
)

const DefaultSuperchainRegistryURL = "https://raw.githubusercontent.com/ethereum-optimism/superchain-registry/main/chainList.json"
const DefaultSuperchainRecheckInterval = 24 * time.Hour

// converts a shorthand `superchain://` specification into a raw
// URL that points to the JSON registry file.
// Supported forms:
//
//	superchain://github.com/{org}/{repo}
//	  -> https://raw.githubusercontent.com/{org}/{repo}/main/chainList.json
//	superchain://github.com/{org}/{repo}/{branch}/{file}.json
//	  -> https://raw.githubusercontent.com/{org}/{repo}/{branch}/{file}.json
//	github.com/{org}/{repo} (shorthand)
//	  -> https://raw.githubusercontent.com/{org}/{repo}/main/chainList.json
//	https://github.com/{org}/{repo} (full repo URL)
//	  -> https://raw.githubusercontent.com/{org}/{repo}/main/chainList.json
//
// Any non‑GitHub spec is treated as a literal URL; if it lacks a scheme we prepend
// `https://`. If it's already a raw.githubusercontent.com URL, it's returned as is.
func parseSuperchainSpec(spec string) (string, error) {
	// Handle GitHub URLs (shorthand, full repo URLs, but not yet raw content URLs)
	if strings.HasPrefix(spec, "github.com/") || strings.HasPrefix(spec, "https://github.com/") || strings.HasPrefix(spec, "http://github.com/") {
		// Avoid re-processing if it's already a raw content URL that somehow reached here
		if strings.Contains(spec, "raw.githubusercontent.com") {
			if strings.HasPrefix(spec, "http://") || strings.HasPrefix(spec, "https://") {
				return spec, nil
			}
			// Should ideally not happen: a raw URL without a scheme. Prepend https.
			return "https://" + spec, nil
		}

		var pathPart string
		if strings.HasPrefix(spec, "https://github.com/") {
			pathPart = strings.TrimPrefix(spec, "https://github.com/")
		} else if strings.HasPrefix(spec, "http://github.com/") {
			pathPart = strings.TrimPrefix(spec, "http://github.com/")
		} else { // must be "github.com/"
			pathPart = strings.TrimPrefix(spec, "github.com/")
		}

		parts := strings.Split(strings.Trim(pathPart, "/"), "/")
		if len(parts) < 2 {
			return "", fmt.Errorf("invalid GitHub superchain spec: '%s' (org/repo not found after prefix)", spec)
		}

		org, repo := parts[0], parts[1]
		branch := "main"
		jsonFile := "chainList.json"

		if len(parts) > 2 { // Potential branch or full path
			// Check if parts[2] looks like a common branch name or if it's part of a longer path ending in .json
			// This logic assumes if more than org/repo is given, it might include branch and/or filename.
			// Example: org/repo/my-branch
			// Example: org/repo/my-branch/customList.json
			// Example: org/repo/main/some/dir/customList.json (less common for this use case)
			if len(parts) == 3 && !strings.HasSuffix(parts[2], ".json") { // org/repo/branch
				branch = parts[2]
			} else if len(parts) >= 3 { // org/repo/branch/file.json or org/repo/file.json (implicit main)
				// Determine if parts[2] is a branch or part of the filename
				// If parts[2] is not a .json file, assume it's a branch
				if !strings.HasSuffix(parts[2], ".json") {
					branch = parts[2]
					if len(parts) > 3 {
						jsonFile = strings.Join(parts[3:], "/")
					}
				} else {
					// This means spec was like github.com/org/repo/somefile.json, assume main branch
					jsonFile = strings.Join(parts[2:], "/")
				}
			}
		}
		return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", org, repo, branch, jsonFile), nil
	}

	// Already a full URL (e.g. raw.githubusercontent.com or other custom registry)?
	if strings.HasPrefix(spec, "http://") || strings.HasPrefix(spec, "https://") {
		return spec, nil
	}

	// Fallback: treat as a domain/path and prepend https scheme.
	return "https://" + spec, nil
}

type SuperchainNetwork struct {
	Name                 string   `json:"name"`
	Identifier           string   `json:"identifier"`
	ChainID              int64    `json:"chainId"`
	RPC                  []string `json:"rpc"`
	Explorers            []string `json:"explorers"`
	SuperchainLevel      int      `json:"superchainLevel"`
	GovernedByOptimism   bool     `json:"governedByOptimism"`
	DataAvailabilityType string   `json:"dataAvailabilityType"`
	Parent               struct {
		Type  string `json:"type"`
		Chain string `json:"chain"`
	} `json:"parent"`
}

type SuperchainVendor struct {
	common.Vendor

	remoteDataLock          sync.Mutex
	remoteData              map[string]map[int64]*SuperchainNetwork
	remoteDataLastFetchedAt map[string]time.Time
}

func CreateSuperchainVendor() common.Vendor {
	return &SuperchainVendor{
		remoteData:              make(map[string]map[int64]*SuperchainNetwork),
		remoteDataLastFetchedAt: make(map[string]time.Time),
	}
}

func (v *SuperchainVendor) Name() string {
	return "superchain"
}

func (v *SuperchainVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}

	registryURL, ok := settings["registryUrl"].(string)
	if !ok || registryURL == "" {
		registryURL = DefaultSuperchainRegistryURL
	}

	finalRegistryURL, err := parseSuperchainSpec(registryURL)
	if err != nil {
		return false, fmt.Errorf("failed to parse superchain registry URL from source '%s': %w", registryURL, err)
	}

	recheckInterval, ok := settings["recheckInterval"].(time.Duration)
	if !ok {
		recheckInterval = DefaultSuperchainRecheckInterval
	}

	err = v.ensureRemoteData(ctx, recheckInterval, finalRegistryURL)
	if err != nil {
		return false, fmt.Errorf("unable to load remote data using URL '%s': %w", finalRegistryURL, err)
	}

	networks, ok := v.remoteData[finalRegistryURL]
	if !ok || networks == nil {
		return false, nil
	}

	network, exists := networks[chainID]
	return exists && network != nil && len(network.RPC) > 0, nil
}

func (v *SuperchainVendor) GenerateConfigs(upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint != "" {
		if strings.HasPrefix(upstream.Endpoint, "superchain://") || strings.HasPrefix(upstream.Endpoint, "evm+superchain://") {
			spec := strings.TrimPrefix(strings.TrimPrefix(upstream.Endpoint, "evm+"), "superchain://")
			parsedURL, err := parseSuperchainSpec(spec)
			if err != nil {
				return nil, err
			}
			if settings == nil {
				settings = make(common.VendorSettings)
			}
			settings["registryUrl"] = parsedURL
			// Clear the endpoint so the vendor generates upstreams from the registry data.
			upstream.Endpoint = ""
		} else {
			return []*common.UpstreamConfig{upstream}, nil
		}
	}

	if upstream.Evm == nil {
		return nil, fmt.Errorf("superchain vendor requires upstream.evm to be defined")
	}

	if upstream.Evm.ChainId == 0 {
		return nil, fmt.Errorf("superchain vendor requires upstream.evm.chainId to be defined")
	}
	chainID := upstream.Evm.ChainId

	registryURL, ok := settings["registryUrl"].(string)
	if !ok || registryURL == "" {
		registryURL = DefaultSuperchainRegistryURL
	}

	finalRegistryURL, err := parseSuperchainSpec(registryURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse superchain registry URL from source '%s': %w", registryURL, err)
	}

	recheckInterval, ok := settings["recheckInterval"].(time.Duration)
	if !ok {
		recheckInterval = DefaultSuperchainRecheckInterval
	}

	if err := v.ensureRemoteData(context.Background(), recheckInterval, finalRegistryURL); err != nil {
		return nil, fmt.Errorf("unable to load remote data using URL '%s': %w", finalRegistryURL, err)
	}

	networks, ok := v.remoteData[finalRegistryURL]
	if !ok || networks == nil {
		return nil, fmt.Errorf("network data not available from registry '%s'", finalRegistryURL)
	}

	network, ok := networks[chainID]
	if !ok || network == nil || len(network.RPC) == 0 {
		return nil, fmt.Errorf("chain ID %d not found in remote data from registry '%s' or has no RPC endpoints", chainID, finalRegistryURL)
	}

	// Generate a config for each RPC endpoint
	upsList := []*common.UpstreamConfig{}
	for i, rpcURL := range network.RPC {
		upsCfg := upstream.Copy()
		upsCfg.Type = common.UpstreamTypeEvm
		upsCfg.Endpoint = rpcURL
		upsCfg.VendorName = v.Name()

		// Add a suffix to the ID to make it unique for each RPC endpoint
		if upsCfg.Id != "" {
			upsCfg.Id = fmt.Sprintf("%s-%d", upsCfg.Id, i)
		} else {
			upsCfg.Id = fmt.Sprintf("superchain-%s-%d", network.Identifier, i)
		}

		upsList = append(upsList, upsCfg)
	}

	log.Debug().Int64("chainId", chainID).Interface("upstreams", upsList).Interface("settings", map[string]interface{}{
		"registryUrlUsed": finalRegistryURL,
		"recheckInterval": recheckInterval,
	}).Msg("generated upstreams from superchain provider")

	return upsList, nil
}

func (v *SuperchainVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	return nil
}

func (v *SuperchainVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "superchain://") || strings.HasPrefix(ups.Endpoint, "evm+superchain://") {
		return true
	}

	if ups.VendorName == v.Name() {
		return true
	}

	return false
}

func (v *SuperchainVendor) ensureRemoteData(ctx context.Context, recheckInterval time.Duration, registryURL string) error {
	v.remoteDataLock.Lock()
	defer v.remoteDataLock.Unlock()

	if ltm, ok := v.remoteDataLastFetchedAt[registryURL]; ok && time.Since(ltm) < recheckInterval {
		return nil
	}

	newData, err := v.fetchSuperchainNetworks(ctx, registryURL)
	if err != nil {
		if _, ok := v.remoteData[registryURL]; ok {
			log.Warn().Err(err).Msg("could not refresh Superchain registry data; will use stale data")
			return nil
		}
		return err
	}

	v.remoteData[registryURL] = newData
	v.remoteDataLastFetchedAt[registryURL] = time.Now()
	return nil
}

func (v *SuperchainVendor) fetchSuperchainNetworks(ctx context.Context, registryURL string) (map[int64]*SuperchainNetwork, error) {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, "GET", registryURL, nil)
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
		return nil, fmt.Errorf("Superchain registry API returned non-200 code: %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var networks []*SuperchainNetwork
	if err := common.SonicCfg.Unmarshal(bodyBytes, &networks); err != nil {
		return nil, fmt.Errorf("failed to parse Superchain registry data: %w", err)
	}

	newData := make(map[int64]*SuperchainNetwork)
	for _, network := range networks {
		if network.ChainID > 0 && len(network.RPC) > 0 {
			newData[network.ChainID] = network
		}
	}

	return newData, nil
}
