// Package thirdparty — Satelink vendor for erpc (https://github.com/erpc/erpc).
//
// This file is the reference implementation to be submitted upstream as
// thirdparty/satelink.go, following the pattern established by the
// Blockdaemon vendor (erpc PR #861). It is NOT compiled inside the Satelink
// monorepo — it targets erpc's module (github.com/erpc/erpc).
//
// Wiring it into erpc requires two one-line edits alongside this file:
//
//  1. thirdparty/vendors_registry.go — register the vendor:
//
//     r.Register(CreateSatelinkVendor())
//
//  2. common/defaults.go, buildProviderSettings() — parse the URL scheme:
//
//     case "satelink", "evm+satelink":
//         // satelink://<api_key>@polygon  (authority userinfo = API key)
//         // satelink://<api_key>          (authority host = API key)
//         // satelink://free@polygon       ("free" = keyless free tier)
//         settings := VendorSettings{}
//         if endpoint.User != nil && endpoint.User.Username() != "" {
//             settings["apiKey"] = endpoint.User.Username()
//         } else if endpoint.Host != "" {
//             settings["apiKey"] = endpoint.Host
//         }
//         return settings, nil
package thirdparty

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// satelinkNetworks maps EVM chain IDs to the path segment of Satelink's
// gateway. Full URL: https://rpc.satelink.network/rpc/{path}
//
// Satelink currently serves Polygon PoS mainnet only. New chains appear in
// the machine-readable manifest first:
// https://rpc.satelink.network/.well-known/satelink.json
var satelinkNetworks = map[int64]string{
	137: "polygon", // Polygon PoS mainnet
}

const (
	satelinkBaseURL     = "https://rpc.satelink.network/rpc"
	satelinkManifestURL = "https://rpc.satelink.network/.well-known/satelink.json"
	satelinkPricingURL  = "https://rpc.satelink.network/v1/pricing"

	// satelinkFreeKeySentinel in the api-key position selects the keyless
	// free tier (500 calls/day per IP, no registration). Any other value is
	// sent verbatim as the X-API-Key header.
	satelinkFreeKeySentinel = "free"
)

type SatelinkVendor struct {
	common.Vendor
}

func CreateSatelinkVendor() common.Vendor {
	return &SatelinkVendor{}
}

func (v *SatelinkVendor) Name() string {
	return "satelink"
}

func (v *SatelinkVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}
	_, ok := satelinkNetworks[chainID]
	return ok, nil
}

func (v *SatelinkVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint == "" {
		// Unlike most vendors, an absent/empty apiKey is valid: Satelink has
		// a keyless free tier (500 calls/day per IP), selected here by the
		// "free" sentinel or by omitting the key entirely.
		apiKey, _ := settings["apiKey"].(string)
		if apiKey == "" {
			apiKey = satelinkFreeKeySentinel
		}

		if upstream.Evm == nil {
			return nil, fmt.Errorf("satelink vendor requires upstream.evm to be defined")
		}
		chainID := upstream.Evm.ChainId
		if chainID == 0 {
			return nil, fmt.Errorf("satelink vendor requires upstream.evm.chainId to be defined")
		}

		path, ok := satelinkNetworks[chainID]
		if !ok {
			return nil, nil
		}

		satelinkURL := fmt.Sprintf("%s/%s", satelinkBaseURL, path)
		parsedURL, err := url.Parse(satelinkURL)
		if err != nil {
			return nil, err
		}

		upstream.Endpoint = parsedURL.String()
		upstream.Type = common.UpstreamTypeEvm

		// All JSON-RPC methods (eth_blockNumber, eth_call, eth_getLogs,
		// eth_getTransactionReceipt, eth_sendRawTransaction, eth_getBalance,
		// eth_getCode, …) pass through the gateway body-unmodified; the only
		// Satelink-specific wire detail is the X-API-Key header.
		if apiKey != satelinkFreeKeySentinel {
			if upstream.JsonRpc.Headers == nil {
				upstream.JsonRpc.Headers = make(map[string]string)
			}
			if upstream.JsonRpc.Headers["X-API-Key"] == "" && upstream.JsonRpc.Headers["x-api-key"] == "" {
				upstream.JsonRpc.Headers["X-API-Key"] = apiKey
			}
		}
	}

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *SatelinkVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	bodyMap, ok := jrr.(*common.JsonRpcResponse)
	if !ok {
		return nil
	}

	err := bodyMap.Error
	if err.Data != "" {
		details["data"] = err.Data
	}

	if resp == nil {
		return nil
	}

	switch resp.StatusCode {
	case http.StatusPaymentRequired: // 402 — prepaid USDT credits exhausted
		// Satelink's 402 body is self-contained machine-readable onboarding:
		// deposit.vault_address, deposit.usdt_contract, deposit.chain_id,
		// deposit.calldata_url, register.url, manifest_url. Surface the
		// payment pointers in the error metadata so an erpc operator (or an
		// automated payer reading erpc's error details) can fund the key and
		// resume without consulting external docs.
		details["paymentManifestUrl"] = satelinkManifestURL
		details["paymentVaultAddress"] = "0x577D3716d6Ad5b676d230f5409deF9838FABaCEF" // USDT vault, Polygon 137
		details["paymentTokenAddress"] = "0xc2132D05D31c914a87C6611C10748AEb04B58e8F" // USDT (PoS)
		details["paymentChainId"] = 137
		details["paymentCalldataUrl"] = "https://rpc.satelink.network/credits/deposit/initiate?amount=<usdt>"
		details["pricingUrl"] = satelinkPricingURL
		return common.NewErrEndpointBillingIssue(
			common.NewErrJsonRpcExceptionInternal(
				int(common.JsonRpcErrorCapacityExceeded),
				common.JsonRpcErrorCapacityExceeded,
				"satelink credits exhausted — deposit USDT to the vault to continue: "+err.Message,
				nil,
				details,
			),
		)
	case http.StatusTooManyRequests: // 429 — free-tier daily limit / rate limit
		return common.NewErrEndpointCapacityExceeded(
			common.NewErrJsonRpcExceptionInternal(
				int(common.JsonRpcErrorCapacityExceeded),
				common.JsonRpcErrorCapacityExceeded,
				err.Message,
				nil,
				details,
			),
		)
	case http.StatusUnauthorized: // 401 — unknown or revoked API key
		return common.NewErrEndpointUnauthorized(
			common.NewErrJsonRpcExceptionInternal(
				int(common.JsonRpcErrorUnauthorized),
				common.JsonRpcErrorUnauthorized,
				err.Message,
				nil,
				details,
			),
		)
	}

	return nil
}

func (v *SatelinkVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	return strings.HasPrefix(ups.Endpoint, "satelink://") ||
		strings.HasPrefix(ups.Endpoint, "evm+satelink://") ||
		strings.Contains(ups.Endpoint, "rpc.satelink.network")
}
