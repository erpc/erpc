package thirdparty

import (
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
	"gopkg.in/yaml.v3"
)

const DefaultDrpcChainsYAMLURL = "https://raw.githubusercontent.com/drpcorg/public/main/chains.yaml"
const DefaultDrpcRecheckInterval = 24 * time.Hour

type ChainConfig struct {
	ChainID    string   `yaml:"chain-id"`
	ShortNames []string `yaml:"short-names"`
}

type ProtocolConfig struct {
	Chains []ChainConfig `yaml:"chains"`
}

type ChainsYAML struct {
	ChainSettings struct {
		Protocols []ProtocolConfig `yaml:"protocols"`
	} `yaml:"chain-settings"`
}

type DrpcVendor struct {
	common.Vendor

	remoteDataLock          sync.Mutex
	remoteData              map[string]map[int64]string
	remoteDataLastFetchedAt map[string]time.Time
}

func CreateDrpcVendor() common.Vendor {
	return &DrpcVendor{
		remoteData:              make(map[string]map[int64]string),
		remoteDataLastFetchedAt: make(map[string]time.Time),
	}
}

func (v *DrpcVendor) Name() string {
	return "drpc"
}

func (v *DrpcVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}

	chainsURL, ok := settings["chainsUrl"].(string)
	if !ok || chainsURL == "" {
		chainsURL = DefaultDrpcChainsYAMLURL
	}

	recheckInterval, ok := settings["recheckInterval"].(time.Duration)
	if !ok {
		recheckInterval = DefaultDrpcRecheckInterval
	}

	err = v.ensureRemoteData(ctx, recheckInterval, chainsURL)
	if err != nil {
		return false, fmt.Errorf("unable to load remote data: %w", err)
	}

	networks, ok := v.remoteData[chainsURL]
	if !ok || networks == nil {
		return false, nil
	}

	_, exists := networks[chainID]
	return exists, nil
}

func (v *DrpcVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	// Intentionally not ignore missing method exceptions because dRPC sometimes routes to nodes that don't support the method
	// but it doesn't mean that method is actually not supported, i.e. on next retry to dRPC it might work.
	upstream.AutoIgnoreUnsupportedMethods = &common.FALSE

	if upstream.Endpoint == "" {
		apiKey, ok := settings["apiKey"].(string)
		if !ok || apiKey == "" {
			return nil, fmt.Errorf("apiKey is required in drpc settings")
		}

		if upstream.Evm == nil {
			return nil, fmt.Errorf("drpc vendor requires upstream.evm to be defined")
		}
		chainID := upstream.Evm.ChainId
		if chainID == 0 {
			return nil, fmt.Errorf("drpc vendor requires upstream.evm.chainId to be defined")
		}

		chainsURL, ok := settings["chainsUrl"].(string)
		if !ok || chainsURL == "" {
			chainsURL = DefaultDrpcChainsYAMLURL
		}

		recheckInterval, ok := settings["recheckInterval"].(time.Duration)
		if !ok {
			recheckInterval = DefaultDrpcRecheckInterval
		}

		if err := v.ensureRemoteData(ctx, recheckInterval, chainsURL); err != nil {
			return nil, fmt.Errorf("unable to load remote data: %w", err)
		}

		networks, ok := v.remoteData[chainsURL]
		if !ok || networks == nil {
			return nil, fmt.Errorf("network data not available")
		}

		netName, ok := networks[chainID]
		if !ok {
			return nil, fmt.Errorf("unsupported network chain ID for DRPC: %d", chainID)
		}

		drpcURL := fmt.Sprintf("https://lb.drpc.org/ogrpc?network=%s&dkey=%s", netName, apiKey)
		parsedURL, err := url.Parse(drpcURL)
		if err != nil {
			return nil, err
		}
		upstream.Endpoint = parsedURL.String()
		upstream.Type = common.UpstreamTypeEvm
	}

	if v.OwnsUpstream(upstream) && upstream.Type == "" {
		upstream.Type = common.UpstreamTypeEvm
	}

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *DrpcVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
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

		if strings.Contains(msg, "token is invalid") {
			return common.NewErrEndpointUnauthorized(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorUnauthorized,
					msg,
					nil,
					details,
				),
			)
		}

		if strings.Contains(msg, "ChainException: Unexpected error (code=40000)") ||
			strings.Contains(msg, "invalid block range") {
			return common.NewErrEndpointMissingData(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorMissingData,
					err.Message,
					nil,
					details,
				),
				req.LastUpstream(),
			)
		}
	}

	// Other errors can be properly handled by generic error handling
	return nil
}

func (v *DrpcVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "drpc://") || strings.HasPrefix(ups.Endpoint, "evm+drpc://") {
		return true
	}

	return strings.Contains(ups.Endpoint, ".drpc.org")
}

func (v *DrpcVendor) ensureRemoteData(ctx context.Context, recheckInterval time.Duration, chainsURL string) error {
	v.remoteDataLock.Lock()
	defer v.remoteDataLock.Unlock()

	if ltm, ok := v.remoteDataLastFetchedAt[chainsURL]; ok && time.Since(ltm) < recheckInterval {
		return nil
	}

	newData, err := v.fetchDrpcNetworks(ctx, chainsURL)
	if err != nil {
		if _, ok := v.remoteData[chainsURL]; ok {
			log.Warn().Err(err).Msg("could not refresh DRPC chains data; will use stale data")
			return nil
		}
		return err
	}

	v.remoteData[chainsURL] = newData
	v.remoteDataLastFetchedAt[chainsURL] = time.Now()
	return nil
}

func (v *DrpcVendor) fetchDrpcNetworks(ctx context.Context, chainsURL string) (map[int64]string, error) {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, "GET", chainsURL, nil)
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
		return nil, fmt.Errorf("DRPC chains API returned non-200 code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var chainsData ChainsYAML
	if err := yaml.Unmarshal(body, &chainsData); err != nil {
		return nil, fmt.Errorf("failed to parse DRPC chains YAML: %w", err)
	}

	networkNames := make(map[int64]string)
	for _, protocol := range chainsData.ChainSettings.Protocols {
		for _, chain := range protocol.Chains {
			// Parse chain-id from hex to int64
			chainIDStr := strings.TrimPrefix(chain.ChainID, "0x")
			if chainIDStr == "" {
				continue
			}

			chainID, err := strconv.ParseInt(chainIDStr, 16, 64)
			if err != nil {
				log.Debug().
					Str("chain_id", chain.ChainID).
					Err(err).
					Msg("Failed to parse chain ID")
				continue
			}

			// Use the first short name if available
			if len(chain.ShortNames) > 0 {
				networkNames[chainID] = chain.ShortNames[0]
				log.Debug().
					Int64("chain_id", chainID).
					Str("name", chain.ShortNames[0]).
					Msg("Added DRPC network name")
			}
		}
	}

	log.Info().
		Int("count", len(networkNames)).
		Str("url", chainsURL).
		Msg("Successfully fetched DRPC network names")

	return networkNames, nil
}
