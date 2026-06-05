package common

import "sync/atomic"

// networkAliasResolver maps a networkId (e.g. "evm:42161") to its configured
// network alias (e.g. "arbitrum-one"). It is installed once at startup so that
// network-labeled metrics emitted by components that only know the raw networkId
// — notably the gRPC cache connector, which auto-discovers networks by probing
// chainId and never sees the alias — match the alias used by every other metric.
var networkAliasResolver atomic.Pointer[func(string) string]

// SetNetworkAliasResolver installs the global networkId -> alias resolver. Call
// once during init, before block-head pollers begin emitting metrics. Passing nil
// clears it.
func SetNetworkAliasResolver(f func(networkId string) string) {
	if f == nil {
		networkAliasResolver.Store(nil)
		return
	}
	networkAliasResolver.Store(&f)
}

// NetworkAlias returns the configured alias for networkId, or networkId unchanged
// when no resolver is installed or no alias is configured for it. This mirrors
// NormalizedRequest.NetworkLabel (alias when set, else the networkId), so metrics
// are consistent regardless of which component emits them.
func NetworkAlias(networkId string) string {
	if p := networkAliasResolver.Load(); p != nil {
		if alias := (*p)(networkId); alias != "" {
			return alias
		}
	}
	return networkId
}
