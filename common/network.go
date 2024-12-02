package common

type NetworkArchitecture string

const (
	ArchitectureEvm NetworkArchitecture = "evm"
)

type Network interface {
	Id() string
	Architecture() NetworkArchitecture
	Config() *NetworkConfig
	EvmChainId() (int64, error)
	EvmStatePollerOf(upstreamId string) EvmStatePoller
}

func IsValidArchitecture(architecture string) bool {
	return architecture == string(ArchitectureEvm) // TODO add more architectures when they are supported
}
