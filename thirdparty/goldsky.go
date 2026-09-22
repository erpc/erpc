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

// Goldsky Edge is a multi-region public RPC service built on eRPC. A single
// secret token covers every chain Goldsky serves; the vendor lazy-generates a
// concrete upstream per network the moment it is first requested.
//
// Endpoint shape: https://edge.goldsky.com/<tier>/evm/<chainId>?secret=<secret>
// (the secret may also be passed via the X-ERPC-Secret-Token header).
const (
	defaultGoldskyEndpoint = "https://edge.goldsky.com"
	defaultGoldskyTier     = "standard"
)

type GoldskyVendor struct {
	common.Vendor
	headlessClients sync.Map
}

func CreateGoldskyVendor() common.Vendor {
	return &GoldskyVendor{
		headlessClients: sync.Map{},
	}
}

func (v *GoldskyVendor) Name() string {
	return "goldsky"
}

func (v *GoldskyVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainId, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}

	endpoint, _ := settings["endpoint"].(string)
	tier, _ := settings["tier"].(string)
	secret, _ := settings["secret"].(string)

	// buildEndpointURL also derives a secret embedded in the endpoint
	// (goldsky://<secret> authority or a ?secret= on an https base), matching
	// what GenerateConfigs does.
	parsedURL, err := v.buildEndpointURL(endpoint, tier, secret, chainId)
	if err != nil {
		return false, err
	}

	// Every Goldsky Edge endpoint requires a secret token; if none was supplied
	// via settings or embedded in the endpoint, there is nothing to probe — skip
	// silently rather than hammering an endpoint that will only 401.
	if parsedURL.Query().Get("secret") == "" {
		return false, nil
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

	// Confirm the secret's plan actually serves this chain.
	return cid == chainId, nil
}

func (v *GoldskyVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	// If a concrete endpoint is already set (e.g. a statically-configured
	// upstream pointing directly at edge.goldsky.com), use it as-is. The
	// goldsky:// shorthand is converted to provider settings in
	// common/defaults.go, so it normally never reaches here.
	if upstream.Endpoint != "" {
		if strings.HasPrefix(upstream.Endpoint, "goldsky://") || strings.HasPrefix(upstream.Endpoint, "evm+goldsky://") {
			if upstream.Evm == nil {
				return nil, fmt.Errorf("goldsky vendor requires upstream.evm to be defined")
			}
			// buildEndpointURL extracts the secret/tier from the goldsky://
			// authority and targets the default Goldsky Edge host.
			parsedURL, err := v.buildEndpointURL(upstream.Endpoint, "", "", upstream.Evm.ChainId)
			if err != nil {
				return nil, err
			}
			upstream.Endpoint = parsedURL.String()
		}

		upstream.Type = common.UpstreamTypeEvm
		return []*common.UpstreamConfig{upstream}, nil
	}

	if upstream.Evm == nil {
		return nil, fmt.Errorf("goldsky vendor requires upstream.evm to be defined")
	}
	chainId := upstream.Evm.ChainId
	if chainId == 0 {
		return nil, fmt.Errorf("goldsky vendor requires upstream.evm.chainId to be defined")
	}

	endpoint, _ := settings["endpoint"].(string)
	tier, _ := settings["tier"].(string)
	secret, _ := settings["secret"].(string)

	parsedURL, err := v.buildEndpointURL(endpoint, tier, secret, chainId)
	if err != nil {
		return nil, err
	}

	upstream.Endpoint = parsedURL.String()
	upstream.Type = common.UpstreamTypeEvm

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *GoldskyVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
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

func (v *GoldskyVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "goldsky://") || strings.HasPrefix(ups.Endpoint, "evm+goldsky://") {
		return true
	}

	if ups.VendorName == v.Name() {
		return true
	}

	return strings.Contains(ups.Endpoint, "edge.goldsky.com")
}

// buildEndpointURL assembles a concrete Goldsky Edge URL of the form
// <base>/<tier>/evm/<chainId>?secret=<secret>. The base defaults to
// edge.goldsky.com and the tier to "standard". A base that already carries an
// explicit path is respected verbatim (the tier route is not injected).
func (v *GoldskyVendor) buildEndpointURL(endpoint string, tier string, secret string, chainId int64) (*url.URL, error) {
	// A goldsky:// base is the config shorthand: its authority is the Edge secret
	// token (not a host), with optional ?tier=/?secret= query params. Pull those
	// out and fall back to the default host so the result still targets
	// edge.goldsky.com instead of dialing the secret as a hostname.
	if strings.HasPrefix(endpoint, "goldsky://") || strings.HasPrefix(endpoint, "evm+goldsky://") {
		shorthand := strings.TrimPrefix(strings.TrimPrefix(endpoint, "evm+"), "goldsky://")
		if u, err := url.Parse("//" + shorthand); err == nil {
			if secret == "" && u.Host != "" {
				secret = u.Host
			}
			if u.RawQuery != "" {
				q := u.Query()
				if s := q.Get("secret"); s != "" {
					secret = s
				}
				if t := q.Get("tier"); tier == "" && t != "" {
					tier = t
				}
			}
		}
		endpoint = "" // fall back to the default Goldsky Edge host
	}

	if endpoint == "" {
		endpoint = defaultGoldskyEndpoint
	}
	if tier == "" {
		tier = defaultGoldskyTier
	}

	// Normalize to something url.Parse can handle: a full http(s) URL or a bare host.
	rawURL := endpoint
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		rawURL = "https://" + endpoint
	}

	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	// Default to the tier route only when the base carries no explicit path.
	basePath := strings.TrimSuffix(parsedURL.Path, "/")
	if basePath == "" {
		basePath = "/" + strings.Trim(tier, "/") + "/evm"
	}
	parsedURL.Path = basePath + "/" + strconv.FormatInt(chainId, 10)

	// Attach the secret unless the base URL already carries one.
	if secret != "" {
		q := parsedURL.Query()
		if q.Get("secret") == "" {
			q.Set("secret", secret)
			parsedURL.RawQuery = q.Encode()
		}
	}

	return parsedURL, nil
}

func (v *GoldskyVendor) getOrCreateClient(ctx context.Context, logger *zerolog.Logger, chainId int64, parsedURL *url.URL) (clients.HttpJsonRpcClient, error) {
	clientKey := fmt.Sprintf("%s-%d", parsedURL.String(), chainId)

	if client, ok := v.headlessClients.Load(clientKey); ok {
		return client.(clients.HttpJsonRpcClient), nil
	}

	u := &phonyUpstream{id: fmt.Sprintf("temp-goldsky-%d", chainId)}
	client, err := clients.NewGenericHttpJsonRpcClient(ctx, logger, "n/a", u, parsedURL, nil, nil, archEvm.NewJsonRpcErrorExtractor())
	if err != nil {
		return nil, err
	}

	v.headlessClients.Store(clientKey, client)
	return client, nil
}
