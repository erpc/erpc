package common

import "context"

type UpstreamType string

const (
	UpstreamTypeEvm        UpstreamType = "evm"
	UpstreamTypeEvmErpc    UpstreamType = "evm-erpc"
	UpstreamTypeEvmAlchemy UpstreamType = "evm-alchemy"
)

type Upstream interface {
	Config() *UpstreamConfig
	Vendor() Vendor
	SupportsNetwork(networkId string) (bool, error)
	EvmGetChainId(ctx context.Context) (string, error)
}
