package thirdparty

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	archEvm "github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

type ErpcVendor struct {
	common.Vendor
	headlessClients sync.Map
}

func CreateErpcVendor() common.Vendor {
	return &ErpcVendor{
		headlessClients: sync.Map{},
	}
}

func (v *ErpcVendor) Name() string {
	return "erpc"
}

func (v *ErpcVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainId, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}

	endpoint, _ := settings["endpoint"].(string)
	if endpoint == "" {
		return false, nil
	}

	// Secret from settings is only used if not already in the endpoint URL
	secret, _ := settings["secret"].(string)

	// If endpoint already contains query params (e.g., erpc://domain.com?secret=xxx&other=yyy),
	// parseEndpointURL will preserve them. The secret param is only added if provided separately.
	parsedURL, err := v.parseEndpointURL(endpoint, secret, chainId)
	if err != nil {
		return false, err
	}

	client, err := v.getOrCreateClient(ctx, logger, chainId, parsedURL)
	if err != nil {
		return false, err
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pr := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_chainId","params":[]}`, util.RandomID())))
	resp, err := client.SendRequest(ctx, pr)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil {
		return false, err
	}

	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return false, err
	}

	cids, err := jrr.PeekStringByPath(ctx)
	if err != nil {
		return false, err
	}

	cidh, err := common.NormalizeHex(cids)
	if err != nil {
		return false, err
	}

	cid, err := common.HexToInt64(cidh)
	if err != nil {
		return false, err
	}

	// Check if the returned chain ID matches the requested one
	return cid == chainId, nil
}

func (v *ErpcVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	// If endpoint is already set, use it directly
	if upstream.Endpoint != "" {
		// If it's an erpc:// URL, parse it and convert to http/https based on port
		if strings.HasPrefix(upstream.Endpoint, "erpc://") || strings.HasPrefix(upstream.Endpoint, "evm+erpc://") {
			endpoint := upstream.Endpoint
			if strings.HasPrefix(endpoint, "evm+erpc://") {
				endpoint = strings.TrimPrefix(endpoint, "evm+")
			}

			// Parse the endpoint URL directly - parseEndpointURL handles erpc:// prefix
			// and preserves all query parameters
			parsedURL, err := v.parseEndpointURL(endpoint, "", upstream.Evm.ChainId)
			if err != nil {
				return nil, err
			}
			upstream.Endpoint = parsedURL.String()
		}

		upstream.Type = common.UpstreamTypeEvm
		return []*common.UpstreamConfig{upstream}, nil
	}

	endpoint, ok := settings["endpoint"].(string)
	if !ok || endpoint == "" {
		return nil, fmt.Errorf("endpoint is required in erpc settings")
	}

	secret, _ := settings["secret"].(string)

	parsedURL, err := v.parseEndpointURL(endpoint, secret, upstream.Evm.ChainId)
	if err != nil {
		return nil, err
	}

	upstream.Endpoint = parsedURL.String()
	upstream.Type = common.UpstreamTypeEvm

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *ErpcVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	bodyMap, ok := jrr.(*common.JsonRpcResponse)
	if !ok {
		return nil
	}

	err := bodyMap.Error
	if err.Data != "" {
		details["data"] = err.Data
	}

	return nil
}

func (v *ErpcVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "erpc://") || strings.HasPrefix(ups.Endpoint, "evm+erpc://") {
		return true
	}

	if ups.VendorName == v.Name() {
		return true
	}

	return strings.Contains(ups.Endpoint, ".erpc.cloud")
}

func (v *ErpcVendor) parseEndpointURL(endpoint string, secret string, chainId int64) (*url.URL, error) {
	// First, handle the case where it's already a full URL (http:// or https://)
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		// Parse as-is and append chainId to path
		parsedURL, err := url.Parse(endpoint)
		if err != nil {
			return nil, err
		}

		// Append chainId to path
		if parsedURL.Path == "" || parsedURL.Path == "/" {
			parsedURL.Path = "/" + strconv.FormatInt(chainId, 10)
		} else {
			parsedURL.Path = strings.TrimSuffix(parsedURL.Path, "/") + "/" + strconv.FormatInt(chainId, 10)
		}

		// Add secret if provided (backwards compatibility)
		if secret != "" {
			q := parsedURL.Query()
			q.Set("secret", secret)
			parsedURL.RawQuery = q.Encode()
		}

		return parsedURL, nil
	}

	// For erpc:// or plain endpoints, we need to determine the protocol based on port
	// Parse the endpoint to extract host, port, path, and query params
	var rawURL string
	if strings.HasPrefix(endpoint, "erpc://") {
		rawURL = strings.TrimPrefix(endpoint, "erpc://")
	} else {
		rawURL = endpoint
	}

	// Create a temporary URL with http:// to use Go's URL parser
	tempURL := "http://" + rawURL
	parsedURL, err := url.Parse(tempURL)
	if err != nil {
		return nil, err
	}

	// Determine the protocol based on port
	var scheme string
	port := parsedURL.Port()

	switch port {
	case "443":
		scheme = "https"
	case "80":
		scheme = "http"
	case "":
		// No port specified, default to https
		scheme = "https"
	default:
		// Any other port, use http
		scheme = "http"
	}

	// Update the scheme
	parsedURL.Scheme = scheme

	// Append chainId to path
	if parsedURL.Path == "" || parsedURL.Path == "/" {
		parsedURL.Path = "/" + strconv.FormatInt(chainId, 10)
	} else {
		parsedURL.Path = strings.TrimSuffix(parsedURL.Path, "/") + "/" + strconv.FormatInt(chainId, 10)
	}

	// Add secret if provided
	if secret != "" {
		q := parsedURL.Query()
		q.Set("secret", secret)
		parsedURL.RawQuery = q.Encode()
	}

	return parsedURL, nil
}

func (v *ErpcVendor) getOrCreateClient(ctx context.Context, logger *zerolog.Logger, chainId int64, parsedURL *url.URL) (clients.HttpJsonRpcClient, error) {
	clientKey := fmt.Sprintf("%s-%d", parsedURL.String(), chainId)

	if client, ok := v.headlessClients.Load(clientKey); ok {
		return client.(clients.HttpJsonRpcClient), nil
	}

	u := &phonyUpstream{id: fmt.Sprintf("temp-erpc-%d", chainId)}
	client, err := clients.NewGenericHttpJsonRpcClient(ctx, logger, "n/a", u, parsedURL, nil, nil, archEvm.NewJsonRpcErrorExtractor())
	if err != nil {
		return nil, err
	}

	v.headlessClients.Store(clientKey, client)
	return client, nil
}
