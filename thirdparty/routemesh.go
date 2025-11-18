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

const DefaultRoutemeshBaseURL = "lb.routemes.sh"

type RoutemeshVendor struct {
	common.Vendor
	headlessClients sync.Map
}

func CreateRoutemeshVendor() common.Vendor {
	return &RoutemeshVendor{
		headlessClients: sync.Map{},
	}
}

func (v *RoutemeshVendor) Name() string {
	return "routemesh"
}

func (v *RoutemeshVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainId, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}

	baseURL, ok := settings["baseURL"].(string)
	if !ok || baseURL == "" {
		baseURL = DefaultRoutemeshBaseURL
	}

	apiKey, ok := settings["apiKey"].(string)
	if !ok || apiKey == "" {
		return false, fmt.Errorf("apiKey is required in routemesh settings")
	}

	parsedURL, err := v.generateUrl(chainId, baseURL, apiKey)
	if err != nil {
		return false, err
	}

	// Check against endpoint to see if eth_chainId responds successfully
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

func (v *RoutemeshVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint == "" {
		baseURL, ok := settings["baseURL"].(string)
		if !ok || baseURL == "" {
			baseURL = DefaultRoutemeshBaseURL
		}
		apiKey, ok := settings["apiKey"].(string)
		if !ok || apiKey == "" {
			return nil, fmt.Errorf("apiKey is required in routemesh settings")
		}
		chainID := upstream.Evm.ChainId
		if chainID == 0 {
			return nil, fmt.Errorf("routemesh vendor requires upstream.evm.chainId to be defined")
		}
		parsedURL, err := v.generateUrl(chainID, baseURL, apiKey)
		if err != nil {
			return nil, err
		}
		upstream.Endpoint = parsedURL.String()
		upstream.Type = common.UpstreamTypeEvm
	}

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *RoutemeshVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	return nil
}

func (v *RoutemeshVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if ups.VendorName == v.Name() {
		return true
	}
	return strings.Contains(ups.Endpoint, "routemes.sh")
}

func (v *RoutemeshVendor) generateUrl(chainId int64, baseURL string, apiKey string) (*url.URL, error) {
	// Construct URL: https://lb.routemes.sh/rpc/<chainId>/<apiKey>
	routemeshURL := fmt.Sprintf("https://%s/rpc/%d/%s", baseURL, chainId, apiKey)
	parsedURL, err := url.Parse(routemeshURL)
	if err != nil {
		return nil, err
	}
	return parsedURL, nil
}

func (v *RoutemeshVendor) getOrCreateClient(ctx context.Context, logger *zerolog.Logger, chainId int64, parsedURL *url.URL) (clients.HttpJsonRpcClient, error) {
	clientKey := fmt.Sprintf("%s-%d", parsedURL.String(), chainId)

	if client, ok := v.headlessClients.Load(clientKey); ok {
		return client.(clients.HttpJsonRpcClient), nil
	}

	u := &phonyUpstream{id: fmt.Sprintf("temp-routemesh-%d", chainId)}
	client, err := clients.NewGenericHttpJsonRpcClient(ctx, logger, "n/a", u, parsedURL, nil, nil, archEvm.NewJsonRpcErrorExtractor())
	if err != nil {
		return nil, err
	}

	v.headlessClients.Store(clientKey, client)
	return client, nil
}
