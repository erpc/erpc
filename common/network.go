package common

type NetworkArchitecture string

const (
	ArchitectureEvm NetworkArchitecture = "evm"
)

type Network interface {
	Id() string
	Architecture() NetworkArchitecture
	EvmChainId() (int64, error)
	EvmIsBlockFinalized(blockNumber int64) (bool, error)
	EvmBlockTracker() EvmBlockTracker
}
