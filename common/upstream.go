package common

type UpstreamType string

const (
	UpstreamTypeEvm     UpstreamType = "evm"
	UpstreamTypeEvmErpc    UpstreamType = "evm-erpc"
	UpstreamTypeEvmAlchemy UpstreamType = "evm-alchemy"
)

type Upstream interface {
	Config() *UpstreamConfig
	Vendor() Vendor
	SupportsNetwork(networkId string) bool
}
