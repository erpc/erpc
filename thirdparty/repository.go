package thirdparty

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

const DefaultRepositoryURL = "https://evm-public-endpoints.erpc.cloud"
const DefaultRecheckInterval = 1 * time.Hour

type chainData struct {
	Endpoints []string `json:"endpoints"`
}

// RepositoryVendor uses RemoteDataCache for lock-free, async-refresh
// access to the public-endpoint map. See remote_cache.go for the safety rule.
type RepositoryVendor struct {
	common.Vendor
	cache *RemoteDataCache[map[int64][]string]
}

func CreateRepositoryVendor() common.Vendor {
	return &RepositoryVendor{
		cache: NewRemoteDataCache[map[int64][]string]("repository"),
	}
}

func (v *RepositoryVendor) Name() string {
	return "repository"
}

func (v *RepositoryVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}

	urlStr, ok := settings["repositoryUrl"].(string)
	if !ok || urlStr == "" {
		urlStr = DefaultRepositoryURL
	}

	recheckInterval, ok := settings["recheckInterval"].(time.Duration)
	if !ok {
		recheckInterval = DefaultRecheckInterval
	}

	chains, ok := v.resolveChains(logger, urlStr, recheckInterval)
	if !ok {
		return false, ErrRemoteCacheCold
	}
	endpoints, ok := chains[chainID]
	return ok && len(endpoints) > 0, nil
}

// resolveChains does a lock-free Lookup, kicks off an async refresh on
// staleness, and returns (data, true) on hit or (nil, false) on cold start.
// See remote_cache.go for the request-path safety rule.
func (v *RepositoryVendor) resolveChains(logger *zerolog.Logger, urlStr string, recheckInterval time.Duration) (map[int64][]string, bool) {
	chains, fresh := v.cache.Lookup(urlStr, recheckInterval)
	if !fresh {
		v.cache.TriggerAsyncRefresh(logger, urlStr, func(ctx context.Context) (map[int64][]string, error) {
			return fetchRemoteData(ctx, urlStr)
		})
	}
	if chains == nil {
		return nil, false
	}
	return chains, true
}

func (v *RepositoryVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}
	if upstream.AutoIgnoreUnsupportedMethods == nil {
		// If not explicitly set, default to true because there are so many limited public RPCs out there
		upstream.AutoIgnoreUnsupportedMethods = util.BoolPtr(true)
	}

	if upstream.Evm == nil {
		return nil, fmt.Errorf("remote vendor requires upstream.evm to be defined")
	}

	if upstream.Evm.ChainId == 0 {
		return nil, fmt.Errorf("remote vendor requires upstream.evm.chainId to be defined")
	}
	chainID := upstream.Evm.ChainId

	urlStr, ok := settings["repositoryUrl"].(string)
	if !ok || urlStr == "" {
		urlStr = DefaultRepositoryURL
	}

	recheckInterval, ok := settings["recheckInterval"].(time.Duration)
	if !ok {
		recheckInterval = DefaultRecheckInterval
	}
	chains, ok := v.resolveChains(logger, urlStr, recheckInterval)
	if !ok {
		return nil, ErrRemoteCacheCold
	}
	endpoints, ok := chains[chainID]
	if !ok || len(endpoints) == 0 {
		return nil, fmt.Errorf("chain ID %d not found in remote data or has no endpoints", chainID)
	}

	upsList := []*common.UpstreamConfig{}
	for _, ep := range endpoints {
		if !strings.HasPrefix(strings.ToLower(ep), "http") {
			continue
		}
		var evm *common.EvmUpstreamConfig
		if upstream.Evm != nil {
			evm = &common.EvmUpstreamConfig{}
			*evm = *upstream.Evm
		}
		var jsonRpc *common.JsonRpcUpstreamConfig
		if upstream.JsonRpc != nil {
			jsonRpc = &common.JsonRpcUpstreamConfig{}
			*jsonRpc = *upstream.JsonRpc
		}
		var failsafe []*common.FailsafeConfig
		if upstream.Failsafe != nil {
			failsafe = make([]*common.FailsafeConfig, len(upstream.Failsafe))
			for i, fs := range upstream.Failsafe {
				failsafe[i] = fs.Copy()
			}
		}
		var autoTuner *common.RateLimitAutoTuneConfig
		if upstream.RateLimitAutoTune != nil {
			autoTuner = &common.RateLimitAutoTuneConfig{}
			*autoTuner = *upstream.RateLimitAutoTune
		}
		var tags []string
		if len(upstream.Tags) > 0 {
			tags = append(tags, upstream.Tags...)
		}
		upsList = append(upsList, &common.UpstreamConfig{
			Id:                           fmt.Sprintf("%s-%s", upstream.Id, util.RedactEndpoint(ep)),
			Type:                         common.UpstreamTypeEvm,
			Endpoint:                     ep,
			Tags:                         tags,
			Evm:                          evm,
			JsonRpc:                      jsonRpc,
			IgnoreMethods:                upstream.IgnoreMethods,
			AllowMethods:                 upstream.AllowMethods,
			AutoIgnoreUnsupportedMethods: upstream.AutoIgnoreUnsupportedMethods,
			Failsafe:                     failsafe,
			RateLimitBudget:              upstream.RateLimitBudget,
			RateLimitAutoTune:            autoTuner,
		})
	}

	logger.Debug().Int64("chainId", chainID).Interface("upstreams", upsList).Interface("settings", settings).Msg("generated upstreams from repository provider")

	return upsList, nil
}

func (v *RepositoryVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	return nil
}

func (v *RepositoryVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	// If the user put "repository://" or "evm+repository://"
	return strings.HasPrefix(ups.Endpoint, "repository://") || strings.HasPrefix(ups.Endpoint, "evm+repository://")
}

func fetchRemoteData(ctx context.Context, urlStr string) (map[int64][]string, error) {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, "GET", urlStr, nil)
	if err != nil {
		return nil, err
	}
	var httpClient = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			MaxConnsPerHost:       0, // Unlimited active connections
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("remote fetch returned non-200 code: %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var raw map[string]chainData
	if err := common.SonicCfg.Unmarshal(bodyBytes, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse remote repository data: %w", err)
	}

	newData := make(map[int64][]string)
	for chainIDStr, chainInfo := range raw {
		chainID, parseErr := strconv.ParseInt(chainIDStr, 10, 64)
		if parseErr != nil {
			// if key isn't an integer, skip
			continue
		}
		newData[chainID] = chainInfo.Endpoints
	}

	return newData, nil
}
