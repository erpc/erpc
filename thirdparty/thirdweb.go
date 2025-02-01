package thirdparty

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

type ThirdwebVendor struct {
	common.Vendor
}

func CreateThirdwebVendor() common.Vendor {
	return &ThirdwebVendor{}
}

func (v *ThirdwebVendor) Name() string {
	return "thirdweb"
}

func (v *ThirdwebVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainId, err := strconv.ParseInt(networkId[4:], 10, 64)
	if err != nil {
		return false, err
	}

	clientId, ok := settings["clientId"].(string)
	if !ok || clientId == "" {
		return false, fmt.Errorf("clientId is required in thirdweb settings")
	}
	parsedURL, err := v.generateUrl(chainId, clientId)
	if err != nil {
		return false, err
	}

	// Check against endpoint to see if eth_chainId responds successfully
	client, err := v.createClient(ctx, logger, parsedURL)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pr := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_chainId","params":[]}`, util.RandomID())))
	resp, err := client.SendRequest(ctx, pr)
	if err != nil {
		return false, err
	}

	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return false, err
	}

	cids, err := jrr.PeekStringByPath()
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

	return cid == chainId, nil
}

func (v *ThirdwebVendor) GenerateConfigs(upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint == "" {
		if clientId, ok := settings["clientId"].(string); ok && clientId != "" {
			parsedURL, err := v.generateUrl(upstream.Evm.ChainId, clientId)
			if err != nil {
				return nil, err
			}
			upstream.Endpoint = parsedURL.String()
			upstream.Type = common.UpstreamTypeEvm
		} else {
			return nil, fmt.Errorf("clientId is required in thirdweb settings")
		}
	}
	return []*common.UpstreamConfig{upstream}, nil
}

func (v *ThirdwebVendor) GetVendorSpecificErrorIfAny(resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	return nil
}

func (v *ThirdwebVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "thirdweb://") || strings.HasPrefix(ups.Endpoint, "evm+thirdweb://") {
		return true
	}

	return strings.Contains(ups.Endpoint, ".thirdweb.com")
}

func (v *ThirdwebVendor) generateUrl(chainId int64, clientId string) (*url.URL, error) {
	thirdwebUrl := fmt.Sprintf("https://%d.rpc.thirdweb.com/%s", chainId, clientId)
	parsedURL, err := url.Parse(thirdwebUrl)
	if err != nil {
		return nil, err
	}
	return parsedURL, nil
}

func (v *ThirdwebVendor) createClient(ctx context.Context, logger *zerolog.Logger, parsedURL *url.URL) (clients.HttpJsonRpcClient, error) {
	client, err := clients.NewGenericHttpJsonRpcClient(ctx, logger, "n/a", "n/a", parsedURL, nil, nil)
	if err != nil {
		return nil, err
	}
	return client, nil
}
