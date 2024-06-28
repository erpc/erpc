package common

type NetworkArchitecture string

const (
	ArchitectureEvm    NetworkArchitecture = "evm"
	ArchitectureSolana NetworkArchitecture = "solana"
)

type Network interface {
	Id() string
	Architecture() NetworkArchitecture
	EvmChainId() (uint64, error)
	EvmIsBlockFinalized(blockNumber uint64) (bool, error)
	EvmBlockTracker() EvmBlockTracker
}
