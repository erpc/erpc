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

type ThirdwebSettings struct {
	ClientId string `yaml:"clientId" json:"clientId"`
}

func (s *ThirdwebSettings) IsObjectNull() bool {
	return s == nil || s.ClientId == ""
}

func (s *ThirdwebSettings) Validate() error {
	if s == nil || s.ClientId == "" {
		return fmt.Errorf("thirdweb vendor requires clientId")
	}
	return nil
}

func (s *ThirdwebSettings) SetDefaults() {}

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

	stg, ok := settings.(*ThirdwebSettings)
	if !ok {
		return false, fmt.Errorf("invalid settings type for thirdweb vendor")
	}

	parsedURL, err := v.generateUrl(chainId, stg.ClientId)
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

func (v *ThirdwebVendor) OverrideConfig(upstream *common.UpstreamConfig, settings common.VendorSettings) error {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint == "" && settings != nil && !settings.IsObjectNull() {
		if settings, ok := settings.(*ThirdwebSettings); ok {
			if settings.ClientId != "" {
				parsedURL, err := v.generateUrl(upstream.Evm.ChainId, settings.ClientId)
				if err != nil {
					return err
				}
				upstream.Endpoint = parsedURL.String()
			}
		}
	}
	return nil
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
	client, err := clients.NewGenericHttpJsonRpcClient(ctx, logger, "n/a", "n/a", parsedURL, nil)
	if err != nil {
		return nil, err
	}
	return client, nil
}
