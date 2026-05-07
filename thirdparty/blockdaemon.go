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

// blockdaemonNetworks maps EVM chain IDs to the path suffix used by Blockdaemon's
// shared RPC service. Full URL: https://svc.blockdaemon.com/{path}
//
// Path suffixes vary per chain — some end in /native, others in /native/http-rpc,
// Avalanche uses /native/ext/bc/c/eth (C-Chain EVM), and Arbitrum uses
// /{network}-one/. A single Blockdaemon API key grants access to every chain
// their RPC product supports.
//
// See https://docs.blockdaemon.com/docs/rpc-api
var blockdaemonNetworks = map[int64]string{
	// Ethereum
	1:        "ethereum/mainnet/native",
	11155111: "ethereum/sepolia/native",
	560048:   "ethereum/hoodi/native",
	// Base
	8453:  "base/mainnet/native/http-rpc",
	84532: "base/testnet/native/http-rpc",
	// Optimism
	10: "optimism/mainnet/native/http-rpc",
	// Arbitrum One
	42161:  "arbitrum/mainnet-one/native/http-rpc",
	421614: "arbitrum/sepolia-one/native/http-rpc",
	// Avalanche C-Chain (EVM)
	43114: "avalanche/mainnet/native/ext/bc/c/eth",
	43113: "avalanche/testnet/native/ext/bc/c/eth",
	// Ink
	57073: "ink/mainnet/native",
	// Monad
	10143: "monad/testnet/native",
	// Polygon
	137:   "polygon/mainnet/native/http-rpc",
	80002: "polygon/amoy/native/http-rpc",
	// Tron (TVM is EVM-compatible; path uses /native/jsonrpc)
	728126428: "tron/mainnet/native/jsonrpc",
	3448148188: "tron/nile/native/jsonrpc",
	// X Layer
	196: "xlayer/mainnet/native",
	195: "xlayer/testnet/native",
}

type BlockdaemonVendor struct {
	common.Vendor
}

func CreateBlockdaemonVendor() common.Vendor {
	return &BlockdaemonVendor{}
}

func (v *BlockdaemonVendor) Name() string {
	return "blockdaemon"
}

func (v *BlockdaemonVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}
	_, ok := blockdaemonNetworks[chainID]
	return ok, nil
}

func (v *BlockdaemonVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	apiKey, ok := settings["apiKey"].(string)
	if !ok || apiKey == "" {
		return nil, fmt.Errorf("apiKey is required in blockdaemon settings")
	}

	if upstream.Endpoint == "" {
		if upstream.Evm == nil {
			return nil, fmt.Errorf("blockdaemon vendor requires upstream.evm to be defined")
		}
		chainID := upstream.Evm.ChainId
		if chainID == 0 {
			return nil, fmt.Errorf("blockdaemon vendor requires upstream.evm.chainId to be defined")
		}

		path, ok := blockdaemonNetworks[chainID]
		if !ok {
			return nil, fmt.Errorf("unsupported network chain ID for Blockdaemon: %d", chainID)
		}

		blockdaemonURL := fmt.Sprintf("https://svc.blockdaemon.com/%s", path)
		parsedURL, err := url.Parse(blockdaemonURL)
		if err != nil {
			return nil, err
		}

		upstream.Endpoint = parsedURL.String()
		upstream.Type = common.UpstreamTypeEvm
	}

	if upstream.JsonRpc.Headers == nil {
		upstream.JsonRpc.Headers = make(map[string]string)
	}
	if _, exists := upstream.JsonRpc.Headers["Authorization"]; !exists {
		upstream.JsonRpc.Headers["Authorization"] = "Bearer " + apiKey
	}

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *BlockdaemonVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	bodyMap, ok := jrr.(*common.JsonRpcResponse)
	if !ok {
		return nil
	}

	err := bodyMap.Error
	if err.Data != "" {
		details["data"] = err.Data
	}

	if resp != nil && resp.StatusCode == http.StatusUnauthorized {
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

func (v *BlockdaemonVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "blockdaemon://") || strings.HasPrefix(ups.Endpoint, "evm+blockdaemon://") {
		return true
	}

	return strings.Contains(ups.Endpoint, "svc.blockdaemon.com")
}
