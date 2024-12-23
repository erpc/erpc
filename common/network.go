package common

type NetworkArchitecture string

const (
	ArchitectureEvm NetworkArchitecture = "evm"
)

type Network interface {
	Id() string
	Architecture() NetworkArchitecture
	Config() *NetworkConfig
	GetMethodMetrics(method string) TrackedMetrics
	EvmChainId() (int64, error)
	EvmStatePollerOf(upstreamId string) EvmStatePoller
}

func IsValidArchitecture(architecture string) bool {
	return architecture == string(ArchitectureEvm) // TODO add more architectures when they are supported
}

type QuantileTracker interface {
	Add(value float64)
	GetQuantile(qtile float64) float64
	P90() float64
	P95() float64
	P99() float64
	Reset()
}

type TrackedMetrics interface {
	ErrorRate() float64
	GetLatencySecs() QuantileTracker
}
