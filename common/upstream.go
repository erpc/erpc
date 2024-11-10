package common

import "context"

type Scope string

const (
	// Policies must be created with a "network" in mind,
	// assuming there will be many upstreams e.g. Retry might endup using a different upstream
	ScopeNetwork Scope = "network"

	// Policies must be created with one only "upstream" in mind
	// e.g. Retry with be towards the same upstream
	ScopeUpstream Scope = "upstream"
)

type UpstreamType string

const (
	UpstreamTypeEvm          UpstreamType = "evm"
	UpstreamTypeEvmAlchemy   UpstreamType = "evm+alchemy"
	UpstreamTypeEvmDrpc      UpstreamType = "evm+drpc"
	UpstreamTypeEvmBlastapi  UpstreamType = "evm+blastapi"
	UpstreamTypeEvmEnvio     UpstreamType = "evm+envio"
	UpstreamTypeEvmPimlico   UpstreamType = "evm+pimlico"
	UpstreamTypeEvmThirdweb  UpstreamType = "evm+thirdweb"
	UpstreamTypeEvmEtherspot UpstreamType = "evm+etherspot"
	UpstreamTypeEvmInfura    UpstreamType = "evm+infura"
)

type EvmSyncingState int

const (
	EvmSyncingStateUnknown EvmSyncingState = iota
	EvmSyncingStateSyncing
	EvmSyncingStateNotSyncing
)

type Upstream interface {
	Config() *UpstreamConfig
	Vendor() Vendor
	SupportsNetwork(networkId string) (bool, error)
	EvmGetChainId(ctx context.Context) (string, error)
	EvmSyncingState() EvmSyncingState
}
